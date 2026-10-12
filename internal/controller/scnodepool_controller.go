/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package controller holds the reconcilers of the manager.
package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	karpenterv1alpha1 "github.com/SpareCores/sc-karpenter-nodepool/api/v1alpha1"
)

// SCNodePoolReconciler reconciles an SCNodePool.
//
// This is the inert scaffold version: it only records that it saw the current
// generation and reports Ready=Unknown until the real reconciler lands.
type SCNodePoolReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=karpenter.sparecores.com,resources=scnodepools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=karpenter.sparecores.com,resources=scnodepools/status,verbs=get;update;patch

// Reconcile sets status.observedGeneration and a Ready condition with status
// Unknown and reason NotImplemented.
func (r *SCNodePoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pool karpenterv1alpha1.SCNodePool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("getting SCNodePool %s: %w", req.Name, err)
	}

	original := pool.DeepCopy()
	pool.Status.ObservedGeneration = pool.Generation
	meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
		Type:               karpenterv1alpha1.ConditionReady,
		Status:             metav1.ConditionUnknown,
		Reason:             karpenterv1alpha1.ReasonNotImplemented,
		Message:            "the reconciler is not implemented yet",
		ObservedGeneration: pool.Generation,
	})

	if err := r.Status().Patch(ctx, &pool, client.MergeFrom(original)); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status of SCNodePool %s: %w", req.Name, err)
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler with the manager.
func (r *SCNodePoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&karpenterv1alpha1.SCNodePool{}).
		Named("scnodepool").
		Complete(r)
}
