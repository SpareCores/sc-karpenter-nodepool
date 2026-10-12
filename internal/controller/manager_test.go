/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package controller

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/yaml"

	"github.com/SpareCores/sc-karpenter-nodepool/api/v1alpha1"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/nodepool"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/providers"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/testutil"
)

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestManager runs the reconciler in a manager configured like cmd/main.go:
// the SCNodePool watch creates the NodePool, the NodeClass watch (added by
// discovery) re-detects regions, and the Event watch drives
// NodePoolSchedulable.
func TestManager(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                 k8sClient.Scheme(),
		Cache:                  CacheOptions(),
		Client:                 client.Options{Cache: &client.CacheOptions{Unstructured: true}},
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		t.Fatal(err)
	}
	r := &SCNodePoolReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Registry: providers.NewRegistry(),
		Data:     newDataSource(keeperURL),
	}
	if err := r.SetupWithManager(ctx, mgr, SetupOptions{MaxConcurrentReconciles: 2}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()

	nc := createNodeClass(t, "nc-manager")
	createPool(t, newPool("managed", "nc-manager", familiesSelector(2, 4)))
	eventually(t, "RegionPending", func() bool {
		return cond(getPool(t, "managed"), v1alpha1.ConditionReady).Reason == v1alpha1.ReasonRegionPending
	})

	// The NodeClass gets its subnets: the NodeClass watch triggers a refresh.
	nc = getUnstructured(t, nc)
	nc.Object["status"] = map[string]any{"subnets": []any{map[string]any{"id": "subnet-1", "zoneID": "use1-az1"}}}
	if err := k8sClient.Update(ctx, nc); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the NodePool", func() bool {
		np := getNodePool(t, "managed")
		return np != nil && slices.Equal(nodepool.InstanceTypes(np), sizes2to4)
	})

	seen := metav1.Now()
	mustCreate(t, &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "managed.nocompat", Namespace: "default"},
		InvolvedObject: corev1.ObjectReference{APIVersion: "karpenter.sh/v1", Kind: "NodePool", Name: "managed"},
		Reason:         reasonNoCompatibleInstanceTypes,
		Message:        "NodePool requirements filtered out all compatible available instance types",
		Type:           corev1.EventTypeWarning,
		FirstTimestamp: seen,
		LastTimestamp:  seen,
	})
	eventually(t, "NodePoolSchedulable=False", func() bool {
		return cond(getPool(t, "managed"), v1alpha1.ConditionNodePoolSchedulable).Status == metav1.ConditionFalse
	})

	// Our own Events are recorded through the manager's recorder.
	eventually(t, "a NodePoolCreated Event", func() bool {
		var list corev1.EventList
		if err := k8sClient.List(ctx, &list); err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(list.Items, func(e corev1.Event) bool {
			return e.Reason == "NodePoolCreated" && e.InvolvedObject.Name == "managed"
		})
	})
}

func getUnstructured(t *testing.T, u *unstructured.Unstructured) *unstructured.Unstructured {
	t.Helper()
	cp := u.DeepCopy()
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(u), cp); err != nil {
		t.Fatal(err)
	}
	return cp
}

// TestRBACCoversNodeClasses checks the generated ClusterRole grants
// get/list/watch on the NodeClass of every adapter (make manifests).
func TestRBACCoversNodeClasses(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(testutil.RepoRoot(), "config", "rbac", "role.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var role rbacv1.ClusterRole
	if err := yaml.Unmarshal(raw, &role); err != nil {
		t.Fatal(err)
	}
	allows := func(group, resource string, verbs ...string) bool {
		for _, rule := range role.Rules {
			if slices.Contains(rule.APIGroups, group) && slices.Contains(rule.Resources, resource) &&
				!slices.ContainsFunc(verbs, func(v string) bool { return !slices.Contains(rule.Verbs, v) }) {
				return true
			}
		}
		return false
	}
	for _, a := range providers.NewRegistry().All() {
		group, kind := a.NodeClass()
		if resource := strings.ToLower(kind) + "es"; !allows(group, resource, "get", "list", "watch") {
			t.Errorf("%s: no get/list/watch on %s.%s", a.ID(), resource, group)
		}
	}
	for _, c := range []struct{ group, resource string }{
		{"karpenter.sh", "nodepools"}, {"karpenter.sh", "nodeclaims"}, {"", "nodes"}, {"", "events"},
	} {
		if !allows(c.group, c.resource, "get", "list", "watch") && !allows(c.group, c.resource, "list", "watch") {
			t.Errorf("no list/watch on %s.%s", c.resource, c.group)
		}
	}
	if !allows("karpenter.sh", "nodepools", "create", "patch") || !allows("karpenter.sparecores.com", "scnodepools/finalizers", "update") {
		t.Error("missing NodePool write or finalizer permissions")
	}
}
