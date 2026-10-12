/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog

import (
	"encoding/json"
	"regexp"
	"slices"
	"testing"

	"github.com/SpareCores/sc-karpenter-nodepool/internal/keeper/keepertest"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/providers"
)

var labelValue = regexp.MustCompile(`^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$`)

func TestSanitizeLabelValue(t *testing.T) {
	for in, want := range map[string]string{
		"General Purpose":      "General-Purpose",
		"cax":                  "cax",
		" Cloud  Native (v2) ": "Cloud-Native-v2",
		"-._x_.-":              "x",
		"":                     "",
	} {
		if got := SanitizeLabelValue(in); got != want {
			t.Errorf("SanitizeLabelValue(%q) = %q, want %q", in, got, want)
		}
	}
	long := SanitizeLabelValue(string(make([]byte, 100)) + "a")
	if len(long) > 63 || !labelValue.MatchString(long) {
		t.Errorf("long value not truncated into a valid one: %q", long)
	}
}

// checkType checks one instance type of a catalog.
func checkType(t *testing.T, v Vendor, c *Catalog, it InstanceType) {
	t.Helper()
	if it.Architecture != "amd64" && it.Architecture != "arm64" {
		t.Errorf("%s: architecture %q", it.Name, it.Architecture)
	}
	if len(it.Offerings) < len(c.Zones) {
		t.Errorf("%s: %d offerings for %d zones", it.Name, len(it.Offerings), len(c.Zones))
	}
	for _, o := range it.Offerings {
		keys := map[string]bool{}
		for _, r := range o.Requirements {
			keys[r.Key] = true
			if r.Operator != "In" || len(r.Values) != 1 || !labelValue.MatchString(r.Values[0]) || r.Values[0] == "" {
				t.Errorf("%s: bad requirement %+v", it.Name, r)
			}
		}
		if !keys[labelZone] || !keys[labelCapacityType] || keys[labelRegion] != v.RegionLabel {
			t.Errorf("%s: offering keys %v (region label wanted: %v)", it.Name, keys, v.RegionLabel)
		}
		if o.Price <= 0 || !o.Available {
			t.Errorf("%s: offering %+v", it.Name, o)
		}
	}
}

// TestGenerate checks every vendor's catalog against the committed snapshot.
func TestGenerate(t *testing.T) {
	registry := providers.NewRegistry()
	for _, v := range Vendors() {
		t.Run(v.Name, func(t *testing.T) {
			a, ok := registry.Get(v.Adapter)
			if !ok || a.Vendor() != v.Name {
				t.Fatalf("adapter %s does not belong to vendor %s", v.Adapter, v.Name)
			}
			c, err := Generate(keepertest.SnapshotDir(), v)
			if err != nil {
				t.Fatal(err)
			}
			if len(c.Types) == 0 || len(c.Zones) == 0 || len(c.CapacityTypes) == 0 || c.Region == "" {
				t.Fatalf("empty catalog: %d types, zones %v, capacity types %v, region %q",
					len(c.Types), c.Zones, c.CapacityTypes, c.Region)
			}
			if !slices.Contains(c.CapacityTypes, providers.CapacityTypeOnDemand) {
				t.Errorf("no on-demand offerings: %v", c.CapacityTypes)
			}
			names := c.InstanceTypeNames()
			if !slices.IsSorted(names) || len(slices.Compact(slices.Clone(names))) != len(names) {
				t.Error("types are not sorted and unique")
			}
			for _, it := range c.Types {
				checkType(t, v, c, it)
			}
			raw, err := c.JSON()
			if err != nil {
				t.Fatal(err)
			}
			var back []InstanceType
			if err := json.Unmarshal(raw, &back); err != nil || len(back) != len(c.Types) {
				t.Fatalf("catalog does not round-trip: %d types, %v", len(back), err)
			}
		})
	}
}

// TestWithoutProviderKeys: the catalogs of core v1.2 carry only well-known
// keys.
func TestWithoutProviderKeys(t *testing.T) {
	wellKnown := []string{labelZone, labelRegion, labelCapacityType}
	for _, v := range Vendors() {
		c, err := Generate(keepertest.SnapshotDir(), v, WithoutProviderKeys())
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range c.Types {
			for _, o := range it.Offerings {
				for _, r := range o.Requirements {
					if !slices.Contains(wellKnown, r.Key) {
						t.Fatalf("%s/%s: key %s is not well-known", v.Name, it.Name, r.Key)
					}
				}
			}
		}
	}
}

// TestTypesAboveTheCatalogExist: the L3 scenario about instance types core
// Karpenter does not know selects types above MaxVCPUs, which every vendor
// must have in Spare Cores (so the selection is not empty).
func TestTypesAboveTheCatalogExist(t *testing.T) {
	for _, v := range Vendors() {
		var servers []struct {
			VCPUs int `json:"vcpus"`
		}
		if err := readGzipJSON(keepertest.SnapshotDir()+"/"+v.Name+"/servers-ondemand.json.gz", &servers); err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(servers, func(s struct {
			VCPUs int `json:"vcpus"`
		}) bool {
			return s.VCPUs > MaxVCPUs
		}) {
			t.Errorf("%s has no type above %d vCPUs in the snapshot", v.Name, MaxVCPUs)
		}
	}
}
