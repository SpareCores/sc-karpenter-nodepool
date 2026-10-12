/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package providers

import (
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/SpareCores/sc-karpenter-nodepool/api/v1alpha1"
)

// The NodeClass field paths of overview §6.3, checked against the provider
// commits of §5.2. NodeClasses are read as unstructured objects so that any
// served version works. Paths are dotted (status.subnets); no field name on
// them contains a dot.

// nodeClassRegions extracts Kubernetes region values from a NodeClass.
type nodeClassRegions func(nc *unstructured.Unstructured, vendor string, zones *ZoneIndex) []string

// stringField reads a string at path.
func stringField(nc *unstructured.Unstructured, path string) string {
	if nc == nil {
		return ""
	}
	v, _, _ := unstructured.NestedString(nc.Object, strings.Split(path, ".")...)
	return v
}

// stringList reads a list of strings at path.
func stringList(nc *unstructured.Unstructured, path string) []string {
	if nc == nil {
		return nil
	}
	raw, _, _ := unstructured.NestedSlice(nc.Object, strings.Split(path, ".")...)
	var out []string
	for _, v := range raw {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// listFields reads, for every object of the list at path, the first
// non-empty string among fields (in order of preference).
func listFields(nc *unstructured.Unstructured, path string, fields ...string) []string {
	if nc == nil {
		return nil
	}
	raw, _, _ := unstructured.NestedSlice(nc.Object, strings.Split(path, ".")...)
	var out []string
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		for _, f := range fields {
			if s, ok := m[f].(string); ok && s != "" {
				out = append(out, s)
				break
			}
		}
	}
	return out
}

// zonesToRegions maps zone values to their Spare Cores region ids, which are
// the Kubernetes region values for the vendors that use it (AWS, Alibaba).
// Zones missing from the table are ignored; regions are never derived by
// string manipulation.
func zonesToRegions(vendor string, zoneValues []string, zones *ZoneIndex) []string {
	var out []string
	for _, z := range zoneValues {
		if r, ok := zones.RegionOf(vendor, z); ok {
			out = append(out, r)
		}
	}
	return dedupe(out)
}

// fromListZones: zones in a status list (AWS subnets, Alibaba vSwitches).
func fromListZones(path string, fields ...string) nodeClassRegions {
	return func(nc *unstructured.Unstructured, vendor string, zones *ZoneIndex) []string {
		return zonesToRegions(vendor, listFields(nc, path, fields...), zones)
	}
}

// fromStringList: region values listed verbatim (Hetzner spec.locations).
func fromStringList(path string) nodeClassRegions {
	return func(nc *unstructured.Unstructured, _ string, _ *ZoneIndex) []string {
		return dedupe(stringList(nc, path))
	}
}

// fromListNames: region values in a resolved status list (itsh
// status.locations[].name, UpCloud status.zones[].id).
func fromListNames(path, field string) nodeClassRegions {
	return func(nc *unstructured.Unstructured, _ string, _ *ZoneIndex) []string {
		return dedupe(listFields(nc, path, field))
	}
}

// fromString: a single region value (Vultr and OVH spec.region).
func fromString(path string) nodeClassRegions {
	return func(nc *unstructured.Unstructured, _ string, _ *ZoneIndex) []string {
		if s := stringField(nc, path); s != "" {
			return []string{s}
		}
		return nil
	}
}

// hcloudResolvedArchitectures reads status.resolvedImages[].architecture of
// a Hetzner NodeClass: stubbi spells it x86/arm, echohello amd64/arm64.
func hcloudResolvedArchitectures(nc *unstructured.Unstructured) ([]v1alpha1.Architecture, bool) {
	var out []v1alpha1.Architecture
	for _, a := range listFields(nc, "status.resolvedImages", "architecture") {
		switch a {
		case "x86", "amd64":
			out = append(out, v1alpha1.ArchitectureAMD64)
		case "arm", "arm64":
			out = append(out, v1alpha1.ArchitectureARM64)
		}
	}
	out = dedupe(out)
	return out, len(out) > 0
}
