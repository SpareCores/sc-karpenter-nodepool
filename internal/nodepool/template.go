/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package nodepool

import (
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	utiljson "k8s.io/apimachinery/pkg/util/json"

	"github.com/SpareCores/sc-karpenter-nodepool/api/v1alpha1"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/providers"
)

// Requirement operators of Karpenter NodePools.
const (
	OpIn           = "In"
	OpNotIn        = "NotIn"
	OpExists       = "Exists"
	OpDoesNotExist = "DoesNotExist"
	OpGt           = "Gt"
	OpLt           = "Lt"
	OpGte          = "Gte"
	OpLte          = "Lte"
)

// LabelZoneID is the AWS zone-ID key, an alternative zone requirement
// (overview §3.2).
const LabelZoneID = "topology.k8s.aws/zone-id"

// NodeClassRef is a NodePool's spec.template.spec.nodeClassRef.
type NodeClassRef struct {
	Group string
	Kind  string
	Name  string
}

// String formats the reference as group/kind/name.
func (r NodeClassRef) String() string {
	return r.Group + "/" + r.Kind + "/" + r.Name
}

// ParseNodeClassRef parses group/kind/name (the test-mode override flag).
func ParseNodeClassRef(s string) (NodeClassRef, error) {
	parts := strings.Split(s, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return NodeClassRef{}, fmt.Errorf("nodeClassRef %q: want group/kind/name", s)
	}
	return NodeClassRef{Group: parts[0], Kind: parts[1], Name: parts[2]}, nil
}

// Requirement is one NodePool requirement.
type Requirement struct {
	Key       string
	Operator  string
	Values    []string
	MinValues *int
}

func requirementFromMap(m map[string]any) Requirement {
	r := Requirement{Key: str(m["key"]), Operator: str(m["operator"])}
	if vs, ok := m["values"].([]any); ok {
		for _, v := range vs {
			r.Values = append(r.Values, fmt.Sprint(v))
		}
	}
	switch v := m["minValues"].(type) {
	case int64:
		n := int(v)
		r.MinValues = &n
	case float64:
		n := int(v)
		r.MinValues = &n
	}
	return r
}

func (r Requirement) toMap() map[string]any {
	values := make([]any, len(r.Values))
	for i, v := range r.Values {
		values[i] = v
	}
	m := map[string]any{"key": r.Key, "operator": r.Operator, "values": values}
	if r.MinValues != nil {
		m["minValues"] = int64(*r.MinValues)
	}
	return m
}

// Template is a parsed nodePoolTemplate.
type Template struct {
	// Labels and Annotations of the NodePool object.
	Labels      map[string]string
	Annotations map[string]string
	// NodeClassRef of the template (never the test-mode override).
	NodeClassRef NodeClassRef

	spec map[string]any
	// rawRequirements are the user requirements as written, emitted
	// verbatim; requirements is their parsed form.
	rawRequirements []any
	requirements    []Requirement
}

// ParseTemplate parses and checks an SCNodePool's nodePoolTemplate.
func ParseTemplate(t v1alpha1.NodePoolTemplate) (*Template, error) {
	if len(t.Spec.Raw) == 0 {
		return nil, fmt.Errorf("%w: spec is required", ErrInvalidTemplate)
	}
	var spec map[string]any
	// utiljson keeps integers as int64, as unstructured objects need.
	if err := utiljson.Unmarshal(t.Spec.Raw, &spec); err != nil {
		return nil, fmt.Errorf("%w: spec: %v", ErrInvalidTemplate, err)
	}
	if spec == nil {
		return nil, fmt.Errorf("%w: spec is required", ErrInvalidTemplate)
	}
	tmpl := &Template{Labels: t.Metadata.Labels, Annotations: t.Metadata.Annotations, spec: spec}

	ref, _ := nested(spec, "template", "spec", "nodeClassRef").(map[string]any)
	tmpl.NodeClassRef = NodeClassRef{Group: str(ref["group"]), Kind: str(ref["kind"]), Name: str(ref["name"])}
	if tmpl.NodeClassRef.Group == "" || tmpl.NodeClassRef.Kind == "" || tmpl.NodeClassRef.Name == "" {
		return nil, fmt.Errorf("%w: spec.template.spec.nodeClassRef needs group, kind and name", ErrInvalidTemplate)
	}

	switch reqs := nested(spec, "template", "spec", "requirements").(type) {
	case nil:
	case []any:
		for i, item := range reqs {
			m, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%w: spec.template.spec.requirements[%d] is not an object", ErrInvalidTemplate, i)
			}
			r := requirementFromMap(m)
			if r.Key == "" || r.Operator == "" {
				return nil, fmt.Errorf("%w: spec.template.spec.requirements[%d] needs key and operator", ErrInvalidTemplate, i)
			}
			tmpl.rawRequirements = append(tmpl.rawRequirements, m)
			tmpl.requirements = append(tmpl.requirements, r)
		}
	default:
		return nil, fmt.Errorf("%w: spec.template.spec.requirements is not a list", ErrInvalidTemplate)
	}
	return tmpl, nil
}

// Spec returns a deep copy of the NodePool spec of the template.
func (t *Template) Spec() map[string]any {
	return runtime.DeepCopyJSON(t.spec)
}

// Requirements returns the parsed user requirements.
func (t *Template) Requirements() []Requirement {
	return slices.Clone(t.requirements)
}

// restricts reports whether an operator restricts the values of a key.
func restricts(op string) bool {
	return op == OpIn || op == OpNotIn || op == OpDoesNotExist
}

// allowed returns the values the template's requirements on key allow:
// In values as written (so values the provider cannot launch stay visible),
// NotIn subtracted from universe. restricted is false when no requirement
// restricts the key's values.
func (t *Template) allowed(key string, universe []string) (values []string, restricted bool) {
	for _, r := range t.requirements {
		if r.Key != key || !restricts(r.Operator) {
			continue
		}
		if !restricted {
			values, restricted = slices.Clone(universe), true
			if r.Operator == OpIn {
				values = dedupe(r.Values)
				continue
			}
		}
		switch r.Operator {
		case OpIn:
			values = slices.DeleteFunc(values, func(v string) bool { return !slices.Contains(r.Values, v) })
		case OpNotIn:
			values = slices.DeleteFunc(values, func(v string) bool { return slices.Contains(r.Values, v) })
		case OpDoesNotExist:
			values = values[:0]
		}
	}
	if restricted && values == nil {
		values = []string{}
	}
	return values, restricted
}

// knownArchitectures is the universe a NotIn arch requirement subtracts
// from.
var knownArchitectures = []string{string(v1alpha1.ArchitectureAMD64), string(v1alpha1.ArchitectureARM64)}

// knownCapacityTypes is the universe of karpenter.sh/capacity-type values.
var knownCapacityTypes = []string{providers.CapacityTypeOnDemand, providers.CapacityTypeSpot, providers.CapacityTypeReserved}

// Architectures returns the kubernetes.io/arch values the template allows
// (nil: unrestricted).
func (t *Template) Architectures() []v1alpha1.Architecture {
	values, ok := t.allowed(providers.LabelArch, knownArchitectures)
	if !ok {
		return nil
	}
	out := make([]v1alpha1.Architecture, len(values))
	for i, v := range values {
		out[i] = v1alpha1.Architecture(v)
	}
	return out
}

// CapacityTypes returns the karpenter.sh/capacity-type values the template
// allows (nil: unrestricted). A NotIn subtracts from universe, normally the
// provider's allowed capacity types.
func (t *Template) CapacityTypes(universe []string) []string {
	values, ok := t.allowed(providers.LabelCapacityType, universe)
	if !ok {
		return nil
	}
	return values
}

// DefaultBasis is the price basis the template implies: Spot when it allows
// spot but not on-demand capacity, else OnDemand (overview §3.4 step 4).
func (t *Template) DefaultBasis() v1alpha1.PriceBasis {
	ct := t.CapacityTypes(knownCapacityTypes)
	if ct != nil && slices.Contains(ct, providers.CapacityTypeSpot) && !slices.Contains(ct, providers.CapacityTypeOnDemand) {
		return v1alpha1.PriceBasisSpot
	}
	return v1alpha1.PriceBasisOnDemand
}

// HasInstanceTypeConstraint reports whether the template restricts
// node.kubernetes.io/instance-type.
func (t *Template) HasInstanceTypeConstraint() bool {
	return slices.ContainsFunc(t.requirements, func(r Requirement) bool {
		return r.Key == providers.LabelInstanceType && restricts(r.Operator)
	})
}

// InstanceTypeAllowed reports whether the template's instance-type
// requirements allow a type.
func (t *Template) InstanceTypeAllowed(name string) bool {
	for _, r := range t.requirements {
		if r.Key != providers.LabelInstanceType {
			continue
		}
		switch r.Operator {
		case OpIn:
			if !slices.Contains(r.Values, name) {
				return false
			}
		case OpNotIn:
			if slices.Contains(r.Values, name) {
				return false
			}
		case OpDoesNotExist:
			return false
		}
	}
	return true
}

// HasZoneRequirement reports whether the template has a zone requirement
// (topology.kubernetes.io/zone or, on AWS, topology.k8s.aws/zone-id).
func (t *Template) HasZoneRequirement() bool {
	return slices.ContainsFunc(t.requirements, func(r Requirement) bool {
		return r.Key == providers.LabelZone || r.Key == LabelZoneID
	})
}

func nested(m map[string]any, path ...string) any {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[p]
	}
	return cur
}

func dedupe(vs []string) []string {
	out := slices.Clone(vs)
	slices.Sort(out)
	return slices.Compact(out)
}
