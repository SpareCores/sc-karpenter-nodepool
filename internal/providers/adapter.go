/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package providers holds one adapter per supported Karpenter provider
// (docs/plans/v1/wip/00-overview.md §5, §6): detection from the NodePool
// template's nodeClassRef, the eligibility filter applied before selectors,
// region detection and mapping to Keeper region ids, the requirement rules
// of the generated NodePool, and the EKS Auto Mode management fees.
//
// Adapters are data plus small pure functions. The only Kubernetes access is
// the region detection, which reads through the ClusterReader interface.
package providers

import (
	"context"
	"errors"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/SpareCores/sc-karpenter-nodepool/api/v1alpha1"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/selection"
)

// ID identifies a provider adapter (SCNodePool status.provider).
type ID string

// Adapter ids, one per row of overview §6.1.
const (
	AWS                ID = "aws"
	AWSAutoMode        ID = "aws-auto"
	Azure              ID = "azure"
	GCP                ID = "gcp"
	AlicloudACK        ID = "alicloud-ack"
	AlicloudCloudpilot ID = "alicloud-cloudpilot"
	Hcloud             ID = "hcloud"
	HcloudItsh         ID = "hcloud-itsh"
	UpCloud            ID = "upcloud"
	OVH                ID = "ovh"
	Vultr              ID = "vultr"
)

// Errors carrying the Ready condition reasons of overview §6. They are
// wrapped with details; test with errors.Is.
var (
	// ErrUnsupportedProvider: no adapter for the nodeClassRef (reason
	// UnsupportedProvider).
	ErrUnsupportedProvider = errors.New("unsupported provider")
	// ErrRegionPending: no source has a region yet (reason RegionPending).
	ErrRegionPending = errors.New("region pending")
	// ErrRegionUnknown: a detected region is not in Spare Cores for the
	// vendor (reason RegionUnknown).
	ErrRegionUnknown = errors.New("region unknown")
	// ErrCapacityTypeUnsupported: the template asks for capacity types the
	// provider cannot launch (reason CapacityTypeUnsupported).
	ErrCapacityTypeUnsupported = errors.New("capacity type unsupported")
	// ErrArchitectureUnsupported: the template or the selection needs
	// architectures the provider cannot launch together or at all.
	ErrArchitectureUnsupported = errors.New("architecture unsupported")
	// ErrNoAutoModeFees: the price list has no Auto Mode product for the
	// region.
	ErrNoAutoModeFees = errors.New("no EKS Auto Mode fees")
)

// Env is the per-evaluation input of the eligibility filter.
type Env struct {
	// ManagedFees is the EKS Auto Mode fee table of the target region
	// (instance type -> USD per hour). On Auto Mode, a type without an entry
	// has no Auto Mode SKU and is ineligible; nil makes every type
	// ineligible there. Other adapters ignore it.
	ManagedFees map[string]float64
	// AllowGPUs keeps GPU flavors that are dropped by default (OVH); set it
	// when the selector explicitly asks for accelerators (see
	// SelectsAccelerators).
	AllowGPUs bool
}

// Adapter is one provider adapter.
type Adapter interface {
	// ID is the adapter id.
	ID() ID
	// Vendor is the Keeper vendor id.
	Vendor() string
	// Matches reports whether a nodeClassRef group and kind, with the served
	// version of the NodeClass CRD, belong to this adapter.
	Matches(group, kind, servedVersion string) bool

	// Eligible is the provider's eligibility filter (overview §6.2); reason
	// explains a false result.
	Eligible(c *selection.Candidate, env Env) (ok bool, reason string)

	// RegionsFromNodeClass returns the Kubernetes region values the
	// NodeClass (spec or status) says nodes launch in (§6.3); ok is false
	// when it has none (yet). Zone values are mapped through the Spare Cores
	// zone table.
	RegionsFromNodeClass(nodeClass *unstructured.Unstructured, zones *ZoneIndex) ([]string, bool)
	// NodeClassRegionsAuthoritative is false where the NodeClass value is
	// only a cross-check (OVH: the provider uses its own environment).
	NodeClassRegionsAuthoritative() bool
	// MultiRegion reports whether one NodePool can span several Spare Cores
	// regions (Hetzner locations, UpCloud zones).
	MultiRegion() bool
	// ToKeeperRegion maps a Kubernetes region value to the Keeper region id
	// of Vendor (§6.3 mapping table).
	ToKeeperRegion(k8sRegion string, regions *RegionIndex) (string, error)
	// ArchitecturesFromNodeClass returns the architectures the NodeClass can
	// launch, when it says so (hcloud: resolved images, §6.5).
	ArchitecturesFromNodeClass(nodeClass *unstructured.Unstructured) ([]v1alpha1.Architecture, bool)

	// RequirementRules are the provider's rules for the emitted requirements
	// (§6.4, §6.5).
	RequirementRules() RequirementRules

	// ManagedFees returns the per-instance-type management fee of a
	// Kubernetes region (EKS Auto Mode); nil for every other adapter.
	ManagedFees(ctx context.Context, src FeeSource, k8sRegion string) (map[string]float64, error)
}

// EligibleFunc adapts an adapter's filter to selection.Options.Eligible.
func EligibleFunc(a Adapter, env Env) func(*selection.Candidate) bool {
	return func(c *selection.Candidate) bool {
		ok, _ := a.Eligible(c, env)
		return ok
	}
}

// SelectsAccelerators reports whether the selector explicitly asks for
// accelerators: a positive minimum count, or any constraint on their type,
// manufacturer, model or memory. Env.AllowGPUs is meant to be set from it.
func SelectsAccelerators(spec *v1alpha1.SCNodePoolSpec) bool {
	if spec == nil || spec.Selector == nil || spec.Selector.Accelerator == nil {
		return false
	}
	a := spec.Selector.Accelerator
	if a.Count != nil && a.Count.Max != nil && *a.Count.Max == 0 {
		return false
	}
	return (a.Count != nil && a.Count.Min != nil && *a.Count.Min > 0) ||
		len(a.Types) > 0 || len(a.Manufacturers) > 0 || len(a.Models) > 0 ||
		(a.MemoryPerDevice != nil && a.MemoryPerDevice.MinGiB != nil && *a.MemoryPerDevice.MinGiB > 0) ||
		(a.MemoryTotal != nil && a.MemoryTotal.MinGiB != nil && *a.MemoryTotal.MinGiB > 0)
}
