/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package nodepool

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/SpareCores/sc-karpenter-nodepool/api/v1alpha1"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/providers"
)

// Input is everything Render needs.
type Input struct {
	// SCNodePool names the NodePool and owns it (deletionPolicy Delete).
	SCNodePool *v1alpha1.SCNodePool
	// Template is the parsed spec.nodePoolTemplate.
	Template *Template
	// InstanceTypes are the types to emit (after churn control).
	InstanceTypes []string
	// Architectures are the architectures of InstanceTypes, emitted when
	// the template has no kubernetes.io/arch requirement.
	Architectures []v1alpha1.Architecture
	// Rules are the provider adapter's requirement rules.
	Rules providers.RequirementRules
	// ForcedZones are emitted as topology.kubernetes.io/zone when the rules
	// force zones and the template has no zone requirement.
	ForcedZones []string
	// DataTimestamp is the freshness of the Spare Cores data (annotation).
	DataTimestamp *time.Time
	// NodeClassRefOverride replaces the template's nodeClassRef (test mode,
	// feature F12); the adapter is still chosen from the template.
	NodeClassRefOverride *NodeClassRef
}

// Rendered is the outcome of Render.
type Rendered struct {
	// NodePool is the object to apply.
	NodePool *unstructured.Unstructured
	// InstanceTypes are the emitted instance types, sorted.
	InstanceTypes []string
	// Invalid are instance types dropped because their names are not valid
	// label values (Karpenter would reject the NodePool at runtime).
	Invalid []string
}

// Render builds the NodePool of overview §3.4 steps 7–8: the template copied
// verbatim, a user instance-type In list intersected with the selection,
// capacity-type, arch and forced zones added per the adapter rules,
// ownership labels, volatile data only in metadata.annotations, a default
// disruption.consolidateAfter, and an ownerReference unless deletionPolicy
// is Orphan.
func Render(in Input) (*Rendered, error) {
	t, rules := in.Template, in.Rules
	for _, r := range t.requirements {
		if slices.Contains(rules.NeverEmit, r.Key) {
			return nil, fmt.Errorf("%w: %s is not supported by the provider; remove it from the template", ErrForbiddenKey, r.Key)
		}
	}

	out := &Rendered{}
	for _, name := range dedupe(in.InstanceTypes) {
		switch {
		case !t.InstanceTypeAllowed(name):
		case len(validation.IsValidLabelValue(name)) > 0:
			out.Invalid = append(out.Invalid, name)
		default:
			out.InstanceTypes = append(out.InstanceTypes, name)
		}
	}
	if len(out.InstanceTypes) == 0 {
		return out, fmt.Errorf("%w: the selection does not intersect the template's instance-type requirement", ErrEmptySelection)
	}

	var reqs []any
	var typeMinValues *int
	for i, r := range t.requirements {
		if r.Key == providers.LabelInstanceType && r.Operator == OpIn {
			// Merged into the generated list, keeping the strictest minValues.
			if r.MinValues != nil && (typeMinValues == nil || *r.MinValues > *typeMinValues) {
				typeMinValues = r.MinValues
			}
			continue
		}
		reqs = append(reqs, runtime.DeepCopyJSONValue(t.rawRequirements[i]))
	}

	if archs := t.Architectures(); archs != nil {
		if _, _, err := rules.ResolveArchitectures(archs, nil); err != nil {
			return nil, err
		}
	} else {
		values, emit, err := rules.ResolveArchitectures(nil, in.Architectures)
		if err != nil {
			return nil, err
		}
		if emit {
			reqs = append(reqs, Requirement{Key: providers.LabelArch, Operator: OpIn, Values: toStrings(values)}.toMap())
		}
	}

	templateCT := t.CapacityTypes(rules.AllowedCapacityTypes)
	ct, err := rules.ResolveCapacityTypes(templateCT)
	if err != nil {
		return nil, err
	}
	if templateCT == nil {
		reqs = append(reqs, Requirement{Key: providers.LabelCapacityType, Operator: OpIn, Values: ct}.toMap())
	}

	if rules.ForceZones && !t.HasZoneRequirement() && len(in.ForcedZones) > 0 {
		reqs = append(reqs, Requirement{Key: providers.LabelZone, Operator: OpIn, Values: dedupe(in.ForcedZones)}.toMap())
	}
	reqs = append(reqs, Requirement{
		Key: providers.LabelInstanceType, Operator: OpIn, Values: out.InstanceTypes, MinValues: typeMinValues,
	}.toMap())

	spec := t.Spec()
	tmplSpec := ensureMap(ensureMap(spec, "template"), "spec")
	tmplSpec["requirements"] = reqs
	if o := in.NodeClassRefOverride; o != nil {
		tmplSpec["nodeClassRef"] = map[string]any{"group": o.Group, "kind": o.Kind, "name": o.Name}
	}
	disruption := ensureMap(spec, "disruption")
	if _, ok := disruption["consolidateAfter"]; !ok {
		disruption["consolidateAfter"] = DefaultConsolidateAfter
	}

	np := New()
	np.SetName(in.SCNodePool.Name)
	labels := maps.Clone(t.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[LabelManagedBy] = ManagedByValue
	labels[LabelSCNodePool] = in.SCNodePool.Name
	np.SetLabels(labels)
	annotations := maps.Clone(t.Annotations)
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[AnnotationSelectionHash] = SelectionHash(out.InstanceTypes)
	if in.DataTimestamp != nil {
		annotations[AnnotationDataTimestamp] = in.DataTimestamp.UTC().Format(time.RFC3339)
	}
	np.SetAnnotations(annotations)
	if in.SCNodePool.Spec.DeletionPolicy != v1alpha1.DeletionPolicyOrphan {
		np.Object["metadata"].(map[string]any)["ownerReferences"] = []any{map[string]any{
			"apiVersion":         v1alpha1.GroupVersion.String(),
			"kind":               "SCNodePool",
			"name":               in.SCNodePool.Name,
			"uid":                string(in.SCNodePool.UID),
			"controller":         true,
			"blockOwnerDeletion": true,
		}}
	}
	np.Object["spec"] = spec
	out.NodePool = np
	return out, nil
}

// SelectionHash identifies an instance-type list (order-independent).
func SelectionHash(types []string) string {
	sum := sha256.Sum256([]byte(strings.Join(dedupe(types), "\n")))
	return hex.EncodeToString(sum[:8])
}

func ensureMap(m map[string]any, key string) map[string]any {
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	v := map[string]any{}
	m[key] = v
	return v
}

func toStrings[T ~string](vs []T) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = string(v)
	}
	return out
}
