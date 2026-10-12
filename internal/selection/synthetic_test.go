/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package selection

import (
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/SpareCores/sc-karpenter-nodepool/api/v1alpha1"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/keeper"
)

// Synthetic rows (no snapshot) for exact arithmetic and edge cases.

var testKey = keeper.BenchmarkKey{ID: "geekbench:score", Config: `{"cores": "single"}`}

func row(name, family string, vcpus int, price float64) keeper.Server {
	return keeper.Server{
		APIReference: name, Family: family, VCPUs: vcpus, CPUArchitecture: "x86_64",
		MemoryAmount: float64(vcpus) * 4096, Price: &price,
	}
}

// synth builds candidates from base rows and per-type scores.
func synth(rows []keeper.Server, scores map[string]float64) []*Candidate {
	scored := make([]keeper.Server, 0, len(rows))
	for _, r := range rows {
		s := r
		if v, ok := scores[r.APIReference]; ok {
			s.SelectedBenchmarkScore = &v
		}
		scored = append(scored, s)
	}
	k := testKey
	return BuildCandidates([]keeper.QueryResult{
		{Query: keeper.Query{Allocation: keeper.AllocationOnDemand}, Servers: rows},
		{Query: keeper.Query{Allocation: keeper.AllocationOnDemand, Benchmark: &k}, Servers: scored},
	})
}

func evalSynth(t *testing.T, spec *v1alpha1.SCNodePoolSpec, cands []*Candidate, opts ...Options) Result {
	t.Helper()
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	o.Catalog = catalog
	res, err := Evaluate(spec, cands, o)
	if err != nil {
		t.Fatal(err)
	}
	checkCounters(t, res, cands)
	return res
}

func TestBuildCandidates(t *testing.T) {
	k := testKey
	low, high, score := 0.1, 0.2, 1000.0
	spot := 0.05
	extra := row("extra", "x", 2, 0.3)
	extra.SelectedBenchmarkScore = &score
	a := row("a", "f", 2, 0.2)
	a.SelectedBenchmarkScore = &score
	results := []keeper.QueryResult{
		{Query: keeper.Query{Allocation: keeper.AllocationOnDemand, Benchmark: &k, PushDown: keeper.PushDown{BenchmarkScoreMin: &score}},
			Servers: []keeper.Server{a, extra}},
		{Query: keeper.Query{Allocation: keeper.AllocationOnDemand}, Servers: []keeper.Server{
			{APIReference: "b", Family: "f", VCPUs: 4, CPUArchitecture: "arm64", Price: &high},
			{APIReference: "a", Family: "f", VCPUs: 2, CPUArchitecture: "x86_64", Price: &low},
			{APIReference: "m", Family: "mac", VCPUs: 4, CPUArchitecture: "arm64_mac", Price: &low},
		}},
		{Query: keeper.Query{Allocation: keeper.AllocationSpot}, Servers: []keeper.Server{
			{APIReference: "a", Price: &spot}, {APIReference: "unknown-to-base", Price: &spot},
		}},
	}
	cands := BuildCandidates(results)
	names := make([]string, 0, len(cands))
	for _, c := range cands {
		names = append(names, c.Name())
	}
	if !slices.Equal(names, []string{"a", "b", "m", "unknown-to-base"}) {
		t.Fatalf("candidates = %v (only rows of queries without a benchmark, sorted)", names)
	}
	ca, cb, cm := cands[0], cands[1], cands[2]
	if ca.Prices[keeper.AllocationOnDemand] != 0.1 || ca.Prices[keeper.AllocationSpot] != 0.05 || ca.Scores[k] != 1000 {
		t.Errorf("a = %+v", ca)
	}
	if ca.Server.Price == nil || *ca.Server.Price != 0.1 {
		t.Error("Server is not the base row")
	}
	if !cb.KeeperFiltered[k] || ca.KeeperFiltered[k] {
		t.Error("KeeperFiltered: b was dropped by a pushed-down threshold")
	}
	if ca.Architecture != v1alpha1.ArchitectureAMD64 || cb.Architecture != v1alpha1.ArchitectureARM64 || cm.Architecture != "" {
		t.Error("architecture mapping")
	}
	// Without a base query every row is a candidate.
	if n := len(BuildCandidates(results[:1])); n != 2 {
		t.Errorf("benchmark-only join: %d candidates", n)
	}
}

func TestCandidateDerived(t *testing.T) {
	s := func(f func(*keeper.Server)) *Candidate {
		r := row("x", "f", 4, 0.1)
		f(&r)
		return BuildCandidates([]keeper.QueryResult{{Servers: []keeper.Server{r}}})[0]
	}
	str := func(v string) *string { return &v }
	tests := []struct {
		name string
		mod  func(*keeper.Server)
		want v1alpha1.AcceleratorType
	}{
		{name: "none", mod: func(*keeper.Server) {}},
		{name: "typed", mod: func(r *keeper.Server) { r.AcceleratorCount = 1; r.AcceleratorType = str("gpu") }, want: "GPU"},
		{name: "TPU family", mod: func(r *keeper.Server) {
			r.AcceleratorCount = 4
			r.AcceleratorFamily = str("TPU")
			r.AcceleratorManufacturer = str("Google")
		}, want: "TPU"},
		{name: "AMD", mod: func(r *keeper.Server) { r.AcceleratorCount = 1; r.AcceleratorManufacturer = str("AMD") }, want: "GPU"},
		{name: "Habana untyped", mod: func(r *keeper.Server) { r.AcceleratorCount = 8; r.AcceleratorManufacturer = str("Habana") }},
		{name: "nothing known", mod: func(r *keeper.Server) { r.AcceleratorCount = 1 }},
	}
	for _, tt := range tests {
		if got := s(tt.mod).AcceleratorType; got != tt.want {
			t.Errorf("%s: %q, want %q", tt.name, got, tt.want)
		}
	}
	c := s(func(r *keeper.Server) { r.CPUCores = new(2) })
	if on, ok := c.Hyperthreading(); !on || !ok || c.MemoryGiB() != 16 || c.MemoryPerVCPU() != 4 {
		t.Errorf("derived: %v %v %v %v", on, ok, c.MemoryGiB(), c.MemoryPerVCPU())
	}
	if _, ok := s(func(r *keeper.Server) { r.CPUCores = new(0) }).Hyperthreading(); ok {
		t.Error("0 cores is unknown")
	}
	if s(func(r *keeper.Server) { r.VCPUs = 0 }).MemoryPerVCPU() != 0 {
		t.Error("0 vCPUs")
	}
	if _, ok := s(func(r *keeper.Server) { r.Price = new(0.0) }).Price(keeper.AllocationOnDemand, 0); ok {
		t.Error("zero price accepted")
	}
}

func TestOptions(t *testing.T) {
	rows := []keeper.Server{row("a.large", "a", 2, 0.10), row("b.large", "b", 2, 0.10), row("c.large", "c", 2, 0.10), row("d.large", "d", 2, 0.10)}
	rows[1].CPUArchitecture = "arm64"
	cands := synth(rows, nil)
	spec := specOf(&v1alpha1.Selector{Price: &v1alpha1.PriceSelector{MaxHourly: "0.11"}})
	opts := Options{
		Eligible:      func(c *Candidate) bool { return c.Name() != "c.large" },
		ManagedFees:   map[string]float64{"a.large": 0.012, "b.large": 0.01, "c.large": 0.01},
		Architectures: []v1alpha1.Architecture{v1alpha1.ArchitectureAMD64},
	}
	res := evalSynth(t, spec, cands, opts)
	if got := res.Names(); !slices.Equal(got, []string{}) {
		t.Errorf("selected %v", got)
	}
	want := map[string]string{
		"a.large": "filters.price.maxHourly", // 0.10 + 0.012 fee > 0.11
		"b.large": ReasonTemplateConstraints,
		"c.large": ReasonProviderIneligible,
		"d.large": ReasonMissingData, // no fee entry: no usable price
	}
	for n, r := range want {
		if res.Misses[n].Reason != r {
			t.Errorf("%s: %+v, want %s", n, res.Misses[n], r)
		}
	}
	if !res.Misses["c.large"].NotOffered || !res.Misses["d.large"].NotOffered || res.Misses["b.large"].NotOffered {
		t.Error("NotOffered")
	}
	if m := res.Misses["a.large"].Relative; math.Abs(m-0.002/0.11) > 1e-9 {
		t.Errorf("fee miss %v", m)
	}

	// includeManagedFee: false drops the fee; it is ignored without fees.
	spec.Selector.Price.IncludeManagedFee = new(false)
	opts.Eligible, opts.Architectures = nil, nil
	res = evalSynth(t, spec, cands, opts)
	if got := res.Names(); !slices.Equal(got, []string{"a.large", "b.large", "c.large", "d.large"}) {
		t.Errorf("without fee: %v", got)
	}
	if res.InstanceTypes[0].Price != 0.10 || res.Families[0].Rank != 0 || res.Families[0].Objective != nil {
		t.Errorf("types %+v families %+v", res.InstanceTypes[0], res.Families[0])
	}
	spec.Selector.Price.IncludeManagedFee = new(true)
	res = evalSynth(t, spec, cands)
	if len(res.InstanceTypes) != 4 {
		t.Error("includeManagedFee without fees must be ignored")
	}

	// Spot basis default from the template; no spot prices: nothing priced.
	res = evalSynth(t, specOf(nil), cands, Options{DefaultBasis: v1alpha1.PriceBasisSpot})
	if res.Basis != v1alpha1.PriceBasisSpot || len(res.InstanceTypes) != 0 || res.Excluded.MissingData != 4 {
		t.Errorf("spot: %+v", res)
	}
	st := res.Excluded.Status()
	if st.MissingData != 4 || st.Filters != nil {
		t.Errorf("status %+v", st)
	}
	if (*Band)(nil).Status() != nil {
		t.Error("nil band status")
	}
}

func TestSpecErrors(t *testing.T) {
	for name, sel := range map[string]*v1alpha1.Selector{
		"maxHourly":        {Price: &v1alpha1.PriceSelector{MaxHourly: "cheap"}},
		"maxHourlyPerVcpu": {Price: &v1alpha1.PriceSelector{MaxHourlyPerVCPU: "-1"}},
		"glob":             {InstanceTypes: &v1alpha1.GlobFilter{Include: []string{"[a-"}}},
		"family glob":      {Families: &v1alpha1.GlobFilter{Exclude: []string{"\\"}}},
	} {
		if _, err := Evaluate(specOf(sel), nil, Options{}); !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("%s: want ErrInvalidSpec, got %v", name, err)
		}
	}
}

// Exact band arithmetic: families' medians 100 (a), 95 (b), 90 (c: sizes
// 80/100 → median 90), 80 (d), 70 (e).
func rankingRows() ([]keeper.Server, map[string]float64) {
	rows := []keeper.Server{
		row("a.1", "a", 2, 1), row("b.1", "b", 2, 1), row("c.1", "c", 2, 1), row("c.2", "c", 4, 2),
		row("d.1", "d", 2, 1), row("e.1", "e", 2, 1), row("u.1", "u", 2, 1), row("a.2", "a", 2, 1),
	}
	scores := map[string]float64{"a.1": 100, "b.1": 95, "c.1": 80, "c.2": 100, "d.1": 80, "e.1": 70}
	return rows, scores
}

func TestRankingArithmetic(t *testing.T) {
	rows, scores := rankingRows()
	cands := synth(rows, scores)
	tests := []struct {
		name     string
		band     *float64
		min, max *int32
		kept     []string
		widened  bool
		trimmed  bool
		misses   map[string]float64
	}{
		// boundary 85: a, b, c inside; d misses 85 by 5/85.
		{name: "band", band: new(15.0), kept: []string{"a", "b", "c"},
			misses: map[string]float64{"d.1": 5.0 / 85, "e.1": 15.0 / 85}},
		// boundary 95 keeps a, b; widened to 4: cut-off is d (80).
		{name: "widened", band: new(5.0), min: new(int32(4)), kept: []string{"a", "b", "c", "d"}, widened: true,
			misses: map[string]float64{"e.1": 10.0 / 80}},
		// all inside, trimmed to 2: cut-off is b (95).
		{name: "trimmed", band: new(50.0), max: new(int32(2)), kept: []string{"a", "b"}, trimmed: true,
			misses: map[string]float64{"c.1": 5.0 / 95, "c.2": 5.0 / 95, "d.1": 15.0 / 95}},
		{name: "min beyond candidates", band: new(0.0), min: new(int32(9)), kept: []string{"a", "b", "c", "d", "e"}, widened: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := v1alpha1.Ranking{Score: v1alpha1.RankingScore{Benchmark: geekbenchSingle()}, Families: &v1alpha1.RankingFamilies{Min: tt.min, Max: tt.max}}
			if tt.band != nil {
				r.Band = &v1alpha1.RankingBand{WithinPercent: *tt.band}
			}
			res := evalSynth(t, rankingSpec(nil, r), cands)
			var kept []string
			for _, f := range res.Families {
				kept = append(kept, f.Name)
			}
			if !slices.Equal(kept, tt.kept) {
				t.Errorf("kept %v, want %v", kept, tt.kept)
			}
			if res.Band.Widened != tt.widened || res.Band.Trimmed != tt.trimmed || res.Band.Reference != "a" || res.Band.ReferenceValue != 100 {
				t.Errorf("band %+v", res.Band)
			}
			for n, want := range tt.misses {
				if m := res.Misses[n]; m.Reason != ReasonRanking || math.Abs(m.Relative-want) > 1e-12 {
					t.Errorf("%s: %+v, want ranking miss %v", n, m, want)
				}
			}
			if m := res.Misses["u.1"]; m.Reason != ReasonMissingData {
				t.Errorf("unscored u.1: %+v", m)
			}
			if f := res.Families[0]; f.Sizes != 1 || f.Rank != 1 {
				t.Errorf("family a: %+v (a.2 is unscored and excluded)", f)
			}
		})
	}
	if median([]float64{3, 1, 2, 4}) != 2.5 || median([]float64{5}) != 5 {
		t.Error("median")
	}
}

// Price ranking does not need a score; candidates without scores rank too.
func TestRankingPriceNeedsNoScore(t *testing.T) {
	rows := []keeper.Server{row("a.1", "a", 2, 0.10), row("b.1", "b", 2, 0.12), row("c.1", "c", 2, 0.20)}
	res := evalSynth(t, rankingSpec(nil, v1alpha1.Ranking{
		Score: v1alpha1.RankingScore{Benchmark: geekbenchSingle()}, Objective: v1alpha1.ObjectivePrice,
		Band: &v1alpha1.RankingBand{WithinPercent: 25},
	}), synth(rows, nil))
	if got := res.Names(); !slices.Equal(got, []string{"a.1", "b.1"}) {
		t.Errorf("selected %v", got)
	}
	if m := res.Misses["c.1"]; math.Abs(m.Relative-(0.20-0.125)/0.125) > 1e-12 {
		t.Errorf("c.1 miss %+v", m)
	}
	// Nothing survives the filters: no band.
	res = evalSynth(t, rankingSpec(&v1alpha1.Selector{VCPUs: &v1alpha1.IntRange{Min: new(int32(64))}}, v1alpha1.Ranking{
		Score: v1alpha1.RankingScore{Benchmark: geekbenchSingle()}, Families: &v1alpha1.RankingFamilies{Max: new(int32(1))},
	}), synth(rows, nil))
	if res.Band != nil || len(res.InstanceTypes) != 0 {
		t.Errorf("empty ranking: %+v", res)
	}
}

// A benchmark threshold Keeper applied: types it dropped fail the filter
// (the threshold was loosened by the margin, so the miss exceeds it).
func TestKeeperFilteredBenchmark(t *testing.T) {
	k := testKey
	min := 2000.0
	a, b := row("a", "f", 2, 0.1), row("b", "f", 2, 0.1)
	s := 2500.0
	a2 := a
	a2.SelectedBenchmarkScore = &s
	cands := BuildCandidates([]keeper.QueryResult{
		{Query: keeper.Query{Allocation: keeper.AllocationOnDemand}, Servers: []keeper.Server{a, b}},
		{Query: keeper.Query{Allocation: keeper.AllocationOnDemand, Benchmark: &k, PushDown: keeper.PushDown{BenchmarkScoreMin: &min}},
			Servers: []keeper.Server{a2}},
	})
	spec := specOf(&v1alpha1.Selector{Benchmarks: []v1alpha1.BenchmarkFilter{{
		BenchmarkRef: *geekbenchSingle(), Min: &min,
	}}})
	res := evalSynth(t, spec, cands)
	if got := res.Names(); !slices.Equal(got, []string{"a"}) {
		t.Errorf("selected %v", got)
	}
	if m := res.Misses["b"]; m.Reason != "filters.benchmarks" || !math.IsInf(m.Relative, 1) {
		t.Errorf("b: %+v", m)
	}
}

// planSpec uses every pushed-down selector, a 10% churn margin and a
// ranking score without a filter.
func planSpec() *v1alpha1.SCNodePoolSpec {
	return &v1alpha1.SCNodePoolSpec{
		Selector: &v1alpha1.Selector{
			VCPUs:         &v1alpha1.IntRange{Min: new(int32(2)), Max: new(int32(300))},
			Architectures: []v1alpha1.Architecture{v1alpha1.ArchitectureAMD64, v1alpha1.ArchitectureARM64},
			CPU: &v1alpha1.CPUSelector{
				ClockSpeedMinGHz: new(3.0), Hyperthreading: new(false), Manufacturers: []string{"AMD"},
				Caches: &v1alpha1.CPUCaches{L1dPerCoreMinKiB: new(32.0), L1iPerCoreMinKiB: new(32.0), L2PerCoreMinKiB: new(1024.0), L3MinMiB: new(32.0), L3TotalMinMiB: new(64.0)},
			},
			Memory:      &v1alpha1.MemorySelector{MinGiB: new(4.0), MaxGiB: new(64.0), PerVCPU: &v1alpha1.GiBRange{MinGiB: new(2.0), MaxGiB: new(8.0)}},
			Accelerator: &v1alpha1.AcceleratorSelector{Count: &v1alpha1.FloatRange{Min: new(1.0), Max: new(0.0)}, MemoryPerDevice: &v1alpha1.GiBMin{MinGiB: new(16.0)}, MemoryTotal: &v1alpha1.GiBMin{MinGiB: new(24.0)}},
			Network:     &v1alpha1.NetworkSelector{BaselineMinGbps: new(10.0), PeakMinGbps: new(25.0)},
			Storage:     &v1alpha1.StorageSelector{Local: &v1alpha1.LocalStorageSelector{MinGB: new(200.0), CountMin: new(int32(4))}},
			Price:       &v1alpha1.PriceSelector{MaxHourly: "0.40", MaxHourlyPerVCPU: "0.05"},
			Benchmarks: []v1alpha1.BenchmarkFilter{
				{BenchmarkRef: *geekbenchSingle(), Min: new(2000.0)},
				{BenchmarkRef: v1alpha1.BenchmarkRef{ID: "geekbench:score", Config: cfg(`{"cores":"single"}`)}, Min: new(2500.0)},
				{BenchmarkRef: v1alpha1.BenchmarkRef{ID: "stress_ng:bestn"}, Min: new(1000.0), Normalize: v1alpha1.NormalizePerVcpu},
				{BenchmarkRef: v1alpha1.BenchmarkRef{ID: "stress_ng:bestn"}, Min: new(10.0), Normalize: v1alpha1.NormalizePerPrice},
				{BenchmarkRef: v1alpha1.BenchmarkRef{ID: "membench:latency", Config: cfg(`{"scope":"RAM"}`)}, Max: new(100.0)},
			},
			WorkloadProfiles: []v1alpha1.WorkloadProfileFilter{{Name: "web", Min: 1.5}},
		},
		Ranking: &v1alpha1.Ranking{Score: v1alpha1.RankingScore{Benchmark: &v1alpha1.BenchmarkRef{ID: "passmark:cpu_single_threaded_test"}},
			Families: &v1alpha1.RankingFamilies{Max: new(int32(3))}},
		Churn: v1alpha1.Churn{RemoveMarginPercent: new(10.0)},
	}
}

func near(p *float64, want float64) bool {
	return p != nil && math.Abs(*p-want) <= 1e-6*math.Max(1, math.Abs(want))
}

// checkNear compares named float push-downs.
func checkNear(t *testing.T, got map[string]*float64, want map[string]float64) {
	t.Helper()
	for name, w := range want {
		if p := got[name]; !near(p, w) {
			t.Errorf("%s = %v, want %v", name, p, w)
		}
	}
}

func TestPlanQueries(t *testing.T) {
	qs, err := PlanQueries(planSpec(), "aws", []string{"us-east-1", "us-west-2"}, Options{Catalog: catalog})
	if err != nil {
		t.Fatal(err)
	}
	if len(qs) != 6 {
		t.Fatalf("want base + 5 benchmark queries, got %d", len(qs))
	}
	base := qs[0]
	if base.Benchmark != nil || base.Allocation != keeper.AllocationOnDemand || len(base.Regions) != 2 ||
		base.PushDown.MemoryMin != nil || base.PushDown.Architectures != nil {
		t.Errorf("base query must be unfiltered: %+v", base)
	}
	ids := make([]string, 0, len(qs)-1)
	for _, q := range qs[1:] {
		ids = append(ids, q.Benchmark.ID)
	}
	if !slices.Equal(ids, []string{"geekbench:score", "stress_ng:bestn", "membench:latency", "workload_profile:web", "passmark:cpu_single_threaded_test"}) {
		t.Errorf("benchmark queries %v", ids)
	}
	if qs[5].PushDown.HasBenchmarkThreshold() || qs[3].PushDown.BenchmarkScoreMin != nil {
		t.Error("unexpected benchmark thresholds")
	}
	gb, sng, lat, web := qs[1].PushDown, qs[2].PushDown, qs[3].PushDown, qs[4].PushDown
	// Benchmark thresholds: the strictest of several filters, loosened by 10%.
	checkNear(t, map[string]*float64{
		"geekbench min": gb.BenchmarkScoreMin, "stress-ng per vCPU": sng.BenchmarkScorePerVCPUMin,
		"stress-ng per price": sng.BenchmarkScorePerPriceMin, "latency max": lat.BenchmarkScoreMax, "web min": web.BenchmarkScoreMin,
	}, map[string]float64{
		"geekbench min": 2250, "stress-ng per vCPU": 900, "stress-ng per price": 9, "latency max": 110, "web min": 1.35,
	})
	p := gb
	checkNear(t, map[string]*float64{
		"memory_min": p.MemoryMin, "memory_max": p.MemoryMax, "memory_per_vcpu_min": p.MemoryPerVCPUMin,
		"memory_per_vcpu_max": p.MemoryPerVCPUMax, "cpu_speed_min": p.CPUSpeedMin, "l1d": p.CPUL1dCacheMin,
		"l1i": p.CPUL1iCacheMin, "l2": p.CPUL2CacheMin, "l3": p.CPUL3CacheMin, "l3 total": p.CPUL3CacheTotalMin,
		"network baseline": p.NetworkSpeedBaselineMin, "network max": p.NetworkSpeedMaxMin, "storage_size": p.StorageSizeMin,
		"gpu_min": p.GPUMin, "gpu_max": p.GPUMax, "gpu_memory_min": p.GPUMemoryMin, "gpu_memory_total": p.GPUMemoryTotal,
		"price_max": p.PriceMax,
	}, map[string]float64{
		"memory_min": 3.6, "memory_max": 70.4, "memory_per_vcpu_min": 1.8, "memory_per_vcpu_max": 8.8, "cpu_speed_min": 2.7,
		"l1d": 28.8, "l1i": 28.8, "l2": 921.6, "l3": 28.8, "l3 total": 57.6, "network baseline": 9, "network max": 22.5,
		"storage_size": 180, "gpu_min": 0.9, "gpu_max": 0, "gpu_memory_min": 14.4, "gpu_memory_total": 21.6, "price_max": 0.44,
	})
	if p.VCPUsMin == nil || *p.VCPUsMin != 1 || p.VCPUsMax != nil {
		t.Errorf("vcpus %v %v (max 300 > 256 is not sent)", p.VCPUsMin, p.VCPUsMax)
	}
	if !slices.Equal(p.Architectures, []string{"x86_64", "arm64"}) || p.CPUHyperthreading == nil || *p.CPUHyperthreading {
		t.Errorf("architectures %v, hyperthreading %v", p.Architectures, p.CPUHyperthreading)
	}
	if p.StorageCountMin == nil || *p.StorageCountMin != 3 {
		t.Errorf("storage_count_min %v", p.StorageCountMin)
	}
}

// Lenient policies push no benchmark thresholds; countMin 1 becomes
// storage_size >= 1; vcpus_min above Keeper's limit is not sent.
func TestPlanQueriesLenient(t *testing.T) {
	spec := planSpec()
	spec.Selector.Unbenchmarked = v1alpha1.UnbenchmarkedInclude
	spec.Selector.Storage.Local = &v1alpha1.LocalStorageSelector{CountMin: new(int32(1))}
	spec.Selector.VCPUs = &v1alpha1.IntRange{Min: new(int32(300))}
	spec.Selector.Price.Basis = v1alpha1.PriceBasisSpot
	qs, err := PlanQueries(spec, "aws", []string{"us-east-1"}, Options{Catalog: catalog})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range qs[1:] {
		if q.PushDown.HasBenchmarkThreshold() || q.Allocation != keeper.AllocationSpot {
			t.Errorf("%s: %+v", q.Benchmark, q.PushDown)
		}
		if !near(q.PushDown.StorageSizeMin, 1) || q.PushDown.StorageCountMin != nil || q.PushDown.VCPUsMin != nil {
			t.Errorf("storage/vcpus %+v", q.PushDown)
		}
	}
}

func TestPlanQueriesMargins(t *testing.T) {
	spec := planSpec()
	spec.Selector.Storage.Local = &v1alpha1.LocalStorageSelector{MinGB: new(100.0), CountMin: new(int32(2))}
	tests := []struct {
		margin    float64
		count     *int
		sizeMin   *float64
		memoryMin *float64
	}{
		// Zero margin: exact bounds; countMin 2 stays a count.
		{margin: 0, count: new(2), sizeMin: new(100.0), memoryMin: new(4.0)},
		// 5%: countMin 2 loosens to "has local storage".
		{margin: 5, sizeMin: new(95.0), memoryMin: new(3.8)},
		// 100% or more drops lower bounds.
		{margin: 100},
	}
	for _, tt := range tests {
		spec.Churn.RemoveMarginPercent = new(tt.margin)
		qs, err := PlanQueries(spec, "aws", []string{"us-east-1"}, Options{Catalog: catalog})
		if err != nil {
			t.Fatal(err)
		}
		p := qs[1].PushDown
		if (p.StorageCountMin == nil) != (tt.count == nil) || (tt.count != nil && *p.StorageCountMin != *tt.count) {
			t.Errorf("margin %v: storage_count_min %v", tt.margin, p.StorageCountMin)
		}
		for name, pair := range map[string][2]*float64{"storage_size": {p.StorageSizeMin, tt.sizeMin}, "memory_min": {p.MemoryMin, tt.memoryMin}} {
			if (pair[0] == nil) != (pair[1] == nil) || (pair[1] != nil && !near(pair[0], *pair[1])) {
				t.Errorf("margin %v: %s = %v, want %v", tt.margin, name, pair[0], pair[1])
			}
		}
	}
	// Without benchmarks: one unfiltered query.
	qs, _ := PlanQueries(specOf(&v1alpha1.Selector{VCPUs: &v1alpha1.IntRange{Min: new(int32(2))}}), "hcloud", []string{"4"}, Options{})
	if len(qs) != 1 || qs[0].PushDown.VCPUsMin != nil {
		t.Errorf("no benchmarks: %+v", qs)
	}
}
