//go:build kwok

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/SpareCores/sc-karpenter-nodepool/api/v1alpha1"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/keeper/keepertest"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/nodepool"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/providers"
	"github.com/SpareCores/sc-karpenter-nodepool/test/kwok/catalog"
)

// loadReplicas is the number of pods (and therefore NodeClaims: they repel
// each other) of the scenarios that provision nodes.
const loadReplicas = 4

// driftVendor runs the scenarios that need minutes (the drift loop and the
// custom-key stability window): once per core version, on a vendor whose
// catalog does not stamp the region label (core v1.8+ stamps every
// single-valued offering key, so on the others the region is no longer an
// unstamped well-known key).
const driftVendor = "hcloud"

// TestLane runs the scenarios of every vendor, one after the other: each one
// swaps the instance-type catalog of core Karpenter (a restart) and labels
// the real node with the region the controller detects.
func TestLane(t *testing.T) {
	only := map[string]bool{}
	for v := range strings.SplitSeq(os.Getenv("KWOK_VENDORS"), ",") {
		if v = strings.TrimSpace(v); v != "" {
			only[v] = true
		}
	}
	snapshot := keepertest.SnapshotDir()
	// The same catalogs run.sh baked into the Karpenter image.
	var opts []catalog.Option
	if os.Getenv("KWOK_PROVIDER_KEYS") == "false" {
		opts = append(opts, catalog.WithoutProviderKeys())
	}
	for _, v := range catalog.Vendors() {
		if len(only) > 0 && !only[v.Name] {
			continue
		}
		cat, err := catalog.Generate(snapshot, v, opts...)
		if err != nil {
			t.Fatal(err)
		}
		t.Run(v.Name, func(t *testing.T) {
			start := time.Now()
			defer func() { t.Logf("vendor %s took %s", v.Name, time.Since(start).Round(time.Second)) }()
			useCatalog(t, cat)
			v := vendorRun{cat: cat, name: v.Name}
			t.Run("subset and narrowing", v.subsetAndNarrowing)
			t.Run("unknown instance types", v.unknownInstanceTypes)
			if v.name == driftVendor {
				t.Run("drift loop", v.driftLoop)
				t.Run("custom key", v.customKey)
			}
		})
	}
}

// vendorRun is the state shared by the scenarios of one vendor.
type vendorRun struct {
	cat  *catalog.Catalog
	name string
}

func (v vendorRun) pool(suffix string) string {
	if suffix == "" {
		return "kw-" + v.name
	}
	return "kw-" + v.name + "-" + suffix
}

// useCatalog points core Karpenter at the vendor's catalog (restarting it)
// and labels the real node with the vendor's region: the template's NodeClass
// does not exist in the test cluster, so the controller falls back to node
// labels (overview §6.3).
func useCatalog(t *testing.T, cat *catalog.Catalog) {
	t.Helper()
	path := "/catalogs/" + cat.Vendor.Name + ".json"
	dep := &appsv1.Deployment{}
	key := client.ObjectKey{Namespace: karpenterNamespace, Name: karpenterDeployment}
	if err := k8s.Get(ctx(), key, dep); err != nil {
		t.Fatalf("reading Deployment %s: %v", karpenterDeployment, err)
	}
	cur := ""
	for _, c := range dep.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if e.Name == catalogEnv {
				cur = e.Value
			}
		}
	}
	if cur != path {
		patch := client.MergeFrom(dep.DeepCopy())
		c := &dep.Spec.Template.Spec.Containers[0]
		set := false
		for i := range c.Env {
			if c.Env[i].Name == catalogEnv {
				c.Env[i].Value, set = path, true
			}
		}
		if !set {
			c.Env = append(c.Env, corev1.EnvVar{Name: catalogEnv, Value: path})
		}
		if err := k8s.Patch(ctx(), dep, patch); err != nil {
			t.Fatalf("switching the catalog: %v", err)
		}
		eventually(t, 3*time.Minute, time.Second, "Karpenter restarted with "+path, func() (bool, string) {
			d := &appsv1.Deployment{}
			if err := k8s.Get(ctx(), key, d); err != nil {
				return false, err.Error()
			}
			n := ptr.Deref(d.Spec.Replicas, 1)
			ok := d.Status.ObservedGeneration >= d.Generation && d.Status.UpdatedReplicas == n &&
				d.Status.Replicas == n && d.Status.AvailableReplicas == n
			return ok, fmt.Sprintf("%+v", d.Status)
		})
	}

	var nodes corev1.NodeList
	if err := k8s.List(ctx(), &nodes); err != nil {
		t.Fatal(err)
	}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if _, fake := n.Annotations["kwok.x-k8s.io/node"]; fake {
			continue
		}
		patch := client.MergeFrom(n.DeepCopy())
		n.Labels[providers.LabelRegion] = cat.Region
		if err := k8s.Patch(ctx(), n, patch); err != nil {
			t.Fatalf("labelling node %s: %v", n.Name, err)
		}
	}
}

// poolOptions vary the SCNodePool a scenario creates.
type poolOptions struct {
	selector *v1alpha1.Selector
	// requirements and labels go into the template.
	requirements []map[string]any
}

// newSCNodePool builds an SCNodePool for the vendor: the vendor's NodeClass
// kind in the template (selecting the adapter; the controller's test-mode
// override replaces it by the KWOKNodeClass), no consolidation noise and no
// disruption budget limit, and unbounded removals so a narrowed selector
// takes effect in one refresh.
func (v vendorRun) newSCNodePool(t *testing.T, name string, o poolOptions) {
	t.Helper()
	a, ok := providers.NewRegistry().Get(v.cat.Vendor.Adapter)
	if !ok {
		t.Fatalf("no adapter %s", v.cat.Vendor.Adapter)
	}
	group, kind := a.NodeClass()
	tmpl := map[string]any{
		"template": map[string]any{"spec": map[string]any{
			"nodeClassRef": map[string]any{"group": group, "kind": kind, "name": "default"},
			"requirements": o.requirements,
		}},
		"disruption": map[string]any{
			"consolidationPolicy": "WhenEmpty",
			"consolidateAfter":    "24h",
			"budgets":             []any{map[string]any{"nodes": "100%"}},
		},
	}
	raw, err := json.Marshal(tmpl)
	if err != nil {
		t.Fatal(err)
	}
	p := &v1alpha1.SCNodePool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.SCNodePoolSpec{
			NodePoolTemplate: v1alpha1.NodePoolTemplate{Spec: runtime.RawExtension{Raw: raw}},
			Selector:         o.selector,
			Churn:            v1alpha1.Churn{MaxRemovalsPerRefresh: new(int32(100000))},
		},
	}
	if err := k8s.Create(ctx(), p); err != nil {
		t.Fatalf("creating SCNodePool %s: %v", name, err)
	}
	t.Cleanup(func() {
		// A failed scenario must not leave its pool behind for the next one.
		_ = k8s.Delete(ctx(), &v1alpha1.SCNodePool{ObjectMeta: metav1.ObjectMeta{Name: name}})
		_ = k8s.Delete(ctx(), &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}},
			client.PropagationPolicy(metav1.DeletePropagationForeground))
	})
}

// vcpuRange selects types by size; the catalogs hold types up to
// catalog.MaxVCPUs.
func vcpuRange(minV, maxV int32) *v1alpha1.Selector {
	r := &v1alpha1.IntRange{Min: new(minV)}
	if maxV > 0 {
		r.Max = new(maxV)
	}
	return &v1alpha1.Selector{VCPUs: r}
}

// waitSynced waits until the controller applied the NodePool of the current
// generation and reported the vendor's provider and region.
func (v vendorRun) waitSynced(t *testing.T, name string) *v1alpha1.SCNodePool {
	t.Helper()
	var got *v1alpha1.SCNodePool
	eventually(t, 2*time.Minute, time.Second, "SCNodePool "+name+" synced", func() (bool, string) {
		p, err := getPool(name)
		if err != nil {
			return false, err.Error()
		}
		synced := condition(p, v1alpha1.ConditionNodePoolSynced)
		ok := p.Status.ObservedGeneration == p.Generation && synced != nil && synced.Status == metav1.ConditionTrue &&
			p.Status.Selected != nil && p.Status.Selected.Count > 0
		if ok {
			got = p
		}
		return ok, fmt.Sprintf("%v", p.Status.Conditions)
	})
	if got.Status.Provider != string(v.cat.Vendor.Adapter) {
		t.Errorf("status.provider = %q, want %q", got.Status.Provider, v.cat.Vendor.Adapter)
	}
	if !slices.Contains(got.Status.Regions, v.cat.Region) {
		t.Errorf("status.regions = %v, want %q (from the node label)", got.Status.Regions, v.cat.Region)
	}
	return got
}

// loadDeployment returns pods that need nodes of the given pool: one pod per
// node, spread over zones where the vendor has several.
func (v vendorRun) loadDeployment(name, pool string, replicas int32) *appsv1.Deployment {
	labels := map[string]string{"app": name}
	spec := corev1.PodSpec{
		NodeSelector: map[string]string{labelNodePool: pool},
		Containers: []corev1.Container{{
			Name: "pause", Image: "registry.k8s.io/pause:3.10",
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1500m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			}},
		}},
		Affinity: &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				LabelSelector: &metav1.LabelSelector{MatchLabels: labels},
				TopologyKey:   "kubernetes.io/hostname",
			}},
		}},
	}
	if len(v.cat.Zones) > 1 {
		spec.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{{
			MaxSkew: 1, TopologyKey: providers.LabelZone, WhenUnsatisfiable: corev1.DoNotSchedule,
			LabelSelector: &metav1.LabelSelector{MatchLabels: labels},
		}}
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: spec},
		},
	}
}

func readyPods(name string) (ready, total int) {
	var pods corev1.PodList
	if err := k8s.List(ctx(), &pods, client.InNamespace(namespace), client.MatchingLabels{"app": name}); err != nil {
		return 0, 0
	}
	for i := range pods.Items {
		if pods.Items[i].DeletionTimestamp != nil {
			continue
		}
		total++
		for _, c := range pods.Items[i].Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue && pods.Items[i].Status.Phase == corev1.PodRunning {
				ready++
			}
		}
	}
	return ready, total
}

// allowed describes what a NodePool allows, read back from the NodePool.
type allowed struct {
	types, zones, capacity, archs []string
}

func (v vendorRun) allowedBy(t *testing.T, pool string) allowed {
	t.Helper()
	np, err := getNodePool(pool)
	if err != nil {
		t.Fatalf("reading NodePool %s: %v", pool, err)
	}
	a := allowed{types: nodepool.InstanceTypes(np), zones: v.cat.Zones}
	if vals, ok := requirementValues(np, providers.LabelZone); ok {
		a.zones = vals
	}
	a.capacity, _ = requirementValues(np, providers.LabelCapacityType)
	a.archs, _ = requirementValues(np, providers.LabelArch)
	return a
}

// check reports every NodeClaim whose type, zone, capacity type or
// architecture the NodePool does not allow.
func (a allowed) check(cs []claim) []string {
	var bad []string
	for _, c := range cs {
		switch {
		case !slices.Contains(a.types, c.InstanceType):
			bad = append(bad, fmt.Sprintf("%s: instance type not allowed", c))
		case !slices.Contains(a.zones, c.Zone):
			bad = append(bad, fmt.Sprintf("%s: zone not in %v", c, a.zones))
		case a.capacity != nil && !slices.Contains(a.capacity, c.CapacityType):
			bad = append(bad, fmt.Sprintf("%s: capacity type not in %v", c, a.capacity))
		case a.archs != nil && !slices.Contains(a.archs, c.Arch):
			bad = append(bad, fmt.Sprintf("%s: architecture not in %v", c, a.archs))
		}
	}
	return bad
}
