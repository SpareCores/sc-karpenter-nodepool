/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package providers

import (
	"context"
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/SpareCores/sc-karpenter-nodepool/api/v1alpha1"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/selection"
)

// regionMapping is how a Kubernetes region value maps to a Keeper region id
// (§6.3 mapping table).
type regionMapping int

const (
	byRegionID regionMapping = iota
	byAPIReference
	byAlias
)

// adapter is the one implementation of Adapter: a row of data.
type adapter struct {
	id     ID
	vendor string
	group  string
	kind   string
	// versions, when set, are the only served NodeClass versions accepted
	// (UpCloud: upcloud-tools serves v1alpha2 under the same group).
	versions []string

	rules          []eligibilityRule
	nodeClass      nodeClassRegions
	crossCheckOnly bool
	multiRegion    bool
	mapping        regionMapping
	archFromClass  func(*unstructured.Unstructured) ([]v1alpha1.Architecture, bool)
	requirements   RequirementRules
	managedFee     bool
}

func (a *adapter) ID() ID         { return a.id }
func (a *adapter) Vendor() string { return a.vendor }

func (a *adapter) Matches(group, kind, servedVersion string) bool {
	if group != a.group || kind != a.kind {
		return false
	}
	return a.versions == nil || slices.Contains(a.versions, servedVersion)
}

func (a *adapter) Eligible(c *selection.Candidate, env Env) (bool, string) {
	return eligible(a.rules, c, env)
}

func (a *adapter) RegionsFromNodeClass(nc *unstructured.Unstructured, zones *ZoneIndex) ([]string, bool) {
	if a.nodeClass == nil || nc == nil {
		return nil, false
	}
	out := a.nodeClass(nc, a.vendor, zones)
	return out, len(out) > 0
}

func (a *adapter) NodeClassRegionsAuthoritative() bool { return !a.crossCheckOnly }
func (a *adapter) MultiRegion() bool                   { return a.multiRegion }

func (a *adapter) ToKeeperRegion(k8sRegion string, regions *RegionIndex) (string, error) {
	var found bool
	var id string
	switch a.mapping {
	case byAPIReference:
		r, ok := regions.ByAPIReference(a.vendor, k8sRegion)
		id, found = r.RegionID, ok
	case byAlias:
		r, ok := regions.ByAlias(a.vendor, k8sRegion)
		id, found = r.RegionID, ok
	default:
		r, ok := regions.ByRegionID(a.vendor, k8sRegion)
		id, found = r.RegionID, ok
	}
	if !found {
		return "", fmt.Errorf("%w: %q is not a Spare Cores region of %s", ErrRegionUnknown, k8sRegion, a.vendor)
	}
	return id, nil
}

func (a *adapter) ArchitecturesFromNodeClass(nc *unstructured.Unstructured) ([]v1alpha1.Architecture, bool) {
	if a.archFromClass == nil || nc == nil {
		return nil, false
	}
	return a.archFromClass(nc)
}

func (a *adapter) RequirementRules() RequirementRules {
	r := a.requirements
	// Hand out copies: the adapters are shared.
	r.AllowedCapacityTypes = slices.Clone(r.AllowedCapacityTypes)
	r.AllowedArchitectures = slices.Clone(r.AllowedArchitectures)
	r.NeverEmit = slices.Clone(r.NeverEmit)
	return r
}

func (a *adapter) ManagedFees(ctx context.Context, src FeeSource, k8sRegion string) (map[string]float64, error) {
	if !a.managedFee {
		return nil, nil
	}
	if src == nil {
		return nil, fmt.Errorf("%s: no EKS Auto Mode fee source configured", a.id)
	}
	return src.AutoModeFees(ctx, k8sRegion)
}

var (
	bothArchs    = []v1alpha1.Architecture{v1alpha1.ArchitectureAMD64, v1alpha1.ArchitectureARM64}
	amd64Only    = []v1alpha1.Architecture{v1alpha1.ArchitectureAMD64}
	onDemandOnly = []string{CapacityTypeOnDemand}
	onDemandSpot = []string{CapacityTypeOnDemand, CapacityTypeSpot}
	awsCapacity  = []string{CapacityTypeOnDemand, CapacityTypeSpot, CapacityTypeReserved}

	awsSubnetZones = fromListZones("status.subnets", "zoneID", "zone")
)

// pinnedOnDemand are the rules of the providers that launch on-demand only
// ("always" in §6.4: a template asking for spot is rejected).
func pinnedOnDemand(archs []v1alpha1.Architecture) RequirementRules {
	return RequirementRules{
		DefaultCapacityType:  CapacityTypeOnDemand,
		AllowedCapacityTypes: onDemandOnly,
		AllowedArchitectures: archs,
	}
}

// builtin are the adapters of overview §6.1, in its order.
func builtin() []*adapter {
	return []*adapter{
		{
			id: AWS, vendor: "aws", group: "karpenter.k8s.aws", kind: "EC2NodeClass",
			nodeClass: awsSubnetZones,
			requirements: RequirementRules{
				DefaultCapacityType: CapacityTypeOnDemand, AllowedCapacityTypes: awsCapacity,
				AllowedArchitectures: bothArchs,
			},
		},
		{
			id: AWSAutoMode, vendor: "aws", group: "eks.amazonaws.com", kind: "NodeClass",
			rules:      []eligibilityRule{requireAutoModeSKU, autoModeMinimumSize},
			nodeClass:  awsSubnetZones,
			managedFee: true,
			requirements: RequirementRules{
				DefaultCapacityType: CapacityTypeOnDemand, AllowedCapacityTypes: awsCapacity,
				AllowedArchitectures: bothArchs,
				NeverEmit:            []string{LabelOS},
			},
		},
		{
			id: Azure, vendor: "azure", group: "karpenter.azure.com", kind: "AKSNodeClass",
			rules:   []eligibilityRule{azureEligibility},
			mapping: byAPIReference,
			requirements: RequirementRules{
				DefaultCapacityType: CapacityTypeOnDemand, AllowedCapacityTypes: onDemandSpot,
				AllowedArchitectures: bothArchs,
			},
		},
		{
			id: GCP, vendor: "gcp", group: "karpenter.k8s.gcp", kind: "GCENodeClass",
			rules:   []eligibilityRule{requireOnDemandPrice, excludeTPUs},
			mapping: byAPIReference,
			requirements: RequirementRules{
				DefaultCapacityType: CapacityTypeOnDemand, AllowedCapacityTypes: onDemandSpot,
				AllowedArchitectures: bothArchs,
			},
		},
		{
			id: AlicloudACK, vendor: "alicloud", group: "karpenter.alibabacloud.com", kind: "ECSNodeClass",
			rules:     []eligibilityRule{requireOnDemandPrice},
			nodeClass: fromListZones("status.vSwitches", "zoneID", "zone"),
			requirements: RequirementRules{
				DefaultCapacityType: CapacityTypeOnDemand, AllowedCapacityTypes: onDemandSpot,
				SingleCapacityType:   true,
				AllowedArchitectures: bothArchs,
			},
		},
		{
			id: AlicloudCloudpilot, vendor: "alicloud", group: "karpenter.k8s.alibabacloud", kind: "ECSNodeClass",
			rules:     []eligibilityRule{requireOnDemandPrice},
			nodeClass: fromListZones("status.vSwitches", "zoneID"),
			requirements: RequirementRules{
				DefaultCapacityType: CapacityTypeOnDemand, AllowedCapacityTypes: onDemandSpot,
				AllowedArchitectures: bothArchs, SingleArchitecture: true,
				WarnOnZoneRequirement: true,
			},
		},
		{
			id: Hcloud, vendor: "hcloud", group: "karpenter.hetzner.cloud", kind: "HCloudNodeClass",
			nodeClass:     fromStringList("spec.locations"),
			multiRegion:   true,
			mapping:       byAlias,
			archFromClass: hcloudResolvedArchitectures,
			requirements:  pinnedOnDemand(bothArchs),
		},
		{
			id: HcloudItsh, vendor: "hcloud", group: "karpenter.itsh.dev", kind: "HCloudNodeClass",
			rules:        []eligibilityRule{onlyArchitectures(amd64Only...)},
			nodeClass:    fromListNames("status.locations", "name"),
			multiRegion:  true,
			mapping:      byAlias,
			requirements: pinnedOnDemand(amd64Only),
		},
		{
			id: UpCloud, vendor: "upcloud", group: "karpenter.k8s.upcloud", kind: "UpCloudNodeClass",
			versions:     []string{"v1alpha1"},
			rules:        []eligibilityRule{onlyArchitectures(amd64Only...)},
			nodeClass:    fromListNames("status.zones", "id"),
			multiRegion:  true,
			requirements: pinnedOnDemand(amd64Only),
		},
		{
			id: OVH, vendor: "ovh", group: "karpenter.ovhcloud.sh", kind: "OVHNodeClass",
			rules: []eligibilityRule{
				onlyArchitectures(amd64Only...),
				excludeGPUsUnlessAsked,
				excludePrefixes("bare metal flavor", "bm-"),
				// MKS node pools offer d2-4 and d2-8, not d2-2.
				excludeNames("flavor not offered by OVH MKS", "d2-2"),
			},
			nodeClass:      fromString("spec.region"),
			crossCheckOnly: true,
			requirements: func() RequirementRules {
				r := pinnedOnDemand(amd64Only)
				r.ForceZones = true
				return r
			}(),
		},
		{
			id: Vultr, vendor: "vultr", group: "karpenter.vultr.com", kind: "VultrNodeClass",
			rules: []eligibilityRule{
				onlyArchitectures(amd64Only...),
				excludePrefixes("bare metal plan", "vbm-"),
				excludePrefixes("GPU plan without advertised GPU resources", "vcg-"),
			},
			nodeClass:    fromString("spec.region"),
			requirements: pinnedOnDemand(amd64Only),
		},
	}
}

// Registry resolves nodeClassRefs to adapters.
type Registry struct {
	adapters []*adapter
}

// NewRegistry returns a registry of every built-in adapter.
func NewRegistry() *Registry {
	return &Registry{adapters: builtin()}
}

// Detect returns the adapter of a nodeClassRef group and kind, given the
// served version of the NodeClass CRD (overview §6.1). Unknown combinations,
// including the unsupported upcloud-tools provider (UpCloudNodeClass served
// as v1alpha2), fail with ErrUnsupportedProvider.
func (r *Registry) Detect(group, kind, servedVersion string) (Adapter, error) {
	for _, a := range r.adapters {
		if a.Matches(group, kind, servedVersion) {
			return a, nil
		}
	}
	for _, a := range r.adapters {
		if a.group == group && a.kind == kind {
			return nil, fmt.Errorf("%w: %s.%s is served as %q; adapter %s supports only %v",
				ErrUnsupportedProvider, kind, group, servedVersion, a.id, a.versions)
		}
	}
	return nil, fmt.Errorf("%w: no adapter for nodeClassRef group %q kind %q", ErrUnsupportedProvider, group, kind)
}

// Get returns an adapter by id.
func (r *Registry) Get(id ID) (Adapter, bool) {
	for _, a := range r.adapters {
		if a.id == id {
			return a, true
		}
	}
	return nil, false
}

// All returns every adapter, in the order of overview §6.1.
func (r *Registry) All() []Adapter {
	out := make([]Adapter, len(r.adapters))
	for i, a := range r.adapters {
		out[i] = a
	}
	return out
}
