/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package providers

import (
	"slices"
	"strings"

	"github.com/SpareCores/sc-karpenter-nodepool/internal/keeper"
)

// RegionIndex looks up the Spare Cores region table (/table/region) per
// vendor. Build it once per refresh with NewRegionIndex.
type RegionIndex struct {
	byVendor map[string][]keeper.Region
}

// NewRegionIndex indexes a region table.
func NewRegionIndex(regions []keeper.Region) *RegionIndex {
	ix := &RegionIndex{byVendor: map[string][]keeper.Region{}}
	for _, r := range regions {
		ix.byVendor[r.VendorID] = append(ix.byVendor[r.VendorID], r)
	}
	return ix
}

// find returns the first region of the vendor whose key matches value:
// exactly if possible, else case-insensitively (OVH's cloud-controller-manager
// writes GRA11, a NodeClass may say gra11).
func (ix *RegionIndex) find(vendor, value string, key func(keeper.Region) []string) (keeper.Region, bool) {
	if ix == nil || value == "" {
		return keeper.Region{}, false
	}
	rs := ix.byVendor[vendor]
	for _, fold := range []bool{false, true} {
		for _, r := range rs {
			for _, k := range key(r) {
				if k == value || (fold && strings.EqualFold(k, value)) {
					return r, true
				}
			}
		}
	}
	return keeper.Region{}, false
}

// ByRegionID finds a region by its Spare Cores region_id.
func (ix *RegionIndex) ByRegionID(vendor, id string) (keeper.Region, bool) {
	return ix.find(vendor, id, func(r keeper.Region) []string { return []string{r.RegionID} })
}

// ByAPIReference finds a region by its api_reference (Azure westeurope, GCP
// us-central1).
func (ix *RegionIndex) ByAPIReference(vendor, ref string) (keeper.Region, bool) {
	return ix.find(vendor, ref, func(r keeper.Region) []string { return []string{r.APIReference} })
}

// ByAlias finds a region by one of its aliases (Hetzner location fsn1).
func (ix *RegionIndex) ByAlias(vendor, alias string) (keeper.Region, bool) {
	return ix.find(vendor, alias, func(r keeper.Region) []string { return r.Aliases })
}

// ZoneIndex looks up the Spare Cores zone table (/table/zone) per vendor.
// Build it once per refresh with NewZoneIndex.
type ZoneIndex struct {
	byVendor map[string][]keeper.Zone
}

// NewZoneIndex indexes a zone table.
func NewZoneIndex(zones []keeper.Zone) *ZoneIndex {
	ix := &ZoneIndex{byVendor: map[string][]keeper.Zone{}}
	for _, z := range zones {
		ix.byVendor[z.VendorID] = append(ix.byVendor[z.VendorID], z)
	}
	return ix
}

// RegionOf returns the Spare Cores region_id of a zone, matched on zone_id
// first (AWS AZ ID use1-az1, Alibaba eu-central-1a) and on api_reference
// second (AWS zone name us-east-1a). Zone names are account-specific on AWS,
// but every name still lies in the same region, which is all this needs.
func (ix *ZoneIndex) RegionOf(vendor, zone string) (string, bool) {
	if ix == nil || zone == "" {
		return "", false
	}
	zs := ix.byVendor[vendor]
	for _, z := range zs {
		if z.ZoneID == zone {
			return z.RegionID, true
		}
	}
	for _, z := range zs {
		if z.APIReference == zone {
			return z.RegionID, true
		}
	}
	return "", false
}

// ZoneIDs returns the sorted zone_ids of the given Spare Cores regions.
func (ix *ZoneIndex) ZoneIDs(vendor string, regionIDs []string) []string {
	if ix == nil {
		return nil
	}
	var out []string
	for _, z := range ix.byVendor[vendor] {
		if slices.Contains(regionIDs, z.RegionID) {
			out = append(out, z.ZoneID)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}
