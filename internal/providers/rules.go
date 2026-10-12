/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package providers

import (
	"fmt"
	"slices"

	"github.com/SpareCores/sc-karpenter-nodepool/api/v1alpha1"
)

// Well-known requirement keys and capacity types (Karpenter core).
const (
	LabelArch         = "kubernetes.io/arch"
	LabelOS           = "kubernetes.io/os"
	LabelZone         = "topology.kubernetes.io/zone"
	LabelRegion       = "topology.kubernetes.io/region"
	LabelCapacityType = "karpenter.sh/capacity-type"
	LabelInstanceType = "node.kubernetes.io/instance-type"

	CapacityTypeOnDemand = "on-demand"
	CapacityTypeSpot     = "spot"
	CapacityTypeReserved = "reserved"
)

// RequirementRules are what a provider needs from the generated NodePool
// requirements (overview §3.4 step 7, §6.4, §6.5).
type RequirementRules struct {
	// DefaultCapacityType is emitted when the template has no
	// karpenter.sh/capacity-type requirement (always explicit: several
	// providers loop or fail without it).
	DefaultCapacityType string
	// AllowedCapacityTypes are the values the provider can launch; a
	// template allowing anything else is rejected
	// (CapacityTypeUnsupported). A single value means "always" in §6.4.
	AllowedCapacityTypes []string
	// SingleCapacityType: a template must not allow more than one capacity
	// type (official Alibaba launches spot whenever spot is allowed).
	SingleCapacityType bool

	// AllowedArchitectures are the architectures the provider can launch.
	AllowedArchitectures []v1alpha1.Architecture
	// SingleArchitecture: exactly one kubernetes.io/arch value per NodePool
	// (cloudpilot Alibaba picks the image from the first instance type).
	SingleArchitecture bool

	// ForceZones: when the template has no zone requirement, emit
	// topology.kubernetes.io/zone In [every Spare Cores zone of the regions]
	// (OVH advertises zones -a/-b/-c even in single-AZ regions); see
	// ForcedZones.
	ForceZones bool
	// WarnOnZoneRequirement: a template zone requirement is not honoured for
	// every capacity type (cloudpilot Alibaba ignores it on-demand).
	WarnOnZoneRequirement bool
	// NeverEmit are requirement keys the generator must never emit.
	NeverEmit []string
	// LegacyCore: the provider pins Karpenter core v1.8 or older (overview
	// §5.1.3), which has no Gte/Lte operators and also restricts the
	// kubernetes.io and k8s.io label domains.
	LegacyCore bool
}

// ResolveCapacityTypes returns the karpenter.sh/capacity-type values to emit
// for the values a template requirement allows (nil: the template has no
// such requirement, so the default is emitted).
func (r RequirementRules) ResolveCapacityTypes(template []string) ([]string, error) {
	if template == nil {
		return []string{r.DefaultCapacityType}, nil
	}
	if len(template) == 0 {
		return nil, fmt.Errorf("%w: the template allows no capacity type", ErrCapacityTypeUnsupported)
	}
	for _, v := range template {
		if !slices.Contains(r.AllowedCapacityTypes, v) {
			return nil, fmt.Errorf("%w: %q (the provider supports %v)", ErrCapacityTypeUnsupported, v, r.AllowedCapacityTypes)
		}
	}
	if r.SingleCapacityType && len(dedupe(template)) > 1 {
		return nil, fmt.Errorf("%w: the provider needs exactly one capacity type per NodePool, the template allows %v",
			ErrCapacityTypeUnsupported, template)
	}
	return template, nil
}

// ResolveArchitectures returns the kubernetes.io/arch values to emit, given
// the template requirement (nil: none) and the architectures of the
// selected types. emit is false only when there is nothing to emit.
func (r RequirementRules) ResolveArchitectures(template, selected []v1alpha1.Architecture) (values []v1alpha1.Architecture, emit bool, err error) {
	if template != nil {
		for _, a := range template {
			if !slices.Contains(r.AllowedArchitectures, a) {
				return nil, false, fmt.Errorf("%w: %q (the provider supports %v)", ErrArchitectureUnsupported, a, r.AllowedArchitectures)
			}
		}
		if r.SingleArchitecture && len(dedupe(template)) != 1 {
			return nil, false, fmt.Errorf("%w: the provider needs exactly one architecture per NodePool, the template allows %v",
				ErrArchitectureUnsupported, template)
		}
		return template, true, nil
	}
	values = dedupe(selected)
	if r.SingleArchitecture && len(values) > 1 {
		return nil, false, fmt.Errorf("%w: the provider needs exactly one architecture per NodePool, the selection has %v; "+
			"constrain kubernetes.io/arch in the template or selector.architectures", ErrArchitectureUnsupported, values)
	}
	return values, len(values) > 0, nil
}

// ForcedZones returns the zone requirement values a ForceZones provider needs
// when the template has none: every Spare Cores zone_id of the Keeper
// regions (nil when the provider does not force zones).
func ForcedZones(a Adapter, keeperRegionIDs []string, zones *ZoneIndex) []string {
	if !a.RequirementRules().ForceZones {
		return nil
	}
	return zones.ZoneIDs(a.Vendor(), keeperRegionIDs)
}

func dedupe[T ~string](vs []T) []T {
	out := slices.Clone(vs)
	slices.Sort(out)
	return slices.Compact(out)
}
