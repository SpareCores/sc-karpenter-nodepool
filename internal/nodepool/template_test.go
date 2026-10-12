/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package nodepool

import (
	"errors"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/SpareCores/sc-karpenter-nodepool/api/v1alpha1"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/providers"
)

func parse(t *testing.T, extra string) *Template {
	t.Helper()
	tmpl, err := ParseTemplate(v1alpha1.NodePoolTemplate{
		Spec: runtime.RawExtension{Raw: []byte(templateSpec("karpenter.k8s.aws", "EC2NodeClass", extra))},
	})
	if err != nil {
		t.Fatal(err)
	}
	return tmpl
}

func TestParseTemplateErrors(t *testing.T) {
	for name, raw := range map[string]string{
		"empty":                   ``,
		"null":                    `null`,
		"not JSON":                `{`,
		"no nodeClassRef":         `{"template":{"spec":{}}}`,
		"nodeClassRef incomplete": `{"template":{"spec":{"nodeClassRef":{"group":"g","kind":"K"}}}}`,
		"requirements not a list": templateSpec("g", "K", `"requirements":{}`),
		"requirement not object":  templateSpec("g", "K", `"requirements":["x"]`),
		"requirement without key": templateSpec("g", "K", `"requirements":[{"operator":"Exists"}]`),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseTemplate(v1alpha1.NodePoolTemplate{Spec: runtime.RawExtension{Raw: []byte(raw)}})
			if !errors.Is(err, ErrInvalidTemplate) {
				t.Errorf("got %v", err)
			}
		})
	}
	tmpl := parse(t, "")
	if tmpl.NodeClassRef != (NodeClassRef{Group: "karpenter.k8s.aws", Kind: "EC2NodeClass", Name: "default"}) {
		t.Errorf("nodeClassRef %+v", tmpl.NodeClassRef)
	}
}

func TestTemplateConstraints(t *testing.T) {
	r := func(key, op string, values ...string) string {
		s := `{"key":"` + key + `","operator":"` + op + `"`
		if values != nil {
			s += `,"values":[`
			for i, v := range values {
				if i > 0 {
					s += ","
				}
				s += `"` + v + `"`
			}
			s += "]"
		}
		return s + "}"
	}
	reqs := func(rs ...string) string {
		out := `"requirements":[`
		for i, x := range rs {
			if i > 0 {
				out += ","
			}
			out += x
		}
		return out + "]"
	}
	const arch, ct, it = providers.LabelArch, providers.LabelCapacityType, providers.LabelInstanceType
	onDemandSpot := []string{providers.CapacityTypeOnDemand, providers.CapacityTypeSpot}

	cases := []struct {
		name      string
		extra     string
		archs     []v1alpha1.Architecture
		capacity  []string
		basis     v1alpha1.PriceBasis
		typeLimit bool
		zone      bool
	}{
		{name: "none", basis: v1alpha1.PriceBasisOnDemand},
		{name: "Exists does not restrict", extra: reqs(r(arch, OpExists), r(ct, OpExists)), basis: v1alpha1.PriceBasisOnDemand},
		{name: "In", extra: reqs(r(arch, OpIn, "arm64"), r(ct, OpIn, "spot")),
			archs: []v1alpha1.Architecture{arm64}, capacity: []string{"spot"}, basis: v1alpha1.PriceBasisSpot},
		{name: "NotIn subtracts from the universe", extra: reqs(r(arch, OpNotIn, "arm64"), r(ct, OpNotIn, "on-demand")),
			archs: []v1alpha1.Architecture{amd64}, capacity: []string{"spot"}, basis: v1alpha1.PriceBasisSpot},
		{name: "In and NotIn intersect", extra: reqs(r(ct, OpIn, "spot", "on-demand"), r(ct, OpNotIn, "spot")),
			capacity: []string{"on-demand"}, basis: v1alpha1.PriceBasisOnDemand},
		{name: "DoesNotExist allows nothing", extra: reqs(r(arch, OpDoesNotExist)),
			archs: []v1alpha1.Architecture{}, basis: v1alpha1.PriceBasisOnDemand},
		{name: "instance types and zones", extra: reqs(r(it, OpIn, "a", "b"), r(LabelZoneID, OpIn, "use1-az1")),
			basis: v1alpha1.PriceBasisOnDemand, typeLimit: true, zone: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tmpl := parse(t, c.extra)
			if got := tmpl.Architectures(); !slices.Equal(got, c.archs) || (got == nil) != (c.archs == nil) {
				t.Errorf("architectures %v, want %v", got, c.archs)
			}
			if got := tmpl.CapacityTypes(onDemandSpot); !slices.Equal(got, c.capacity) || (got == nil) != (c.capacity == nil) {
				t.Errorf("capacity types %v, want %v", got, c.capacity)
			}
			if got := tmpl.DefaultBasis(); got != c.basis {
				t.Errorf("basis %s, want %s", got, c.basis)
			}
			if tmpl.HasInstanceTypeConstraint() != c.typeLimit || tmpl.HasZoneRequirement() != c.zone {
				t.Errorf("type constraint %v zone %v", tmpl.HasInstanceTypeConstraint(), tmpl.HasZoneRequirement())
			}
		})
	}

	tmpl := parse(t, reqs(r(it, OpIn, "a", "b"), r(it, OpNotIn, "b")))
	for name, want := range map[string]bool{"a": true, "b": false, "c": false} {
		if tmpl.InstanceTypeAllowed(name) != want {
			t.Errorf("InstanceTypeAllowed(%s) != %v", name, want)
		}
	}
	if parse(t, reqs(r(it, OpDoesNotExist))).InstanceTypeAllowed("a") {
		t.Error("DoesNotExist allows a type")
	}
}

func TestParseNodeClassRef(t *testing.T) {
	ref, err := ParseNodeClassRef("karpenter.kwok.sh/KWOKNodeClass/default")
	if err != nil || ref != (NodeClassRef{Group: "karpenter.kwok.sh", Kind: "KWOKNodeClass", Name: "default"}) || ref.String() != "karpenter.kwok.sh/KWOKNodeClass/default" {
		t.Errorf("got %+v %v", ref, err)
	}
	for _, bad := range []string{"", "a/b", "a/b/c/d", "a//c"} {
		if _, err := ParseNodeClassRef(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestObjectHelpers(t *testing.T) {
	np := New()
	np.SetCreationTimestamp(metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
	if InstanceTypes(np) != nil || IsOurs(nil, "x") {
		t.Error("empty NodePool")
	}
	if _, ok := NodeClassRefOf(np); ok {
		t.Error("nodeClassRef of an empty NodePool")
	}
	if got := LastAppliedTime(np); !got.Equal(np.GetCreationTimestamp().Time) {
		t.Errorf("LastAppliedTime without managed fields = %v", got)
	}
	applied := metav1.NewTime(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	np.SetManagedFields([]metav1.ManagedFieldsEntry{
		{Manager: "kubectl", Operation: metav1.ManagedFieldsOperationUpdate, Time: &metav1.Time{Time: applied.Add(time.Hour)}},
		{Manager: FieldManager, Operation: metav1.ManagedFieldsOperationApply, Time: &applied},
	})
	if got := LastAppliedTime(np); !got.Equal(applied.Time) {
		t.Errorf("LastAppliedTime = %v", got)
	}

	_ = unstructured.SetNestedSlice(np.Object, []any{
		map[string]any{"type": "Ready", "status": "True", "reason": "R", "message": "m", "lastTransitionTime": "2026-01-03T00:00:00Z"},
		"garbage",
	}, "status", "conditions")
	conds := Conditions(np)
	c, ok := FindCondition(conds, "Ready")
	if len(conds) != 1 || !ok || c.Status != metav1.ConditionTrue || c.Reason != "R" || c.LastTransitionTime.Day() != 3 {
		t.Errorf("conditions %+v", conds)
	}
	if _, ok := FindCondition(conds, "Other"); ok {
		t.Error("found a missing condition")
	}
}
