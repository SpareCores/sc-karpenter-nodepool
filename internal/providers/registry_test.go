/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package providers

import (
	"errors"
	"strings"
	"testing"
)

func TestDetect(t *testing.T) {
	cases := []struct {
		group, kind, version string
		want                 ID
		vendor               string
	}{
		{"karpenter.k8s.aws", "EC2NodeClass", "v1", AWS, "aws"},
		{"eks.amazonaws.com", "NodeClass", "v1", AWSAutoMode, "aws"},
		{"karpenter.azure.com", "AKSNodeClass", "v1beta1", Azure, "azure"},
		{"karpenter.k8s.gcp", "GCENodeClass", "v1alpha1", GCP, "gcp"},
		{"karpenter.alibabacloud.com", "ECSNodeClass", "v1alpha1", AlicloudACK, "alicloud"},
		{"karpenter.k8s.alibabacloud", "ECSNodeClass", "v1alpha1", AlicloudCloudpilot, "alicloud"},
		{"karpenter.hetzner.cloud", "HCloudNodeClass", "v1", Hcloud, "hcloud"},
		{"karpenter.itsh.dev", "HCloudNodeClass", "v1alpha1", HcloudItsh, "hcloud"},
		{"karpenter.k8s.upcloud", "UpCloudNodeClass", "v1alpha1", UpCloud, "upcloud"},
		{"karpenter.ovhcloud.sh", "OVHNodeClass", "v1alpha1", OVH, "ovh"},
		{"karpenter.vultr.com", "VultrNodeClass", "v1alpha1", Vultr, "vultr"},
	}
	seen := map[ID]bool{}
	for _, c := range cases {
		a, err := registry.Detect(c.group, c.kind, c.version)
		if err != nil {
			t.Errorf("%s/%s: %v", c.group, c.kind, err)
			continue
		}
		if a.ID() != c.want || a.Vendor() != c.vendor {
			t.Errorf("%s/%s: got %s (%s), want %s (%s)", c.group, c.kind, a.ID(), a.Vendor(), c.want, c.vendor)
		}
		if g, k := a.NodeClass(); g != c.group || k != c.kind {
			t.Errorf("%s: NodeClass() = %s/%s", c.want, g, k)
		}
		seen[a.ID()] = true
		if got, ok := registry.Get(c.want); !ok || got.ID() != c.want {
			t.Errorf("Get(%s) = %v, %v", c.want, got, ok)
		}
	}
	if all := registry.All(); len(all) != len(cases) || len(seen) != len(cases) {
		t.Errorf("%d adapters, %d detected, %d cases", len(all), len(seen), len(cases))
	}
	if _, ok := registry.Get("nope"); ok {
		t.Error("Get of an unknown id succeeded")
	}
}

func TestDetectUnsupported(t *testing.T) {
	cases := []struct {
		name, group, kind, version, msg string
	}{
		{"upcloud-tools serves v1alpha2", "karpenter.k8s.upcloud", "UpCloudNodeClass", "v1alpha2", "v1alpha2"},
		{"unknown upcloud version", "karpenter.k8s.upcloud", "UpCloudNodeClass", "", "supports only [v1alpha1]"},
		{"unknown group", "karpenter.example.com", "FooNodeClass", "v1", "karpenter.example.com"},
		{"known group, other kind", "karpenter.k8s.aws", "NodeClass", "v1", "NodeClass"},
		{"Auto Mode kind under the AWS group", "eks.amazonaws.com", "EC2NodeClass", "v1", "EC2NodeClass"},
		{"case matters", "Karpenter.k8s.aws", "EC2NodeClass", "v1", "Karpenter.k8s.aws"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, err := registry.Detect(c.group, c.kind, c.version)
			if !errors.Is(err, ErrUnsupportedProvider) || a != nil {
				t.Fatalf("got %v, %v", a, err)
			}
			if !strings.Contains(err.Error(), c.msg) {
				t.Errorf("message %q lacks %q", err, c.msg)
			}
		})
	}
}
