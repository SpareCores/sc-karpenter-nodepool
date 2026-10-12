/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package controller

import (
	"context"
	"fmt"
	"os"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	karpenterv1alpha1 "github.com/SpareCores/sc-karpenter-nodepool/api/v1alpha1"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/testutil"
)

var k8sClient client.Client

func TestMain(m *testing.M) {
	cfg, stop, err := testutil.StartEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	scheme := runtime.NewScheme()
	if err := karpenterv1alpha1.AddToScheme(scheme); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	if err := stop(); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(code)
}

func newPool(name string) *karpenterv1alpha1.SCNodePool {
	return &karpenterv1alpha1.SCNodePool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: karpenterv1alpha1.SCNodePoolSpec{
			NodePoolTemplate: karpenterv1alpha1.NodePoolTemplate{
				Spec: runtime.RawExtension{Raw: []byte(`{"template":{"spec":{"nodeClassRef":{"group":"karpenter.k8s.aws","kind":"EC2NodeClass","name":"default"}}},"disruption":{"consolidateAfter":"1m"}}`)},
			},
		},
	}
}

func TestReconcileReportsNotImplemented(t *testing.T) {
	ctx := context.Background()
	pool := newPool("stub")
	if err := k8sClient.Create(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, pool) })

	r := &SCNodePoolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)}
	// Reconciling twice must be idempotent.
	for range 2 {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}
	}

	var got karpenterv1alpha1.SCNodePool
	if err := k8sClient.Get(ctx, req.NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.ObservedGeneration != got.Generation {
		t.Errorf("observedGeneration = %d, want %d", got.Status.ObservedGeneration, got.Generation)
	}
	ready := meta.FindStatusCondition(got.Status.Conditions, karpenterv1alpha1.ConditionReady)
	if ready == nil {
		t.Fatal("no Ready condition")
	}
	if ready.Status != metav1.ConditionUnknown || ready.Reason != karpenterv1alpha1.ReasonNotImplemented {
		t.Errorf("Ready = %s/%s, want Unknown/NotImplemented", ready.Status, ready.Reason)
	}
	if len(got.Status.Conditions) != 1 {
		t.Errorf("got %d conditions, want 1", len(got.Status.Conditions))
	}
}

func TestReconcileTracksGeneration(t *testing.T) {
	ctx := context.Background()
	pool := newPool("generation")
	if err := k8sClient.Create(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, pool) })

	r := &SCNodePoolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}

	if err := k8sClient.Get(ctx, req.NamespacedName, pool); err != nil {
		t.Fatal(err)
	}
	pool.Spec.DryRun = true
	if err := k8sClient.Update(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}

	var got karpenterv1alpha1.SCNodePool
	if err := k8sClient.Get(ctx, req.NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if got.Generation != 2 || got.Status.ObservedGeneration != 2 {
		t.Errorf("generation %d, observedGeneration %d, want 2 and 2", got.Generation, got.Status.ObservedGeneration)
	}
	ready := meta.FindStatusCondition(got.Status.Conditions, karpenterv1alpha1.ConditionReady)
	if ready == nil || ready.ObservedGeneration != 2 {
		t.Errorf("Ready condition not updated to generation 2: %+v", ready)
	}
}

func TestReconcileIgnoresDeletedObjects(t *testing.T) {
	r := &SCNodePoolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: "gone"}}); err != nil {
		t.Fatalf("reconciling a missing object: %v", err)
	}
}
