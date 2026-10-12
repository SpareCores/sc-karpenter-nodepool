/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package v1alpha1_test

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	karpenterv1alpha1 "github.com/SpareCores/sc-karpenter-nodepool/api/v1alpha1"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/testutil"
)

// These tests install the generated CRD into an envtest API server and send
// objects exactly as users write them (unstructured YAML), so they exercise
// the schema, defaulting and CEL rules the way kubectl apply does.

var k8sClient client.Client

func TestMain(m *testing.M) {
	cfg, stop, err := testutil.StartEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	scheme := runtime.NewScheme()
	if err := karpenterv1alpha1.AddToScheme(scheme); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	if err := stop(); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(code)
}

// baseSpec is a minimal valid spec; tests overlay fields on it.
const baseSpec = `
nodePoolTemplate:
  spec:
    template:
      spec:
        nodeClassRef: {group: karpenter.k8s.aws, kind: EC2NodeClass, name: default}
    disruption:
      consolidateAfter: 1m
`

func parseObject(t *testing.T, name, specYAML string) *unstructured.Unstructured {
	t.Helper()
	var spec map[string]any
	if err := yaml.Unmarshal([]byte(specYAML), &spec); err != nil {
		t.Fatalf("parsing spec: %v", err)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "karpenter.sparecores.com/v1alpha1",
		"kind":       "SCNodePool",
		"metadata":   map[string]any{"name": name},
		"spec":       spec,
	}}
}

// withSpec returns baseSpec with the top-level keys of extra YAML added.
func withSpec(t *testing.T, extra string) string {
	t.Helper()
	var base, over map[string]any
	if err := yaml.Unmarshal([]byte(baseSpec), &base); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal([]byte(extra), &over); err != nil {
		t.Fatalf("parsing overlay: %v", err)
	}
	maps.Copy(base, over)
	out, err := yaml.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func create(t *testing.T, obj *unstructured.Unstructured) error {
	t.Helper()
	err := k8sClient.Create(context.Background(), obj)
	if err == nil {
		t.Cleanup(func() {
			if err := k8sClient.Delete(context.Background(), obj); err != nil && !apierrors.IsNotFound(err) {
				t.Errorf("cleanup: %v", err)
			}
		})
	}
	return err
}

func loadObject(t *testing.T, path string) *unstructured.Unstructured {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	obj := &unstructured.Unstructured{}
	if err := yaml.Unmarshal(data, &obj.Object); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return obj
}

func TestExamplesAreAccepted(t *testing.T) {
	samples, err := filepath.Glob(filepath.Join(testutil.RepoRoot(), "config", "samples", "karpenter_*.yaml"))
	if err != nil || len(samples) < 2 {
		t.Fatalf("expected the samples under config/samples, got %v (err %v)", samples, err)
	}
	files := append(samples, "testdata/selectors-example.yaml")
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			if err := create(t, loadObject(t, f)); err != nil {
				t.Fatalf("create: %v", err)
			}
		})
	}
}

func TestDefaultsAreApplied(t *testing.T) {
	obj := parseObject(t, "defaults", withSpec(t, `
selector:
  benchmarks: [{id: geekbench:score, config: {cores: single}, min: 1000}]
  workloadProfiles: [{name: web, min: 1}]
ranking:
  score: {workloadProfile: web}
  families: {max: 4}
`))
	if err := create(t, obj); err != nil {
		t.Fatalf("create: %v", err)
	}
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(obj.GroupVersionKind())
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(obj), got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"spec.refresh.interval":                       "24h",
		"spec.churn.removeMarginPercent":              int64(5),
		"spec.churn.removeAfterRefreshes":             int64(2),
		"spec.churn.maxRemovalsPerRefresh":            int64(3),
		"spec.dryRun":                                 false,
		"spec.deletionPolicy":                         "Delete",
		"spec.ranking.objective":                      "Score",
		"spec.ranking.normalize":                      "None",
		"spec.ranking.families.min":                   int64(1),
		"spec.selector.unbenchmarked":                 "Exclude",
		"spec.selector.benchmarks[0].normalize":       "None",
		"spec.selector.workloadProfiles[0].normalize": "None",
	}
	for path, w := range want {
		g, found, err := nestedField(got.Object, path)
		if err != nil || !found {
			t.Errorf("%s: not found (err %v)", path, err)
			continue
		}
		// JSON numbers may come back as int64 or float64.
		if gf, ok := toFloat(g); ok {
			if wf, ok := toFloat(w); ok && gf == wf {
				continue
			}
		}
		if !reflect.DeepEqual(g, w) {
			t.Errorf("%s = %v (%T), want %v", path, g, g, w)
		}
	}
}

func TestExplicitValuesOverrideDefaults(t *testing.T) {
	obj := parseObject(t, "explicit", withSpec(t, `
refresh: {interval: 1h}
churn: {removeMarginPercent: 0, removeAfterRefreshes: 0, maxRemovalsPerRefresh: 0}
dryRun: true
deletionPolicy: Orphan
`))
	if err := create(t, obj); err != nil {
		t.Fatalf("create: %v", err)
	}
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(obj.GroupVersionKind())
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(obj), got); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]any{
		"spec.refresh.interval":            "1h",
		"spec.churn.removeAfterRefreshes":  float64(0),
		"spec.churn.maxRemovalsPerRefresh": float64(0),
		"spec.dryRun":                      true,
		"spec.deletionPolicy":              "Orphan",
	} {
		g, found, err := nestedField(got.Object, path)
		if err != nil || !found {
			t.Errorf("%s: not found (err %v)", path, err)
			continue
		}
		if gf, ok := toFloat(g); ok {
			if wf, ok := toFloat(want); ok && gf == wf {
				continue
			}
		}
		if g != want {
			t.Errorf("%s = %v, want %v", path, g, want)
		}
	}
}

func TestInvalidObjectsAreRejected(t *testing.T) {
	tests := []struct {
		name string
		spec string
		// substring of the API error
		wantErr string
	}{
		// CEL rules
		{"ranking without band or families.max",
			`ranking: {score: {workloadProfile: web}}`, "ranking needs band or families.max"},
		{"ranking with only families.min",
			`ranking: {score: {workloadProfile: web}, families: {min: 2}}`, "ranking needs band or families.max"},
		{"families.min above families.max",
			`ranking: {score: {workloadProfile: web}, families: {min: 5, max: 3}}`, "min must not exceed max"},
		{"ranking score with neither benchmark nor profile",
			`ranking: {score: {}, families: {max: 3}}`, "set exactly one of benchmark and workloadProfile"},
		{"ranking score with both benchmark and profile",
			`ranking: {score: {workloadProfile: web, benchmark: {id: geekbench:score}}, families: {max: 3}}`,
			"set exactly one of benchmark and workloadProfile"},
		{"benchmark without min or max",
			`selector: {benchmarks: [{id: geekbench:score}]}`, "a benchmark entry needs min or max"},
		{"benchmark min above max",
			`selector: {benchmarks: [{id: geekbench:score, min: 10, max: 5}]}`, "min must not exceed max"},
		{"vcpus min above max",
			`selector: {vcpus: {min: 8, max: 4}}`, "min must not exceed max"},
		{"memory minGiB above maxGiB",
			`selector: {memory: {minGiB: 16, maxGiB: 8}}`, "minGiB must not exceed maxGiB"},
		{"memory perVcpu minGiB above maxGiB",
			`selector: {memory: {perVcpu: {minGiB: 8, maxGiB: 2}}}`, "minGiB must not exceed maxGiB"},
		{"accelerator count min above max",
			`selector: {accelerator: {count: {min: 4, max: 1}}}`, "min must not exceed max"},
		{"refresh interval below one hour",
			`refresh: {interval: 30m}`, "refresh.interval must be at least 1h"},
		{"refresh interval 59m59s",
			`refresh: {interval: 59m59s}`, "refresh.interval must be at least 1h"},
		{"refresh interval not a duration",
			`refresh: {interval: soon}`, "should match"},
		{"negative removeMarginPercent",
			`churn: {removeMarginPercent: -1}`, "should be greater than or equal to 0"},
		{"negative removeAfterRefreshes",
			`churn: {removeAfterRefreshes: -1}`, "should be greater than or equal to 0"},
		{"negative maxRemovalsPerRefresh",
			`churn: {maxRemovalsPerRefresh: -1}`, "should be greater than or equal to 0"},
		// enums
		{"unknown deletionPolicy", `deletionPolicy: Retain`, "Unsupported value"},
		{"unknown architecture", `selector: {architectures: [riscv64]}`, "Unsupported value"},
		{"unknown unbenchmarked policy", `selector: {unbenchmarked: Maybe}`, "Unsupported value"},
		{"unknown cpu allocation", `selector: {cpu: {allocations: [Exclusive]}}`, "Unsupported value"},
		{"unknown accelerator type", `selector: {accelerator: {types: [FPGA]}}`, "Unsupported value"},
		{"unknown price basis", `selector: {price: {basis: Reserved}}`, "Unsupported value"},
		{"unknown benchmark normalize",
			`selector: {benchmarks: [{id: x, min: 1, normalize: PerGpu}]}`, "Unsupported value"},
		{"PerPrice as ranking normalize",
			`ranking: {score: {workloadProfile: web}, normalize: PerPrice, families: {max: 2}}`, "Unsupported value"},
		{"unknown objective",
			`ranking: {score: {workloadProfile: web}, objective: Fastest, families: {max: 2}}`, "Unsupported value"},
		{"unknown workload profile",
			`selector: {workloadProfiles: [{name: gaming, min: 1}]}`, "Unsupported value"},
		// types and required fields
		{"price maxHourly not a decimal string",
			`selector: {price: {maxHourly: cheap}}`, "should match"},
		{"price maxHourly as bare number",
			`selector: {price: {maxHourly: 0.4}}`, "Invalid value"},
		{"workload profile without min",
			`selector: {workloadProfiles: [{name: web}]}`, "Required value"},
		{"benchmark without id",
			`selector: {benchmarks: [{min: 1}]}`, "Required value"},
		{"band above 100 percent",
			`ranking: {score: {workloadProfile: web}, band: {withinPercent: 150}}`, "should be less than or equal to 100"},
		{"negative memory bound",
			`selector: {memory: {minGiB: -1}}`, "should be greater than or equal to 0"},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := create(t, parseObject(t, fmt.Sprintf("invalid-%d", i), withSpec(t, tc.spec)))
			if err == nil {
				t.Fatalf("object was accepted, want error containing %q", tc.wantErr)
			}
			if !apierrors.IsInvalid(err) {
				t.Fatalf("want an Invalid error, got %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestMissingTemplateIsRejected(t *testing.T) {
	for name, spec := range map[string]string{
		"no nodePoolTemplate":      `dryRun: true`,
		"no nodePoolTemplate.spec": `nodePoolTemplate: {metadata: {labels: {team: web}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			err := create(t, parseObject(t, "missing-template", spec))
			if err == nil || !strings.Contains(err.Error(), "Required value") {
				t.Fatalf("want a Required value error, got %v", err)
			}
		})
	}
}

func TestValidBoundariesAreAccepted(t *testing.T) {
	tests := map[string]string{
		"refresh exactly 1h":            `refresh: {interval: 60m}`,
		"refresh compound duration":     `refresh: {interval: 1h30m}`,
		"min equals max":                `selector: {vcpus: {min: 4, max: 4}, memory: {minGiB: 8, maxGiB: 8}}`,
		"only a lower bound":            `selector: {vcpus: {min: 4}, accelerator: {count: {min: 0.25}}}`,
		"benchmark with only max":       `selector: {benchmarks: [{id: membench:latency, config: {size_kb: 1024}, max: 80}]}`,
		"ranking by benchmark":          `ranking: {score: {benchmark: {id: geekbench:score, config: {cores: single}}}, band: {withinPercent: 5}}`,
		"ranking with families.max":     `ranking: {score: {workloadProfile: cicd}, families: {max: 1}}`,
		"zero churn":                    `churn: {removeAfterRefreshes: 0}`,
		"fractional bounds":             `selector: {memory: {minGiB: 3.8}, cpu: {clockSpeedMinGHz: 2.5}, workloadProfiles: [{name: llm, min: 0.75}]}`,
		"hyperthreading false is a set": `selector: {cpu: {hyperthreading: false}}`,
	}
	i := 0
	for name, spec := range tests {
		i++
		t.Run(name, func(t *testing.T) {
			if err := create(t, parseObject(t, fmt.Sprintf("valid-%d", i), withSpec(t, spec))); err != nil {
				t.Fatalf("create: %v", err)
			}
		})
	}
}

func TestHyperthreadingFalseIsPreserved(t *testing.T) {
	obj := parseObject(t, "ht-false", withSpec(t, `selector: {cpu: {hyperthreading: false}}`))
	if err := create(t, obj); err != nil {
		t.Fatalf("create: %v", err)
	}
	var got karpenterv1alpha1.SCNodePool
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "ht-false"}, &got); err != nil {
		t.Fatal(err)
	}
	ht := got.Spec.Selector.CPU.Hyperthreading
	if ht == nil || *ht {
		t.Fatalf("hyperthreading = %v, want a pointer to false", ht)
	}
}

// The NodePool spec is untyped: unknown fields and enum values of any
// provider's NodePool version must survive storage verbatim.
func TestNodePoolTemplateSpecIsPreservedVerbatim(t *testing.T) {
	const spec = `
nodePoolTemplate:
  metadata:
    labels: {team: web}
    annotations: {note: kept}
  spec:
    weight: 50
    template:
      metadata:
        labels: {sparecores.com/tier: fast}
      spec:
        nodeClassRef: {group: eks.amazonaws.com, kind: NodeClass, name: default}
        requirements:
          - {key: kubernetes.io/arch, operator: In, values: [amd64]}
          - {key: karpenter.sh/capacity-type, operator: In, values: [on-demand], minValues: 1}
        expireAfter: 720h
        someFutureField: {nested: [1, 2, 3]}
    disruption:
      consolidationPolicy: WhenEmptyOrUnderutilized
      consolidateAfter: 1m
      budgets:
        - nodes: 10%
          reasons: [NodeNotHealthy]
        - nodes: "0"
          schedule: "0 9 * * mon-fri"
          duration: 8h
    limits: {cpu: "256"}
    aProviderOnlyTopLevelField: true
`
	obj := parseObject(t, "verbatim", spec)
	if err := create(t, obj); err != nil {
		t.Fatalf("create: %v", err)
	}
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(obj.GroupVersionKind())
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "verbatim"}, got); err != nil {
		t.Fatal(err)
	}
	wantSpec, _, _ := unstructured.NestedMap(obj.Object, "spec", "nodePoolTemplate")
	gotSpec, _, _ := unstructured.NestedMap(got.Object, "spec", "nodePoolTemplate")
	if !reflect.DeepEqual(wantSpec, gotSpec) {
		wantY, _ := yaml.Marshal(wantSpec)
		gotY, _ := yaml.Marshal(gotSpec)
		t.Fatalf("nodePoolTemplate changed in storage\nsent:\n%s\nstored:\n%s", wantY, gotY)
	}
	budgets, ok, _ := unstructured.NestedSlice(got.Object, "spec", "nodePoolTemplate", "spec", "disruption", "budgets")
	if !ok || len(budgets) != 2 {
		t.Fatalf("budgets not preserved: %v", budgets)
	}
}

func TestSCNodePoolIsClusterScopedWithStatusSubresource(t *testing.T) {
	obj := parseObject(t, "scope", baseSpec)
	if err := create(t, obj); err != nil {
		t.Fatalf("create: %v", err)
	}
	var pool karpenterv1alpha1.SCNodePool
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "scope"}, &pool); err != nil {
		t.Fatalf("get without namespace: %v", err)
	}
	if pool.Namespace != "" {
		t.Fatalf("namespace = %q, want cluster-scoped", pool.Namespace)
	}

	// A status written through the main resource must be ignored; the status
	// subresource accepts it.
	pool.Status.Provider = "via-main-resource"
	if err := k8sClient.Update(context.Background(), &pool); err != nil {
		t.Fatal(err)
	}
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "scope"}, &pool); err != nil {
		t.Fatal(err)
	}
	if pool.Status.Provider != "" {
		t.Fatalf("status was written through the main resource: %q", pool.Status.Provider)
	}
	pool.Status.Provider = "aws"
	pool.Status.Selected = &karpenterv1alpha1.SelectedStatus{Count: 2}
	if err := k8sClient.Status().Update(context.Background(), &pool); err != nil {
		t.Fatalf("status update: %v", err)
	}
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "scope"}, &pool); err != nil {
		t.Fatal(err)
	}
	if pool.Status.Provider != "aws" || pool.Status.Selected == nil || pool.Status.Selected.Count != 2 {
		t.Fatalf("status not stored: %+v", pool.Status)
	}
}

func nestedField(obj map[string]any, path string) (any, bool, error) {
	var cur any = obj
	for part := range strings.SplitSeq(path, ".") {
		name, idx := part, -1
		if i := strings.Index(part, "["); i >= 0 {
			name = part[:i]
			if _, err := fmt.Sscanf(part[i:], "[%d]", &idx); err != nil {
				return nil, false, err
			}
		}
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false, nil
		}
		cur, ok = m[name]
		if !ok {
			return nil, false, nil
		}
		if idx >= 0 {
			list, ok := cur.([]any)
			if !ok || idx >= len(list) {
				return nil, false, nil
			}
			cur = list[idx]
		}
	}
	return cur, true, nil
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int64:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}
