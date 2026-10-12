/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/SpareCores/sc-karpenter-nodepool/api/v1alpha1"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/nodepool"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/providers"
)

// Generated-pool health (overview §3.6).
const (
	// managedGracePeriod: a NodePool without status conditions after this
	// long is managed by no Karpenter controller (wrong nodeClassRef).
	managedGracePeriod = 2 * time.Minute
	// schedulableWindow: a NoCompatibleInstanceTypes event keeps
	// NodePoolSchedulable False this long (Karpenter repeats it, deduplicated
	// per minute, while pods wait).
	schedulableWindow = 10 * time.Minute
	// driftWindow and driftLoopThreshold: more than this many NodeClaims
	// launched after the last apply and replaced for RequirementsDrifted
	// within the window is a drift loop (§5.1.5).
	driftWindow        = 10 * time.Minute
	driftLoopThreshold = 3

	conditionValidationSucceeded = "ValidationSucceeded"
	conditionLaunched            = "Launched"
	conditionDrifted             = "Drifted"
	reasonRequirementsDrifted    = "RequirementsDrifted"
)

var healthConditions = []string{
	v1alpha1.ConditionNodePoolManaged, v1alpha1.ConditionNodePoolValid, v1alpha1.ConditionNodePoolSchedulable,
}

// health sets the conditions about the generated NodePool and runs the
// drift-loop detector. Without a NodePool of ours they are removed.
func (ru *run) health(ctx context.Context) error {
	np := ru.np
	if np == nil || !ru.ours {
		for _, c := range healthConditions {
			meta.RemoveStatusCondition(&ru.pool.Status.Conditions, c)
		}
		return nil
	}
	conds := nodepool.Conditions(np)

	switch age := ru.now.Sub(np.GetCreationTimestamp().Time); {
	case len(conds) > 0:
		ru.set(v1alpha1.ConditionNodePoolManaged, metav1.ConditionTrue, "StatusReported",
			"a Karpenter controller reports status for the NodePool")
	case age >= managedGracePeriod:
		ref, _ := nodepool.NodeClassRefOf(np)
		ru.set(v1alpha1.ConditionNodePoolManaged, metav1.ConditionFalse, "NoStatus", fmt.Sprintf(
			"no Karpenter controller has reported status for NodePool %s after %s; is a provider for nodeClassRef %s/%s installed?",
			np.GetName(), managedGracePeriod, ref.Group, ref.Kind))
	default:
		ru.set(v1alpha1.ConditionNodePoolManaged, metav1.ConditionUnknown, "Pending",
			"waiting for a Karpenter controller to report status for the NodePool")
		ru.wakeAt(np.GetCreationTimestamp().Add(managedGracePeriod))
	}

	if c, ok := nodepool.FindCondition(conds, conditionValidationSucceeded); ok && c.Status != metav1.ConditionUnknown {
		reason := c.Reason
		if reason == "" {
			reason = conditionValidationSucceeded
		}
		ru.set(v1alpha1.ConditionNodePoolValid, c.Status, reason, c.Message)
	} else {
		ru.set(v1alpha1.ConditionNodePoolValid, metav1.ConditionUnknown, "NotReported",
			"Karpenter has not reported ValidationSucceeded for the NodePool")
	}

	claims, err := ru.nodeClaims(ctx)
	if err != nil {
		return err
	}
	if err := ru.schedulable(ctx, claims); err != nil {
		return err
	}
	ru.detectDriftLoop(np, claims)
	return nil
}

// nodeClaims lists the NodeClaims of the pool.
func (ru *run) nodeClaims(ctx context.Context) ([]unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(providers.NodeClaimListGVK)
	if err := ru.r.List(ctx, list, client.MatchingLabels{LabelNodePool: ru.pool.Name}); err != nil {
		if meta.IsNoMatchError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing NodeClaims of %s: %w", ru.pool.Name, err)
	}
	return list.Items, nil
}

// eventTime is the latest time an Event was observed.
func eventTime(e *corev1.Event) time.Time {
	t := e.CreationTimestamp.Time
	for _, c := range []time.Time{e.FirstTimestamp.Time, e.LastTimestamp.Time, e.EventTime.Time} {
		if c.After(t) {
			t = c
		}
	}
	if e.Series != nil && e.Series.LastObservedTime.After(t) {
		t = e.Series.LastObservedTime.Time
	}
	return t
}

// schedulable sets NodePoolSchedulable: False while Karpenter keeps
// reporting NoCompatibleInstanceTypes for the pool, True again once a
// NodeClaim of the pool launches after the last such event (or the events
// stop).
func (ru *run) schedulable(ctx context.Context, claims []unstructured.Unstructured) error {
	var events corev1.EventList
	if err := ru.r.List(ctx, &events, client.MatchingFields{eventNameIndex: ru.pool.Name}); err != nil {
		return fmt.Errorf("listing Events of NodePool %s: %w", ru.pool.Name, err)
	}
	var latest *corev1.Event
	var latestAt time.Time
	for i := range events.Items {
		e := &events.Items[i]
		if e.InvolvedObject.Kind != "NodePool" || e.Reason != reasonNoCompatibleInstanceTypes {
			continue
		}
		if t := eventTime(e); latest == nil || t.After(latestAt) {
			latest, latestAt = e, t
		}
	}
	var launched time.Time
	for i := range claims {
		if c, ok := nodepool.FindCondition(nodepool.Conditions(&claims[i]), conditionLaunched); ok &&
			c.Status == metav1.ConditionTrue && c.LastTransitionTime.After(launched) {
			launched = c.LastTransitionTime
		}
	}
	switch {
	case latest != nil && latestAt.After(launched) && ru.now.Sub(latestAt) < schedulableWindow:
		ru.set(v1alpha1.ConditionNodePoolSchedulable, metav1.ConditionFalse, reasonNoCompatibleInstanceTypes, latest.Message)
		ru.wakeAt(latestAt.Add(schedulableWindow))
	case latest != nil && !latestAt.After(launched):
		ru.set(v1alpha1.ConditionNodePoolSchedulable, metav1.ConditionTrue, "NodeClaimLaunched",
			"a NodeClaim of the pool launched after the last NoCompatibleInstanceTypes event")
	default:
		ru.set(v1alpha1.ConditionNodePoolSchedulable, metav1.ConditionTrue, "NoSchedulingFailures",
			"Karpenter reports no NoCompatibleInstanceTypes events for the pool")
	}
	return nil
}

// detectDriftLoop emits a Warning Event when more than driftLoopThreshold
// NodeClaims launched after the last apply were marked RequirementsDrifted
// within driftWindow: they were launched under the current requirements and
// still drifted, the signature of a requirement on a well-known key the
// provider does not stamp on NodeClaims (§5.1.5). Replaced NodeClaims
// disappear quickly, so sightings are remembered in memory.
func (ru *run) detectDriftLoop(np *unstructured.Unstructured, claims []unstructured.Unstructured) {
	lastApply := nodepool.LastAppliedTime(np)
	seen := ru.mem.drifted
	for i := range claims {
		cl := &claims[i]
		c, ok := nodepool.FindCondition(nodepool.Conditions(cl), conditionDrifted)
		if !ok || c.Status != metav1.ConditionTrue || c.Reason != reasonRequirementsDrifted ||
			cl.GetCreationTimestamp().Time.Before(lastApply) {
			continue
		}
		if _, ok := seen[cl.GetUID()]; !ok {
			at := c.LastTransitionTime
			if at.IsZero() {
				at = ru.now
			}
			seen[cl.GetUID()] = at
		}
	}
	for uid, at := range seen {
		if ru.now.Sub(at) > driftWindow {
			delete(seen, uid)
		}
	}
	if len(seen) > driftLoopThreshold && ru.now.Sub(ru.mem.lastDriftWarning) >= driftWindow {
		ru.mem.lastDriftWarning = ru.now
		ru.event(corev1.EventTypeWarning, "DriftLoop", "DetectDrift",
			"%d NodeClaims of NodePool %s were marked RequirementsDrifted within %s although they were launched under the "+
				"current requirements: a requirement on a well-known key the provider does not stamp on NodeClaims "+
				"replaces nodes in an endless loop; remove such requirements from the template",
			len(seen), np.GetName(), driftWindow)
	}
}
