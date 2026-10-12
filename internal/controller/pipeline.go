/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/SpareCores/sc-karpenter-nodepool/api/v1alpha1"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/keeper"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/nodepool"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/providers"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/selection"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/selection/churn"
)

const (
	// defaultRefreshInterval applies when spec.refresh.interval is empty or
	// unparsable (the CRD defaults and validates it).
	defaultRefreshInterval = 24 * time.Hour
	// Backoff of transient failures (RegionPending, DataStale, ApplyFailed).
	retryBase = 30 * time.Second
	retryMax  = 10 * time.Minute
)

// failure is why a reconcile kept the last applied NodePool: the Ready
// reason and message. Transient failures are retried with backoff;
// the others wait for a change or the next refresh interval.
type failure struct {
	reason    string
	message   string
	transient bool
}

func fail(reason string, transient bool, format string, args ...any) *failure {
	return &failure{reason: reason, message: fmt.Sprintf(format, args...), transient: transient}
}

// run is one reconcile of one SCNodePool.
type run struct {
	r    *SCNodePoolReconciler
	pool *v1alpha1.SCNodePool
	orig *v1alpha1.SCNodePool
	mem  *poolMemory
	now  time.Time

	// np is the existing NodePool of the same name (nil: none); ours tells
	// whether it is labelled as generated from this SCNodePool.
	np   *unstructured.Unstructured
	ours bool

	tmpl      *nodepool.Template
	adapter   providers.Adapter
	ncRef     providers.NodeClassRef
	nodeClass *unstructured.Unstructured

	requeueAt time.Time
}

func (ru *run) wakeAt(t time.Time) {
	if !t.IsZero() && (ru.requeueAt.IsZero() || t.Before(ru.requeueAt)) {
		ru.requeueAt = t
	}
}

func (ru *run) requeueAfter() time.Duration {
	if ru.requeueAt.IsZero() {
		return 0
	}
	return max(ru.requeueAt.Sub(ru.now), time.Second)
}

func (ru *run) set(t string, status metav1.ConditionStatus, reason, msg string) {
	setCondition(ru.pool, t, status, reason, msg)
}

func (ru *run) event(eventType, reason, action, format string, args ...any) {
	ru.r.Recorder.Eventf(ru.pool, nil, eventType, reason, action, format, args...)
}

// execute runs the preflight checks, the refresh when it is due, and the
// health checks; it reports whether a refresh ran.
func (ru *run) execute(ctx context.Context) (bool, error) {
	ru.pool.Status.ObservedGeneration = ru.pool.Generation
	ru.pool.Status.NodePoolName = ru.pool.Name
	np, err := ru.r.getNodePool(ctx, ru.pool.Name)
	if err != nil {
		return false, err
	}
	ru.np, ru.ours = np, nodepool.IsOurs(np, ru.pool.Name)

	refreshed := false
	f, err := ru.preflight(ctx)
	switch {
	case err != nil:
		return false, err
	case f != nil:
		ru.mem.blocked, ru.mem.pipeline = true, f
	case ru.refreshDue():
		ru.refresh(ctx)
		refreshed = true
	}
	if err := ru.health(ctx); err != nil {
		return refreshed, err
	}
	ru.summarise()
	ru.wakeAt(ru.nextRefresh())
	return refreshed, nil
}

// preflight runs the checks that need no Spare Cores data (every
// reconcile): the template, the adapter, NameConflict,
// NodeClassRefImmutable and the template's capacity types and
// architectures.
func (ru *run) preflight(ctx context.Context) (*failure, error) {
	tmpl, err := nodepool.ParseTemplate(ru.pool.Spec.NodePoolTemplate)
	if err != nil {
		return fail(v1alpha1.ReasonInvalidTemplate, false, "%v", err), nil
	}
	ru.tmpl = tmpl
	ref := tmpl.NodeClassRef
	a, version, err := detectAdapter(ru.r.Registry, ru.r.RESTMapper(), ref.Group, ref.Kind)
	if errors.Is(err, providers.ErrUnsupportedProvider) {
		ru.pool.Status.Provider = ""
		return fail(v1alpha1.ReasonUnsupportedProvider, false, "%v", err), nil
	}
	if err != nil {
		return nil, err
	}
	ru.adapter = a
	ru.pool.Status.Provider = string(a.ID())
	ru.ncRef = providers.NodeClassRef{Group: ref.Group, Version: version, Kind: ref.Kind, Name: ref.Name}
	if version != "" {
		if ru.nodeClass, err = (providers.ClientReader{Client: ru.r.Client}).NodeClass(ctx, ru.ncRef); err != nil {
			return nil, fmt.Errorf("reading %s %s: %w", ref.Kind, ref.Name, err)
		}
	}

	if ru.np != nil && !ru.ours {
		msg := fmt.Sprintf("NodePool %s exists and is not labelled %s=%s; delete or rename it, or rename this SCNodePool",
			ru.pool.Name, nodepool.LabelSCNodePool, ru.pool.Name)
		ru.set(v1alpha1.ConditionNodePoolSynced, metav1.ConditionFalse, v1alpha1.ReasonNameConflict, msg)
		return fail(v1alpha1.ReasonNameConflict, false, "%s", msg), nil
	}
	want := ref
	if o := ru.r.NodeClassRefOverride; o != nil {
		want = *o
	}
	if have, ok := nodepool.NodeClassRefOf(ru.np); ru.ours && ok && (have.Group != want.Group || have.Kind != want.Kind) {
		msg := fmt.Sprintf("nodeClassRef group and kind are immutable: NodePool %s uses %s/%s, the template asks for %s/%s; "+
			"delete the NodePool (this drains its nodes) or create a new SCNodePool", ru.pool.Name, have.Group, have.Kind, want.Group, want.Kind)
		ru.set(v1alpha1.ConditionNodePoolSynced, metav1.ConditionFalse, v1alpha1.ReasonNodeClassRefImmutable, msg)
		return fail(v1alpha1.ReasonNodeClassRefImmutable, false, "%s", msg), nil
	}

	rules := a.RequirementRules()
	for _, req := range tmpl.Requirements() {
		if slices.Contains(rules.NeverEmit, req.Key) {
			return fail(v1alpha1.ReasonInvalidTemplate, false, "requirement key %s is not supported by provider %s", req.Key, a.ID()), nil
		}
	}
	if _, err := rules.ResolveCapacityTypes(tmpl.CapacityTypes(rules.AllowedCapacityTypes)); err != nil {
		return fail(v1alpha1.ReasonCapacityTypeUnsupported, false, "%v", err), nil
	}
	if archs := tmpl.Architectures(); archs != nil {
		if _, _, err := rules.ResolveArchitectures(archs, nil); err != nil {
			return fail(v1alpha1.ReasonArchitectureUnsupported, false, "%v", err), nil
		}
	}
	if rules.WarnOnZoneRequirement && tmpl.HasZoneRequirement() && ru.mem.zoneWarnedGen != ru.pool.Generation {
		ru.mem.zoneWarnedGen = ru.pool.Generation
		ru.event(corev1.EventTypeWarning, "ZoneRequirementNotHonoured", "Validate",
			"provider %s ignores zone requirements for some capacity types (on-demand); the zone requirement is kept as written", a.ID())
	}
	return nil, nil
}

func (ru *run) interval() time.Duration {
	if d, err := time.ParseDuration(ru.pool.Spec.Refresh.Interval); err == nil && d > 0 {
		return d
	}
	return defaultRefreshInterval
}

func (ru *run) nodeClassVersion() string {
	if ru.nodeClass == nil {
		return ""
	}
	return ru.nodeClass.GetResourceVersion()
}

// refreshDue decides whether this reconcile re-evaluates the selection:
// once after start-up, on a spec change, a new refresh-requested value, a
// changed NodeClass, after a blocked preflight, when a previously applied
// NodePool disappeared, on the retry of a transient failure, and when the
// refresh interval has passed. Everything else (NodePool status, Events,
// NodeClaims) only updates the health conditions.
func (ru *run) refreshDue() bool {
	m := ru.mem
	switch {
	case !m.attempted, m.blocked,
		m.generation != ru.pool.Generation,
		m.annotation != ru.pool.Annotations[AnnotationRefreshRequested],
		m.nodeClassVersion != ru.nodeClassVersion():
		return true
	case m.applied && ru.np == nil && !ru.pool.Spec.DryRun:
		return true
	case !m.retryAt.IsZero():
		return !ru.now.Before(m.retryAt)
	}
	return !ru.now.Before(m.lastAttempt.Add(ru.interval()))
}

// nextRefresh is when the next refresh is due without any change.
func (ru *run) nextRefresh() time.Time {
	if !ru.mem.retryAt.IsZero() {
		return ru.mem.retryAt
	}
	if ru.mem.blocked || !ru.mem.attempted {
		return time.Time{}
	}
	return ru.mem.lastAttempt.Add(ru.interval())
}

func (ru *run) refresh(ctx context.Context) {
	m := ru.mem
	m.attempted, m.lastAttempt, m.blocked = true, ru.now, false
	m.generation = ru.pool.Generation
	m.annotation = ru.pool.Annotations[AnnotationRefreshRequested]
	m.nodeClassVersion = ru.nodeClassVersion()
	if ru.np == nil {
		m.applied = false
	}
	f := ru.evaluateAndApply(ctx)
	m.pipeline = f
	if f != nil && f.transient {
		m.failures++
		m.retryAt = ru.now.Add(min(retryBase<<min(m.failures-1, 10), retryMax))
	} else {
		m.failures, m.retryAt = 0, time.Time{}
	}
	if f != nil {
		logf.FromContext(ctx).Info("Refresh kept the previous NodePool", "reason", f.reason, "message", f.message)
	}
}

// evaluation is the outcome of the Spare Cores part of a refresh.
type evaluation struct {
	result        selection.Result
	candidates    map[string]*selection.Candidate
	keeperRegions []string
	zones         *providers.ZoneIndex
	dataTimestamp *time.Time
}

// evaluate runs overview §3.4 steps 2–5: regions, Keeper queries,
// eligibility, template constraints, selectors and ranking.
func (ru *run) evaluate(ctx context.Context) (*evaluation, *failure) {
	cycle := ru.r.Data.Cycle()
	dataErr := func(what string, err error) *failure {
		ru.r.Data.Invalidate(cycle)
		ru.set(v1alpha1.ConditionDataAvailable, metav1.ConditionFalse, v1alpha1.ReasonDataStale, what+": "+err.Error())
		return fail(v1alpha1.ReasonDataStale, true, "%s: %v", what, err)
	}
	regions, err := cycle.Fetcher.Regions(ctx)
	if err != nil {
		return nil, dataErr("reading the Spare Cores region table", err)
	}
	zones, err := cycle.Fetcher.Zones(ctx)
	if err != nil {
		return nil, dataErr("reading the Spare Cores zone table", err)
	}
	ev := &evaluation{zones: providers.NewZoneIndex(zones)}
	a := ru.adapter

	det, err := providers.DetectRegions(ctx, providers.ClientReader{Client: ru.r.Client}, a, ru.ncRef, ev.zones)
	if err != nil {
		return nil, fail(v1alpha1.ReasonRegionPending, true, "%v", err)
	}
	st := &ru.pool.Status
	st.Regions, st.RegionSource = det.Regions, det.Source.APISource()
	ru.regionConflict(det)
	if ev.keeperRegions, err = providers.KeeperRegions(a, det.Regions, providers.NewRegionIndex(regions)); err != nil {
		return nil, fail(v1alpha1.ReasonRegionUnknown, true, "%v", err)
	}

	archs := ru.tmpl.Architectures()
	if fromClass, ok := a.ArchitecturesFromNodeClass(ru.nodeClass); ok {
		if archs == nil {
			archs = fromClass
		} else {
			archs = slices.DeleteFunc(slices.Clone(archs), func(x v1alpha1.Architecture) bool { return !slices.Contains(fromClass, x) })
		}
	}
	if archs != nil && len(archs) == 0 {
		ru.set(v1alpha1.ConditionSelectionNonEmpty, metav1.ConditionFalse, v1alpha1.ReasonNoCandidates,
			"no architecture is allowed by both the template and the NodeClass images")
		return nil, fail(v1alpha1.ReasonNoCandidates, false, "no architecture is allowed by both the template and the NodeClass images")
	}

	fees, err := managedFees(ctx, a, cycle.FeeSource(), det.Regions)
	if err != nil {
		return nil, dataErr("reading the EKS Auto Mode fees", err)
	}
	catalog, err := cycle.Fetcher.BenchmarkCatalog(ctx)
	if err != nil {
		return nil, dataErr("reading the Keeper benchmark configs", err)
	}
	opts := selection.Options{
		Catalog:       catalog,
		Eligible:      providers.EligibleFunc(a, providers.Env{ManagedFees: fees, AllowGPUs: providers.SelectsAccelerators(&ru.pool.Spec)}),
		ManagedFees:   fees,
		DefaultBasis:  ru.tmpl.DefaultBasis(),
		Architectures: archs,
	}
	if ru.tmpl.HasInstanceTypeConstraint() {
		opts.InstanceTypeAllowed = ru.tmpl.InstanceTypeAllowed
	}
	queries, err := selection.PlanQueries(&ru.pool.Spec, a.Vendor(), ev.keeperRegions, opts)
	if err != nil {
		if errors.Is(err, keeper.ErrUnknownBenchmarkConfig) {
			ru.set(v1alpha1.ConditionDataAvailable, metav1.ConditionFalse, v1alpha1.ReasonUnknownBenchmarkConfig, err.Error())
			return nil, fail(v1alpha1.ReasonUnknownBenchmarkConfig, false, "%v", err)
		}
		return nil, fail(v1alpha1.ReasonInvalidSpec, false, "%v", err)
	}
	results, err := cycle.Fetcher.ServersAll(ctx, queries)
	if err != nil {
		return nil, dataErr("querying Keeper", err)
	}
	if h, err := cycle.Fetcher.Health(ctx); err == nil {
		ev.dataTimestamp = &h.DatabaseLastUpdated
	} else {
		logf.FromContext(ctx).Error(err, "Reading the Keeper data timestamp")
	}
	ru.set(v1alpha1.ConditionDataAvailable, metav1.ConditionTrue, "KeeperReachable", "Spare Cores data and benchmark configs are available")

	cands := selection.BuildCandidates(results)
	ev.candidates = make(map[string]*selection.Candidate, len(cands))
	for _, c := range cands {
		ev.candidates[c.Name()] = c
	}
	if ev.result, err = selection.Evaluate(&ru.pool.Spec, cands, opts); err != nil {
		return nil, fail(v1alpha1.ReasonInvalidSpec, false, "%v", err)
	}
	return ev, nil
}

// managedFees returns the Auto Mode fees of the regions (nil for other
// adapters). Over several regions a type needs a fee in each; the highest
// fee is used.
func managedFees(ctx context.Context, a providers.Adapter, src providers.FeeSource, regions []string) (map[string]float64, error) {
	var out map[string]float64
	for i, region := range regions {
		fees, err := a.ManagedFees(ctx, src, region)
		if err != nil || fees == nil {
			return nil, err
		}
		if i == 0 {
			out = maps.Clone(fees)
			continue
		}
		for name, f := range out {
			switch g, ok := fees[name]; {
			case !ok:
				delete(out, name)
			case g > f:
				out[name] = g
			}
		}
	}
	return out, nil
}

// evaluateAndApply is one refresh: evaluate, churn control, render,
// validate, server-side dry run, apply, status and Event.
func (ru *run) evaluateAndApply(ctx context.Context) *failure {
	ev, f := ru.evaluate(ctx)
	if f != nil {
		return f
	}
	st := &ru.pool.Status
	now := metav1.NewTime(ru.now)
	st.LastRefreshTime = &now
	if ev.dataTimestamp != nil {
		ts := metav1.NewTime(*ev.dataTimestamp)
		st.DataTimestamp = &ts
	}
	st.Excluded = ev.result.Excluded.Status()
	if len(ev.result.InstanceTypes) == 0 {
		msg := "no instance type matches the selection (see status.excluded)"
		ru.set(v1alpha1.ConditionSelectionNonEmpty, metav1.ConditionFalse, v1alpha1.ReasonNoCandidates, msg)
		return fail(v1alpha1.ReasonNoCandidates, false, "%s", msg)
	}
	ru.set(v1alpha1.ConditionSelectionNonEmpty, metav1.ConditionTrue, "Selected",
		fmt.Sprintf("%d instance types match the selection", len(ev.result.InstanceTypes)))

	// Churn control (§3.5) starts from what is applied: the live NodePool,
	// or in dryRun the would-be pool reported in status.
	var prev []string
	switch {
	case ru.ours:
		prev = nodepool.InstanceTypes(ru.np)
	case ru.pool.Spec.DryRun && st.Selected != nil:
		for _, t := range st.Selected.InstanceTypes {
			prev = append(prev, t.Name)
		}
	}
	synced := findCondition(ru.pool, v1alpha1.ConditionNodePoolSynced)
	specChanged := synced == nil || synced.Status != metav1.ConditionTrue || synced.ObservedGeneration != ru.pool.Generation
	types, state := churn.Apply(churn.StateFromStatus(prev, st.PendingRemovals), ev.result,
		churn.ConfigFromSpec(ru.pool.Spec.Churn, ru.now), specChanged)

	rules := ru.adapter.RequirementRules()
	rendered, err := nodepool.Render(nodepool.Input{
		SCNodePool:           ru.pool,
		Template:             ru.tmpl,
		InstanceTypes:        types,
		Architectures:        ru.architecturesOf(types, ev.candidates),
		Rules:                rules,
		ForcedZones:          providers.ForcedZones(ru.adapter, ev.keeperRegions, ev.zones),
		DataTimestamp:        ev.dataTimestamp,
		NodeClassRefOverride: ru.r.NodeClassRefOverride,
	})
	if f := renderFailure(ru, err); f != nil {
		return f
	}
	if len(rendered.Invalid) > 0 {
		ru.event(corev1.EventTypeWarning, "InvalidInstanceTypes", "Render",
			"dropped instance types whose names are not valid label values: %s", strings.Join(rendered.Invalid, ", "))
	}
	if err := nodepool.Validate(ctx, rendered.NodePool, rules); err != nil {
		ru.set(v1alpha1.ConditionNodePoolSynced, metav1.ConditionFalse, v1alpha1.ReasonValidationFailed, err.Error())
		return fail(v1alpha1.ReasonValidationFailed, false, "%v", err)
	}
	if f := ru.apply(ctx, rendered); f != nil {
		return f
	}

	st.Selected = selectedStatus(rendered.InstanceTypes, ev.result, ev.candidates)
	pending := slices.DeleteFunc(state.Pending, func(p churn.Pending) bool {
		return !slices.Contains(rendered.InstanceTypes, p.Name)
	})
	st.PendingRemovals = churn.State{Pending: pending}.PendingRemovals()
	return nil
}

func renderFailure(ru *run, err error) *failure {
	var reason string
	switch {
	case err == nil:
		return nil
	case errors.Is(err, nodepool.ErrEmptySelection):
		ru.set(v1alpha1.ConditionSelectionNonEmpty, metav1.ConditionFalse, v1alpha1.ReasonNoCandidates, err.Error())
		reason = v1alpha1.ReasonNoCandidates
	case errors.Is(err, providers.ErrCapacityTypeUnsupported):
		reason = v1alpha1.ReasonCapacityTypeUnsupported
	case errors.Is(err, providers.ErrArchitectureUnsupported):
		reason = v1alpha1.ReasonArchitectureUnsupported
	default:
		reason = v1alpha1.ReasonInvalidTemplate
	}
	return fail(reason, false, "%v", err)
}

// architecturesOf returns the architectures of the types to emit. Types kept
// by churn control that are no longer offered have no candidate; then the
// architectures the NodePool emitted so far are kept as well, so their
// running nodes do not drift.
func (ru *run) architecturesOf(types []string, cands map[string]*selection.Candidate) []v1alpha1.Architecture {
	var out []v1alpha1.Architecture
	unknown := false
	for _, name := range types {
		if c, ok := cands[name]; ok && c.Architecture != "" {
			out = append(out, c.Architecture)
		} else {
			unknown = true
		}
	}
	if unknown && ru.ours && ru.tmpl.Architectures() == nil {
		for _, r := range nodepool.RequirementsOf(ru.np) {
			if r.Key == providers.LabelArch && r.Operator == nodepool.OpIn {
				for _, v := range r.Values {
					out = append(out, v1alpha1.Architecture(v))
				}
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// apply validates the NodePool with a server-side dry run (the provider's
// own CRD rules) and, unless dryRun, applies it with server-side apply.
func (ru *run) apply(ctx context.Context, rendered *nodepool.Rendered) *failure {
	opts := []client.ApplyOption{client.FieldOwner(nodepool.FieldManager), client.ForceOwnership}
	probe := rendered.NodePool.DeepCopy()
	dryRun := []client.ApplyOption{client.FieldOwner(nodepool.FieldManager), client.ForceOwnership, client.DryRunAll}
	if err := ru.r.Apply(ctx, client.ApplyConfigurationFromUnstructured(probe), dryRun...); err != nil {
		if apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) {
			ru.set(v1alpha1.ConditionNodePoolSynced, metav1.ConditionFalse, v1alpha1.ReasonNodePoolRejected, err.Error())
			return fail(v1alpha1.ReasonNodePoolRejected, false, "the NodePool CRD rejects the generated NodePool: %v", err)
		}
		ru.set(v1alpha1.ConditionNodePoolSynced, metav1.ConditionFalse, v1alpha1.ReasonApplyFailed, err.Error())
		return fail(v1alpha1.ReasonApplyFailed, true, "server-side dry run: %v", err)
	}
	n := len(rendered.InstanceTypes)
	if ru.pool.Spec.DryRun {
		ru.set(v1alpha1.ConditionNodePoolSynced, metav1.ConditionTrue, v1alpha1.ReasonDryRun,
			fmt.Sprintf("dry run: the NodePool would have %d instance types (server-side dry run passed); it is not applied", n))
		return nil
	}

	applied := rendered.NodePool.DeepCopy()
	if err := ru.r.Apply(ctx, client.ApplyConfigurationFromUnstructured(applied), opts...); err != nil {
		ru.set(v1alpha1.ConditionNodePoolSynced, metav1.ConditionFalse, v1alpha1.ReasonApplyFailed, err.Error())
		return fail(v1alpha1.ReasonApplyFailed, true, "applying NodePool %s: %v", ru.pool.Name, err)
	}
	switch {
	case ru.np == nil || !ru.ours:
		ru.event(corev1.EventTypeNormal, "NodePoolCreated", "Apply", "created NodePool %s with %d instance types", ru.pool.Name, n)
	case applied.GetResourceVersion() != ru.np.GetResourceVersion():
		ru.event(corev1.EventTypeNormal, "NodePoolUpdated", "Apply", "updated NodePool %s: %s",
			ru.pool.Name, diffMessage(nodepool.InstanceTypes(ru.np), rendered.InstanceTypes))
	}
	ru.np, ru.ours, ru.mem.applied = applied, true, true
	ru.set(v1alpha1.ConditionNodePoolSynced, metav1.ConditionTrue, "Applied", fmt.Sprintf("NodePool %s has %d instance types", ru.pool.Name, n))
	return nil
}

// maxDiffNames bounds the names listed per side in an Event.
const maxDiffNames = 20

// diffMessage describes the instance types added and removed.
func diffMessage(before, after []string) string {
	var added, removed []string
	for _, t := range after {
		if !slices.Contains(before, t) {
			added = append(added, t)
		}
	}
	for _, t := range before {
		if !slices.Contains(after, t) {
			removed = append(removed, t)
		}
	}
	if len(added) == 0 && len(removed) == 0 {
		return fmt.Sprintf("instance types unchanged (%d); template or metadata changed", len(after))
	}
	list := func(names []string) string {
		if len(names) > maxDiffNames {
			return strings.Join(names[:maxDiffNames], ", ") + fmt.Sprintf(" and %d more", len(names)-maxDiffNames)
		}
		return strings.Join(names, ", ")
	}
	var parts []string
	if len(added) > 0 {
		parts = append(parts, fmt.Sprintf("added %d (%s)", len(added), list(added)))
	}
	if len(removed) > 0 {
		parts = append(parts, fmt.Sprintf("removed %d (%s)", len(removed), list(removed)))
	}
	return strings.Join(parts, "; ") + fmt.Sprintf("; %d instance types", len(after))
}
