/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package controller

import (
	"context"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/SpareCores/sc-karpenter-nodepool/api/v1alpha1"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/nodepool"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/providers"
)

var (
	sizes2to4 = []string{"c7a.large", "c7a.xlarge", "c7g.large", "c7g.xlarge", "m7a.large", "m7a.xlarge"}
	sizes2to8 = []string{
		"c7a.2xlarge", "c7a.large", "c7a.xlarge", "c7g.2xlarge", "c7g.large", "c7g.xlarge", "m7a.2xlarge", "m7a.large", "m7a.xlarge",
	}
)

func requirementOf(np *unstructured.Unstructured, key string) nodepool.Requirement {
	for _, r := range nodepool.RequirementsOf(np) {
		if r.Key == key {
			return r
		}
	}
	return nodepool.Requirement{}
}

// TestCreateUpdateAndChurn: create → NodePool with the expected
// requirements; spec change → update and Event; narrowing → removals capped
// by churn control, the rest removed on a later refresh.
func TestCreateUpdateAndChurn(t *testing.T) {
	createNodeClass(t, "nc-create", "use1-az1", "use1-az2")
	createPool(t, newPool("web", "nc-create", familiesSelector(2, 4)))
	r, rec := newReconciler()

	res := reconcileOnce(t, r, "web")
	np := checkCreated(t, res)
	if evs := drain(rec); !hasEvent(evs, "Normal NodePoolCreated", "6 instance types") {
		t.Errorf("events %v", evs)
	}

	// Idempotent: no refresh is due, nothing changes.
	rv := np.GetResourceVersion()
	reconcileOnce(t, r, "web")
	if getNodePool(t, "web").GetResourceVersion() != rv || len(drain(rec)) != 0 {
		t.Error("a second reconcile changed something")
	}
	checkChurn(t, r, rec)
}

// checkCreated checks the NodePool and status after the first reconcile of
// TestCreateUpdateAndChurn.
func checkCreated(t *testing.T, res ctrl.Result) *unstructured.Unstructured {
	t.Helper()
	np := getNodePool(t, "web")
	if np == nil {
		t.Fatal("no NodePool")
	}
	if got := nodepool.InstanceTypes(np); !slices.Equal(got, sizes2to4) {
		t.Errorf("instance types %v, want %v", got, sizes2to4)
	}
	if req := requirementOf(np, providers.LabelArch); !slices.Equal(req.Values, []string{"amd64", "arm64"}) {
		t.Errorf("arch %+v", req)
	}
	if req := requirementOf(np, providers.LabelCapacityType); !slices.Equal(req.Values, []string{"on-demand"}) {
		t.Errorf("capacity type %+v", req)
	}
	pool := getPool(t, "web")
	if refs := np.GetOwnerReferences(); len(refs) != 1 || refs[0].UID != pool.UID {
		t.Errorf("owner references %+v", refs)
	}
	if !nodepool.IsOurs(np, "web") || np.GetAnnotations()[nodepool.AnnotationDataTimestamp] == "" {
		t.Errorf("labels %v annotations %v", np.GetLabels(), np.GetAnnotations())
	}
	if got, _, _ := unstructured.NestedString(np.Object, "spec", "disruption", "consolidateAfter"); got != "0s" {
		t.Errorf("consolidateAfter %q", got)
	}

	st := pool.Status
	expectCondition(t, pool, v1alpha1.ConditionReady, metav1.ConditionTrue, v1alpha1.ReasonReady)
	expectCondition(t, pool, v1alpha1.ConditionDataAvailable, metav1.ConditionTrue, "")
	expectCondition(t, pool, v1alpha1.ConditionSelectionNonEmpty, metav1.ConditionTrue, "")
	expectCondition(t, pool, v1alpha1.ConditionNodePoolSynced, metav1.ConditionTrue, "Applied")
	expectCondition(t, pool, v1alpha1.ConditionRegionConflict, metav1.ConditionFalse, "")
	expectCondition(t, pool, v1alpha1.ConditionNodePoolManaged, metav1.ConditionUnknown, "Pending")
	if st.Provider != "aws" || !slices.Equal(st.Regions, []string{"us-east-1"}) || st.RegionSource != v1alpha1.RegionSourceNodeClass ||
		st.NodePoolName != "web" || st.ObservedGeneration != pool.Generation {
		t.Errorf("status %+v", st)
	}
	if st.Selected == nil || st.Selected.Count != 6 || len(st.Selected.InstanceTypes) != 6 || st.Selected.InstanceTypes[0].Price == nil ||
		st.LastRefreshTime == nil || st.DataTimestamp == nil || st.Excluded == nil || st.Excluded.Filters["families"] == 0 {
		t.Errorf("status %+v", st)
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > managedGracePeriod+10*time.Second {
		t.Errorf("requeue after %v, want the NodePoolManaged deadline", res.RequeueAfter)
	}
	return np
}

// checkChurn: widening, narrowing under the removal cap, and a requested
// refresh removing the rest.
func checkChurn(t *testing.T, r *SCNodePoolReconciler, rec *events.FakeRecorder) {
	t.Helper()
	// Widening: applied immediately.
	updatePool(t, "web", func(p *v1alpha1.SCNodePool) { p.Spec.Selector = familiesSelector(2, 8) })
	reconcileOnce(t, r, "web")
	if got := nodepool.InstanceTypes(getNodePool(t, "web")); !slices.Equal(got, sizes2to8) {
		t.Errorf("widened: %v", got)
	}
	if evs := drain(rec); !hasEvent(evs, "NodePoolUpdated", "added 3", "c7a.2xlarge") {
		t.Errorf("events %v", evs)
	}

	// Narrowing to the 2-vCPU sizes: a spec change bypasses hysteresis but
	// not the removal cap (3).
	updatePool(t, "web", func(p *v1alpha1.SCNodePool) { p.Spec.Selector = familiesSelector(2, 2) })
	reconcileOnce(t, r, "web")
	got := nodepool.InstanceTypes(getNodePool(t, "web"))
	pool := getPool(t, "web")
	if len(got) != 6 || len(pool.Status.PendingRemovals) != 3 || pool.Status.Selected.Count != 6 {
		t.Errorf("narrowed: %v, pending %+v", got, pool.Status.PendingRemovals)
	}
	if evs := drain(rec); !hasEvent(evs, "NodePoolUpdated", "removed 3") {
		t.Errorf("events %v", evs)
	}

	// A requested refresh removes the rest (second failing refresh).
	updatePool(t, "web", func(p *v1alpha1.SCNodePool) {
		p.Annotations = map[string]string{AnnotationRefreshRequested: "1"}
	})
	reconcileOnce(t, r, "web")
	if got := nodepool.InstanceTypes(getNodePool(t, "web")); !slices.Equal(got, []string{"c7a.large", "c7g.large", "m7a.large"}) {
		t.Errorf("after the requested refresh: %v", got)
	}
	if p := getPool(t, "web"); len(p.Status.PendingRemovals) != 0 || p.Status.Selected.Count != 3 {
		t.Errorf("status %+v", p.Status)
	}
}

// TestEmptySelectionKeepsNodePool: an empty selection never creates a
// NodePool and keeps an existing one.
func TestEmptySelectionKeepsNodePool(t *testing.T) {
	createNodeClass(t, "nc-empty", "use1-az1")
	createPool(t, newPool("empty-first", "nc-empty", familiesSelector(1000, 1000)))
	createPool(t, newPool("empty-later", "nc-empty", familiesSelector(2, 4)))
	r, _ := newReconciler()

	reconcileOnce(t, r, "empty-first")
	if getNodePool(t, "empty-first") != nil {
		t.Error("a NodePool was created from an empty selection")
	}
	p := getPool(t, "empty-first")
	expectCondition(t, p, v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonNoCandidates)
	expectCondition(t, p, v1alpha1.ConditionSelectionNonEmpty, metav1.ConditionFalse, v1alpha1.ReasonNoCandidates)

	reconcileOnce(t, r, "empty-later")
	before := nodepool.InstanceTypes(getNodePool(t, "empty-later"))
	updatePool(t, "empty-later", func(p *v1alpha1.SCNodePool) { p.Spec.Selector = familiesSelector(1000, 1000) })
	reconcileOnce(t, r, "empty-later")
	if got := nodepool.InstanceTypes(getNodePool(t, "empty-later")); !slices.Equal(got, before) || len(got) == 0 {
		t.Errorf("NodePool changed: %v, was %v", got, before)
	}
	p = getPool(t, "empty-later")
	expectCondition(t, p, v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonNoCandidates)
	if p.Status.Selected == nil || p.Status.Selected.Count != int32(len(before)) {
		t.Errorf("status.selected no longer describes the kept NodePool: %+v", p.Status.Selected)
	}

	// The template's instance-type list not intersecting the selection.
	updatePool(t, "empty-later", func(p *v1alpha1.SCNodePool) {
		p.Spec.Selector = familiesSelector(2, 4)
		p.Spec.NodePoolTemplate.Spec.Raw = templateJSON("karpenter.k8s.aws", "EC2NodeClass", "nc-empty",
			`,"requirements":[{"key":"node.kubernetes.io/instance-type","operator":"In","values":["m5.large"]}]`)
	})
	reconcileOnce(t, r, "empty-later")
	expectCondition(t, getPool(t, "empty-later"), v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonNoCandidates)
	if got := nodepool.InstanceTypes(getNodePool(t, "empty-later")); !slices.Equal(got, before) {
		t.Errorf("NodePool changed: %v", got)
	}
}

// foreignNodePool creates a NodePool not generated by the controller.
func foreignNodePool(t *testing.T, name string) *unstructured.Unstructured {
	t.Helper()
	np := nodepool.New()
	np.SetName(name)
	np.Object["spec"] = map[string]any{
		"disruption": map[string]any{"consolidateAfter": "1m"},
		"template": map[string]any{"spec": map[string]any{
			"nodeClassRef": map[string]any{"group": "karpenter.k8s.aws", "kind": "EC2NodeClass", "name": "x"},
			"requirements": []any{},
		}},
	}
	mustCreate(t, np)
	return np
}

func TestNameConflict(t *testing.T) {
	createNodeClass(t, "nc-conflict", "use1-az1")
	np := foreignNodePool(t, "taken")
	createPool(t, newPool("taken", "nc-conflict", familiesSelector(2, 4)))
	r, _ := newReconciler()
	reconcileOnce(t, r, "taken")
	p := getPool(t, "taken")
	expectCondition(t, p, v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonNameConflict)
	expectCondition(t, p, v1alpha1.ConditionNodePoolSynced, metav1.ConditionFalse, v1alpha1.ReasonNameConflict)
	if got := getNodePool(t, "taken"); got.GetResourceVersion() != np.GetResourceVersion() {
		t.Error("the foreign NodePool was modified")
	}

	// Once the foreign NodePool is gone, the next reconcile creates ours.
	if err := k8sClient.Delete(context.Background(), np); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "taken")
	if !nodepool.IsOurs(getNodePool(t, "taken"), "taken") {
		t.Error("NodePool not created after the conflict was resolved")
	}
}

func TestNodeClassRefImmutable(t *testing.T) {
	createNodeClass(t, "nc-immutable", "use1-az1")
	createPool(t, newPool("immutable", "nc-immutable", familiesSelector(2, 4)))
	r, _ := newReconciler()
	reconcileOnce(t, r, "immutable")
	rv := getNodePool(t, "immutable").GetResourceVersion()

	updatePool(t, "immutable", func(p *v1alpha1.SCNodePool) {
		p.Spec.NodePoolTemplate.Spec.Raw = templateJSON("eks.amazonaws.com", "NodeClass", "nc-immutable", "")
	})
	reconcileOnce(t, r, "immutable")
	p := getPool(t, "immutable")
	expectCondition(t, p, v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonNodeClassRefImmutable)
	if getNodePool(t, "immutable").GetResourceVersion() != rv {
		t.Error("the NodePool was modified")
	}
}

func TestDryRun(t *testing.T) {
	createNodeClass(t, "nc-dry", "use1-az1")
	p := newPool("dry", "nc-dry", familiesSelector(2, 4))
	p.Spec.DryRun = true
	createPool(t, p)
	r, rec := newReconciler()
	reconcileOnce(t, r, "dry")
	if getNodePool(t, "dry") != nil {
		t.Fatal("dryRun created a NodePool")
	}
	p = getPool(t, "dry")
	expectCondition(t, p, v1alpha1.ConditionReady, metav1.ConditionTrue, v1alpha1.ReasonDryRun)
	expectCondition(t, p, v1alpha1.ConditionNodePoolSynced, metav1.ConditionTrue, v1alpha1.ReasonDryRun)
	if p.Status.Selected == nil || p.Status.Selected.Count != 6 {
		t.Errorf("selected %+v", p.Status.Selected)
	}
	if c := cond(p, v1alpha1.ConditionNodePoolManaged); c.Type != "" {
		t.Errorf("health condition without a NodePool: %+v", c)
	}
	if evs := drain(rec); hasEvent(evs, "NodePoolCreated") {
		t.Errorf("events %v", evs)
	}

	// Leaving dry-run mode applies the would-be pool.
	updatePool(t, "dry", func(p *v1alpha1.SCNodePool) { p.Spec.DryRun = false })
	reconcileOnce(t, r, "dry")
	if got := nodepool.InstanceTypes(getNodePool(t, "dry")); !slices.Equal(got, sizes2to4) {
		t.Errorf("applied %v", got)
	}
}

func TestDeletionPolicy(t *testing.T) {
	ctx := context.Background()
	createNodeClass(t, "nc-orphan", "use1-az1")
	p := newPool("orphan", "nc-orphan", familiesSelector(2, 4))
	p.Spec.DeletionPolicy = v1alpha1.DeletionPolicyOrphan
	createPool(t, p)
	r, rec := newReconciler()

	reconcileOnce(t, r, "orphan")
	if !controllerutil.ContainsFinalizer(getPool(t, "orphan"), FinalizerOrphan) {
		t.Error("no finalizer with deletionPolicy Orphan")
	}
	if refs := getNodePool(t, "orphan").GetOwnerReferences(); len(refs) != 0 {
		t.Errorf("owner references with Orphan: %+v", refs)
	}

	// Delete: owned, no finalizer.
	updatePool(t, "orphan", func(p *v1alpha1.SCNodePool) { p.Spec.DeletionPolicy = v1alpha1.DeletionPolicyDelete })
	reconcileOnce(t, r, "orphan")
	if controllerutil.ContainsFinalizer(getPool(t, "orphan"), FinalizerOrphan) {
		t.Error("finalizer with deletionPolicy Delete")
	}
	if refs := getNodePool(t, "orphan").GetOwnerReferences(); len(refs) != 1 {
		t.Errorf("owner references with Delete: %+v", refs)
	}

	// Switched to Orphan and deleted before the NodePool was re-applied:
	// the finalizer removes the ownerReference before the object goes.
	updatePool(t, "orphan", func(p *v1alpha1.SCNodePool) {
		p.Spec.DeletionPolicy = v1alpha1.DeletionPolicyOrphan
		controllerutil.AddFinalizer(p, FinalizerOrphan)
	})
	if err := k8sClient.Delete(ctx, getPool(t, "orphan")); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "orphan")
	var gone v1alpha1.SCNodePool
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "orphan"}, &gone); !apierrors.IsNotFound(err) {
		t.Errorf("SCNodePool still exists: %v", err)
	}
	np := getNodePool(t, "orphan")
	if np == nil || len(np.GetOwnerReferences()) != 0 {
		t.Errorf("NodePool not orphaned: %+v", np)
	}
	if evs := drain(rec); !hasEvent(evs, "NodePoolOrphaned") {
		t.Errorf("events %v", evs)
	}
}

func TestRegionPendingAndUnknown(t *testing.T) {
	createPool(t, newPool("regionless", "nc-missing", familiesSelector(2, 4)))
	r, _ := newReconciler()
	res := reconcileOnce(t, r, "regionless")
	p := getPool(t, "regionless")
	expectCondition(t, p, v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonRegionPending)
	if getNodePool(t, "regionless") != nil || p.Status.Provider != "aws" {
		t.Errorf("NodePool created or provider %q", p.Status.Provider)
	}
	if res.RequeueAfter != retryBase {
		t.Errorf("requeue after %v, want %v", res.RequeueAfter, retryBase)
	}

	// A node in a region Spare Cores does not know, after the backoff.
	createNode(t, "node-mars", map[string]string{providers.LabelRegion: "mars-north-1"})
	reconcileOnce(t, r, "regionless")
	expectCondition(t, getPool(t, "regionless"), v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonRegionPending)
	r.Now = func() time.Time { return time.Now().Add(time.Minute) }
	reconcileOnce(t, r, "regionless")
	p = getPool(t, "regionless")
	expectCondition(t, p, v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonRegionUnknown)
	if p.Status.RegionSource != v1alpha1.RegionSourceNodeLabels || getNodePool(t, "regionless") != nil {
		t.Errorf("status %+v", p.Status)
	}
}

func TestRegionConflict(t *testing.T) {
	createNodeClass(t, "nc-conflicting", "use1-az1")
	createNode(t, "node-eu", map[string]string{
		providers.LabelRegion:            "eu-west-1",
		"karpenter.k8s.aws/ec2nodeclass": "nc-conflicting",
	})
	createPool(t, newPool("conflicting", "nc-conflicting", familiesSelector(2, 4)))
	r, rec := newReconciler()
	reconcileOnce(t, r, "conflicting")
	p := getPool(t, "conflicting")
	expectCondition(t, p, v1alpha1.ConditionRegionConflict, metav1.ConditionTrue, "SourcesDisagree")
	expectCondition(t, p, v1alpha1.ConditionReady, metav1.ConditionTrue, v1alpha1.ReasonReady)
	if !slices.Equal(p.Status.Regions, []string{"us-east-1"}) {
		t.Errorf("regions %v: the NodeClass must win", p.Status.Regions)
	}
	if evs := drain(rec); !hasEvent(evs, "Warning RegionConflict", "eu-west-1") {
		t.Errorf("events %v", evs)
	}
}

func TestPreflightFailures(t *testing.T) {
	cases := []struct {
		name   string
		spec   []byte
		reason string
		msg    string
	}{
		{"unknown group", templateJSON("karpenter.example.com", "FooNodeClass", "x", ""), v1alpha1.ReasonUnsupportedProvider, "karpenter.example.com"},
		{"upcloud-tools", templateJSON("karpenter.k8s.upcloud", "UpCloudNodeClass", "x", ""), v1alpha1.ReasonUnsupportedProvider, "v1alpha2"},
		{"spot on Vultr", templateJSON("karpenter.vultr.com", "VultrNodeClass", "x",
			`,"requirements":[{"key":"karpenter.sh/capacity-type","operator":"In","values":["spot"]}]`),
			v1alpha1.ReasonCapacityTypeUnsupported, "spot"},
		{"arm64 on Vultr", templateJSON("karpenter.vultr.com", "VultrNodeClass", "x",
			`,"requirements":[{"key":"kubernetes.io/arch","operator":"In","values":["arm64"]}]`),
			v1alpha1.ReasonArchitectureUnsupported, "arm64"},
		{"os on Auto Mode", templateJSON("eks.amazonaws.com", "NodeClass", "x",
			`,"requirements":[{"key":"kubernetes.io/os","operator":"In","values":["linux"]}]`),
			v1alpha1.ReasonInvalidTemplate, "kubernetes.io/os"},
	}
	r, _ := newReconciler()
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			name := "preflight-" + string(rune('a'+i))
			p := newPool(name, "x", nil)
			p.Spec.NodePoolTemplate.Spec = runtime.RawExtension{Raw: c.spec}
			createPool(t, p)
			if res := reconcileOnce(t, r, name); res.RequeueAfter != 0 {
				t.Errorf("requeue after %v for a permanent failure", res.RequeueAfter)
			}
			p = getPool(t, name)
			expectCondition(t, p, v1alpha1.ConditionReady, metav1.ConditionFalse, c.reason)
			if msg := cond(p, v1alpha1.ConditionReady).Message; !strings.Contains(msg, c.msg) {
				t.Errorf("message %q lacks %q", msg, c.msg)
			}
			if getNodePool(t, name) != nil {
				t.Error("NodePool created")
			}
		})
	}
}

func TestUnknownBenchmarkConfig(t *testing.T) {
	createNodeClass(t, "nc-bench", "use1-az1")
	sel := familiesSelector(2, 4)
	sel.Benchmarks = []v1alpha1.BenchmarkFilter{{BenchmarkRef: v1alpha1.BenchmarkRef{ID: "geekbench:score"}, Min: new(1.0)}}
	createPool(t, newPool("bench", "nc-bench", sel))
	r, _ := newReconciler()
	reconcileOnce(t, r, "bench")
	p := getPool(t, "bench")
	expectCondition(t, p, v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonUnknownBenchmarkConfig)
	expectCondition(t, p, v1alpha1.ConditionDataAvailable, metav1.ConditionFalse, v1alpha1.ReasonUnknownBenchmarkConfig)
}

// TestDataStaleKeepsNodePool: Keeper unreachable → the NodePool is kept,
// DataAvailable=False, retried with backoff.
func TestDataStaleKeepsNodePool(t *testing.T) {
	createNodeClass(t, "nc-stale", "use1-az1")
	createPool(t, newPool("stale", "nc-stale", familiesSelector(2, 4)))
	r, _ := newReconciler()
	reconcileOnce(t, r, "stale")
	rv := getNodePool(t, "stale").GetResourceVersion()

	dead := httptest.NewServer(nil)
	dead.Close()
	r2, _ := newReconciler()
	r2.Data = newDataSource(dead.URL)
	res := reconcileOnce(t, r2, "stale")
	p := getPool(t, "stale")
	expectCondition(t, p, v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonDataStale)
	expectCondition(t, p, v1alpha1.ConditionDataAvailable, metav1.ConditionFalse, v1alpha1.ReasonDataStale)
	expectCondition(t, p, v1alpha1.ConditionNodePoolSynced, metav1.ConditionTrue, "Applied")
	if getNodePool(t, "stale").GetResourceVersion() != rv || res.RequeueAfter != retryBase {
		t.Errorf("NodePool changed or requeue %v", res.RequeueAfter)
	}
}

func TestNodeClassRefOverride(t *testing.T) {
	createNodeClass(t, "nc-override", "use1-az1")
	createPool(t, newPool("override", "nc-override", familiesSelector(2, 4)))
	r, _ := newReconciler()
	r.NodeClassRefOverride = &nodepool.NodeClassRef{Group: "karpenter.kwok.sh", Kind: "KWOKNodeClass", Name: "default"}
	reconcileOnce(t, r, "override")
	np := getNodePool(t, "override")
	if ref, _ := nodepool.NodeClassRefOf(np); ref != *r.NodeClassRefOverride {
		t.Errorf("nodeClassRef %+v", ref)
	}
	if p := getPool(t, "override"); p.Status.Provider != "aws" || !slices.Equal(p.Status.Regions, []string{"us-east-1"}) {
		t.Errorf("adapter and regions must come from the template: %+v", p.Status)
	}
}

// setStatusConditions writes status.conditions of a Karpenter object.
func setStatusConditions(t *testing.T, u *unstructured.Unstructured, conds ...map[string]any) {
	t.Helper()
	list := make([]any, len(conds))
	for i, c := range conds {
		if _, ok := c["lastTransitionTime"]; !ok {
			c["lastTransitionTime"] = time.Now().UTC().Format(time.RFC3339)
		}
		if _, ok := c["message"]; !ok {
			c["message"] = ""
		}
		list[i] = c
	}
	if err := unstructured.SetNestedSlice(u.Object, list, "status", "conditions"); err != nil {
		t.Fatal(err)
	}
	if err := k8sClient.Status().Update(context.Background(), u); err != nil {
		t.Fatal(err)
	}
}

// createNodeClaim creates a NodeClaim of the pool with status conditions.
func createNodeClaim(t *testing.T, name, pool string, conds ...map[string]any) {
	t.Helper()
	nc := &unstructured.Unstructured{}
	nc.SetGroupVersionKind(NodeClaimGVK)
	nc.SetName(name)
	nc.SetLabels(map[string]string{LabelNodePool: pool})
	nc.Object["spec"] = map[string]any{
		"nodeClassRef": map[string]any{"group": "karpenter.k8s.aws", "kind": "EC2NodeClass", "name": "x"},
		"requirements": []any{map[string]any{"key": providers.LabelInstanceType, "operator": "In", "values": []any{"c7a.large"}}},
	}
	mustCreate(t, nc)
	if len(conds) > 0 {
		setStatusConditions(t, nc, conds...)
	}
}

func TestHealthConditions(t *testing.T) {
	createNodeClass(t, "nc-health", "use1-az1")
	createPool(t, newPool("health", "nc-health", familiesSelector(2, 4)))
	r, rec := newReconciler()
	reconcileOnce(t, r, "health")

	// No Karpenter status after 2 minutes: not managed.
	r.Now = func() time.Time { return time.Now().Add(3 * time.Minute) }
	reconcileOnce(t, r, "health")
	p := getPool(t, "health")
	expectCondition(t, p, v1alpha1.ConditionNodePoolManaged, metav1.ConditionFalse, "NoStatus")
	expectCondition(t, p, v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonNodePoolNotManaged)
	if evs := drain(rec); !hasEvent(evs, "Warning NodePoolNotManaged") {
		t.Errorf("events %v", evs)
	}

	// Karpenter reports status; validation failed.
	np := getNodePool(t, "health")
	setStatusConditions(t, np,
		map[string]any{"type": "Ready", "status": "False", "reason": "NotReady"},
		map[string]any{"type": "ValidationSucceeded", "status": "False", "reason": "Invalid", "message": "bad requirement"})
	reconcileOnce(t, r, "health")
	p = getPool(t, "health")
	expectCondition(t, p, v1alpha1.ConditionNodePoolManaged, metav1.ConditionTrue, "StatusReported")
	expectCondition(t, p, v1alpha1.ConditionNodePoolValid, metav1.ConditionFalse, "Invalid")
	expectCondition(t, p, v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonNodePoolInvalid)

	// Valid, but Karpenter reports NoCompatibleInstanceTypes.
	np = getNodePool(t, "health")
	setStatusConditions(t, np,
		map[string]any{"type": "Ready", "status": "True", "reason": "Ready"},
		map[string]any{"type": "ValidationSucceeded", "status": "True", "reason": "ValidationSucceeded"})
	seen := metav1.NewTime(time.Now().Add(-10 * time.Second))
	mustCreate(t, &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "health.nocompat", Namespace: "default"},
		InvolvedObject: corev1.ObjectReference{APIVersion: "karpenter.sh/v1", Kind: "NodePool", Name: "health"},
		Reason:         reasonNoCompatibleInstanceTypes,
		Message:        "NodePool requirements filtered out all compatible available instance types",
		Type:           "Warning",
		FirstTimestamp: seen,
		LastTimestamp:  seen,
	})
	r.Now = time.Now
	reconcileOnce(t, r, "health")
	p = getPool(t, "health")
	expectCondition(t, p, v1alpha1.ConditionNodePoolValid, metav1.ConditionTrue, "")
	expectCondition(t, p, v1alpha1.ConditionNodePoolSchedulable, metav1.ConditionFalse, reasonNoCompatibleInstanceTypes)
	expectCondition(t, p, v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonNodePoolUnschedulable)

	// A NodeClaim of the pool launches afterwards: schedulable again.
	createNodeClaim(t, "health-launched", "health", map[string]any{"type": "Launched", "status": "True", "reason": "Launched"})
	reconcileOnce(t, r, "health")
	p = getPool(t, "health")
	expectCondition(t, p, v1alpha1.ConditionNodePoolSchedulable, metav1.ConditionTrue, "NodeClaimLaunched")
	expectCondition(t, p, v1alpha1.ConditionReady, metav1.ConditionTrue, v1alpha1.ReasonReady)

	// Drift loop: more than 3 NodeClaims launched after the last apply and
	// marked RequirementsDrifted.
	drain(rec)
	for _, n := range []string{"a", "b", "c", "d"} {
		createNodeClaim(t, "health-drift-"+n, "health",
			map[string]any{"type": "Drifted", "status": "True", "reason": "RequirementsDrifted"})
	}
	reconcileOnce(t, r, "health")
	if evs := drain(rec); !hasEvent(evs, "Warning DriftLoop", "4 NodeClaims") {
		t.Errorf("events %v", evs)
	}
	reconcileOnce(t, r, "health")
	if evs := drain(rec); hasEvent(evs, "DriftLoop") {
		t.Error("the drift-loop warning repeated within its window")
	}
}

// TestRefreshDue covers the scheduling decision without the API server.
func TestRefreshDue(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	base := func() *run {
		pool := &v1alpha1.SCNodePool{ObjectMeta: metav1.ObjectMeta{Name: "p", Generation: 2}}
		pool.Spec.Refresh.Interval = "6h"
		return &run{pool: pool, now: now, np: nodepool.New(), mem: &poolMemory{
			attempted: true, lastAttempt: now.Add(-time.Hour), generation: 2, applied: true,
		}}
	}
	cases := []struct {
		name   string
		mutate func(*run)
		due    bool
	}{
		{"nothing changed", func(*run) {}, false},
		{"never attempted", func(ru *run) { ru.mem.attempted = false }, true},
		{"blocked before", func(ru *run) { ru.mem.blocked = true }, true},
		{"spec changed", func(ru *run) { ru.pool.Generation = 3 }, true},
		{"refresh requested", func(ru *run) { ru.pool.Annotations = map[string]string{AnnotationRefreshRequested: "x"} }, true},
		{"NodeClass changed", func(ru *run) {
			ru.nodeClass = &unstructured.Unstructured{Object: map[string]any{}}
			ru.nodeClass.SetResourceVersion("7")
		}, true},
		{"NodePool deleted", func(ru *run) { ru.np = nil }, true},
		{"NodePool deleted in dryRun", func(ru *run) { ru.np, ru.pool.Spec.DryRun = nil, true }, false},
		{"retry pending", func(ru *run) { ru.mem.retryAt = now.Add(time.Second) }, false},
		{"retry due", func(ru *run) { ru.mem.retryAt = now }, true},
		{"interval passed", func(ru *run) { ru.mem.lastAttempt = now.Add(-6 * time.Hour) }, true},
		{"bad interval falls back to 24h", func(ru *run) {
			ru.pool.Spec.Refresh.Interval = "x"
			ru.mem.lastAttempt = now.Add(-23 * time.Hour)
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ru := base()
			c.mutate(ru)
			if got := ru.refreshDue(); got != c.due {
				t.Errorf("due = %v, want %v", got, c.due)
			}
		})
	}
	if got := base().nextRefresh(); !got.Equal(now.Add(5 * time.Hour)) {
		t.Errorf("next refresh %v", got)
	}
}

func TestDiffMessage(t *testing.T) {
	if got := diffMessage([]string{"a", "b"}, []string{"b", "c"}); got != "added 1 (c); removed 1 (a); 2 instance types" {
		t.Errorf("got %q", got)
	}
	if got := diffMessage([]string{"a"}, []string{"a"}); !strings.Contains(got, "unchanged") {
		t.Errorf("got %q", got)
	}
	many := make([]string, 0, 25)
	for i := range 25 {
		many = append(many, string(rune('A'+i)))
	}
	if got := diffMessage(nil, many); !strings.Contains(got, "and 5 more") {
		t.Errorf("got %q", got)
	}
}
