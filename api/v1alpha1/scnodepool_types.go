/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package v1alpha1

import (
	selapi "github.com/SpareCores/sc-keeper-go/selection/api"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Numeric conventions of this API (the CRD is generated with controller-gen's
// allowDangerousTypes, see the Makefile):
//
//   - counts and ranks are integers;
//   - every other number (GiB, GB, GHz, Gbps, KiB, MiB, percent, benchmark
//     scores) is a float64, so YAML such as `minGiB: 3.8` or `min: 1.5` works
//     without quoting;
//   - money (USD per hour) is the exception: it is a decimal string such as
//     "0.40", so the value never goes through binary floating point.
//
// Memory sizes are GiB (2^30 bytes), prices are USD per hour. All selector
// semantics are specified in docs/reference/selectors.md.

// DeletionPolicy decides what happens to the generated NodePool when the
// SCNodePool is deleted.
//   - Delete: the NodePool is owned by the SCNodePool and garbage-collected
//     with it (Karpenter then drains its nodes);
//   - Orphan: the ownerReference is removed and the NodePool is kept.
//
// +kubebuilder:validation:Enum=Delete;Orphan
type DeletionPolicy string

// RegionSource says where the controller found the regions of the cluster.
// +kubebuilder:validation:Enum=NodeClass;NodeLabels
type RegionSource string

const (
	DeletionPolicyDelete DeletionPolicy = "Delete"
	DeletionPolicyOrphan DeletionPolicy = "Orphan"

	RegionSourceNodeClass  RegionSource = "NodeClass"
	RegionSourceNodeLabels RegionSource = "NodeLabels"
)

// Condition types of SCNodePool.status.conditions.
const (
	// ConditionReady summarises the other conditions.
	ConditionReady = "Ready"
	// ConditionDataAvailable: Keeper is reachable and the benchmarks are valid.
	ConditionDataAvailable = "DataAvailable"
	// ConditionSelectionNonEmpty: at least one instance type was selected.
	ConditionSelectionNonEmpty = "SelectionNonEmpty"
	// ConditionNodePoolSynced: the NodePool was applied (or skipped in dryRun).
	ConditionNodePoolSynced = "NodePoolSynced"
	// ConditionNodePoolManaged: a Karpenter controller set conditions on the NodePool.
	ConditionNodePoolManaged = "NodePoolManaged"
	// ConditionNodePoolValid mirrors the NodePool's ValidationSucceeded condition.
	ConditionNodePoolValid = "NodePoolValid"
	// ConditionNodePoolSchedulable is False while Karpenter reports
	// NoCompatibleInstanceTypes for the NodePool.
	ConditionNodePoolSchedulable = "NodePoolSchedulable"
	// ConditionRegionConflict is a warning: the region detection sources disagree.
	ConditionRegionConflict = "RegionConflict"
)

// Reasons of the Ready condition (and of the step condition they concern).
// Every reason except Ready and DryRun keeps the last applied NodePool
// unchanged.
const (
	// ReasonReady: the NodePool is applied and healthy.
	ReasonReady = "Ready"
	// ReasonDryRun: the selection was computed; dryRun never applies it.
	ReasonDryRun = "DryRun"
	// ReasonInvalidTemplate: nodePoolTemplate cannot be parsed, or uses a
	// requirement key the provider forbids.
	ReasonInvalidTemplate = "InvalidTemplate"
	// ReasonInvalidSpec: the selector or ranking is invalid in a way the
	// CRD schema cannot express.
	ReasonInvalidSpec = "InvalidSpec"
	// ReasonUnsupportedProvider: no adapter for the template's nodeClassRef.
	ReasonUnsupportedProvider = "UnsupportedProvider"
	// ReasonCapacityTypeUnsupported: the template asks for capacity types
	// the provider cannot launch.
	ReasonCapacityTypeUnsupported = "CapacityTypeUnsupported"
	// ReasonArchitectureUnsupported: the template or the selection needs
	// architectures the provider cannot launch (together).
	ReasonArchitectureUnsupported = "ArchitectureUnsupported"
	// ReasonNameConflict: a NodePool of the same name exists and is not
	// labelled as generated from this SCNodePool.
	ReasonNameConflict = "NameConflict"
	// ReasonNodeClassRefImmutable: the template changes nodeClassRef group
	// or kind of the existing NodePool.
	ReasonNodeClassRefImmutable = "NodeClassRefImmutable"
	// ReasonRegionPending: no region source has a value yet.
	ReasonRegionPending = "RegionPending"
	// ReasonRegionUnknown: a detected region is not in Spare Cores.
	ReasonRegionUnknown = "RegionUnknown"
	// ReasonDataStale: Keeper (or the provider price list) is unreachable.
	ReasonDataStale = "DataStale"
	// ReasonUnknownBenchmarkConfig: a benchmark or config is not in Keeper.
	ReasonUnknownBenchmarkConfig = "UnknownBenchmarkConfig"
	// ReasonNoCandidates: the selection is empty (or does not intersect the
	// template's instance-type list).
	ReasonNoCandidates = "NoCandidates"
	// ReasonValidationFailed: the rendered NodePool fails validation.
	ReasonValidationFailed = "ValidationFailed"
	// ReasonNodePoolRejected: the server-side dry run rejected the NodePool.
	ReasonNodePoolRejected = "NodePoolRejected"
	// ReasonApplyFailed: applying the NodePool failed.
	ReasonApplyFailed = "ApplyFailed"
	// ReasonNodePoolNotManaged: no Karpenter controller manages the NodePool.
	ReasonNodePoolNotManaged = "NodePoolNotManaged"
	// ReasonNodePoolInvalid: Karpenter reports ValidationSucceeded=False.
	ReasonNodePoolInvalid = "NodePoolInvalid"
	// ReasonNodePoolUnschedulable: Karpenter reports
	// NoCompatibleInstanceTypes for the NodePool.
	ReasonNodePoolUnschedulable = "NodePoolUnschedulable"
)

// ---------------------------------------------------------------------------
// Spec
// ---------------------------------------------------------------------------

// SCNodePoolSpec defines the desired state of SCNodePool.
type SCNodePoolSpec struct {
	// nodePoolTemplate is the template of the generated Karpenter NodePool.
	// +required
	NodePoolTemplate NodePoolTemplate `json:"nodePoolTemplate"`

	// selector filters the Spare Cores candidates. All fields are optional;
	// without a selector every candidate that passes the provider's
	// eligibility filter is emitted.
	// +optional
	Selector *selapi.Selector `json:"selector,omitempty"`

	// ranking keeps only the best families of the candidates that passed the
	// selector. Without ranking, every candidate is emitted.
	// +optional
	Ranking *selapi.Ranking `json:"ranking,omitempty"`

	// refresh controls how often the selection is re-evaluated.
	// +kubebuilder:default={}
	// +optional
	Refresh Refresh `json:"refresh,omitzero"`

	// churn limits how fast instance types are removed from the NodePool
	// (removing a type drifts every running node of that type).
	// +kubebuilder:default={}
	// +optional
	Churn selapi.Churn `json:"churn,omitzero"`

	// dryRun computes and reports the selection in status without ever
	// creating or updating the NodePool.
	// +kubebuilder:default=false
	// +optional
	DryRun bool `json:"dryRun,omitempty"`

	// deletionPolicy decides what happens to the NodePool when this object is
	// deleted.
	// +kubebuilder:default=Delete
	// +optional
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`
}

// NodePoolTemplate is the template of the generated karpenter.sh/v1 NodePool.
type NodePoolTemplate struct {
	// metadata holds the labels and annotations of the NodePool object
	// (not of the nodes).
	// +optional
	Metadata NodePoolTemplateMetadata `json:"metadata,omitzero"`

	// spec is a karpenter.sh/v1 NodePoolSpec, verbatim. It is deliberately
	// untyped so that every provider's NodePool schema version is accepted
	// (for example EKS Auto Mode adds fields and enum values). The controller
	// validates it at runtime with a server-side dry run against the
	// cluster's NodePool CRD. template.spec.nodeClassRef is required and
	// selects the provider adapter.
	// +required
	Spec runtime.RawExtension `json:"spec"`
}

// NodePoolTemplateMetadata are the labels and annotations of the generated
// NodePool object.
type NodePoolTemplateMetadata struct {
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Refresh controls the re-evaluation period.
type Refresh struct {
	// interval is the re-evaluation period, at least one hour. Spec changes
	// re-evaluate immediately, as does a new value of the annotation
	// karpenter.sparecores.com/refresh-requested.
	// +kubebuilder:default="24h"
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$`
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1h')",message="refresh.interval must be at least 1h"
	// +optional
	Interval string `json:"interval,omitempty"`
}

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

// SCNodePoolStatus defines the observed state of SCNodePool.
type SCNodePoolStatus struct {
	// observedGeneration is the metadata.generation the controller last
	// processed.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions: Ready, DataAvailable, SelectionNonEmpty, NodePoolSynced,
	// NodePoolManaged, NodePoolValid, NodePoolSchedulable, RegionConflict.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// provider is the id of the provider adapter chosen from the nodeClassRef.
	// +optional
	Provider string `json:"provider,omitempty"`

	// regions are the detected Kubernetes region values.
	// +optional
	Regions []string `json:"regions,omitempty"`

	// regionSource tells where the regions were detected.
	// +optional
	RegionSource RegionSource `json:"regionSource,omitempty"`

	// nodePoolName is the name of the generated NodePool.
	// +optional
	NodePoolName string `json:"nodePoolName,omitempty"`

	// dataTimestamp is the freshness of the Keeper data used.
	// +optional
	DataTimestamp *metav1.Time `json:"dataTimestamp,omitempty"`

	// lastRefreshTime is when the selection was last evaluated.
	// +optional
	LastRefreshTime *metav1.Time `json:"lastRefreshTime,omitempty"`

	// selected describes the current selection.
	// +optional
	Selected *SelectedStatus `json:"selected,omitempty"`

	// excluded counts the candidates dropped by each pipeline step.
	// +optional
	Excluded *selapi.ExcludedStatus `json:"excluded,omitempty"`

	// pendingRemovals are types that no longer qualify but are kept until the
	// churn control lets them go.
	// +listType=map
	// +listMapKey=name
	// +optional
	PendingRemovals []selapi.PendingRemoval `json:"pendingRemovals,omitempty"`
}

// SelectedStatus describes the selected instance types.
type SelectedStatus struct {
	// count is the number of selected instance types.
	Count int32 `json:"count"`
	// band reports what the ranking applied.
	// +optional
	Band *selapi.BandStatus `json:"band,omitempty"`
	// families are the kept families in rank order.
	// +optional
	Families []FamilyStatus `json:"families,omitempty"`
	// instanceTypes are the selected types with their score and price.
	// +optional
	InstanceTypes []InstanceTypeStatus `json:"instanceTypes,omitempty"`
}

// FamilyStatus is one kept family.
type FamilyStatus struct {
	Name string `json:"name"`
	// rank starts at 1.
	Rank int32 `json:"rank"`
	// objective is the family's median objective value.
	// +optional
	Objective *float64 `json:"objective,omitempty"`
	// sizes is the number of selected sizes.
	Sizes int32 `json:"sizes"`
}

// InstanceTypeStatus is one selected instance type.
type InstanceTypeStatus struct {
	Name   string `json:"name"`
	Family string `json:"family,omitempty"`
	// score is the ranking score, when ranking is configured.
	// +optional
	Score *float64 `json:"score,omitempty"`
	// price is the hourly USD price on the price basis.
	// +optional
	Price *float64 `json:"price,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="NodePool",type=string,JSONPath=`.status.nodePoolName`
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.status.provider`
// +kubebuilder:printcolumn:name="Types",type=integer,JSONPath=`.status.selected.count`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Last refresh",type=date,JSONPath=`.status.lastRefreshTime`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SCNodePool selects instance types from Spare Cores data and generates a
// Karpenter NodePool from them. Cluster-scoped; the generated NodePool has the
// same name.
type SCNodePool struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of SCNodePool
	// +required
	Spec SCNodePoolSpec `json:"spec"`

	// status defines the observed state of SCNodePool
	// +optional
	Status SCNodePoolStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SCNodePoolList contains a list of SCNodePool
type SCNodePoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SCNodePool `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &SCNodePool{}, &SCNodePoolList{})
		return nil
	})
}

// Selection returns the part of the spec the selection engine reads.
func (s *SCNodePoolSpec) Selection() *selapi.Spec {
	return &selapi.Spec{Selector: s.Selector, Ranking: s.Ranking, Churn: s.Churn}
}
