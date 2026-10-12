/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package catalog generates the instance-type catalogs of the KWOK lane (L3,
// docs/plans/v1/wip/00-overview.md §8.2) from the committed Spare Cores
// snapshot: one file per vendor in the format of core Karpenter's KWOK
// provider (INSTANCE_TYPES_FILE_PATH).
//
// A catalog emulates the label surface of the vendor's real Karpenter
// provider (§5.3, §5.4): zone and region label values in the provider's
// format, the provider's own label keys as offering requirements (KWOK stamps
// single-valued offering requirements on the NodeClaims and, from core v1.8,
// registers their keys as well-known), and sanitised label values. Prices are
// the snapshot's.
package catalog

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/SpareCores/sc-karpenter-nodepool/internal/keeper"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/keeper/keepertest"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/providers"
)

// MaxVCPUs bounds the size of the types in a catalog: bigger types only make
// the files (and Karpenter's scheduling) heavier without exercising anything
// else. The L3 scenario that needs types Karpenter does not know selects
// types above it.
const MaxVCPUs = 8

// Well-known keys written into offerings.
const (
	labelZone         = "topology.kubernetes.io/zone"
	labelRegion       = "topology.kubernetes.io/region"
	labelCapacityType = "karpenter.sh/capacity-type"
)

// Vendor describes how one Spare Cores vendor's catalog is emulated.
type Vendor struct {
	// Name is the Spare Cores vendor id (the snapshot directory).
	Name string
	// Adapter is the provider adapter the scenarios run with.
	Adapter providers.ID
	// RegionLabel: the provider stamps topology.kubernetes.io/region on its
	// nodes (so the offerings carry it).
	RegionLabel bool
	// Zone returns the topology.kubernetes.io/zone value of a Spare Cores
	// zone (the provider's format, §5.3), ok false to skip the zone.
	Zone func(z keeper.Zone, region keeper.Region) (string, bool)
	// Keys returns the provider's own label keys and values of one type in
	// one zone; values are sanitised by the generator.
	Keys func(s keeper.Server, z keeper.Zone) map[string]string
}

// Vendors returns the emulated vendors, one adapter each.
func Vendors() []Vendor {
	return []Vendor{
		{
			Name: "aws", Adapter: providers.AWS, RegionLabel: true,
			Zone: func(z keeper.Zone, _ keeper.Region) (string, bool) { return z.APIReference, true },
			Keys: func(s keeper.Server, z keeper.Zone) map[string]string {
				family, size, _ := strings.Cut(s.APIReference, ".")
				return map[string]string{
					"topology.k8s.aws/zone-id":              z.ZoneID,
					"karpenter.k8s.aws/instance-family":     family,
					"karpenter.k8s.aws/instance-size":       size,
					"karpenter.k8s.aws/instance-category":   strings.TrimRight(family, "0123456789"),
					"karpenter.k8s.aws/instance-generation": digits.FindString(family),
				}
			},
		},
		{
			Name: "azure", Adapter: providers.Azure, RegionLabel: true,
			// Zones are <region>-<n> (§5.4).
			Zone: func(z keeper.Zone, r keeper.Region) (string, bool) {
				return strings.ToLower(r.APIReference) + "-" + z.ZoneID, z.ZoneID != "0" && z.ZoneID != ""
			},
			Keys: func(s keeper.Server, _ keeper.Zone) map[string]string {
				sku := strings.TrimPrefix(s.APIReference, "Standard_")
				return map[string]string{
					"karpenter.azure.com/sku-name":   s.APIReference,
					"karpenter.azure.com/sku-family": firstLetter(sku),
					"karpenter.azure.com/sku-cpu":    strconv.Itoa(s.VCPUs),
				}
			},
		},
		{
			Name: "gcp", Adapter: providers.GCP, RegionLabel: true,
			Zone: func(z keeper.Zone, r keeper.Region) (string, bool) {
				// SC has odd zones such as us-central1-ai1a (§5.4).
				ok := z.APIReference != "" && strings.HasPrefix(z.APIReference, r.APIReference+"-") &&
					len(strings.TrimPrefix(z.APIReference, r.APIReference+"-")) == 1
				return z.APIReference, ok
			},
			Keys: func(s keeper.Server, _ keeper.Zone) map[string]string {
				family, _, _ := strings.Cut(s.APIReference, "-")
				return map[string]string{
					"karpenter.k8s.gcp/instance-family": family,
					"karpenter.k8s.gcp/instance-cpu":    strconv.Itoa(s.VCPUs),
				}
			},
		},
		{
			Name: "alicloud", Adapter: providers.AlicloudACK, RegionLabel: true,
			Zone: func(z keeper.Zone, _ keeper.Region) (string, bool) { return z.ZoneID, true },
			Keys: func(s keeper.Server, _ keeper.Zone) map[string]string {
				return map[string]string{
					"karpenter.alibabacloud.com/instance-family": s.Family,
					"karpenter.alibabacloud.com/instance-cpu":    strconv.Itoa(s.VCPUs),
				}
			},
		},
		{
			// Zone = location (the SC region alias), no region label (§5.3).
			Name: "hcloud", Adapter: providers.Hcloud,
			Zone: func(_ keeper.Zone, r keeper.Region) (string, bool) {
				if len(r.Aliases) == 0 {
					return "", false
				}
				return r.Aliases[0], true
			},
			Keys: func(s keeper.Server, _ keeper.Zone) map[string]string {
				return map[string]string{"karpenter.hetzner.cloud/server-family": s.Family}
			},
		},
		{
			// Only the five safe keys: the provider stamps no region and no
			// karpenter.ovhcloud.sh/* labels (§5.4).
			Name: "ovh", Adapter: providers.OVH,
			Zone: func(z keeper.Zone, _ keeper.Region) (string, bool) { return z.ZoneID, true },
		},
		{
			// Region and zone labels both carry the zone id (§5.4).
			Name: "upcloud", Adapter: providers.UpCloud, RegionLabel: true,
			Zone: func(z keeper.Zone, _ keeper.Region) (string, bool) { return z.ZoneID, true },
		},
		{
			// Region and zone labels both carry the region id (§5.4).
			Name: "vultr", Adapter: providers.Vultr, RegionLabel: true,
			Zone: func(_ keeper.Zone, r keeper.Region) (string, bool) { return r.RegionID, true },
			Keys: func(s keeper.Server, _ keeper.Zone) map[string]string {
				return map[string]string{"karpenter.vultr.com/plan-family": s.Family}
			},
		},
	}
}

// VendorByName returns the emulated vendor.
func VendorByName(name string) (Vendor, bool) {
	for _, v := range Vendors() {
		if v.Name == name {
			return v, true
		}
	}
	return Vendor{}, false
}

var digits = regexp.MustCompile(`[0-9]+`)

func firstLetter(s string) string {
	if s == "" {
		return ""
	}
	return s[:1]
}

// Requirement is a node selector requirement of an offering.
type Requirement struct {
	Key      string   `json:"key"`
	Operator string   `json:"operator"`
	Values   []string `json:"values"`
}

// Offering is one (zone, capacity type) offering of an instance type. The
// capitalised field names are KWOK's.
type Offering struct {
	Price        float64       `json:"Price"`
	Available    bool          `json:"Available"`
	Requirements []Requirement `json:"Requirements"`
}

// InstanceType is one entry of a KWOK instance-type file.
type InstanceType struct {
	Name             string            `json:"name"`
	Architecture     string            `json:"architecture"`
	OperatingSystems []string          `json:"operatingSystems"`
	Resources        map[string]string `json:"resources"`
	Offerings        []Offering        `json:"offerings"`
}

// Catalog is the generated catalog of one vendor and what a scenario needs to
// know about it.
type Catalog struct {
	Vendor Vendor
	// Region is the Kubernetes region value of the snapshot's test region:
	// the value to put on a node's topology.kubernetes.io/region label so
	// the controller detects the region (the template's NodeClass does not
	// exist in the test cluster).
	Region string
	// Zones are the zone label values of the offerings.
	Zones []string
	// CapacityTypes are the capacity types the offerings cover.
	CapacityTypes []string
	// Types are the instance types, sorted by name.
	Types []InstanceType
}

// InstanceTypeNames returns the names of the catalog's types.
func (c *Catalog) InstanceTypeNames() []string {
	out := make([]string, len(c.Types))
	for i, t := range c.Types {
		out[i] = t.Name
	}
	return out
}

// JSON encodes the catalog the way KWOK reads it.
func (c *Catalog) JSON() ([]byte, error) {
	return json.Marshal(c.Types)
}

var invalidLabelChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// SanitizeLabelValue turns an arbitrary string into a valid Kubernetes label
// value: invalid runs become "-", leading and trailing non-alphanumerics are
// trimmed, at most 63 characters remain.
func SanitizeLabelValue(v string) string {
	v = invalidLabelChars.ReplaceAllString(v, "-")
	v = strings.Trim(v, "-_.")
	if len(v) > 63 {
		v = strings.Trim(v[:63], "-_.")
	}
	return v
}

func readGzipJSON(path string, into any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if err := json.Unmarshal(b, into); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// placement is one zone of the test region and its label value.
type placement struct {
	zone  keeper.Zone
	label string
}

// tables reads the vendor's test region and the placements of its zones.
func tables(dir string, v Vendor, keeperRegion string) (keeper.Region, []placement, error) {
	var regions []keeper.Region
	var zones []keeper.Zone
	if err := readGzipJSON(filepath.Join(dir, "tables", "region.json.gz"), &regions); err != nil {
		return keeper.Region{}, nil, err
	}
	if err := readGzipJSON(filepath.Join(dir, "tables", "zone.json.gz"), &zones); err != nil {
		return keeper.Region{}, nil, err
	}
	var region keeper.Region
	for _, r := range regions {
		if r.VendorID == v.Name && r.RegionID == keeperRegion {
			region = r
		}
	}
	if region.RegionID == "" {
		return region, nil, fmt.Errorf("region %s of vendor %s is not in the snapshot region table", keeperRegion, v.Name)
	}
	var places []placement
	for _, z := range zones {
		if z.VendorID != v.Name || z.RegionID != keeperRegion || z.Status != "active" {
			continue
		}
		if label, ok := v.Zone(z, region); ok && label != "" {
			places = append(places, placement{z, label})
		}
	}
	if len(places) == 0 {
		// Vendors without zone rows (location = region): one pseudo zone.
		z := keeper.Zone{VendorID: v.Name, RegionID: keeperRegion}
		if label, ok := v.Zone(z, region); ok && label != "" {
			places = append(places, placement{z, label})
		}
	}
	if len(places) == 0 {
		return region, nil, fmt.Errorf("vendor %s has no zones in region %s", v.Name, keeperRegion)
	}
	return region, places, nil
}

// spotPrices reads the spot prices of the vendor's servers by name.
func spotPrices(dir string, v Vendor) (map[string]float64, error) {
	var spot []keeper.Server
	if err := readGzipJSON(filepath.Join(dir, v.Name, "servers-spot.json.gz"), &spot); err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for _, s := range spot {
		if s.Price != nil {
			out[s.APIReference] = *s.Price
		}
	}
	return out, nil
}

// keep reports whether a server belongs into a catalog: a plain, active type
// of an architecture providers launch, within the size bound.
func keep(s keeper.Server) bool {
	return architecture(s.CPUArchitecture) != "" && s.Price != nil && s.Status == "active" && s.AcceleratorCount == 0 &&
		s.VCPUs >= 1 && s.VCPUs <= MaxVCPUs && s.MemoryAmount > 0 && s.APIReference == SanitizeLabelValue(s.APIReference)
}

// Option changes how a catalog is generated.
type Option func(*options)

type options struct{ providerKeys bool }

// WithoutProviderKeys leaves the provider's own label keys out of the
// offerings. Core v1.2's KWOK provider does not register offering keys as
// well-known, and core treats an offering requirement on a key that is
// neither well-known nor in the NodePool as incompatible: every type would be
// filtered out. From v1.8 on the keys are registered (see
// test/kwok/core-versions.txt).
func WithoutProviderKeys() Option { return func(o *options) { o.providerKeys = false } }

// offering builds one offering: the capacity type and zone, the region when
// the provider stamps it, and (unless left out) the provider's own keys.
func (v Vendor) offering(s keeper.Server, p placement, region string, cp capacityPrice, providerKeys bool) Offering {
	reqs := []Requirement{
		{Key: labelCapacityType, Operator: "In", Values: []string{cp.capacity}},
		{Key: labelZone, Operator: "In", Values: []string{SanitizeLabelValue(p.label)}},
	}
	if v.RegionLabel {
		reqs = append(reqs, Requirement{Key: labelRegion, Operator: "In", Values: []string{SanitizeLabelValue(region)}})
	}
	if v.Keys != nil && providerKeys {
		keys := v.Keys(s, p.zone)
		names := make([]string, 0, len(keys))
		for k := range keys {
			names = append(names, k)
		}
		slices.Sort(names)
		for _, k := range names {
			if val := SanitizeLabelValue(keys[k]); val != "" {
				reqs = append(reqs, Requirement{Key: k, Operator: "In", Values: []string{val}})
			}
		}
	}
	return Offering{Price: cp.price, Available: true, Requirements: reqs}
}

// Generate builds the catalog of a vendor from the snapshot in dir.
func Generate(dir string, v Vendor, opts ...Option) (*Catalog, error) {
	o := options{providerKeys: true}
	for _, opt := range opts {
		opt(&o)
	}
	m, err := keepertest.LoadManifest(dir)
	if err != nil {
		return nil, err
	}
	info, ok := m.Vendors[v.Name]
	if !ok {
		return nil, fmt.Errorf("vendor %s is not in the snapshot manifest", v.Name)
	}
	keeperRegion, ok := m.KeeperRegion(v.Name)
	if !ok {
		return nil, fmt.Errorf("no /servers query of vendor %s in the snapshot manifest", v.Name)
	}
	_, places, err := tables(dir, v, keeperRegion)
	if err != nil {
		return nil, err
	}
	var onDemand []keeper.Server
	if err := readGzipJSON(filepath.Join(dir, v.Name, "servers-ondemand.json.gz"), &onDemand); err != nil {
		return nil, err
	}
	spot := map[string]float64{}
	if info.Spot {
		if spot, err = spotPrices(dir, v); err != nil {
			return nil, err
		}
	}

	// info.Region is the Kubernetes region value of the test region.
	cat := &Catalog{Vendor: v, Region: info.Region}
	capacity := map[string]bool{}
	for _, s := range onDemand {
		if !keep(s) {
			continue
		}
		it := InstanceType{
			Name:             s.APIReference,
			Architecture:     architecture(s.CPUArchitecture),
			OperatingSystems: []string{"linux"},
			Resources: map[string]string{
				"cpu":    strconv.Itoa(s.VCPUs),
				"memory": strconv.FormatFloat(s.MemoryAmount, 'f', -1, 64) + "Mi",
			},
		}
		prices := []capacityPrice{{providers.CapacityTypeOnDemand, *s.Price}}
		if sp, ok := spot[s.APIReference]; ok {
			prices = append(prices, capacityPrice{providers.CapacityTypeSpot, sp})
		}
		for _, p := range places {
			for _, cp := range prices {
				it.Offerings = append(it.Offerings, v.offering(s, p, info.Region, cp, o.providerKeys))
				capacity[cp.capacity] = true
			}
		}
		cat.Types = append(cat.Types, it)
	}
	if len(cat.Types) == 0 {
		return nil, fmt.Errorf("vendor %s: no instance types left for the catalog", v.Name)
	}
	slices.SortFunc(cat.Types, func(a, b InstanceType) int { return strings.Compare(a.Name, b.Name) })
	for _, p := range places {
		cat.Zones = append(cat.Zones, SanitizeLabelValue(p.label))
	}
	slices.Sort(cat.Zones)
	cat.Zones = slices.Compact(cat.Zones)
	cat.CapacityTypes = slices.Sorted(maps.Keys(capacity))
	return cat, nil
}

type capacityPrice struct {
	capacity string
	price    float64
}

// architecture maps a Spare Cores cpu_architecture to a Kubernetes one; "" for
// architectures Karpenter providers do not launch.
func architecture(sc string) string {
	switch sc {
	case "x86_64":
		return "amd64"
	case "arm64":
		return "arm64"
	}
	return ""
}

// WriteAll generates the catalog of every vendor into dir/<vendor>.json and
// returns them by vendor name.
func WriteAll(snapshotDir, dir string, opts ...Option) (map[string]*Catalog, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	out := map[string]*Catalog{}
	for _, v := range Vendors() {
		c, err := Generate(snapshotDir, v, opts...)
		if err != nil {
			return nil, fmt.Errorf("vendor %s: %w", v.Name, err)
		}
		b, err := c.JSON()
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, v.Name+".json"), b, 0o644); err != nil {
			return nil, err
		}
		out[v.Name] = c
	}
	return out, nil
}
