//go:build crd

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package crd is the CRD admission matrix (L2, docs/plans/v1/wip/00-overview.md
// §8.3): the NodePool CRD of every supported Karpenter provider is installed
// into a kind cluster, one after the other, and server-side dry-runs show
// which NodePools it admits. test/crd/run.sh creates the cluster.
package crd

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

const crdName = "nodepools.karpenter.sh"

// entry is one CRD of providers.yaml (a provider's release or a fixture).
type entry struct {
	Name      string            `json:"name"`
	Tag       string            `json:"tag"`
	URL       string            `json:"url"`
	Goldens   []string          `json:"goldens"`
	Overrides map[string]string `json:"overrides"`
}

type matrix struct {
	Providers []entry `json:"providers"`
	Fixtures  []entry `json:"fixtures"`
}

const (
	accept = "accept"
	reject = "reject"
)

// negativeCase mutates a valid NodePool; want is the outcome on a current
// core CRD (providers.yaml overrides it per CRD).
type negativeCase struct {
	name   string
	want   string
	mutate func(np map[string]any)
}

func spec(np map[string]any) map[string]any {
	return np["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
}

func requirements(np map[string]any) []any { return spec(np)["requirements"].([]any) }

func setRequirements(np map[string]any, rs []any) { spec(np)["requirements"] = rs }

func req(key string, values ...string) map[string]any {
	vs := make([]any, len(values))
	for i, v := range values {
		vs[i] = v
	}
	return map[string]any{"key": key, "operator": "In", "values": vs}
}

// withRequirement appends a requirement to the NodePool.
func withRequirement(r map[string]any) func(map[string]any) {
	return func(np map[string]any) { setRequirements(np, append(requirements(np), r)) }
}

// instanceTypeRequirement returns the generated instance-type requirement.
func instanceTypeRequirement(np map[string]any) map[string]any {
	for _, r := range requirements(np) {
		if m := r.(map[string]any); m["key"] == "node.kubernetes.io/instance-type" {
			return m
		}
	}
	panic("golden NodePool without an instance-type requirement")
}

// padTo adds custom-key requirements until the NodePool has n requirements.
func padTo(n int) func(map[string]any) {
	return func(np map[string]any) {
		rs := requirements(np)
		for i := len(rs); i < n; i++ {
			rs = append(rs, req(fmt.Sprintf("example.com/key-%d", i), "v"))
		}
		setRequirements(np, rs)
	}
}

// The cases of overview §8.3. The default outcome is that of core's own CRD;
// providers.yaml lists where a provider's CRD differs.
var cases = []negativeCase{
	{"restricted-hostname", reject, withRequirement(req("kubernetes.io/hostname", "node-1"))},
	{"restricted-karpenter-domain", reject, withRequirement(req("karpenter.sh/bogus", "x"))},
	{"capacity-type-allowed", accept, withRequirement(req("karpenter.sh/capacity-type", "spot"))},
	{"empty-in", reject, func(np map[string]any) { instanceTypeRequirement(np)["values"] = []any{} }},
	{"requirements-at-limit", accept, padTo(100)},
	{"requirements-over-limit", reject, padTo(101)},
	{"minvalues-valid", accept, func(np map[string]any) { instanceTypeRequirement(np)["minValues"] = int64(1) }},
	{"minvalues-zero", reject, func(np map[string]any) { instanceTypeRequirement(np)["minValues"] = int64(0) }},
	{"minvalues-over-50", reject, func(np map[string]any) { instanceTypeRequirement(np)["minValues"] = int64(51) }},
	{"minvalues-over-values", reject, func(np map[string]any) {
		r := instanceTypeRequirement(np)
		r["minValues"] = int64(len(r["values"].([]any)) + 1)
	}},
	// No CRD validates label values: sanitising them is the controller's job.
	{"invalid-label-value", accept, func(np map[string]any) {
		instanceTypeRequirement(np)["values"] = []any{"not a valid label value!"}
	}},
	{"weight-over-100", reject, func(np map[string]any) { np["spec"].(map[string]any)["weight"] = int64(101) }},
	{"consolidate-after-missing", reject, func(np map[string]any) {
		delete(np["spec"].(map[string]any)["disruption"].(map[string]any), "consolidateAfter")
	}},
	// Provider-specific key domains (overview §5.4).
	{"aws-domain", accept, withRequirement(req("karpenter.k8s.aws/bogus", "x"))},
	{"azure-domain", accept, withRequirement(req("karpenter.azure.com/bogus", "x"))},
	{"eks-domain", accept, withRequirement(req("eks.amazonaws.com/bogus", "x"))},
	{"eks-allowed-key", accept, withRequirement(req("eks.amazonaws.com/instance-family", "m7i"))},
	{"zone-id", accept, withRequirement(req("topology.k8s.aws/zone-id", "use1-az1"))},
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(wd, "..", "..")
}

// kubectl runs kubectl with stdin; it returns stdout+stderr and the error.
func kubectl(stdin []byte, args ...string) (string, error) {
	cmd := exec.Command("kubectl", args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func download(url, path string) error {
	var err error
	for attempt := range 3 {
		if err = tryDownload(url, path); err == nil {
			return nil
		}
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}
	return err
}

func tryDownload(url, path string) error {
	resp, err := http.Get(url) //nolint:gosec,noctx // fixed URLs of providers.yaml
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var crd struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	err = yaml.Unmarshal(b, &crd)
	if err != nil || crd.Kind != "CustomResourceDefinition" || crd.Metadata.Name != crdName {
		return fmt.Errorf("%s is not the %s CRD (%v)", url, crdName, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// crdFile returns the path of the entry's CRD, downloading it into the cache.
func crdFile(t *testing.T, root string, e entry, fixture bool) string {
	t.Helper()
	if fixture {
		return filepath.Join(root, "test", "crd", "fixtures", e.Name, "karpenter.sh_nodepools.yaml")
	}
	path := filepath.Join(root, "test", "crd", ".cache", e.Name+".yaml")
	if _, err := os.Stat(path); err != nil {
		if err := download(e.URL, path); err != nil {
			t.Fatalf("downloading the CRD of %s: %v", e.Name, err)
		}
	}
	return path
}

// install replaces the NodePool CRD of the cluster.
func install(t *testing.T, path string) {
	t.Helper()
	if out, err := kubectl(nil, "delete", "crd", crdName, "--ignore-not-found", "--wait=true"); err != nil {
		t.Fatalf("deleting the previous CRD: %v\n%s", err, out)
	}
	if out, err := kubectl(nil, "apply", "--server-side", "-f", path); err != nil {
		t.Fatalf("installing %s: %v\n%s", path, err, out)
	}
	if out, err := kubectl(nil, "wait", "--for=condition=Established", "crd/"+crdName, "--timeout=60s"); err != nil {
		t.Fatalf("waiting for the CRD: %v\n%s", err, out)
	}
}

func loadGolden(t *testing.T, root, name string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "internal", "nodepool", "testdata", "golden", name+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var np map[string]any
	if err := yaml.Unmarshal(b, &np); err != nil {
		t.Fatal(err)
	}
	// The golden files carry an ownerReference with a synthetic UID, which the
	// garbage collector would not like and a dry-run does not need.
	delete(np["metadata"].(map[string]any), "ownerReferences")
	return np
}

// dryRun applies the NodePool with a server-side dry-run. rejected is true
// when the API server refused it as invalid; any other failure fails the test.
func dryRun(t *testing.T, np map[string]any) (rejected bool, message string) {
	t.Helper()
	b, err := yaml.Marshal(np)
	if err != nil {
		t.Fatal(err)
	}
	out, err := kubectl(b, "apply", "--dry-run=server", "-f", "-")
	if err == nil {
		return false, out
	}
	if !strings.Contains(out, "is invalid") && !strings.Contains(out, "denied the request") {
		t.Fatalf("dry-run failed for a reason other than validation: %v\n%s", err, out)
	}
	return true, out
}

func deepCopy(t *testing.T, np map[string]any) map[string]any {
	t.Helper()
	b, err := yaml.Marshal(np)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := yaml.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAdmission(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "test", "crd", "providers.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var m matrix
	if err := yaml.UnmarshalStrict(raw, &m); err != nil {
		t.Fatalf("providers.yaml: %v", err)
	}
	type target struct {
		entry   entry
		fixture bool
	}
	var targets []target
	for _, p := range m.Providers {
		targets = append(targets, target{p, false})
	}
	listed := map[string]bool{}
	for _, f := range m.Fixtures {
		listed[f.Name] = true
		targets = append(targets, target{f, true})
	}
	// Every fixture present is tested, listed or not.
	found, err := filepath.Glob(filepath.Join(root, "test", "crd", "fixtures", "*", "karpenter.sh_nodepools.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range found {
		name := filepath.Base(filepath.Dir(f))
		if !listed[name] {
			t.Logf("fixture %s is not listed in providers.yaml: tested with the aws golden and the default outcomes", name)
			targets = append(targets, target{entry{Name: name, Goldens: []string{"aws"}}, true})
		}
	}
	for _, f := range m.Fixtures {
		if !slices.ContainsFunc(found, func(p string) bool { return filepath.Base(filepath.Dir(p)) == f.Name }) {
			t.Errorf("fixture %s of providers.yaml has no karpenter.sh_nodepools.yaml", f.Name)
		}
	}

	names := map[string]bool{}
	for _, tg := range targets {
		for c := range tg.entry.Overrides {
			if !slices.ContainsFunc(cases, func(n negativeCase) bool { return n.name == c }) {
				t.Errorf("%s: override for unknown case %s", tg.entry.Name, c)
			}
		}
		if names[tg.entry.Name] {
			t.Errorf("duplicate entry %s", tg.entry.Name)
		}
		names[tg.entry.Name] = true
	}

	var summary []string
	for _, tg := range targets {
		e := tg.entry
		t.Run(e.Name, func(t *testing.T) {
			if len(e.Goldens) == 0 {
				t.Fatal("no goldens")
			}
			install(t, crdFile(t, root, e, tg.fixture))
			for _, g := range e.Goldens {
				np := loadGolden(t, root, g)
				if rejected, msg := dryRun(t, np); rejected {
					t.Errorf("golden %s rejected: %s", g, msg)
				}
			}
			base := loadGolden(t, root, e.Goldens[0])
			row := e.Name + ":"
			for _, c := range cases {
				want := c.want
				if o, ok := e.Overrides[c.name]; ok {
					want = o
				}
				np := deepCopy(t, base)
				c.mutate(np)
				rejected, msg := dryRun(t, np)
				got := accept
				if rejected {
					got = reject
				}
				row += " " + c.name + "=" + got
				if got != want {
					t.Errorf("%s: %s: the CRD outcome is %s, expected %s "+
						"(if the new outcome is right, set `%s: %s` under %s in providers.yaml)\n%s",
						e.Name, c.name, got, want, c.name, got, e.Name, msg)
				}
			}
			summary = append(summary, row)
		})
	}
	t.Logf("%d CRDs x %d cases:\n%s", len(targets), len(cases), strings.Join(summary, "\n"))
}
