/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package providers

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/SpareCores/sc-karpenter-nodepool/api/v1alpha1"
)

// NodeClassRef identifies the NodeClass of a NodePool template. Version is
// the served version to read it with (nodeClassRef itself has none).
type NodeClassRef struct {
	Group   string
	Version string
	Kind    string
	Name    string
}

// GroupVersionKind of the NodeClass.
func (r NodeClassRef) GroupVersionKind() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: r.Group, Version: r.Version, Kind: r.Kind}
}

// NodeClaimLabel is the label Karpenter core stamps on NodeClaims (and their
// nodes) of a NodeClass: <group>/<lowercase kind>, value the NodeClass name.
func (r NodeClassRef) NodeClaimLabel() string {
	return r.Group + "/" + strings.ToLower(r.Kind)
}

// ClusterReader is the read access region detection needs.
type ClusterReader interface {
	// NodeClass returns the NodeClass, or nil if it does not exist.
	NodeClass(ctx context.Context, ref NodeClassRef) (*unstructured.Unstructured, error)
	// NodeClaimLabels returns the labels of the NodeClaims matching every
	// label of selector.
	NodeClaimLabels(ctx context.Context, selector map[string]string) ([]map[string]string, error)
	// NodeLabels returns the labels of the nodes matching every label of
	// selector (nil: all nodes).
	NodeLabels(ctx context.Context, selector map[string]string) ([]map[string]string, error)
}

// Source is where a region value was seen.
type Source string

// Region sources in detection order (overview §6.3).
const (
	SourceNodeClass  Source = "NodeClass"
	SourceNodeClaims Source = "NodeClaims"
	// SourceNodes: nodes of the NodeClass.
	SourceNodes Source = "Nodes"
	// SourceAllNodes: every node of the cluster, the last resort.
	SourceAllNodes Source = "AllNodes"
)

// APISource maps the source to SCNodePool status.regionSource, which only
// tells the NodeClass from labels (of NodeClaims or nodes).
func (s Source) APISource() v1alpha1.RegionSource {
	if s == SourceNodeClass {
		return v1alpha1.RegionSourceNodeClass
	}
	return v1alpha1.RegionSourceNodeLabels
}

// SeenRegion is one region value one source reported, with the number of
// objects carrying it (1 for the NodeClass).
type SeenRegion struct {
	Source Source
	Region string
	Count  int
}

// RegionDetection is the outcome of DetectRegions.
type RegionDetection struct {
	// Regions are the Kubernetes region values to generate for, sorted.
	Regions []string
	// Source is where Regions come from.
	Source Source
	// Conflict: the sources disagree, or a single-region provider showed
	// several regions; Seen lists every value for the RegionConflict
	// condition and Event.
	Conflict bool
	// Seen are all values of all consulted sources, in detection order.
	Seen []SeenRegion
}

// ConflictMessage formats Seen for the RegionConflict condition.
func (d RegionDetection) ConflictMessage() string {
	parts := make([]string, 0, len(d.Seen))
	for _, s := range d.Seen {
		parts = append(parts, fmt.Sprintf("%s=%s (%d)", s.Source, s.Region, s.Count))
	}
	return fmt.Sprintf("region sources disagree, using %v from %s; seen: %s",
		d.Regions, d.Source, strings.Join(parts, ", "))
}

// regionCounts counts distinct region values case-insensitively, keeping
// the first spelling seen.
type regionCounts struct {
	source Source
	order  []string // lower-case keys, first-seen order
	values map[string]string
	counts map[string]int
}

func newRegionCounts(source Source) *regionCounts {
	return &regionCounts{source: source, values: map[string]string{}, counts: map[string]int{}}
}

func (rc *regionCounts) add(v string) {
	if v == "" {
		return
	}
	k := strings.ToLower(v)
	if _, ok := rc.values[k]; !ok {
		rc.values[k] = v
		rc.order = append(rc.order, k)
	}
	rc.counts[k]++
}

func (rc *regionCounts) empty() bool { return len(rc.order) == 0 }

func (rc *regionCounts) keys() []string {
	out := slices.Clone(rc.order)
	slices.Sort(out)
	return out
}

// majority returns the key with the most objects (ties: the smallest key).
func (rc *regionCounts) majority() string {
	return slices.MinFunc(rc.keys(), func(a, b string) int {
		return cmp.Or(cmp.Compare(rc.counts[b], rc.counts[a]), cmp.Compare(a, b))
	})
}

func (rc *regionCounts) seen() []SeenRegion {
	out := make([]SeenRegion, 0, len(rc.order))
	for _, k := range rc.keys() {
		out = append(out, SeenRegion{Source: rc.source, Region: rc.values[k], Count: rc.counts[k]})
	}
	return out
}

func labelCounts(source Source, labelSets []map[string]string) *regionCounts {
	rc := newRegionCounts(source)
	for _, l := range labelSets {
		rc.add(l[LabelRegion])
	}
	return rc
}

// DetectRegions implements overview §6.3: the template's NodeClass, then
// NodeClaims of that NodeClass, then nodes of that NodeClass, else all
// nodes. The NodeClass wins when it has a value (unless the adapter uses it
// only as a cross-check); otherwise the first source with values is used,
// and when the sources disagree or a single-region provider shows several
// regions, the majority region of this NodeClass's nodes (else of the
// NodeClaims, else of all nodes) is chosen. Values are compared
// case-insensitively. Without any value it fails with ErrRegionPending.
func DetectRegions(ctx context.Context, r ClusterReader, a Adapter, ref NodeClassRef, zones *ZoneIndex) (RegionDetection, error) {
	nc, err := r.NodeClass(ctx, ref)
	if err != nil {
		return RegionDetection{}, fmt.Errorf("reading %s %s: %w", ref.Kind, ref.Name, err)
	}
	classCounts := newRegionCounts(SourceNodeClass)
	if values, ok := a.RegionsFromNodeClass(nc, zones); ok {
		for _, v := range values {
			classCounts.add(v)
		}
	}
	selector := map[string]string{ref.NodeClaimLabel(): ref.Name}
	claimLabels, err := r.NodeClaimLabels(ctx, selector)
	if err != nil {
		return RegionDetection{}, fmt.Errorf("listing NodeClaims: %w", err)
	}
	claims := labelCounts(SourceNodeClaims, claimLabels)
	nodeLabels, err := r.NodeLabels(ctx, selector)
	if err != nil {
		return RegionDetection{}, fmt.Errorf("listing nodes: %w", err)
	}
	nodes := labelCounts(SourceNodes, nodeLabels)

	authoritative := a.NodeClassRegionsAuthoritative() && !classCounts.empty()
	consulted := []*regionCounts{classCounts, claims, nodes}
	if !authoritative && claims.empty() && nodes.empty() {
		all, err := r.NodeLabels(ctx, nil)
		if err != nil {
			return RegionDetection{}, fmt.Errorf("listing nodes: %w", err)
		}
		consulted = append(consulted, labelCounts(SourceAllNodes, all))
	}

	var primary *regionCounts
	if authoritative {
		primary = classCounts
	} else {
		for _, rc := range consulted[1:] {
			if !rc.empty() {
				primary = rc
				break
			}
		}
	}
	if primary == nil {
		return RegionDetection{}, fmt.Errorf("%w: neither %s %s, its NodeClaims nor any node has a region yet",
			ErrRegionPending, ref.Kind, ref.Name)
	}

	// Every distinct value of every consulted source.
	distinct := map[string]bool{}
	var seen []SeenRegion
	for _, rc := range consulted {
		for _, k := range rc.order {
			distinct[k] = true
		}
		seen = append(seen, rc.seen()...)
	}
	resultKeys := primary.keys()
	conflict := len(distinct) > len(resultKeys) || (!a.MultiRegion() && len(resultKeys) > 1)

	if conflict && !authoritative && !a.MultiRegion() {
		pick := primary
		switch {
		case !nodes.empty():
			pick = nodes
		case !claims.empty():
			pick = claims
		}
		primary = pick
		resultKeys = []string{pick.majority()}
	}
	regions := make([]string, len(resultKeys))
	for i, k := range resultKeys {
		regions[i] = primary.values[k]
	}
	slices.Sort(regions)
	return RegionDetection{Regions: regions, Source: primary.source, Conflict: conflict, Seen: seen}, nil
}

// KeeperRegions maps detected Kubernetes region values to Keeper region ids
// (sorted, deduplicated). An unknown region fails with ErrRegionUnknown.
func KeeperRegions(a Adapter, k8sRegions []string, regions *RegionIndex) ([]string, error) {
	out := make([]string, 0, len(k8sRegions))
	for _, r := range k8sRegions {
		id, err := a.ToKeeperRegion(r, regions)
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return dedupe(out), nil
}
