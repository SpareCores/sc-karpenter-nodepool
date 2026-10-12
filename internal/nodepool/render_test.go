/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package nodepool

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"

	"github.com/SpareCores/sc-karpenter-nodepool/api/v1alpha1"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/providers"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata/golden")

var registry = providers.NewRegistry()

// dataTime is a fixed, synthetic data timestamp.
var dataTime = time.Date(2026, 10, 5, 10, 2, 57, 0, time.UTC)

// scNodePool returns a synthetic SCNodePool with the given template spec
// (JSON) and template metadata labels.
func scNodePool(name, spec string, labels map[string]string) *v1alpha1.SCNodePool {
	return &v1alpha1.SCNodePool{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: "00000000-0000-4000-8000-000000000001"},
		Spec: v1alpha1.SCNodePoolSpec{
			NodePoolTemplate: v1alpha1.NodePoolTemplate{
				Metadata: v1alpha1.NodePoolTemplateMetadata{Labels: labels},
				Spec:     runtime.RawExtension{Raw: []byte(spec)},
			},
			DeletionPolicy: v1alpha1.DeletionPolicyDelete,
		},
	}
}

func templateSpec(group, kind, extra string) string {
	s := `{"template":{"spec":{"nodeClassRef":{"group":"` + group + `","kind":"` + kind + `","name":"default"}`
	if extra != "" {
		s += "," + extra
	}
	return s + `}}}`
}

func rulesOf(t *testing.T, id providers.ID) providers.RequirementRules {
	t.Helper()
	a, ok := registry.Get(id)
	if !ok {
		t.Fatalf("no adapter %s", id)
	}
	return a.RequirementRules()
}

func mustRender(t *testing.T, in Input) *Rendered {
	t.Helper()
	tmpl, err := ParseTemplate(in.SCNodePool.Spec.NodePoolTemplate)
	if err != nil {
		t.Fatal(err)
	}
	in.Template = tmpl
	out, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

var (
	amd64 = v1alpha1.ArchitectureAMD64
	arm64 = v1alpha1.ArchitectureARM64
)

// TestGolden renders one NodePool per adapter from synthetic selections
// (instance-type names are real, so the files double as admission fixtures
// for the CRD matrix lane). Regenerate with: go test ./internal/nodepool -update
func TestGolden(t *testing.T) {
	cases := []struct {
		id     providers.ID
		group  string
		kind   string
		extra  string // template spec fields, JSON inside spec.template.spec
		top    string // top-level spec fields, JSON
		labels map[string]string
		types  []string
		archs  []v1alpha1.Architecture
		zones  []string
	}{
		{
			// The example of overview §3.1.
			id: providers.AWS, group: "karpenter.k8s.aws", kind: "EC2NodeClass",
			extra: `"requirements":[{"key":"kubernetes.io/arch","operator":"In","values":["amd64","arm64"]},` +
				`{"key":"karpenter.sh/capacity-type","operator":"In","values":["spot","on-demand"]}],"expireAfter":"720h"`,
			top:    `"weight":50,"disruption":{"consolidationPolicy":"WhenEmptyOrUnderutilized","consolidateAfter":"1m"},"limits":{"cpu":"256"}`,
			labels: map[string]string{"team": "web"},
			types:  []string{"c7a.xlarge", "c7a.large", "m7a.large", "m7g.large"},
			archs:  []v1alpha1.Architecture{amd64, arm64},
		},
		{
			id: providers.AWSAutoMode, group: "eks.amazonaws.com", kind: "NodeClass",
			extra: `"requirements":[{"key":"topology.k8s.aws/zone-id","operator":"In","values":["use1-az1","use1-az2"]}]`,
			types: []string{"c7i.large", "m7g.xlarge"}, archs: []v1alpha1.Architecture{amd64, arm64},
		},
		{
			id: providers.Azure, group: "karpenter.azure.com", kind: "AKSNodeClass",
			extra: `"requirements":[{"key":"topology.kubernetes.io/zone","operator":"In","values":["westeurope-1","westeurope-2"]}]`,
			types: []string{"Standard_D2as_v5", "Standard_D4as_v5"}, archs: []v1alpha1.Architecture{amd64},
		},
		{
			id: providers.GCP, group: "karpenter.k8s.gcp", kind: "GCENodeClass",
			types: []string{"n2d-standard-2", "c3-standard-4"}, archs: []v1alpha1.Architecture{amd64},
		},
		{
			id: providers.AlicloudACK, group: "karpenter.alibabacloud.com", kind: "ECSNodeClass",
			extra: `"requirements":[{"key":"karpenter.sh/capacity-type","operator":"In","values":["spot"]}]`,
			types: []string{"ecs.g7.large", "ecs.c7.xlarge"}, archs: []v1alpha1.Architecture{amd64},
		},
		{
			id: providers.AlicloudCloudpilot, group: "karpenter.k8s.alibabacloud", kind: "ECSNodeClass",
			extra: `"requirements":[{"key":"kubernetes.io/arch","operator":"In","values":["arm64"]}]`,
			types: []string{"ecs.g8y.large", "ecs.c8y.xlarge"}, archs: []v1alpha1.Architecture{arm64},
		},
		{
			id: providers.Hcloud, group: "karpenter.hetzner.cloud", kind: "HCloudNodeClass",
			types: []string{"cx22", "cpx31", "cax21"}, archs: []v1alpha1.Architecture{amd64, arm64},
		},
		{
			id: providers.HcloudItsh, group: "karpenter.itsh.dev", kind: "HCloudNodeClass",
			types: []string{"cx22", "cpx31"}, archs: []v1alpha1.Architecture{amd64},
		},
		{
			id: providers.UpCloud, group: "karpenter.k8s.upcloud", kind: "UpCloudNodeClass",
			types: []string{"CLOUDNATIVE-2xCPU-4GB", "DEV-1xCPU-2GB"}, archs: []v1alpha1.Architecture{amd64},
		},
		{
			id: providers.OVH, group: "karpenter.ovhcloud.sh", kind: "OVHNodeClass",
			types: []string{"b3-8", "c3-4"}, archs: []v1alpha1.Architecture{amd64}, zones: []string{"gra11-a"},
		},
		{
			id: providers.Vultr, group: "karpenter.vultr.com", kind: "VultrNodeClass",
			extra: `"requirements":[{"key":"node.kubernetes.io/instance-type","operator":"In","values":["vc2-2c-4gb","vc2-4c-8gb"],"minValues":1}]`,
			types: []string{"vc2-2c-4gb", "vhf-2c-4gb"}, archs: []v1alpha1.Architecture{amd64},
		},
	}
	for _, c := range cases {
		t.Run(string(c.id), func(t *testing.T) {
			spec := templateSpec(c.group, c.kind, c.extra)
			if c.top != "" {
				spec = strings.TrimSuffix(spec, "}") + "," + c.top + "}"
			}
			rules := rulesOf(t, c.id)
			out := mustRender(t, Input{
				SCNodePool:    scNodePool("sc-"+string(c.id), spec, c.labels),
				InstanceTypes: c.types,
				Architectures: c.archs,
				Rules:         rules,
				ForcedZones:   c.zones,
				DataTimestamp: &dataTime,
			})
			if err := Validate(context.Background(), out.NodePool, rules); err != nil {
				t.Fatalf("golden NodePool does not validate: %v", err)
			}
			got, err := yaml.Marshal(out.NodePool.Object)
			if err != nil {
				t.Fatal(err)
			}
			file := filepath.Join("testdata", "golden", string(c.id)+".yaml")
			if *update {
				if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if string(got) != string(want) {
				t.Errorf("%s differs from the rendered NodePool (run with -update):\n%s", file, got)
			}
		})
	}
}

func requirement(t *testing.T, np *unstructured.Unstructured, key string) (Requirement, int) {
	t.Helper()
	n := 0
	var found Requirement
	for _, r := range RequirementsOf(np) {
		if r.Key == key {
			found = r
			n++
		}
	}
	return found, n
}

// TestRenderRequirements covers the requirement and metadata rules of Render.
func TestRenderRequirements(t *testing.T) {
	t.Run("defaults from the selection and the adapter", renderDefaults)
	t.Run("user requirements are kept and win", renderUserRequirements)
	t.Run("forced zones unless the template has a zone requirement", renderForcedZones)
	t.Run("invalid label values are dropped", renderInvalidValues)
	t.Run("orphan, override and template fields", renderTemplateFields)
}

// renderDefaults: defaults from the selection and the adapter.
func renderDefaults(t *testing.T) {
	aws := rulesOf(t, providers.AWS)
	out := mustRender(t, Input{
		SCNodePool:    scNodePool("p", templateSpec("karpenter.k8s.aws", "EC2NodeClass", ""), nil),
		InstanceTypes: []string{"m7g.large", "c7a.large", "c7a.large"},
		Architectures: []v1alpha1.Architecture{arm64, amd64},
		Rules:         aws,
	})
	if !slices.Equal(out.InstanceTypes, []string{"c7a.large", "m7g.large"}) {
		t.Errorf("types %v", out.InstanceTypes)
	}
	if r, n := requirement(t, out.NodePool, providers.LabelArch); n != 1 || !slices.Equal(r.Values, []string{"amd64", "arm64"}) {
		t.Errorf("arch %+v (%d)", r, n)
	}
	if r, n := requirement(t, out.NodePool, providers.LabelCapacityType); n != 1 || !slices.Equal(r.Values, []string{"on-demand"}) {
		t.Errorf("capacity type %+v (%d)", r, n)
	}
	if _, n := requirement(t, out.NodePool, providers.LabelZone); n != 0 {
		t.Error("zone emitted on AWS")
	}
	if got, _, _ := unstructured.NestedString(out.NodePool.Object, "spec", "disruption", "consolidateAfter"); got != "0s" {
		t.Errorf("consolidateAfter %q", got)
	}
	if refs := out.NodePool.GetOwnerReferences(); len(refs) != 1 || refs[0].Kind != "SCNodePool" || refs[0].Name != "p" ||
		refs[0].Controller == nil || !*refs[0].Controller {
		t.Errorf("owner references %+v", refs)
	}
	if !IsOurs(out.NodePool, "p") || IsOurs(out.NodePool, "q") {
		t.Error("ownership labels")
	}
	if a := out.NodePool.GetAnnotations(); a[AnnotationSelectionHash] != SelectionHash(out.InstanceTypes) || a[AnnotationDataTimestamp] != "" {
		t.Errorf("annotations %v", a)
	}
	if !slices.Equal(InstanceTypes(out.NodePool), out.InstanceTypes) {
		t.Errorf("InstanceTypes() = %v", InstanceTypes(out.NodePool))
	}
}

// renderUserRequirements: user requirements are kept and win.
func renderUserRequirements(t *testing.T) {
	aws := rulesOf(t, providers.AWS)
	extra := `"requirements":[` +
		`{"key":"kubernetes.io/arch","operator":"NotIn","values":["arm64"]},` +
		`{"key":"karpenter.sh/capacity-type","operator":"In","values":["spot"]},` +
		`{"key":"node.kubernetes.io/instance-type","operator":"In","values":["c7a.large","c7a.xlarge"],"minValues":2},` +
		`{"key":"node.kubernetes.io/instance-type","operator":"NotIn","values":["c7a.2xlarge"]},` +
		`{"key":"sparecores.com/tier","operator":"In","values":["fast"]}]`
	out := mustRender(t, Input{
		SCNodePool:    scNodePool("p", templateSpec("karpenter.k8s.aws", "EC2NodeClass", extra), map[string]string{LabelManagedBy: "someone"}),
		InstanceTypes: []string{"c7a.large", "c7a.xlarge", "c7a.2xlarge", "m7a.large"},
		Architectures: []v1alpha1.Architecture{amd64},
		Rules:         aws,
	})
	if !slices.Equal(out.InstanceTypes, []string{"c7a.large", "c7a.xlarge"}) {
		t.Errorf("types %v", out.InstanceTypes)
	}
	reqs := RequirementsOf(out.NodePool)
	keys := make([]string, 0, len(reqs))
	for _, r := range reqs {
		keys = append(keys, r.Key+" "+r.Operator)
	}
	want := []string{
		"kubernetes.io/arch NotIn", "karpenter.sh/capacity-type In", "node.kubernetes.io/instance-type NotIn",
		"sparecores.com/tier In", "node.kubernetes.io/instance-type In",
	}
	if !slices.Equal(keys, want) {
		t.Errorf("requirements %v, want %v", keys, want)
	}
	if last := reqs[len(reqs)-1]; last.MinValues == nil || *last.MinValues != 2 {
		t.Errorf("minValues not carried over: %+v", last)
	}
	if out.NodePool.GetLabels()[LabelManagedBy] != ManagedByValue {
		t.Error("the template overrode the managed-by label")
	}
}

// renderForcedZones: forced zones unless the template has a zone requirement.
func renderForcedZones(t *testing.T) {
	ovh := rulesOf(t, providers.OVH)
	in := Input{
		SCNodePool:    scNodePool("p", templateSpec("karpenter.ovhcloud.sh", "OVHNodeClass", ""), nil),
		InstanceTypes: []string{"b3-8"}, Architectures: []v1alpha1.Architecture{amd64},
		Rules: ovh, ForcedZones: []string{"gra11-b", "gra11-a"},
	}
	if r, n := requirement(t, mustRender(t, in).NodePool, providers.LabelZone); n != 1 || !slices.Equal(r.Values, []string{"gra11-a", "gra11-b"}) {
		t.Errorf("zone %+v (%d)", r, n)
	}
	in.SCNodePool = scNodePool("p", templateSpec("karpenter.ovhcloud.sh", "OVHNodeClass",
		`"requirements":[{"key":"topology.kubernetes.io/zone","operator":"In","values":["gra11-a"]}]`), nil)
	if _, n := requirement(t, mustRender(t, in).NodePool, providers.LabelZone); n != 1 {
		t.Errorf("%d zone requirements, want the template's only", n)
	}
}

// renderInvalidValues: invalid label values are dropped.
func renderInvalidValues(t *testing.T) {
	aws := rulesOf(t, providers.AWS)
	out := mustRender(t, Input{
		SCNodePool:    scNodePool("p", templateSpec("karpenter.k8s.aws", "EC2NodeClass", ""), nil),
		InstanceTypes: []string{"ok.large", "bad name", strings.Repeat("x", 64)},
		Rules:         aws,
	})
	if !slices.Equal(out.InstanceTypes, []string{"ok.large"}) || len(out.Invalid) != 2 {
		t.Errorf("types %v invalid %v", out.InstanceTypes, out.Invalid)
	}
	if _, n := requirement(t, out.NodePool, providers.LabelArch); n != 0 {
		t.Error("arch emitted without architectures")
	}
}

// renderTemplateFields: orphan, override and template fields.
func renderTemplateFields(t *testing.T) {
	aws := rulesOf(t, providers.AWS)
	scnp := scNodePool("p", `{"weight":10,"disruption":{"consolidateAfter":"5m"},`+
		`"template":{"metadata":{"labels":{"a":"b"}},"spec":{"nodeClassRef":{"group":"karpenter.k8s.aws","kind":"EC2NodeClass","name":"default"}}}}`, nil)
	scnp.Spec.DeletionPolicy = v1alpha1.DeletionPolicyOrphan
	scnp.Spec.NodePoolTemplate.Metadata.Annotations = map[string]string{"note": "x"}
	override := NodeClassRef{Group: "karpenter.kwok.sh", Kind: "KWOKNodeClass", Name: "default"}
	out := mustRender(t, Input{
		SCNodePool: scnp, InstanceTypes: []string{"c7a.large"}, Rules: aws,
		NodeClassRefOverride: &override, DataTimestamp: &dataTime,
	})
	np := out.NodePool
	if len(np.GetOwnerReferences()) != 0 {
		t.Error("ownerReference with deletionPolicy Orphan")
	}
	if ref, _ := NodeClassRefOf(np); ref != override {
		t.Errorf("nodeClassRef %+v", ref)
	}
	if got, _, _ := unstructured.NestedString(np.Object, "spec", "disruption", "consolidateAfter"); got != "5m" {
		t.Errorf("consolidateAfter %q", got)
	}
	if got, _, _ := unstructured.NestedInt64(np.Object, "spec", "weight"); got != 10 {
		t.Errorf("weight %d", got)
	}
	if got, _, _ := unstructured.NestedString(np.Object, "spec", "template", "metadata", "labels", "a"); got != "b" {
		t.Error("template labels not copied")
	}
	if a := np.GetAnnotations(); a["note"] != "x" || a[AnnotationDataTimestamp] != "2026-10-05T10:02:57Z" {
		t.Errorf("annotations %v", a)
	}
	// Volatile data never goes into spec.template (static drift).
	if _, found, _ := unstructured.NestedMap(np.Object, "spec", "template", "metadata", "annotations"); found {
		t.Error("annotations in spec.template")
	}
}

func TestRenderErrors(t *testing.T) {
	cases := []struct {
		name  string
		id    providers.ID
		group string
		kind  string
		extra string
		types []string
		archs []v1alpha1.Architecture
		want  error
	}{
		{"never-emit key", providers.AWSAutoMode, "eks.amazonaws.com", "NodeClass",
			`"requirements":[{"key":"kubernetes.io/os","operator":"In","values":["linux"]}]`,
			[]string{"c7a.large"}, nil, ErrForbiddenKey},
		{"spot on an on-demand-only provider", providers.Hcloud, "karpenter.hetzner.cloud", "HCloudNodeClass",
			`"requirements":[{"key":"karpenter.sh/capacity-type","operator":"In","values":["spot"]}]`,
			[]string{"cx22"}, nil, providers.ErrCapacityTypeUnsupported},
		{"both capacity types on official Alibaba", providers.AlicloudACK, "karpenter.alibabacloud.com", "ECSNodeClass",
			`"requirements":[{"key":"karpenter.sh/capacity-type","operator":"NotIn","values":["reserved"]}]`,
			[]string{"ecs.g7.large"}, nil, providers.ErrCapacityTypeUnsupported},
		{"two architectures on cloudpilot Alibaba", providers.AlicloudCloudpilot, "karpenter.k8s.alibabacloud", "ECSNodeClass", "",
			[]string{"ecs.g7.large", "ecs.g8y.large"}, []v1alpha1.Architecture{amd64, arm64}, providers.ErrArchitectureUnsupported},
		{"template arm64 on an amd64-only provider", providers.Vultr, "karpenter.vultr.com", "VultrNodeClass",
			`"requirements":[{"key":"kubernetes.io/arch","operator":"In","values":["arm64"]}]`,
			[]string{"vc2-2c-4gb"}, nil, providers.ErrArchitectureUnsupported},
		{"no intersection with the template list", providers.AWS, "karpenter.k8s.aws", "EC2NodeClass",
			`"requirements":[{"key":"node.kubernetes.io/instance-type","operator":"In","values":["m5.large"]}]`,
			[]string{"c7a.large"}, nil, ErrEmptySelection},
		{"empty selection", providers.AWS, "karpenter.k8s.aws", "EC2NodeClass", "", nil, nil, ErrEmptySelection},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			scnp := scNodePool("p", templateSpec(c.group, c.kind, c.extra), nil)
			tmpl, err := ParseTemplate(scnp.Spec.NodePoolTemplate)
			if err != nil {
				t.Fatal(err)
			}
			_, err = Render(Input{SCNodePool: scnp, Template: tmpl, InstanceTypes: c.types, Architectures: c.archs, Rules: rulesOf(t, c.id)})
			if !errors.Is(err, c.want) {
				t.Errorf("got %v, want %v", err, c.want)
			}
		})
	}
}

// nodePoolWith returns a valid generated NodePool with extra requirements
// (JSON objects) appended.
func nodePoolWith(t *testing.T, extra ...string) *unstructured.Unstructured {
	t.Helper()
	spec := templateSpec("karpenter.k8s.aws", "EC2NodeClass", `"requirements":[`+strings.Join(extra, ",")+`]`)
	return mustRender(t, Input{
		SCNodePool:    scNodePool("p", spec, nil),
		InstanceTypes: []string{"c7a.large"},
		Architectures: []v1alpha1.Architecture{amd64},
		Rules:         rulesOf(t, providers.AWS),
	}).NodePool
}

func TestValidate(t *testing.T) {
	modern := rulesOf(t, providers.AWS)
	legacy := rulesOf(t, providers.OVH)
	many := make([]string, MaxRequirements)
	for i := range many {
		many[i] = `{"key":"example.com/k` + strings.Repeat("x", i%5) + `","operator":"Exists"}`
	}
	cases := []struct {
		name    string
		reqs    []string
		rules   providers.RequirementRules
		wantErr string // "" = valid
	}{
		{"custom key", []string{`{"key":"sparecores.com/tier","operator":"In","values":["fast"]}`}, modern, ""},
		{"too many requirements", many, modern, "at most 100"},
		{"karpenter.sh key", []string{`{"key":"karpenter.sh/nodepool","operator":"In","values":["x"]}`}, modern, "reserved"},
		{"karpenter.sh subdomain", []string{`{"key":"x.karpenter.sh/foo","operator":"Exists"}`}, modern, "not allowed"},
		{"hostname", []string{`{"key":"kubernetes.io/hostname","operator":"In","values":["n1"]}`}, modern, "kubernetes.io/hostname"},
		{"bad value", []string{`{"key":"example.com/a","operator":"In","values":["a b"]}`}, modern, "invalid value"},
		{"long value", []string{`{"key":"example.com/a","operator":"In","values":["` + strings.Repeat("y", 64) + `"]}`}, modern, "63"},
		{"empty In", []string{`{"key":"example.com/a","operator":"In","values":[]}`}, modern, "must have a value"},
		{"minValues above values", []string{`{"key":"example.com/a","operator":"In","values":["a"],"minValues":2}`}, modern, "minimum number"},
		{"minValues above 50", []string{`{"key":"example.com/a","operator":"Exists","minValues":51}`}, modern, "outside 1..50"},
		{"unknown operator", []string{`{"key":"example.com/a","operator":"Foo","values":["a"]}`}, modern, "unsupported operator"},
		{"Gte on core >= v1.9", []string{`{"key":"example.com/gen","operator":"Gte","values":["7"]}`}, modern, ""},
		{"Gte on core v1.8", []string{`{"key":"example.com/gen","operator":"Gte","values":["7"]}`}, legacy, "Gte needs"},
		{"kubernetes.io key on core >= v1.9", []string{`{"key":"example.kubernetes.io/a","operator":"Exists"}`}, modern, ""},
		{"kubernetes.io key on core v1.8", []string{`{"key":"example.kubernetes.io/a","operator":"Exists"}`}, legacy, "reserve the example.kubernetes.io domain"},
		{"node.kubernetes.io key on core v1.8", []string{`{"key":"node.kubernetes.io/foo","operator":"Exists"}`}, legacy, ""},
		{"well-known key on core v1.8", []string{`{"key":"topology.kubernetes.io/zone","operator":"In","values":["a"]}`}, legacy, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Validate(context.Background(), nodePoolWith(t, c.reqs...), c.rules)
			switch {
			case c.wantErr == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case c.wantErr != "" && (!errors.Is(err, ErrInvalidNodePool) || !strings.Contains(err.Error(), c.wantErr)):
				t.Errorf("got %v, want an error containing %q", err, c.wantErr)
			}
		})
	}

	np := nodePoolWith(t)
	unstructured.RemoveNestedField(np.Object, "spec", "disruption", "consolidateAfter")
	unstructured.RemoveNestedField(np.Object, "spec", "template", "spec", "nodeClassRef")
	err := Validate(context.Background(), np, modern)
	if err == nil || !strings.Contains(err.Error(), "consolidateAfter") || !strings.Contains(err.Error(), "nodeClassRef") {
		t.Errorf("got %v", err)
	}
}
