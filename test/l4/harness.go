/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package l4

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/nodeclaim/disruption"
	"sigs.k8s.io/karpenter/pkg/controllers/nodepool/hash"
	"sigs.k8s.io/karpenter/pkg/controllers/nodepool/readiness"
	"sigs.k8s.io/karpenter/pkg/controllers/nodepool/validation"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	coretest "sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/pkg/test/expectations"

	"github.com/SpareCores/sc-karpenter-nodepool/internal/nodepool"
)

// Harness drives one provider's real CloudProvider through Karpenter core's
// own provisioning, hash and drift code against an envtest API server, the
// way the providers' cloudprovider suites do.
type Harness struct {
	// Client is the cached client the controllers use; Reader reads the
	// API server directly, for assertions.
	Client        client.Client
	Reader        client.Reader
	Cluster       *state.Cluster
	CloudProvider cloudprovider.CloudProvider
	Provisioner   *provisioning.Provisioner
	// Clock drives the drift controller; it is stepped past core's one-hour
	// delay of the InstanceTypeNotFound check.
	Clock *clock.FakeClock
	// AfterLaunch runs after core created the NodeClaims (Azure: wait for
	// the asynchronous VM creation).
	AfterLaunch func()
	// Reset clears the provider's fakes and caches between scenarios.
	Reset func(ctx context.Context)
}

// Apply renders the scenario, applies the NodePool server-side as the
// controller does (the provider's own NodePool CRD validates it), runs
// core's NodePool hash, validation and readiness controllers on it and
// requires it to be Ready.
func (h *Harness) Apply(ctx context.Context, t *testing.T, s Scenario, dataTime time.Time) *karpv1.NodePool {
	t.Helper()
	u, err := s.Render(ctx, dataTime)
	if err != nil {
		t.Fatalf("rendering %s: %v", s.Name, err)
	}
	if err := h.Client.Apply(ctx, client.ApplyConfigurationFromUnstructured(u),
		client.FieldOwner(nodepool.FieldManager), client.ForceOwnership); err != nil {
		t.Fatalf("applying NodePool %s: %v", s.Name, err)
	}
	// Core's NodePool controllers, as in a cluster: the hash (static
	// drift), runtime validation (ValidationSucceeded) and NodeClass
	// readiness. The provisioner only uses Ready NodePools.
	for _, r := range []reconcile.ObjectReconciler[*karpv1.NodePool]{
		hash.NewController(h.Client, h.CloudProvider),
		validation.NewController(h.Clock, h.Client, h.CloudProvider),
		readiness.NewController(h.Clock, h.Client, h.CloudProvider),
	} {
		np := &karpv1.NodePool{}
		if err := h.Reader.Get(ctx, client.ObjectKey{Name: s.Name}, np); err != nil {
			t.Fatal(err)
		}
		expectations.ExpectObjectReconciled(ctx, h.Client, r, np)
	}
	stored := &karpv1.NodePool{}
	if err := h.Reader.Get(ctx, client.ObjectKey{Name: s.Name}, stored); err != nil {
		t.Fatal(err)
	}
	if !stored.StatusConditions().Root().IsTrue() {
		t.Fatalf("NodePool %s is not Ready: %+v", s.Name, stored.Status.Conditions)
	}
	if stored.Annotations[karpv1.NodePoolHashAnnotationKey] == "" {
		t.Fatalf("NodePool %s: core's hash controller set no %s", s.Name, karpv1.NodePoolHashAnnotationKey)
	}
	// The provisioner and the drift controller read through the cache.
	np := h.waitForNodePool(ctx, t, s.Name, func(np *karpv1.NodePool) bool {
		return np.ResourceVersion == stored.ResourceVersion
	})
	return np
}

func (h *Harness) waitForNodePool(ctx context.Context, t *testing.T, name string,
	ok func(*karpv1.NodePool) bool) *karpv1.NodePool {
	t.Helper()
	np := &karpv1.NodePool{}
	deadline := time.Now().Add(30 * time.Second)
	for {
		err := h.Client.Get(ctx, client.ObjectKey{Name: name}, np)
		if err == nil && ok(np) {
			return np
		}
		if time.Now().After(deadline) {
			t.Fatalf("NodePool %s did not reach the cache (last error: %v)", name, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Launch schedules the pods with core's provisioner, creates the NodeClaims
// through the provider's Create and registers fake nodes (core's
// ExpectProvisioned), and returns each pod's NodeClaim as stored.
func (h *Harness) Launch(ctx context.Context, t *testing.T, pods ...*corev1.Pod) []*karpv1.NodeClaim {
	t.Helper()
	result := expectations.ExpectProvisioned(ctx, h.Client, h.Cluster, h.CloudProvider, h.Provisioner, pods...)
	if h.AfterLaunch != nil {
		h.AfterLaunch()
	}
	// Bindings are keyed by the provisioner's copies of the pods.
	bound := map[client.ObjectKey]*expectations.Binding{}
	for pod, b := range result.Bindings {
		bound[client.ObjectKeyFromObject(pod)] = b
	}
	out := make([]*karpv1.NodeClaim, 0, len(pods))
	for _, pod := range pods {
		b, ok := bound[client.ObjectKeyFromObject(pod)]
		if !ok || b.NodeClaim == nil {
			t.Fatalf("pod %s (node selector %v) was not launched", pod.Name, pod.Spec.NodeSelector)
		}
		nc := &karpv1.NodeClaim{}
		if err := h.Reader.Get(ctx, client.ObjectKeyFromObject(b.NodeClaim), nc); err != nil {
			t.Fatal(err)
		}
		if slices.ContainsFunc(out, func(o *karpv1.NodeClaim) bool { return o.Name == nc.Name }) {
			t.Fatalf("pod %s did not get a NodeClaim of its own", pod.Name)
		}
		out = append(out, nc)
	}
	return out
}

// CheckNodeClaim asserts what overview §8.4 asks of every launched
// NodeClaim: its instance type is one of the NodePool's, every NodePool
// requirement key is stamped on it (no drift loop, §5.1.5), core's
// requirement-drift check passes, the template's custom labels are there,
// and core's drift controller (with the provider's IsDrifted) finds no
// drift.
func (h *Harness) CheckNodeClaim(ctx context.Context, t *testing.T, np *karpv1.NodePool, nc *karpv1.NodeClaim) {
	t.Helper()
	it := nc.Labels[corev1.LabelInstanceTypeStable]
	if !slices.Contains(InstanceTypesOf(np), it) {
		t.Errorf("NodeClaim %s: instance type %q is not in the NodePool's list %v", nc.Name, it, InstanceTypesOf(np))
	}
	if missing := MissingKeys(np, nc.Labels); len(missing) > 0 {
		t.Errorf("NodeClaim %s (%s): requirement keys not stamped by the provider (drift loop): %v", nc.Name, it, missing)
	}
	if err := RequirementsDrifted(np, nc); err != nil {
		t.Errorf("NodeClaim %s (%s): labels incompatible with the NodePool requirements: %v", nc.Name, it, err)
	}
	for k, v := range np.Spec.Template.Labels {
		if nc.Labels[k] != v {
			t.Errorf("NodeClaim %s: template label %s=%q is %q", nc.Name, k, v, nc.Labels[k])
		}
	}
	if reason := h.DriftReason(ctx, t, nc); reason != "" {
		t.Errorf("NodeClaim %s (%s): drifted right after launch: %s", nc.Name, it, reason)
	}
	keys := make([]string, 0, len(np.Spec.Template.Spec.Requirements))
	for _, r := range np.Spec.Template.Spec.Requirements {
		keys = append(keys, r.Key+"="+nc.Labels[r.Key])
	}
	t.Logf("NodeClaim %s: %s", nc.Name, strings.Join(keys, " "))
}

// DriftReason runs core's NodeClaim disruption controller (static,
// requirement and instance-type drift, then the provider's IsDrifted) and
// returns the Drifted condition's reason, "" when not drifted.
func (h *Harness) DriftReason(ctx context.Context, t *testing.T, nc *karpv1.NodeClaim) string {
	t.Helper()
	h.Clock.Step(2 * time.Hour)
	fresh := &karpv1.NodeClaim{}
	if err := h.Reader.Get(ctx, client.ObjectKeyFromObject(nc), fresh); err != nil {
		t.Fatal(err)
	}
	expectations.ExpectObjectReconciled(ctx, h.Client, disruption.NewController(h.Clock, h.Client, h.CloudProvider), fresh)
	if err := h.Reader.Get(ctx, client.ObjectKeyFromObject(nc), fresh); err != nil {
		t.Fatal(err)
	}
	if c := fresh.StatusConditions().Get(karpv1.ConditionTypeDrifted); c.IsTrue() {
		return c.Reason
	}
	return ""
}

// Cleanup deletes the scenario's objects and resets the provider's fakes.
func (h *Harness) Cleanup(ctx context.Context) {
	expectations.ExpectCleanedUp(ctx, h.Client)
	h.Cluster.Reset()
	if h.Reset != nil {
		h.Reset(ctx)
	}
}

// Gomega makes the Gomega assertions of Karpenter's test expectations fail
// the given test (they use the global Gomega instance).
func Gomega(t *testing.T) {
	gomega.RegisterTestingT(t)
}

// podLabel marks the test pods (value l4); their hostname anti-affinity
// makes every launch a new NodeClaim instead of reusing an earlier node.
const podLabel = "sparecores.com/l4-pod"

// Pod returns a pending pod with a small CPU request and the given node
// selector, which never shares a node with another test pod.
func Pod(name string, selector map[string]string) *corev1.Pod {
	return coretest.UnschedulablePod(coretest.PodOptions{
		ObjectMeta:   metav1.ObjectMeta{Name: name, Namespace: "default", Labels: map[string]string{podLabel: "l4"}},
		NodeSelector: selector,
		ResourceRequirements: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
		},
		PodAntiRequirements: []corev1.PodAffinityTerm{{
			TopologyKey:   corev1.LabelHostname,
			LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{podLabel: "l4"}},
		}},
	})
}
