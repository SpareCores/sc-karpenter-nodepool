/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package controller

import (
	"fmt"
	"math"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/SpareCores/sc-karpenter-nodepool/api/v1alpha1"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/providers"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/selection"
)

// maxConditionMessage is the API limit of metav1.Condition.Message.
const maxConditionMessage = 32768

func setCondition(pool *v1alpha1.SCNodePool, t string, status metav1.ConditionStatus, reason, msg string) {
	if len(msg) > maxConditionMessage {
		msg = msg[:maxConditionMessage-3] + "..."
	}
	meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
		Type:               t,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: pool.Generation,
	})
}

func findCondition(pool *v1alpha1.SCNodePool, t string) *metav1.Condition {
	return meta.FindStatusCondition(pool.Status.Conditions, t)
}

// summarise sets the Ready condition: the failure of the last refresh (or
// preflight) if any, else the first unhealthy generated-pool condition,
// else Ready (DryRun in dry-run mode). A transition to False is also
// reported as a Warning Event.
func (ru *run) summarise() {
	status, reason, msg := metav1.ConditionTrue, v1alpha1.ReasonReady, ""
	health := []struct{ cond, reason string }{
		{v1alpha1.ConditionNodePoolManaged, v1alpha1.ReasonNodePoolNotManaged},
		{v1alpha1.ConditionNodePoolValid, v1alpha1.ReasonNodePoolInvalid},
		{v1alpha1.ConditionNodePoolSchedulable, v1alpha1.ReasonNodePoolUnschedulable},
	}
	if f := ru.mem.pipeline; f != nil {
		status, reason, msg = metav1.ConditionFalse, f.reason, f.message
	} else {
		for _, h := range health {
			if c := findCondition(ru.pool, h.cond); c != nil && c.Status == metav1.ConditionFalse {
				status, reason, msg = metav1.ConditionFalse, h.reason, c.Message
				break
			}
		}
	}
	if status == metav1.ConditionTrue {
		n := int32(0)
		if s := ru.pool.Status.Selected; s != nil {
			n = s.Count
		}
		msg = fmt.Sprintf("NodePool %s has %d instance types", ru.pool.Name, n)
		if ru.pool.Spec.DryRun {
			reason = v1alpha1.ReasonDryRun
			msg = fmt.Sprintf("dry run: %d instance types selected; the NodePool is not applied", n)
		}
	}
	prev := findCondition(ru.pool, v1alpha1.ConditionReady)
	if status == metav1.ConditionFalse && (prev == nil || prev.Status != status || prev.Reason != reason) {
		ru.event(corev1.EventTypeWarning, reason, "Reconcile", "%s", msg)
	}
	ru.set(v1alpha1.ConditionReady, status, reason, msg)
}

// regionConflict sets the RegionConflict warning condition, with an Event
// when it turns True.
func (ru *run) regionConflict(det providers.RegionDetection) {
	if !det.Conflict {
		ru.set(v1alpha1.ConditionRegionConflict, metav1.ConditionFalse, "SourcesAgree",
			fmt.Sprintf("regions %v from %s", det.Regions, det.Source))
		return
	}
	msg := det.ConflictMessage()
	if c := findCondition(ru.pool, v1alpha1.ConditionRegionConflict); c == nil || c.Status != metav1.ConditionTrue || c.Message != msg {
		ru.event(corev1.EventTypeWarning, v1alpha1.ConditionRegionConflict, "DetectRegions", "%s", msg)
	}
	ru.set(v1alpha1.ConditionRegionConflict, metav1.ConditionTrue, "SourcesDisagree", msg)
}

// selectedStatus describes the emitted instance types. Types that qualify
// now carry their score and price; types kept only by churn control carry
// their family when Spare Cores still lists them.
func selectedStatus(types []string, res selection.Result, cands map[string]*selection.Candidate) *v1alpha1.SelectedStatus {
	byName := make(map[string]selection.InstanceType, len(res.InstanceTypes))
	for _, t := range res.InstanceTypes {
		byName[t.Name] = t
	}
	out := &v1alpha1.SelectedStatus{Count: int32(len(types)), Band: res.Band.Status()}
	sizes := map[string]int32{}
	for _, name := range types {
		it := v1alpha1.InstanceTypeStatus{Name: name}
		if t, ok := byName[name]; ok {
			it.Family = t.Family
			it.Score = round(t.Score, 1e4)
			price := t.Price
			it.Price = round(&price, 1e6)
		} else if c, ok := cands[name]; ok {
			it.Family = c.Family()
		}
		sizes[it.Family]++
		out.InstanceTypes = append(out.InstanceTypes, it)
	}
	seen := map[string]bool{}
	for _, f := range res.Families {
		if sizes[f.Name] == 0 {
			continue
		}
		seen[f.Name] = true
		out.Families = append(out.Families, v1alpha1.FamilyStatus{
			Name: f.Name, Rank: int32(f.Rank), Objective: round(f.Objective, 1e4), Sizes: sizes[f.Name],
		})
	}
	// Families only present through churn control, unranked.
	var extra []string
	for name := range sizes {
		if !seen[name] && name != "" {
			extra = append(extra, name)
		}
	}
	slices.SortFunc(extra, strings.Compare)
	for _, name := range extra {
		out.Families = append(out.Families, v1alpha1.FamilyStatus{Name: name, Sizes: sizes[name]})
	}
	return out
}

func round(v *float64, scale float64) *float64 {
	if v == nil {
		return nil
	}
	r := math.Round(*v*scale) / scale
	return &r
}
