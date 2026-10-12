/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package controller holds the SCNodePool reconciler: it turns an SCNodePool
// into exactly one Karpenter NodePool (overview §3), keeps it up to date as
// Spare Cores data changes, and reports the health of the generated pool.
package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/SpareCores/sc-karpenter-nodepool/api/v1alpha1"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/nodepool"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/providers"
)

const (
	// AnnotationRefreshRequested: a new value triggers an immediate refresh.
	AnnotationRefreshRequested = "karpenter.sparecores.com/refresh-requested"
	// FinalizerOrphan is set with deletionPolicy Orphan: on deletion the
	// controller removes its ownerReference from the NodePool first, so the
	// garbage collector keeps the NodePool.
	FinalizerOrphan = "karpenter.sparecores.com/orphan-nodepool"
	// LabelNodePool is the label Karpenter core stamps on NodeClaims.
	LabelNodePool = "karpenter.sh/nodepool"
	// eventNameIndex indexes Events by involvedObject.name (also a field
	// selector the API server supports, so uncached clients work too).
	eventNameIndex = "involvedObject.name"
	// reasonNoCompatibleInstanceTypes is Karpenter's Warning event when a
	// NodePool's requirements filter out every instance type (§3.6).
	reasonNoCompatibleInstanceTypes = "NoCompatibleInstanceTypes"
	// controllerName is the reporting controller of our Events.
	controllerName = "sc-karpenter-nodepool"
)

// NodeClaimGVK is Karpenter core's NodeClaim.
var NodeClaimGVK = providers.NodeClaimListGVK.GroupVersion().WithKind("NodeClaim")

// SCNodePoolReconciler reconciles SCNodePools.
type SCNodePoolReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
	Registry *providers.Registry
	Data     *DataSource
	// NodeClassRefOverride replaces the nodeClassRef of every generated
	// NodePool (test mode, feature F12).
	NodeClassRefOverride *nodepool.NodeClassRef
	// Now is the clock (tests); nil means time.Now.
	Now func() time.Time

	mu     sync.Mutex
	memory map[string]*poolMemory
}

// poolMemory is what the reconciler remembers about one SCNodePool between
// reconciles. It is in memory only: after a restart every SCNodePool is
// refreshed once. controller-runtime never reconciles one object
// concurrently, so only the map itself needs the mutex.
type poolMemory struct {
	// attempted: a refresh ran since the start; lastAttempt is when.
	attempted   bool
	lastAttempt time.Time
	// The inputs of the last refresh.
	generation       int64
	annotation       string
	nodeClassVersion string
	// applied: the last refresh applied the NodePool.
	applied bool
	// retryAt and failures implement the backoff of transient failures.
	retryAt  time.Time
	failures int
	// blocked: the last reconcile stopped before refreshing (preflight).
	blocked bool
	// pipeline is the failure of the last refresh or preflight (nil: ok).
	pipeline *failure
	// drifted are NodeClaims seen RequirementsDrifted (drift-loop detector).
	drifted          map[types.UID]time.Time
	lastDriftWarning time.Time
	zoneWarnedGen    int64
}

func (r *SCNodePoolReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *SCNodePoolReconciler) memoryOf(name string) *poolMemory {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.memory == nil {
		r.memory = map[string]*poolMemory{}
	}
	m, ok := r.memory[name]
	if !ok {
		m = &poolMemory{drifted: map[types.UID]time.Time{}}
		r.memory[name] = m
	}
	return m
}

func (r *SCNodePoolReconciler) forget(name string) {
	r.mu.Lock()
	delete(r.memory, name)
	r.mu.Unlock()
	forgetMetrics(name)
}

// +kubebuilder:rbac:groups=karpenter.sparecores.com,resources=scnodepools,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=karpenter.sparecores.com,resources=scnodepools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=karpenter.sparecores.com,resources=scnodepools/finalizers,verbs=update
// +kubebuilder:rbac:groups=karpenter.sh,resources=nodepools,verbs=get;list;watch;create;patch;update
// +kubebuilder:rbac:groups=karpenter.sh,resources=nodeclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// NodeClasses of every adapter (overview §6.1):
// +kubebuilder:rbac:groups=karpenter.k8s.aws,resources=ec2nodeclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=eks.amazonaws.com,resources=nodeclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=karpenter.azure.com,resources=aksnodeclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=karpenter.k8s.gcp,resources=gcenodeclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=karpenter.alibabacloud.com,resources=ecsnodeclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=karpenter.k8s.alibabacloud,resources=ecsnodeclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=karpenter.hetzner.cloud,resources=hcloudnodeclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=karpenter.itsh.dev,resources=hcloudnodeclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=karpenter.k8s.upcloud,resources=upcloudnodeclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=karpenter.ovhcloud.sh,resources=ovhnodeclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=karpenter.vultr.com,resources=vultrnodeclasses,verbs=get;list;watch

// Reconcile refreshes the selection when it is due (overview §3.4), applies
// the NodePool, reports the health of the generated pool and updates status.
func (r *SCNodePoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pool v1alpha1.SCNodePool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		if apierrors.IsNotFound(err) {
			r.forget(req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("getting SCNodePool %s: %w", req.Name, err)
	}
	if !pool.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, &pool)
	}
	if err := r.ensureFinalizer(ctx, &pool); err != nil {
		return ctrl.Result{}, err
	}

	start := time.Now()
	ru := &run{r: r, pool: &pool, orig: pool.DeepCopy(), mem: r.memoryOf(pool.Name), now: r.now()}
	refreshed, err := ru.execute(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !equality.Semantic.DeepEqual(ru.orig.Status, pool.Status) {
		if err := r.Status().Patch(ctx, &pool, client.MergeFrom(ru.orig)); err != nil {
			return ctrl.Result{}, fmt.Errorf("updating status of SCNodePool %s: %w", pool.Name, err)
		}
	}
	recordConditions(pool.Name, pool.Status.Conditions)
	if pool.Status.Selected != nil {
		selectedTypes.WithLabelValues(pool.Name).Set(float64(pool.Status.Selected.Count))
	}
	if refreshed {
		result := v1alpha1.ReasonReady
		if ru.mem.pipeline != nil {
			result = ru.mem.pipeline.reason
		}
		refreshDuration.WithLabelValues(result).Observe(time.Since(start).Seconds())
	}
	return ctrl.Result{RequeueAfter: ru.requeueAfter()}, nil
}

// ensureFinalizer keeps FinalizerOrphan in sync with deletionPolicy.
func (r *SCNodePoolReconciler) ensureFinalizer(ctx context.Context, pool *v1alpha1.SCNodePool) error {
	want := pool.Spec.DeletionPolicy == v1alpha1.DeletionPolicyOrphan
	if want == controllerutil.ContainsFinalizer(pool, FinalizerOrphan) {
		return nil
	}
	orig := pool.DeepCopy()
	if want {
		controllerutil.AddFinalizer(pool, FinalizerOrphan)
	} else {
		controllerutil.RemoveFinalizer(pool, FinalizerOrphan)
	}
	if err := r.Patch(ctx, pool, client.MergeFrom(orig)); err != nil {
		return fmt.Errorf("updating finalizers of SCNodePool %s: %w", pool.Name, err)
	}
	return nil
}

// finalize handles a deleted SCNodePool. With deletionPolicy Delete the
// garbage collector removes the NodePool through its ownerReference; with
// Orphan the ownerReference is removed first so the NodePool stays.
func (r *SCNodePoolReconciler) finalize(ctx context.Context, pool *v1alpha1.SCNodePool) error {
	if !controllerutil.ContainsFinalizer(pool, FinalizerOrphan) {
		return nil
	}
	if pool.Spec.DeletionPolicy == v1alpha1.DeletionPolicyOrphan {
		np, err := r.getNodePool(ctx, pool.Name)
		if err != nil {
			return err
		}
		if np != nil {
			refs := np.GetOwnerReferences()
			kept := refs[:0:0]
			for _, ref := range refs {
				if ref.UID != pool.UID {
					kept = append(kept, ref)
				}
			}
			if len(kept) != len(refs) {
				orig := np.DeepCopy()
				np.SetOwnerReferences(kept)
				if err := r.Patch(ctx, np, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); err != nil {
					return fmt.Errorf("orphaning NodePool %s: %w", np.GetName(), err)
				}
			}
			r.Recorder.Eventf(pool, nil, corev1.EventTypeNormal, "NodePoolOrphaned", "Delete",
				"NodePool %s is kept (deletionPolicy Orphan)", np.GetName())
		}
	}
	orig := pool.DeepCopy()
	controllerutil.RemoveFinalizer(pool, FinalizerOrphan)
	if err := r.Patch(ctx, pool, client.MergeFrom(orig)); err != nil {
		return fmt.Errorf("removing the finalizer of SCNodePool %s: %w", pool.Name, err)
	}
	r.forget(pool.Name)
	return nil
}

// getNodePool returns the NodePool of the given name, or nil.
func (r *SCNodePoolReconciler) getNodePool(ctx context.Context, name string) (*unstructured.Unstructured, error) {
	np := nodepool.New()
	if err := r.Get(ctx, client.ObjectKey{Name: name}, np); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("getting NodePool %s: %w", name, err)
	}
	return np, nil
}

// CacheOptions restrict the manager's Event informer to NodePool events
// (the only ones the controller reads).
func CacheOptions() cache.Options {
	return cache.Options{ByObject: map[client.Object]cache.ByObject{
		&corev1.Event{}: {Field: fields.OneTermEqualSelector("involvedObject.kind", "NodePool")},
	}}
}

// SetupOptions configure SetupWithManager.
type SetupOptions struct {
	MaxConcurrentReconciles int
	// NodeClassDiscoveryInterval (default 5 minutes).
	NodeClassDiscoveryInterval time.Duration
}

// SetupWithManager registers the reconciler, its watches (SCNodePools,
// NodePools, NodeClaims, NodePool Events, installed NodeClasses) and field
// indexes. The manager's cache should use CacheOptions.
func (r *SCNodePoolReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager, opts SetupOptions) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorder(controllerName)
	}
	idx := mgr.GetFieldIndexer()
	if err := idx.IndexField(ctx, &v1alpha1.SCNodePool{}, nodeClassRefIndex, indexNodeClassRef); err != nil {
		return fmt.Errorf("indexing SCNodePools: %w", err)
	}
	if err := idx.IndexField(ctx, &corev1.Event{}, eventNameIndex, func(o client.Object) []string {
		return []string{o.(*corev1.Event).InvolvedObject.Name}
	}); err != nil {
		return fmt.Errorf("indexing Events: %w", err)
	}

	byName := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: o.GetName()}}}
	})
	byNodePoolLabel := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
		if name := o.GetLabels()[LabelNodePool]; name != "" {
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: name}}}
		}
		return nil
	})
	byInvolvedObject := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
		e, ok := o.(*corev1.Event)
		if !ok || e.InvolvedObject.Kind != "NodePool" || e.Reason != reasonNoCompatibleInstanceTypes {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: e.InvolvedObject.Name}}}
	})
	claim := &unstructured.Unstructured{}
	claim.SetGroupVersionKind(NodeClaimGVK)

	c, err := ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.SCNodePool{}, builder.WithPredicates(predicate.Or[client.Object](
			predicate.GenerationChangedPredicate{},
			predicate.AnnotationChangedPredicate{},
			deletionPredicate(),
		))).
		Watches(nodepool.New(), byName).
		Watches(claim, byNodePoolLabel).
		Watches(&corev1.Event{}, byInvolvedObject).
		WithOptions(controller.Options{MaxConcurrentReconciles: max(opts.MaxConcurrentReconciles, 1)}).
		Named("scnodepool").
		Build(r)
	if err != nil {
		return err
	}
	interval := opts.NodeClassDiscoveryInterval
	if interval <= 0 {
		interval = DefaultNodeClassDiscoveryInterval
	}
	return mgr.Add(&nodeClassWatcher{
		controller: c,
		cache:      mgr.GetCache(),
		mapper:     mgr.GetRESTMapper(),
		reader:     mgr.GetClient(),
		registry:   r.Registry,
		interval:   interval,
		watched:    map[schema.GroupKind]bool{},
	})
}

// deletionPredicate passes updates that start the deletion of an object.
func deletionPredicate() predicate.Funcs {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			return e.ObjectOld.GetDeletionTimestamp().IsZero() && !e.ObjectNew.GetDeletionTimestamp().IsZero()
		},
	}
}
