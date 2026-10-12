//go:build kwok

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package e2e holds the scenarios of the KWOK lane (L3, docs/plans/v1/wip/
// 00-overview.md §8.2). test/kwok/run.sh builds the cluster they run in: core
// Karpenter with the KWOK provider, the controller under test with the
// test-mode nodeClassRef override, and a Keeper replay server.
package e2e

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/SpareCores/sc-karpenter-nodepool/api/v1alpha1"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/nodepool"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/providers"
)

const (
	namespace = "default"
	// karpenterNamespace, karpenterDeployment and catalogEnv: where the chart
	// installed the KWOK provider and the variable that selects its catalog.
	karpenterNamespace  = "kube-system"
	karpenterDeployment = "karpenter"
	catalogEnv          = "INSTANCE_TYPES_FILE_PATH"

	labelNodePool = "karpenter.sh/nodepool"
)

var (
	k8s client.Client
	// coreMinor is the Karpenter core minor line under test (v1.2 ... v1.14).
	coreMinor string
)

func TestMain(m *testing.M) {
	coreMinor = os.Getenv("KWOK_CORE_VERSION")
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, v1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	cfg, err := ctrl.GetConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "kubeconfig:", err)
		os.Exit(1)
	}
	cfg.QPS, cfg.Burst = 50, 100
	if k8s, err = client.New(cfg, client.Options{Scheme: scheme}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func ctx() context.Context { return context.Background() }

// eventually polls cond until it returns true; on timeout the test fails with
// the last explanation cond gave.
func eventually(t *testing.T, timeout, interval time.Duration, what string, cond func() (bool, string)) {
	t.Helper()
	start := time.Now()
	last := ""
	for {
		ok, why := cond()
		if ok {
			t.Logf("%s (%s)", what, time.Since(start).Round(100*time.Millisecond))
			return
		}
		last = why
		if time.Since(start) > timeout {
			t.Fatalf("timed out after %s waiting for: %s; last state: %s", timeout, what, last)
		}
		time.Sleep(interval)
	}
}

func newClaimList() *unstructured.UnstructuredList {
	l := &unstructured.UnstructuredList{}
	l.SetAPIVersion("karpenter.sh/v1")
	l.SetKind("NodeClaimList")
	return l
}

// claim is the part of a NodeClaim the scenarios look at.
type claim struct {
	Name         string
	InstanceType string
	Zone         string
	CapacityType string
	Arch         string
	Deleting     bool
	// Drifted: the Drifted condition is True with reason RequirementsDrifted.
	Drifted bool
}

func (c claim) String() string {
	return fmt.Sprintf("%s(%s %s %s %s)", c.Name, c.InstanceType, c.Zone, c.CapacityType, c.Arch)
}

func listClaims(pool string) ([]claim, error) {
	l := newClaimList()
	if err := k8s.List(ctx(), l, client.MatchingLabels{labelNodePool: pool}); err != nil {
		return nil, err
	}
	out := make([]claim, 0, len(l.Items))
	for i := range l.Items {
		u := &l.Items[i]
		lb := u.GetLabels()
		c := claim{
			Name: u.GetName(), Deleting: u.GetDeletionTimestamp() != nil,
			InstanceType: lb[providers.LabelInstanceType], Zone: lb[providers.LabelZone],
			CapacityType: lb[providers.LabelCapacityType], Arch: lb[providers.LabelArch],
		}
		if cond, ok := nodepool.FindCondition(nodepool.Conditions(u), "Drifted"); ok &&
			cond.Status == metav1.ConditionTrue && cond.Reason == "RequirementsDrifted" {
			c.Drifted = true
		}
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b claim) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func live(cs []claim) []claim {
	return slices.DeleteFunc(slices.Clone(cs), func(c claim) bool { return c.Deleting })
}

func getNodePool(name string) (*unstructured.Unstructured, error) {
	np := nodepool.New()
	if err := k8s.Get(ctx(), client.ObjectKey{Name: name}, np); err != nil {
		return nil, err
	}
	return np, nil
}

func getPool(name string) (*v1alpha1.SCNodePool, error) {
	p := &v1alpha1.SCNodePool{}
	return p, k8s.Get(ctx(), client.ObjectKey{Name: name}, p)
}

func condition(p *v1alpha1.SCNodePool, t string) *metav1.Condition {
	for i := range p.Status.Conditions {
		if p.Status.Conditions[i].Type == t {
			return &p.Status.Conditions[i]
		}
	}
	return nil
}

// updatePool applies mutate to the SCNodePool, retrying on conflicts with the
// controller's status updates.
func updatePool(t *testing.T, name string, mutate func(*v1alpha1.SCNodePool)) {
	t.Helper()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		p, err := getPool(name)
		if err != nil {
			return err
		}
		mutate(p)
		return k8s.Update(ctx(), p)
	})
	if err != nil {
		t.Fatalf("updating SCNodePool %s: %v", name, err)
	}
}

// requirementValues returns the values of the NodePool's requirement on key.
func requirementValues(np *unstructured.Unstructured, key string) ([]string, bool) {
	for _, r := range nodepool.RequirementsOf(np) {
		if r.Key == key {
			return r.Values, true
		}
	}
	return nil, false
}

// deleteAndWait removes the load and the SCNodePool of a scenario and waits
// until the NodePool, its NodeClaims and its nodes are gone, so the next
// scenario (and the next catalog) starts from a clean cluster.
func deleteAndWait(t *testing.T, pool string, deployments ...string) {
	t.Helper()
	deleteAndWaitOpt(t, pool, false, deployments...)
}

// cleanupTimeout is how long a scenario's teardown may take before leftovers
// are forced out. A normal teardown takes seconds; slow CI runners and core
// v1.2 (whose stuck NodeClaims need the nudge below) get more room.
func cleanupTimeout() time.Duration {
	if coreMinor == "v1.2" {
		return 6 * time.Minute
	}
	return 4 * time.Minute
}

// stuckAfter is how long a NodeClaim may be terminating before the cleanup
// deletes the nodes of its pool itself.
//
// Core v1.2's KWOK provider implements Delete as a delete of the NodeClaim
// (not of the node) and then waits for the node to disappear. Karpenter
// deletes the node first only for a registered NodeClaim, so a NodeClaim that
// is deleted before its node registered (the drift loop replaces claims every
// few seconds, and the teardown can hit one just launched) waits for a node
// nobody deletes, forever. Deleting the node releases it.
const stuckAfter = 30 * time.Second

// forceGrace is how long the cleanup keeps waiting after it stripped the
// finalizers of leftovers.
const forceGrace = time.Minute

// deleteAndWaitOpt is deleteAndWait; reapOrphans also deletes the nodes left
// behind once the NodePool and its NodeClaims are gone. KWOK creates a node
// some time after Karpenter asks for it, so a NodeClaim that is replaced at
// once (the drift loop) can be deleted before its node appears, which then
// belongs to nobody.
//
// The order matters for a pool whose NodeClaims are replaced all the time:
// the load goes first (no pending pods, nothing to replace for), then the
// SCNodePool and the NodePool, so Karpenter creates no new NodeClaims, and
// only then the NodeClaims that are left. If the teardown still does not
// finish within cleanupTimeout, the NodeClaims and nodes of the pool are
// released from their finalizers (logged) as a last resort, so one stuck
// scenario does not fail on, or poison, what follows it.
func deleteAndWaitOpt(t *testing.T, pool string, reapOrphans bool, deployments ...string) {
	t.Helper()
	selector := client.MatchingLabels{labelNodePool: pool}
	for _, d := range deployments {
		dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: d, Namespace: namespace}}
		err := k8s.Delete(ctx(), dep, client.PropagationPolicy(metav1.DeletePropagationForeground))
		if err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("deleting Deployment %s: %v", d, err)
		}
	}
	p := &v1alpha1.SCNodePool{ObjectMeta: metav1.ObjectMeta{Name: pool}}
	if err := k8s.Delete(ctx(), p); err != nil && !apierrors.IsNotFound(err) {
		t.Errorf("deleting SCNodePool %s: %v", pool, err)
	}

	start := time.Now()
	deadline := start.Add(cleanupTimeout())
	forced := false
	for {
		// Do not rely on the garbage collector alone: delete the NodePool and
		// its NodeClaims directly (all are no-ops once they are gone).
		if np, err := getNodePool(pool); err == nil && np.GetDeletionTimestamp() == nil {
			_ = k8s.Delete(ctx(), np)
		}
		claimObjs := newClaimList()
		_ = k8s.List(ctx(), claimObjs, selector)
		for i := range claimObjs.Items {
			if claimObjs.Items[i].GetDeletionTimestamp() == nil {
				_ = k8s.Delete(ctx(), &claimObjs.Items[i])
			}
		}
		_, npErr := getNodePool(pool)
		var nodes corev1.NodeList
		_ = k8s.List(ctx(), &nodes, selector)
		if stuck := stuckClaims(claimObjs); len(stuck) > 0 {
			for i := range nodes.Items {
				if nodes.Items[i].DeletionTimestamp == nil {
					t.Logf("NodeClaims %v are terminating for more than %s: deleting node %s",
						stuck, stuckAfter, nodes.Items[i].Name)
					_ = k8s.Delete(ctx(), &nodes.Items[i])
				}
			}
		}
		var pods corev1.PodList
		_ = k8s.List(ctx(), &pods, client.InNamespace(namespace))
		gone := apierrors.IsNotFound(npErr) && len(claimObjs.Items) == 0 && len(pods.Items) == 0
		if gone && reapOrphans {
			for i := range nodes.Items {
				t.Logf("deleting orphaned node %s", nodes.Items[i].Name)
				_ = k8s.Delete(ctx(), &nodes.Items[i])
			}
		}
		if gone && len(nodes.Items) == 0 {
			t.Logf("cleanup of %s took %s", pool, time.Since(start).Round(time.Second))
			return
		}
		if time.Now().After(deadline) {
			if forced {
				t.Errorf("NodePool %s, its NodeClaims, nodes or the load pods are still there after the cleanup timeout "+
					"(%d NodeClaims, %d nodes, %d pods, NodePool: %v)",
					pool, len(claimObjs.Items), len(nodes.Items), len(pods.Items), npErr)
				return
			}
			forced = true
			deadline = time.Now().Add(forceGrace)
			t.Logf("cleanup of %s still incomplete after %s (NodePool: %v, %d NodeClaims, %d nodes, %d pods): "+
				"removing finalizers", pool, time.Since(start).Round(time.Second), npErr,
				len(claimObjs.Items), len(nodes.Items), len(pods.Items))
			releaseLeftovers(t, claimObjs, &nodes)
		}
		time.Sleep(time.Second)
	}
}

// stuckClaims returns the names of the NodeClaims that have been terminating
// for longer than stuckAfter.
func stuckClaims(claims *unstructured.UnstructuredList) []string {
	var out []string
	for i := range claims.Items {
		if ts := claims.Items[i].GetDeletionTimestamp(); ts != nil && time.Since(ts.Time) > stuckAfter {
			out = append(out, claims.Items[i].GetName())
		}
	}
	return out
}

// releaseLeftovers strips the finalizers of the NodeClaims and nodes a
// scenario left behind, and deletes them, logging every object it touches.
func releaseLeftovers(t *testing.T, claims *unstructured.UnstructuredList, nodes *corev1.NodeList) {
	t.Helper()
	release := func(obj client.Object, kind string) {
		if len(obj.GetFinalizers()) > 0 {
			t.Logf("force-removing finalizers %v of %s %s (deleting since %v)",
				obj.GetFinalizers(), kind, obj.GetName(), obj.GetDeletionTimestamp())
			patch := client.MergeFrom(obj.DeepCopyObject().(client.Object))
			obj.SetFinalizers(nil)
			if err := k8s.Patch(ctx(), obj, patch); err != nil && !apierrors.IsNotFound(err) {
				t.Logf("releasing %s %s: %v", kind, obj.GetName(), err)
			}
		}
		if err := k8s.Delete(ctx(), obj); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("deleting %s %s: %v", kind, obj.GetName(), err)
		}
	}
	for i := range claims.Items {
		release(&claims.Items[i], "NodeClaim")
	}
	for i := range nodes.Items {
		release(&nodes.Items[i], "node")
	}
	// Pods of the scenario's load that are stuck on a node that is gone.
	var pods corev1.PodList
	_ = k8s.List(ctx(), &pods, client.InNamespace(namespace))
	for i := range pods.Items {
		pod := &pods.Items[i]
		t.Logf("force-deleting pod %s (node %q, phase %s)", pod.Name, pod.Spec.NodeName, pod.Status.Phase)
		_ = k8s.Delete(ctx(), &pods.Items[i], client.GracePeriodSeconds(0))
	}
}
