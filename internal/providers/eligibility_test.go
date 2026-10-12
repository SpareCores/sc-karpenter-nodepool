/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package providers

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/SpareCores/sc-karpenter-nodepool/api/v1alpha1"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/keeper"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/selection"
)

// split runs an adapter's filter over candidates.
func split(a Adapter, cands []*selection.Candidate, env Env) (kept []*selection.Candidate, dropped map[string]string) {
	dropped = map[string]string{}
	for _, c := range cands {
		if ok, reason := a.Eligible(c, env); ok {
			kept = append(kept, c)
		} else {
			if reason == "" {
				panic("dropped without a reason: " + c.Name())
			}
			dropped[c.Name()] = reason
		}
	}
	return kept, dropped
}

func names(cs []*selection.Candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Name()
	}
	return out
}

// TestEligibilitySnapshot runs every adapter's filter on the snapshot rows
// of its vendor and checks plausible counts plus rule-specific invariants.
func TestEligibilitySnapshot(t *testing.T) {
	autoFees, realFees := autoModeFees(t)
	cases := []eligibilityCase{
		{
			id: AWS, minKept: 1200, maxKept: 1500,
			check: func(t *testing.T, kept []*selection.Candidate, dropped map[string]string) {
				for name, reason := range dropped {
					if !strings.Contains(reason, "no Kubernetes equivalent") {
						t.Errorf("aws dropped %s: %s", name, reason)
					}
				}
			},
			keptMust: []string{"m5.large", "c7g.xlarge"},
		},
		{
			id: AWSAutoMode, env: Env{ManagedFees: autoFees}, minKept: 1, maxKept: 1500,
			check: func(t *testing.T, kept []*selection.Candidate, _ map[string]string) {
				for _, c := range kept {
					if _, ok := autoFees[c.Name()]; !ok {
						t.Errorf("auto mode kept %s without a fee", c.Name())
					}
					if c.VCPUs() < 2 {
						t.Errorf("auto mode kept %s with %d vCPUs", c.Name(), c.VCPUs())
					}
				}
				if realFees && len(kept) < 900 {
					t.Errorf("auto mode kept only %d types of the snapshot fee table", len(kept))
				}
			},
			droppedMust: []string{"t3.nano"},
		},
		{
			id: Azure, minKept: 850, maxKept: 1150,
			droppedMust: []string{"Standard_B1s", "Standard_E4-2s_v3", "Standard_DC2s_v3", "Standard_A1_v2", "Standard_NG8ads_V620_v1"},
			keptMust:    []string{"Standard_D2s_v5", "Standard_NC4as_T4_v3"},
			check: func(t *testing.T, kept []*selection.Candidate, _ map[string]string) {
				for _, c := range kept {
					n := c.Name()
					if c.VCPUs() < 2 || c.MemoryGiB() < 3.5 || strings.Contains(n, "-") ||
						strings.HasPrefix(n, "Standard_DC") || strings.HasPrefix(n, "Standard_EC") ||
						c.Server.Status != "active" {
						t.Errorf("azure kept %s", n)
					}
				}
			},
		},
		{
			id: GCP, minKept: 400, maxKept: 600,
			check: func(t *testing.T, kept []*selection.Candidate, _ map[string]string) {
				for _, c := range kept {
					if c.AcceleratorType == v1alpha1.AcceleratorTypeTPU {
						t.Errorf("gcp kept TPU type %s", c.Name())
					}
				}
			},
			keptMust: []string{"n2-standard-2", "e2-micro"},
		},
		{id: AlicloudACK, minKept: 100, maxKept: 200},
		{id: AlicloudCloudpilot, minKept: 100, maxKept: 200},
		{id: Hcloud, minKept: 5, maxKept: 100, keptMust: []string{"cax11", "cpx22"}},
		{id: HcloudItsh, minKept: 5, maxKept: 100, droppedMust: []string{"cax11"}, keptMust: []string{"cpx22"}},
		{id: UpCloud, minKept: 20, maxKept: 200, keptMust: []string{"CLOUDNATIVE-2xCPU-4GB"}},
		{
			id: OVH, minKept: 20, maxKept: 200,
			check: func(t *testing.T, kept []*selection.Candidate, _ map[string]string) {
				for _, c := range kept {
					if c.Server.AcceleratorCount > 0 || strings.HasPrefix(c.Name(), "bm-") {
						t.Errorf("ovh kept %s", c.Name())
					}
				}
			},
			droppedMust: []string{"d2-2"},
			keptMust:    []string{"b3-8"},
		},
		{
			id: Vultr, minKept: 20, maxKept: 200,
			check: func(t *testing.T, kept []*selection.Candidate, _ map[string]string) {
				for _, c := range kept {
					if strings.HasPrefix(c.Name(), "vbm-") || strings.HasPrefix(c.Name(), "vcg-") {
						t.Errorf("vultr kept %s", c.Name())
					}
				}
			},
			keptMust: []string{"vc2-2c-4gb"},
		},
	}
	for _, c := range cases {
		t.Run(string(c.id), func(t *testing.T) { checkEligibility(t, c) })
	}
}

type eligibilityCase struct {
	id          ID
	env         Env
	minKept     int
	maxKept     int
	check       func(t *testing.T, kept []*selection.Candidate, dropped map[string]string)
	droppedMust []string
	keptMust    []string
}

func checkEligibility(t *testing.T, c eligibilityCase) {
	a := mustAdapter(t, c.id)
	cands := snapshotCandidates(t, a.Vendor(), keeper.AllocationOnDemand)
	kept, dropped := split(a, cands, c.env)
	t.Logf("%s: kept %d of %d", c.id, len(kept), len(cands))
	if len(kept) < c.minKept || len(kept) > c.maxKept {
		t.Errorf("kept %d of %d, want %d..%d", len(kept), len(cands), c.minKept, c.maxKept)
	}
	rules := a.RequirementRules()
	keptNames := names(kept)
	for _, k := range kept {
		if !slices.Contains(rules.AllowedArchitectures, k.Architecture) {
			t.Errorf("kept %s of architecture %q", k.Name(), k.Architecture)
		}
	}
	for _, n := range c.keptMust {
		if !slices.Contains(keptNames, n) {
			t.Errorf("%s dropped: %s", n, dropped[n])
		}
	}
	for _, n := range c.droppedMust {
		if _, ok := dropped[n]; !ok {
			t.Errorf("%s kept (in snapshot: %v)", n, slices.ContainsFunc(cands, func(x *selection.Candidate) bool { return x.Name() == n }))
		}
	}
	if c.check != nil {
		c.check(t, kept, dropped)
	}
}

// TestEligibilityAutoModeFees: Auto Mode keeps exactly the snapshot types
// with a fee entry, and nothing without a fee table.
func TestEligibilityAutoModeFees(t *testing.T) {
	a := mustAdapter(t, AWSAutoMode)
	cands := snapshotCandidates(t, "aws", keeper.AllocationOnDemand)
	fees := fixtureFees(t)
	kept, _ := split(a, cands, Env{ManagedFees: fees})
	var want []string
	for _, c := range cands {
		if _, ok := fees[c.Name()]; ok && c.Architecture != "" && c.VCPUs() >= 2 {
			want = append(want, c.Name())
		}
	}
	// The fixture has one type missing from the snapshot and one 1-vCPU type.
	if got := names(kept); !slices.Equal(got, want) || len(got) != len(fees)-2 {
		t.Errorf("kept %v, want %v (fixture: %v)", got, want, slices.Sorted(maps.Keys(fees)))
	}
	if kept, _ := split(a, cands, Env{}); len(kept) != 0 {
		t.Errorf("kept %d types without a fee table", len(kept))
	}
	// The plain AWS adapter ignores the fee table.
	if kept, _ := split(mustAdapter(t, AWS), cands, Env{ManagedFees: fees}); len(kept) < 1000 {
		t.Errorf("aws kept only %d types", len(kept))
	}
}

// TestEligibilitySpotBasis: with a spot-only query, GCP and Alibaba fall
// back to Keeper's on-demand minimum for the on-demand price check.
func TestEligibilitySpotBasis(t *testing.T) {
	for _, id := range []ID{GCP, AlicloudACK} {
		a := mustAdapter(t, id)
		od, _ := split(a, snapshotCandidates(t, a.Vendor(), keeper.AllocationOnDemand), Env{})
		spot, dropped := split(a, snapshotCandidates(t, a.Vendor(), keeper.AllocationSpot), Env{})
		if len(spot) == 0 || len(spot) > len(od)+5 {
			t.Errorf("%s: kept %d on the spot basis, %d on-demand", id, len(spot), len(od))
		}
		for name, reason := range dropped {
			if reason == "no on-demand price" && slices.Contains(names(od), name) {
				t.Errorf("%s: %s has an on-demand price but was dropped on the spot basis", id, name)
			}
		}
	}
}

func TestEligibilityOVHGPUs(t *testing.T) {
	a := mustAdapter(t, OVH)
	cands := snapshotCandidates(t, "ovh", keeper.AllocationOnDemand)
	var gpus int
	for _, c := range cands {
		if c.Server.AcceleratorCount > 0 && c.Architecture == v1alpha1.ArchitectureAMD64 && !strings.HasPrefix(c.Name(), "bm-") {
			gpus++
		}
	}
	without, _ := split(a, cands, Env{})
	with, _ := split(a, cands, Env{AllowGPUs: true})
	if gpus == 0 || len(with) != len(without)+gpus {
		t.Errorf("GPUs %d, kept %d without and %d with AllowGPUs", gpus, len(without), len(with))
	}
}

// TestEligibilityRules checks single rules and their reasons on synthetic
// candidates.
func TestEligibilityRules(t *testing.T) {
	cand := func(name, arch string, vcpus int, memGiB float64, mod func(*selection.Candidate)) *selection.Candidate {
		res := selection.BuildCandidates([]keeper.QueryResult{{
			Query: keeper.Query{Allocation: keeper.AllocationOnDemand},
			Servers: []keeper.Server{{
				APIReference: name, CPUArchitecture: arch, VCPUs: vcpus,
				MemoryAmount: memGiB * 1024, Status: "active", Price: new(0.1),
			}},
		}})
		c := res[0]
		if mod != nil {
			mod(c)
		}
		return c
	}
	gpu := func(c *selection.Candidate) { c.Server.AcceleratorCount = 1 }
	retiring := func(c *selection.Candidate) { c.Server.Status = "planned-for-retirement" }
	tpu := func(c *selection.Candidate) {
		c.Server.AcceleratorCount = 4
		c.AcceleratorType = v1alpha1.AcceleratorTypeTPU
	}
	noPrice := func(c *selection.Candidate) { c.Prices = map[keeper.Allocation]float64{} }
	spotOnlyWithMin := func(c *selection.Candidate) {
		c.Prices = map[keeper.Allocation]float64{keeper.AllocationSpot: 0.05}
		c.Server.MinPriceOnDemand = new(0.2)
	}

	cases := []struct {
		id     ID
		c      *selection.Candidate
		env    Env
		reason string // "" = eligible
	}{
		{AWS, cand("m5.large", "x86_64", 2, 8, nil), Env{}, ""},
		{AWS, cand("mac1.metal", "x86_64_mac", 12, 32, nil), Env{}, "no Kubernetes equivalent"},
		{AWS, cand("t1.micro", "i386", 1, 0.6, nil), Env{}, "no Kubernetes equivalent"},
		{AWSAutoMode, cand("m5.large", "x86_64", 2, 8, nil), Env{ManagedFees: map[string]float64{"m5.large": 0.01}}, ""},
		{AWSAutoMode, cand("m5.large", "x86_64", 2, 8, nil), Env{ManagedFees: map[string]float64{"c5.large": 0.01}}, "no EKS Auto Mode SKU"},
		{AWSAutoMode, cand("c7g.medium", "arm64", 1, 2, nil), Env{ManagedFees: map[string]float64{"c7g.medium": 0.01}}, "fewer than 2 vCPUs"},
		{AWSAutoMode, cand("t3.small", "x86_64", 2, 2, nil), Env{ManagedFees: map[string]float64{"t3.small": 0.01}}, "small sizes"},
		{AWSAutoMode, cand("t3.medium", "x86_64", 2, 4, nil), Env{ManagedFees: map[string]float64{"t3.medium": 0.01}}, ""},

		{Azure, cand("Standard_D2s_v5", "x86_64", 2, 8, nil), Env{}, ""},
		{Azure, cand("Standard_D2ps_v5", "arm64", 2, 8, nil), Env{}, ""},
		{Azure, cand("Standard_B1ms", "x86_64", 1, 2, nil), Env{}, "restricted by AKS"},
		{Azure, cand("Standard_E64is_v3", "x86_64", 64, 432, nil), Env{}, "restricted by AKS"},
		{Azure, cand("Basic_A2", "x86_64", 2, 3.5, nil), Env{}, "restricted by AKS"},
		{Azure, cand("Standard_B2ts_v2", "x86_64", 2, 1, nil), Env{}, "less than 3.5 GiB"},
		{Azure, cand("Standard_B2ls_v2", "x86_64", 2, 4, nil), Env{}, ""},
		{Azure, cand("Standard_D1_v2", "x86_64", 1, 3.5, nil), Env{}, "fewer than 2 vCPUs"},
		{Azure, cand("Standard_E8-4ds_v5", "x86_64", 4, 64, nil), Env{}, "constrained"},
		{Azure, cand("Standard_DC4as_v5", "x86_64", 4, 16, nil), Env{}, "confidential"},
		{Azure, cand("Standard_EC8as_v5", "x86_64", 8, 64, nil), Env{}, "confidential"},
		{Azure, cand("Standard_NC6s_v3", "x86_64", 6, 112, gpu), Env{}, ""},
		{Azure, cand("Standard_NC6_Promo", "x86_64", 6, 56, gpu), Env{}, ""},
		{Azure, cand("Standard_NV32as_v4", "x86_64", 32, 112, gpu), Env{}, "GPU size not supported"},
		{Azure, cand("Standard_D4_v3", "x86_64", 4, 16, retiring), Env{}, "planned for retirement"},

		{GCP, cand("n2-standard-2", "x86_64", 2, 8, nil), Env{}, ""},
		{GCP, cand("ct5lp-hightpu-4t", "x86_64", 112, 192, tpu), Env{}, "TPU"},
		{GCP, cand("a4x-highgpu-4g", "arm64", 140, 884, noPrice), Env{}, "no on-demand price"},
		{GCP, cand("n2-standard-4", "x86_64", 4, 16, spotOnlyWithMin), Env{}, ""},
		{AlicloudACK, cand("ecs.g7.large", "x86_64", 2, 8, noPrice), Env{}, "no on-demand price"},
		{AlicloudCloudpilot, cand("ecs.g8y.large", "arm64", 2, 8, nil), Env{}, ""},

		{Hcloud, cand("cax11", "arm64", 2, 4, nil), Env{}, ""},
		{HcloudItsh, cand("cax11", "arm64", 2, 4, nil), Env{}, "architecture arm64"},
		{UpCloud, cand("CLOUDNATIVE-2xCPU-4GB", "x86_64", 2, 4, nil), Env{}, ""},
		{UpCloud, cand("ARM-2xCPU-4GB", "arm64", 2, 4, nil), Env{}, "architecture arm64"},

		{OVH, cand("b3-8", "x86_64", 2, 8, nil), Env{}, ""},
		{OVH, cand("d2-2", "x86_64", 1, 2, nil), Env{}, "not offered by OVH MKS"},
		{OVH, cand("bm-s1", "x86_64", 8, 32, nil), Env{}, "bare metal"},
		{OVH, cand("l4-90", "x86_64", 22, 90, gpu), Env{}, "GPU flavor"},
		{OVH, cand("l4-90", "x86_64", 22, 90, gpu), Env{AllowGPUs: true}, ""},

		{Vultr, cand("vc2-2c-4gb", "x86_64", 2, 4, nil), Env{}, ""},
		{Vultr, cand("vbm-4c-32gb", "x86_64", 4, 32, nil), Env{}, "bare metal"},
		{Vultr, cand("vcg-a16-2c-8g-2vram", "x86_64", 2, 8, gpu), Env{}, "GPU plan"},
	}
	for _, c := range cases {
		ok, reason := mustAdapter(t, c.id).Eligible(c.c, c.env)
		switch {
		case c.reason == "" && !ok:
			t.Errorf("%s %s dropped: %s", c.id, c.c.Name(), reason)
		case c.reason != "" && (ok || !strings.Contains(reason, c.reason)):
			t.Errorf("%s %s: got %v %q, want reason containing %q", c.id, c.c.Name(), ok, reason, c.reason)
		}
	}
}

func TestEligibleFuncAndEvaluate(t *testing.T) {
	a := mustAdapter(t, AWSAutoMode)
	fees := fixtureFees(t)
	cands := snapshotCandidates(t, "aws", keeper.AllocationOnDemand)
	spec := &v1alpha1.SCNodePoolSpec{}
	res, err := selection.Evaluate(spec, cands, selection.Options{
		Eligible:    EligibleFunc(a, Env{ManagedFees: fees}),
		ManagedFees: fees,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.InstanceTypes) != len(fees)-2 || res.Excluded.ProviderIneligible != len(cands)-len(fees)+2 {
		t.Errorf("selected %v, excluded %+v", res.Names(), res.Excluded)
	}
	byName := map[string]*selection.Candidate{}
	for _, c := range cands {
		byName[c.Name()] = c
	}
	for _, it := range res.InstanceTypes {
		want := byName[it.Name].Prices[keeper.AllocationOnDemand] + fees[it.Name]
		if it.Price != want {
			t.Errorf("%s price %v, want %v (with fee)", it.Name, it.Price, want)
		}
	}
}

func TestSelectsAccelerators(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	cases := []struct {
		sel  *v1alpha1.AcceleratorSelector
		want bool
	}{
		{nil, false},
		{&v1alpha1.AcceleratorSelector{}, false},
		{&v1alpha1.AcceleratorSelector{Count: &v1alpha1.FloatRange{Min: f(1)}}, true},
		{&v1alpha1.AcceleratorSelector{Count: &v1alpha1.FloatRange{Min: f(0)}}, false},
		{&v1alpha1.AcceleratorSelector{Count: &v1alpha1.FloatRange{Max: f(0)}}, false},
		{&v1alpha1.AcceleratorSelector{Types: []v1alpha1.AcceleratorType{v1alpha1.AcceleratorTypeGPU}}, true},
		{&v1alpha1.AcceleratorSelector{Manufacturers: []string{"NVIDIA"}}, true},
		{&v1alpha1.AcceleratorSelector{Models: []string{"L4"}}, true},
		{&v1alpha1.AcceleratorSelector{MemoryPerDevice: &v1alpha1.GiBMin{MinGiB: f(16)}}, true},
		{&v1alpha1.AcceleratorSelector{MemoryTotal: &v1alpha1.GiBMin{MinGiB: f(16)}}, true},
		{&v1alpha1.AcceleratorSelector{Count: &v1alpha1.FloatRange{Max: f(0)}, Models: []string{"L4"}}, false},
	}
	for i, c := range cases {
		spec := &v1alpha1.SCNodePoolSpec{Selector: &v1alpha1.Selector{Accelerator: c.sel}}
		if got := SelectsAccelerators(spec); got != c.want {
			t.Errorf("case %d: got %v", i, got)
		}
	}
	if SelectsAccelerators(nil) || SelectsAccelerators(&v1alpha1.SCNodePoolSpec{}) {
		t.Error("empty spec selects accelerators")
	}
}
