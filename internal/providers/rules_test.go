/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package providers

import (
	"errors"
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/SpareCores/sc-karpenter-nodepool/api/v1alpha1"
)

var (
	amd64 = v1alpha1.ArchitectureAMD64
	arm64 = v1alpha1.ArchitectureARM64
)

// TestRequirementRules is the §6.4/§6.5 table.
func TestRequirementRules(t *testing.T) {
	cases := []struct {
		id               ID
		allowed          []string
		single           bool
		archs            []v1alpha1.Architecture
		singleArch       bool
		forceZones       bool
		warnZone         bool
		neverEmitOS      bool
		templateBothOK   bool // template allowing on-demand and spot
		templateSpotOK   bool
		templateReserved bool
	}{
		{id: AWS, allowed: awsCapacity, archs: bothArchs,
			templateBothOK: true, templateSpotOK: true, templateReserved: true},
		{id: AWSAutoMode, allowed: awsCapacity, archs: bothArchs, neverEmitOS: true,
			templateBothOK: true, templateSpotOK: true, templateReserved: true},
		{id: Azure, allowed: onDemandSpot, archs: bothArchs, templateBothOK: true, templateSpotOK: true},
		{id: GCP, allowed: onDemandSpot, archs: bothArchs, templateBothOK: true, templateSpotOK: true},
		{id: AlicloudACK, allowed: onDemandSpot, single: true, archs: bothArchs, templateSpotOK: true},
		{id: AlicloudCloudpilot, allowed: onDemandSpot, archs: bothArchs, singleArch: true, warnZone: true,
			templateBothOK: true, templateSpotOK: true},
		{id: Hcloud, allowed: onDemandOnly, archs: bothArchs},
		{id: HcloudItsh, allowed: onDemandOnly, archs: amd64Only},
		{id: UpCloud, allowed: onDemandOnly, archs: amd64Only},
		{id: OVH, allowed: onDemandOnly, archs: amd64Only, forceZones: true},
		{id: Vultr, allowed: onDemandOnly, archs: amd64Only},
	}
	for _, c := range cases {
		t.Run(string(c.id), func(t *testing.T) {
			r := mustAdapter(t, c.id).RequirementRules()
			if r.DefaultCapacityType != CapacityTypeOnDemand {
				t.Errorf("default capacity type %q", r.DefaultCapacityType)
			}
			if !slices.Equal(r.AllowedCapacityTypes, c.allowed) || r.SingleCapacityType != c.single {
				t.Errorf("capacity types %v single %v", r.AllowedCapacityTypes, r.SingleCapacityType)
			}
			if !slices.Equal(r.AllowedArchitectures, c.archs) || r.SingleArchitecture != c.singleArch {
				t.Errorf("architectures %v single %v", r.AllowedArchitectures, r.SingleArchitecture)
			}
			if r.ForceZones != c.forceZones || r.WarnOnZoneRequirement != c.warnZone {
				t.Errorf("force zones %v warn %v", r.ForceZones, r.WarnOnZoneRequirement)
			}
			if slices.Contains(r.NeverEmit, LabelOS) != c.neverEmitOS || (!c.neverEmitOS && len(r.NeverEmit) > 0) {
				t.Errorf("never emit %v", r.NeverEmit)
			}

			if got, err := r.ResolveCapacityTypes(nil); err != nil || !slices.Equal(got, []string{CapacityTypeOnDemand}) {
				t.Errorf("no template: %v %v", got, err)
			}
			if got, err := r.ResolveCapacityTypes([]string{CapacityTypeOnDemand}); err != nil || len(got) != 1 {
				t.Errorf("on-demand template: %v %v", got, err)
			}
			check := func(name string, template []string, ok bool) {
				got, err := r.ResolveCapacityTypes(template)
				if ok && (err != nil || !slices.Equal(got, template)) {
					t.Errorf("%s: %v %v", name, got, err)
				}
				if !ok && !errors.Is(err, ErrCapacityTypeUnsupported) {
					t.Errorf("%s: accepted (%v)", name, got)
				}
			}
			check("spot", []string{CapacityTypeSpot}, c.templateSpotOK)
			check("both", []string{CapacityTypeSpot, CapacityTypeOnDemand}, c.templateBothOK)
			check("reserved", []string{CapacityTypeReserved}, c.templateReserved)
			check("empty", []string{}, false)
			check("unknown", []string{"preemptible"}, false)
		})
	}
}

func TestRulesAreCopies(t *testing.T) {
	a := mustAdapter(t, AWSAutoMode)
	r := a.RequirementRules()
	r.AllowedCapacityTypes[0] = "x"
	r.NeverEmit[0] = "x"
	r.AllowedArchitectures[0] = "x"
	if r2 := a.RequirementRules(); r2.AllowedCapacityTypes[0] != CapacityTypeOnDemand || r2.NeverEmit[0] != LabelOS || r2.AllowedArchitectures[0] != amd64 {
		t.Errorf("rules were modified through a copy: %+v", r2)
	}
}

func TestResolveArchitectures(t *testing.T) {
	both := []v1alpha1.Architecture{arm64, amd64, arm64}
	cases := []struct {
		id          ID
		template    []v1alpha1.Architecture
		selected    []v1alpha1.Architecture
		want        []v1alpha1.Architecture
		emit        bool
		unsupported bool
	}{
		{id: AWS, selected: both, want: bothArchs, emit: true},
		{id: AWS, template: []v1alpha1.Architecture{arm64}, selected: both, want: []v1alpha1.Architecture{arm64}, emit: true},
		{id: AWS, selected: nil, want: nil, emit: false},
		// EKS Auto Mode stamps kubernetes.io/arch like AWS; only
		// kubernetes.io/os is never emitted.
		{id: AWSAutoMode, selected: both, want: bothArchs, emit: true},
		{id: AWSAutoMode, template: []v1alpha1.Architecture{amd64}, selected: both, want: []v1alpha1.Architecture{amd64}, emit: true},
		{id: AlicloudCloudpilot, selected: []v1alpha1.Architecture{arm64}, want: []v1alpha1.Architecture{arm64}, emit: true},
		{id: AlicloudCloudpilot, selected: both, unsupported: true},
		{id: AlicloudCloudpilot, template: []v1alpha1.Architecture{amd64, arm64}, unsupported: true},
		{id: AlicloudCloudpilot, template: []v1alpha1.Architecture{amd64}, selected: both, want: []v1alpha1.Architecture{amd64}, emit: true},
		{id: AlicloudACK, selected: both, want: bothArchs, emit: true},
		{id: HcloudItsh, template: []v1alpha1.Architecture{arm64}, unsupported: true},
		{id: Vultr, selected: []v1alpha1.Architecture{amd64}, want: amd64Only, emit: true},
	}
	for i, c := range cases {
		got, emit, err := mustAdapter(t, c.id).RequirementRules().ResolveArchitectures(c.template, c.selected)
		if c.unsupported {
			if !errors.Is(err, ErrArchitectureUnsupported) {
				t.Errorf("case %d (%s): got %v %v %v", i, c.id, got, emit, err)
			}
			continue
		}
		if err != nil || emit != c.emit || !slices.Equal(got, c.want) {
			t.Errorf("case %d (%s): got %v %v %v, want %v %v", i, c.id, got, emit, err, c.want, c.emit)
		}
	}
}

func TestForcedZones(t *testing.T) {
	ovh := mustAdapter(t, OVH)
	if got := ForcedZones(ovh, []string{"GRA11"}, zoneIx); !slices.Equal(got, []string{"gra11-a"}) {
		t.Errorf("GRA11: %v", got)
	}
	if got := ForcedZones(ovh, []string{"EU-WEST-PAR"}, zoneIx); !slices.Equal(got, []string{"eu-west-par-a", "eu-west-par-b", "eu-west-par-c"}) {
		t.Errorf("EU-WEST-PAR: %v", got)
	}
	if got := ForcedZones(ovh, []string{"NOPE"}, zoneIx); len(got) != 0 {
		t.Errorf("unknown region: %v", got)
	}
	for _, a := range registry.All() {
		if a.ID() == OVH {
			continue
		}
		region, _ := manifest.KeeperRegion(a.Vendor())
		if got := ForcedZones(a, []string{region}, zoneIx); got != nil {
			t.Errorf("%s forces zones %v", a.ID(), got)
		}
	}
}

func TestArchitecturesFromNodeClass(t *testing.T) {
	nc := func(archs ...string) *unstructured.Unstructured {
		images := make([]any, 0, len(archs))
		for _, a := range archs {
			images = append(images, map[string]any{"architecture": a, "imageID": int64(1)})
		}
		return &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"resolvedImages": images}}}
	}
	hcloud := mustAdapter(t, Hcloud)
	cases := []struct {
		nc   *unstructured.Unstructured
		want []v1alpha1.Architecture
		ok   bool
	}{
		{nc("x86", "arm"), bothArchs, true},                 // stubbi spelling
		{nc("arm64"), []v1alpha1.Architecture{arm64}, true}, // echohello spelling
		{nc("amd64", "x86"), amd64Only, true},
		{nc("riscv"), nil, false},
		{nc(), nil, false},
		{nil, nil, false},
	}
	for i, c := range cases {
		got, ok := hcloud.ArchitecturesFromNodeClass(c.nc)
		if ok != c.ok || !slices.Equal(got, c.want) {
			t.Errorf("case %d: %v %v", i, got, ok)
		}
	}
	if _, ok := mustAdapter(t, AWS).ArchitecturesFromNodeClass(nc("x86")); ok {
		t.Error("aws reads resolved images")
	}
}
