/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package selection

import (
	"testing"

	"github.com/SpareCores/sc-karpenter-nodepool/api/v1alpha1"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/keeper"
)

// One case per selector field: types that must pass (in), types that must
// fail this filter (out), and a predicate for rows lacking the data (each
// such row must be excluded as missing data, and at least one must exist).
// Instance types are long-lived ones, so snapshot refreshes keep the cases
// valid.
type selectorCase struct {
	name    string
	vendor  string
	sel     v1alpha1.Selector
	field   string
	in      []string
	out     []string
	missing func(*keeper.Server) bool
}

func selectorCases() []selectorCase {
	return []selectorCase{
		{name: "instanceTypes globs", vendor: "aws", field: "instanceTypes",
			sel: v1alpha1.Selector{InstanceTypes: &v1alpha1.GlobFilter{Include: []string{"m7i.*", "c7?.large"}, Exclude: []string{"*.metal*", "*.48xlarge"}}},
			in:  []string{"m7i.large", "c7a.large"}, out: []string{"m7i.48xlarge", "m7i.metal-24xl", "m8g.large", "c7a.xlarge"}},
		{name: "families exclude", vendor: "azure", field: "families",
			sel: v1alpha1.Selector{Families: &v1alpha1.GlobFilter{Exclude: []string{"DSv5", "[BE]*"}}},
			in:  []string{"Standard_D2as_v5"}, out: []string{"Standard_D2s_v5", "Standard_B2s", "Standard_E2s_v5"}},
		{name: "vcpus inclusive", vendor: "gcp", field: "vcpus",
			sel: v1alpha1.Selector{VCPUs: &v1alpha1.IntRange{Min: new(int32(2)), Max: new(int32(4))}},
			in:  []string{"n2-standard-2", "c3-standard-4"}, out: []string{"t2a-standard-1", "g2-standard-8"}},
		{name: "architectures", vendor: "hcloud", field: "architectures",
			sel: v1alpha1.Selector{Architectures: []v1alpha1.Architecture{v1alpha1.ArchitectureARM64}},
			in:  []string{"cax11", "cax41"}, out: []string{"ccx13", "cpx22"}},
		{name: "cpu.manufacturers", vendor: "azure", field: "cpu.manufacturers",
			sel: v1alpha1.Selector{CPU: &v1alpha1.CPUSelector{Manufacturers: []string{"AMD"}}},
			in:  []string{"Standard_D2as_v5"}, out: []string{"Standard_D2s_v5", "Standard_D2ps_v5"},
			missing: func(s *keeper.Server) bool { return s.CPUManufacturer == nil }},
		{name: "cpu.allocations", vendor: "aws", field: "cpu.allocations",
			sel: v1alpha1.Selector{CPU: &v1alpha1.CPUSelector{Allocations: []v1alpha1.CPUAllocation{v1alpha1.CPUAllocationBurstable}}},
			in:  []string{"t3.micro"}, out: []string{"m7i.large"}},
		{name: "cpu.clockSpeedMinGHz", vendor: "gcp", field: "cpu.clockSpeedMinGHz",
			sel: v1alpha1.Selector{CPU: &v1alpha1.CPUSelector{ClockSpeedMinGHz: new(2.5)}},
			in:  []string{"n2-standard-2"}, out: []string{"e2-standard-2"},
			missing: func(s *keeper.Server) bool { return s.CPUSpeed == nil }},
		{name: "cpu.hyperthreading on", vendor: "aws", field: "cpu.hyperthreading",
			sel: v1alpha1.Selector{CPU: &v1alpha1.CPUSelector{Hyperthreading: new(true)}},
			in:  []string{"m7i.large", "i4i.large"}, out: []string{"m8g.large", "c7a.large"}},
		{name: "cpu.hyperthreading off", vendor: "gcp", field: "cpu.hyperthreading",
			sel: v1alpha1.Selector{CPU: &v1alpha1.CPUSelector{Hyperthreading: new(false)}},
			in:  []string{"t2a-standard-1"}, out: []string{"n2-standard-2"},
			missing: func(s *keeper.Server) bool { return s.CPUCores == nil }},
		{name: "cpu.flags allOf", vendor: "aws", field: "cpu.flags",
			sel: v1alpha1.Selector{CPU: &v1alpha1.CPUSelector{Flags: &v1alpha1.CPUFlags{AllOf: []string{"avx512f", "amx_tile"}}}},
			in:  []string{"m7i.large"}, out: []string{"c7a.large", "m8g.large"},
			missing: func(s *keeper.Server) bool { return len(s.CPUFlags) == 0 }},
		{name: "cpu.flags anyOf", vendor: "aws", field: "cpu.flags",
			sel: v1alpha1.Selector{CPU: &v1alpha1.CPUSelector{Flags: &v1alpha1.CPUFlags{AnyOf: []string{"sve", "amx_tile"}}}},
			in:  []string{"m7i.large", "m8g.large"}, out: []string{"c7a.large"},
			missing: func(s *keeper.Server) bool { return len(s.CPUFlags) == 0 }},
		{name: "cpu.flags noneOf", vendor: "azure", field: "cpu.flags",
			sel: v1alpha1.Selector{CPU: &v1alpha1.CPUSelector{Flags: &v1alpha1.CPUFlags{NoneOf: []string{"avx512f"}}}},
			in:  []string{"Standard_D2ps_v5"}, out: []string{"Standard_D2s_v5"},
			missing: func(s *keeper.Server) bool { return len(s.CPUFlags) == 0 }},
		{name: "cpu.caches L2 per core", vendor: "hcloud", field: "cpu.caches",
			sel: v1alpha1.Selector{CPU: &v1alpha1.CPUSelector{Caches: &v1alpha1.CPUCaches{L2PerCoreMinKiB: new(1024.0)}}},
			in:  []string{"cpx22"}, out: []string{"ccx13"},
			missing: func(s *keeper.Server) bool { return s.CPUL2Cache == nil }},
		{name: "cpu.caches L1 and L3", vendor: "aws", field: "cpu.caches",
			sel: v1alpha1.Selector{CPU: &v1alpha1.CPUSelector{Caches: &v1alpha1.CPUCaches{
				L1dPerCoreMinKiB: new(48.0), L1iPerCoreMinKiB: new(32.0), L3MinMiB: new(64.0), L3TotalMinMiB: new(100.0)}}},
			in: []string{"m7i.large"}, out: []string{"m8g.large", "c7a.large"},
			missing: func(s *keeper.Server) bool { return s.CPUL3Cache == nil }},
		{name: "memory GiB (Hetzner GB)", vendor: "hcloud", field: "memory",
			sel: v1alpha1.Selector{Memory: &v1alpha1.MemorySelector{MinGiB: new(3.8), MaxGiB: new(8.0)}},
			in:  []string{"cax11", "cax21", "cpx22"}, out: []string{"cpx12", "cax31"}},
		{name: "memory per vCPU", vendor: "aws", field: "memory.perVcpu",
			sel: v1alpha1.Selector{Memory: &v1alpha1.MemorySelector{PerVCPU: &v1alpha1.GiBRange{MinGiB: new(4.0), MaxGiB: new(4.0)}}},
			in:  []string{"m7i.large", "m8g.large"}, out: []string{"c7a.large", "i4i.large"}},
		{name: "accelerator.count none", vendor: "aws", field: "accelerator.count",
			sel: v1alpha1.Selector{Accelerator: &v1alpha1.AcceleratorSelector{Count: &v1alpha1.FloatRange{Max: new(0.0)}}},
			in:  []string{"m7i.large"}, out: []string{"g6.xlarge"}},
		{name: "accelerator.types TPU (derived)", vendor: "gcp", field: "accelerator.types",
			sel: v1alpha1.Selector{Accelerator: &v1alpha1.AcceleratorSelector{Types: []v1alpha1.AcceleratorType{v1alpha1.AcceleratorTypeTPU}}},
			in:  []string{"ct5l-hightpu-1t"}, out: []string{"g2-standard-4", "n2-standard-2"}},
		{name: "accelerator.types GPU (derived)", vendor: "gcp", field: "accelerator.types",
			sel: v1alpha1.Selector{Accelerator: &v1alpha1.AcceleratorSelector{Types: []v1alpha1.AcceleratorType{v1alpha1.AcceleratorTypeGPU}}},
			in:  []string{"g2-standard-4"}, out: []string{"ct5l-hightpu-1t"}},
		{name: "accelerator.memoryPerDevice", vendor: "azure", field: "accelerator.memoryPerDevice",
			sel: v1alpha1.Selector{Accelerator: &v1alpha1.AcceleratorSelector{MemoryPerDevice: &v1alpha1.GiBMin{MinGiB: new(20.0)}}},
			in:  []string{"Standard_NC16ads_A10_v4", "Standard_NC24ads_A100_v4"}, out: []string{"Standard_D2s_v5", "Standard_NV6ads_A10_v5"},
			missing: func(s *keeper.Server) bool {
				return s.AcceleratorCount > 0 && (s.AcceleratorMemoryMin == nil || *s.AcceleratorMemoryMin == 0)
			}},
		{name: "accelerator.memoryTotal", vendor: "azure", field: "accelerator.memoryTotal",
			sel: v1alpha1.Selector{Accelerator: &v1alpha1.AcceleratorSelector{MemoryTotal: &v1alpha1.GiBMin{MinGiB: new(40.0)}}},
			in:  []string{"Standard_NV72ads_A10_v5", "Standard_NC24ads_A100_v4"}, out: []string{"Standard_NC16ads_A10_v4", "Standard_D2s_v5"},
			missing: func(s *keeper.Server) bool {
				return s.AcceleratorCount > 0 && (s.AcceleratorMemoryTotal == nil || *s.AcceleratorMemoryTotal == 0)
			}},
		{name: "accelerator.manufacturers", vendor: "azure", field: "accelerator.manufacturers",
			sel: v1alpha1.Selector{Accelerator: &v1alpha1.AcceleratorSelector{Manufacturers: []string{"NVIDIA"}}},
			in:  []string{"Standard_NC24ads_A100_v4"}, out: []string{"Standard_D2s_v5"},
			missing: func(s *keeper.Server) bool { return s.AcceleratorCount > 0 && s.AcceleratorManufacturer == nil }},
		{name: "accelerator.models", vendor: "gcp", field: "accelerator.models",
			sel: v1alpha1.Selector{Accelerator: &v1alpha1.AcceleratorSelector{Models: []string{"L4"}}},
			in:  []string{"g2-standard-4"}, out: []string{"ct5l-hightpu-1t", "n2-standard-2"}},
		{name: "accelerator.models missing", vendor: "azure", field: "accelerator.models",
			sel: v1alpha1.Selector{Accelerator: &v1alpha1.AcceleratorSelector{Models: []string{"A100"}}},
			in:  []string{"Standard_NC24ads_A100_v4"}, out: []string{"Standard_NC16ads_A10_v4"},
			missing: func(s *keeper.Server) bool { return s.AcceleratorCount > 0 && s.AcceleratorModel == nil }},
		{name: "network baseline", vendor: "aws", field: "network",
			sel: v1alpha1.Selector{Network: &v1alpha1.NetworkSelector{BaselineMinGbps: new(10.0)}},
			in:  []string{"i4i.32xlarge"}, out: []string{"m7i.large"}},
		{name: "network peak inclusive", vendor: "aws", field: "network",
			sel: v1alpha1.Selector{Network: &v1alpha1.NetworkSelector{PeakMinGbps: new(12.5)}},
			in:  []string{"m7i.large"}, out: []string{"t3.micro"}},
		{name: "network missing everywhere", vendor: "gcp", field: "network",
			sel:     v1alpha1.Selector{Network: &v1alpha1.NetworkSelector{BaselineMinGbps: new(1.0)}},
			missing: func(s *keeper.Server) bool { return s.NetworkSpeedBaseline == nil }},
		{name: "storage.local minGB", vendor: "aws", field: "storage.local",
			sel: v1alpha1.Selector{Storage: &v1alpha1.StorageSelector{Local: &v1alpha1.LocalStorageSelector{MinGB: new(200.0)}}},
			in:  []string{"i4i.large", "i4i.32xlarge"}, out: []string{"m7i.large"}},
		{name: "storage.local countMin 2", vendor: "aws", field: "storage.local",
			sel: v1alpha1.Selector{Storage: &v1alpha1.StorageSelector{Local: &v1alpha1.LocalStorageSelector{CountMin: new(int32(2))}}},
			in:  []string{"i4i.32xlarge"}, out: []string{"i4i.large", "m7i.large"}},
		{name: "storage.local countMin 1 without per-disk data", vendor: "hcloud", field: "storage.local",
			sel: v1alpha1.Selector{Storage: &v1alpha1.StorageSelector{Local: &v1alpha1.LocalStorageSelector{CountMin: new(int32(1))}}},
			in:  []string{"cax11", "ccx13"}},
		{name: "storage.local countMin 2 without per-disk data", vendor: "hcloud", field: "storage.local",
			sel:     v1alpha1.Selector{Storage: &v1alpha1.StorageSelector{Local: &v1alpha1.LocalStorageSelector{CountMin: new(int32(2))}}},
			missing: func(s *keeper.Server) bool { return s.StorageSize > 0 && len(s.Storages) == 0 }},
		{name: "price.maxHourly inclusive", vendor: "aws", field: "price.maxHourly",
			sel: v1alpha1.Selector{Price: &v1alpha1.PriceSelector{MaxHourly: "0.1008"}},
			in:  []string{"m7i.large", "t3.micro"}, out: []string{"i4i.large"}},
		{name: "price.maxHourlyPerVcpu", vendor: "hcloud", field: "price.maxHourlyPerVcpu",
			sel: v1alpha1.Selector{Price: &v1alpha1.PriceSelector{MaxHourlyPerVCPU: "0.02"}},
			in:  []string{"cax11", "cpx22"}, out: []string{"ccx13"}},
	}
}

func TestSelectors(t *testing.T) {
	for _, tc := range selectorCases() {
		t.Run(tc.name, func(t *testing.T) {
			sel := tc.sel
			res, cands := run(t, tc.vendor, specOf(&sel))
			all := byName(cands)
			selected := selectedSet(res)
			for _, n := range append(append([]string{}, tc.in...), tc.out...) {
				if all[n] == nil {
					t.Fatalf("snapshot lacks %s/%s", tc.vendor, n)
				}
			}
			for _, n := range tc.in {
				if _, ok := selected[n]; !ok {
					t.Errorf("%s: want selected, excluded as %+v", n, res.Misses[n])
				}
			}
			for _, n := range tc.out {
				if got := res.Misses[n].Reason; got != ReasonFilterPrefix+tc.field {
					t.Errorf("%s: want excluded by %s, got %q", n, tc.field, got)
				}
			}
			if tc.missing != nil {
				n := 0
				for _, c := range cands {
					if c.Architecture == "" || !tc.missing(&c.Server) {
						continue
					}
					n++
					if got := res.Misses[c.Name()].Reason; got != ReasonMissingData {
						t.Errorf("%s lacks data: want missingData, got %q", c.Name(), got)
					}
				}
				if n == 0 {
					t.Errorf("no %s row lacks the data; pick another vendor", tc.vendor)
				}
				if res.Excluded.MissingData < n {
					t.Errorf("missingData = %d, want >= %d", res.Excluded.MissingData, n)
				}
			}
			if len(tc.out) > 0 && res.Excluded.Filters[tc.field] < len(tc.out) {
				t.Errorf("filters.%s = %d, want >= %d", tc.field, res.Excluded.Filters[tc.field], len(tc.out))
			}
		})
	}
}

// Every selector at once still works and AND-s the filters (spot basis on
// the aws snapshot).
func TestSelectorsCombined(t *testing.T) {
	sel := &v1alpha1.Selector{
		InstanceTypes: &v1alpha1.GlobFilter{Exclude: []string{"*.metal*"}},
		Families:      &v1alpha1.GlobFilter{Include: []string{"m*", "c*", "r*"}},
		VCPUs:         &v1alpha1.IntRange{Min: new(int32(2)), Max: new(int32(16))},
		Architectures: []v1alpha1.Architecture{v1alpha1.ArchitectureAMD64, v1alpha1.ArchitectureARM64},
		CPU: &v1alpha1.CPUSelector{
			Manufacturers: []string{"AMD", "AWS", "Intel"},
			Allocations:   []v1alpha1.CPUAllocation{v1alpha1.CPUAllocationDedicated},
			Flags:         &v1alpha1.CPUFlags{NoneOf: []string{"amx_tile"}},
		},
		Memory:      &v1alpha1.MemorySelector{MinGiB: new(4.0), MaxGiB: new(64.0), PerVCPU: &v1alpha1.GiBRange{MinGiB: new(2.0), MaxGiB: new(8.0)}},
		Accelerator: &v1alpha1.AcceleratorSelector{Count: &v1alpha1.FloatRange{Min: new(0.0), Max: new(0.0)}},
		Price:       &v1alpha1.PriceSelector{Basis: v1alpha1.PriceBasisSpot, MaxHourly: "0.40", MaxHourlyPerVCPU: "0.05"},
	}
	res, cands := run(t, "aws", specOf(sel))
	if res.Basis != v1alpha1.PriceBasisSpot || len(res.InstanceTypes) == 0 {
		t.Fatalf("basis %s, %d types", res.Basis, len(res.InstanceTypes))
	}
	all := byName(cands)
	for _, it := range res.InstanceTypes {
		c := all[it.Name]
		p, _ := c.Price(keeper.AllocationSpot, 0)
		if c.VCPUs() < 2 || c.VCPUs() > 16 || p > 0.40 || p/float64(c.VCPUs()) > 0.05 || it.Price != p ||
			c.Server.AcceleratorCount != 0 || c.Server.CPUAllocation != "Dedicated" {
			t.Errorf("%s violates the selector: %+v", it.Name, c.Server)
		}
	}
}
