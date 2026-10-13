//go:build kwok

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package e2e

import (
	"fmt"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/SpareCores/sc-karpenter-nodepool/api/v1alpha1"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/nodepool"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/providers"
	"github.com/SpareCores/sc-karpenter-nodepool/test/kwok/catalog"
)

// subsetAndNarrowing: real core Karpenter provisions NodeClaims only from the
// emitted instance-type list (and the allowed zones, capacity types and
// architectures); narrowing the selection drifts the nodes of the removed
// types (RequirementsDrifted) and the pool converges onto the new set.
// Assertions are relative to what the NodePool says, never fixed names.
func (v vendorRun) subsetAndNarrowing(t *testing.T) {
	pool := v.pool("")
	v.newSCNodePool(t, pool, poolOptions{selector: vcpuRange(2, 4)})
	p := v.waitSynced(t, pool)
	t.Logf("selected %d instance types, regions %v", p.Status.Selected.Count, p.Status.Regions)

	dep := v.loadDeployment(pool, pool, loadReplicas)
	if err := k8s.Create(ctx(), dep); err != nil {
		t.Fatal(err)
	}
	defer deleteAndWait(t, pool, pool)

	eventually(t, 3*time.Minute, time.Second, "all pods running on provisioned nodes", func() (bool, string) {
		r, n := readyPods(pool)
		return r == loadReplicas, fmt.Sprintf("%d/%d pods ready of %d", r, loadReplicas, n)
	})
	claims, err := listClaims(pool)
	if err != nil {
		t.Fatal(err)
	}
	// Pods repel each other, so at least one claim per pod; Karpenter may
	// legitimately launch a spare one (never consolidated here), so only log it.
	if got := len(live(claims)); got < loadReplicas {
		t.Errorf("%d NodeClaims for %d pods that repel each other: %v", got, loadReplicas, claims)
	} else if got > loadReplicas {
		t.Logf("%d NodeClaims for %d pods (spare capacity): %v", got, loadReplicas, claims)
	}
	before := v.allowedBy(t, pool)
	if bad := before.check(live(claims)); len(bad) > 0 {
		t.Errorf("NodeClaims outside what the NodePool allows:\n%v", bad)
	}
	if want := p.Status.Selected.Count; int(want) != len(before.types) {
		t.Errorf("status.selected.count = %d, NodePool lists %d instance types", want, len(before.types))
	}
	t.Logf("subset honoured: %d NodeClaims, types in use %v", len(claims), usedTypes(claims))

	// Narrow: exclude every type in use. Their nodes drift; the replacements
	// must come from the remaining types.
	used := usedTypes(claims)
	updatePool(t, pool, func(p *v1alpha1.SCNodePool) {
		p.Spec.Selector.InstanceTypes = &v1alpha1.GlobFilter{Exclude: slices.Clone(used)}
	})
	eventually(t, 2*time.Minute, time.Second, "NodePool narrowed", func() (bool, string) {
		np, err := getNodePool(pool)
		if err != nil {
			return false, err.Error()
		}
		listed := nodepool.InstanceTypes(np)
		left := slices.DeleteFunc(slices.Clone(used), func(n string) bool { return !slices.Contains(listed, n) })
		return len(left) == 0, fmt.Sprintf("still listed: %v", left)
	})
	after := v.allowedBy(t, pool)
	if len(after.types) == 0 || len(after.types) >= len(before.types) {
		t.Fatalf("narrowing did not reduce the list: %d -> %d", len(before.types), len(after.types))
	}

	sawDrift := false
	eventually(t, 4*time.Minute, 250*time.Millisecond, "converged onto the narrowed list", func() (bool, string) {
		cs, err := listClaims(pool)
		if err != nil {
			return false, err.Error()
		}
		for _, c := range cs {
			if c.Drifted {
				sawDrift = true
			}
		}
		l := live(cs)
		r, _ := readyPods(pool)
		bad := after.check(l)
		// At least one live claim per pod (hostname anti-affinity); an extra,
		// empty replacement within the narrowed list is legitimate Karpenter
		// behaviour and is never consolidated here (WhenEmpty after 24h).
		ok := len(l) >= loadReplicas && len(bad) == 0 && len(cs) == len(l) && r == loadReplicas
		return ok, fmt.Sprintf("%d claims (%d live), %d/%d pods ready, outside: %v", len(cs), len(l), r, loadReplicas, bad)
	})
	if !sawDrift {
		t.Error("never saw a NodeClaim with Drifted=True reason RequirementsDrifted while narrowing")
	}
	final, _ := listClaims(pool)
	t.Logf("converged: %v", final)
}

func usedTypes(cs []claim) []string {
	var out []string
	for _, c := range cs {
		if !slices.Contains(out, c.InstanceType) {
			out = append(out, c.InstanceType)
		}
	}
	slices.Sort(out)
	return out
}

// unknownInstanceTypes: a list core Karpenter has no such types for (the
// catalog stops at catalog.MaxVCPUs vCPUs, the selection starts above it)
// leaves pods Pending, and the controller reports it through the
// NodePoolSchedulable condition (overview §3.6).
func (v vendorRun) unknownInstanceTypes(t *testing.T) {
	pool := v.pool("unknown")
	v.newSCNodePool(t, pool, poolOptions{selector: vcpuRange(catalog.MaxVCPUs+1, 0)})
	v.waitSynced(t, pool)
	if err := k8s.Create(ctx(), v.loadDeployment(pool, pool, 1)); err != nil {
		t.Fatal(err)
	}
	defer deleteAndWait(t, pool, pool)

	eventually(t, 3*time.Minute, time.Second, "NodePoolSchedulable=False", func() (bool, string) {
		p, err := getPool(pool)
		if err != nil {
			return false, err.Error()
		}
		c := condition(p, v1alpha1.ConditionNodePoolSchedulable)
		if c == nil {
			return false, "no NodePoolSchedulable condition yet"
		}
		return c.Status == metav1.ConditionFalse, fmt.Sprintf("%s: %s", c.Status, c.Message)
	})
	p, _ := getPool(pool)
	if c := condition(p, v1alpha1.ConditionNodePoolSchedulable); c.Reason != "NoCompatibleInstanceTypes" {
		t.Errorf("NodePoolSchedulable reason %q", c.Reason)
	}
	ready := condition(p, v1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != v1alpha1.ReasonNodePoolUnschedulable {
		t.Errorf("Ready = %+v, want False/%s", ready, v1alpha1.ReasonNodePoolUnschedulable)
	}
	if cs, _ := listClaims(pool); len(cs) != 0 {
		t.Errorf("NodeClaims although no instance type is known: %v", cs)
	}
	if r, n := readyPods(pool); r != 0 || n != 1 {
		t.Errorf("pods ready %d of %d, want a Pending pod", r, n)
	}
}

// requirement is a template requirement of a scenario's SCNodePool.
func requirement(key string, values ...string) map[string]any {
	vs := make([]any, len(values))
	for i, s := range values {
		vs[i] = s
	}
	return map[string]any{"key": key, "operator": "In", "values": vs}
}

// driftLoop: a multi-valued requirement on a well-known key the provider does
// not stamp on its nodes (here the region, which the catalog leaves off) makes
// core Karpenter mark every new NodeClaim RequirementsDrifted at once and
// replace it, forever (overview §5.1.5). The controller's detector reports it.
func (v vendorRun) driftLoop(t *testing.T) {
	pool := v.pool("loop")
	v.newSCNodePool(t, pool, poolOptions{
		selector:     vcpuRange(2, 4),
		requirements: []map[string]any{requirement(providers.LabelRegion, v.cat.Region, "elsewhere-1")},
	})
	v.waitSynced(t, pool)
	if err := k8s.Create(ctx(), v.loadDeployment(pool, pool, 1)); err != nil {
		t.Fatal(err)
	}
	defer deleteAndWaitOpt(t, pool, true, pool)

	created, err := getPool(pool)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	eventually(t, 6*time.Minute, time.Second, "DriftLoop warning Event", func() (bool, string) {
		cs, _ := listClaims(pool)
		for _, c := range cs {
			names[c.Name] = true
		}
		var events corev1.EventList
		if err := k8s.List(ctx(), &events, client.InNamespace(namespace)); err != nil {
			return false, err.Error()
		}
		for i := range events.Items {
			e := &events.Items[i]
			// By UID: an earlier run of the lane may have left the same Event.
			if e.InvolvedObject.Kind == "SCNodePool" && e.InvolvedObject.UID == created.UID && e.Reason == "DriftLoop" {
				return true, e.Message
			}
		}
		return false, fmt.Sprintf("%d NodeClaims created so far", len(names))
	})
	if len(names) < 4 {
		t.Errorf("only %d NodeClaims were created before the detector fired, want more than 3", len(names))
	}
}

// customKey: a multi-valued requirement on a custom key is stamped by core
// Karpenter itself, so the pool is stable: one NodeClaim, never drifted.
func (v vendorRun) customKey(t *testing.T) {
	pool := v.pool("custom")
	v.newSCNodePool(t, pool, poolOptions{
		selector:     vcpuRange(2, 4),
		requirements: []map[string]any{requirement("sparecores.test/tier", "fast", "faster")},
	})
	v.waitSynced(t, pool)
	if err := k8s.Create(ctx(), v.loadDeployment(pool, pool, 1)); err != nil {
		t.Fatal(err)
	}
	defer deleteAndWait(t, pool, pool)

	eventually(t, 3*time.Minute, time.Second, "pod running", func() (bool, string) {
		r, n := readyPods(pool)
		return r == 1, fmt.Sprintf("%d/%d ready", r, n)
	})
	first, _ := listClaims(pool)
	if len(first) != 1 {
		t.Fatalf("want one NodeClaim, got %v", first)
	}
	// The drift loop replaces a claim every ~20 s; watch for three times that.
	const window = 70 * time.Second
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		cs, _ := listClaims(pool)
		if len(cs) != 1 || cs[0].Name != first[0].Name || cs[0].Drifted {
			t.Fatalf("pool not stable after %s: first %v, now %v",
				time.Since(deadline.Add(-window)).Round(time.Second), first, cs)
		}
		time.Sleep(2 * time.Second)
	}
	t.Logf("stable for %s: %v", window, first)
}
