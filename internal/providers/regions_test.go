/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package providers

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/SpareCores/sc-karpenter-nodepool/api/v1alpha1"
)

// fakeReader serves one NodeClass and label sets of NodeClaims and nodes.
type fakeReader struct {
	nodeClass  *unstructured.Unstructured
	nodeClaims []map[string]string
	nodes      []map[string]string
	err        error
	calls      []string
}

func matches(labels, selector map[string]string) bool {
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

func filter(sets []map[string]string, selector map[string]string) []map[string]string {
	var out []map[string]string
	for _, l := range sets {
		if matches(l, selector) {
			out = append(out, l)
		}
	}
	return out
}

func (f *fakeReader) NodeClass(_ context.Context, ref NodeClassRef) (*unstructured.Unstructured, error) {
	f.calls = append(f.calls, "NodeClass "+ref.Name)
	return f.nodeClass, f.err
}

func (f *fakeReader) NodeClaimLabels(_ context.Context, selector map[string]string) ([]map[string]string, error) {
	f.calls = append(f.calls, "NodeClaims")
	return filter(f.nodeClaims, selector), nil
}

func (f *fakeReader) NodeLabels(_ context.Context, selector map[string]string) ([]map[string]string, error) {
	if selector == nil {
		f.calls = append(f.calls, "AllNodes")
	} else {
		f.calls = append(f.calls, "Nodes")
	}
	return filter(f.nodes, selector), nil
}

func obj(o map[string]any) *unstructured.Unstructured { return &unstructured.Unstructured{Object: o} }

func list(items ...map[string]any) []any {
	out := make([]any, len(items))
	for i, it := range items {
		out[i] = it
	}
	return out
}

// nodeClasses are synthetic NodeClasses per adapter, using the field paths
// of overview §6.3.
var nodeClasses = map[ID]*unstructured.Unstructured{
	AWS: obj(map[string]any{"status": map[string]any{"subnets": list(
		map[string]any{"id": "subnet-1", "zone": "us-east-1a", "zoneID": "use1-az1"},
		map[string]any{"id": "subnet-2", "zone": "us-east-1b"},
	)}}),
	AWSAutoMode: obj(map[string]any{"status": map[string]any{"subnets": list(
		map[string]any{"id": "subnet-3", "zone": "us-east-1c", "zoneID": "use1-az6"},
	)}}),
	Azure: obj(map[string]any{"spec": map[string]any{"imageFamily": "Ubuntu2204"}}),
	GCP:   obj(map[string]any{"spec": map[string]any{"imageSelectorTerms": list(map[string]any{"alias": "ContainerOptimizedOS@latest"})}}),
	AlicloudACK: obj(map[string]any{"status": map[string]any{"vSwitches": list(
		map[string]any{"id": "vsw-1", "zone": "eu-central-1a"},
		map[string]any{"id": "vsw-2", "zone": "x", "zoneID": "eu-central-1b"},
	)}}),
	AlicloudCloudpilot: obj(map[string]any{"status": map[string]any{"vSwitches": list(
		map[string]any{"id": "vsw-1", "zoneID": "eu-central-1c"},
	)}}),
	Hcloud: obj(map[string]any{"spec": map[string]any{"locations": []any{"fsn1", "nbg1"}}}),
	HcloudItsh: obj(map[string]any{
		"spec":   map[string]any{"locations": []any{"fsn1", "hel1"}},
		"status": map[string]any{"locations": list(map[string]any{"id": int64(1), "name": "fsn1"})},
	}),
	UpCloud: obj(map[string]any{
		"spec":   map[string]any{"zones": []any{"de-fra1"}},
		"status": map[string]any{"zones": list(map[string]any{"id": "de-fra1", "description": "Frankfurt #1"})},
	}),
	OVH:   obj(map[string]any{"spec": map[string]any{"region": "GRA11"}}),
	Vultr: obj(map[string]any{"spec": map[string]any{"region": "ewr"}}),
}

func TestRegionsFromNodeClass(t *testing.T) {
	cases := map[ID][]string{
		AWS:                {"us-east-1"},
		AWSAutoMode:        {"us-east-1"},
		Azure:              nil,
		GCP:                nil,
		AlicloudACK:        {"eu-central-1"},
		AlicloudCloudpilot: {"eu-central-1"},
		Hcloud:             {"fsn1", "nbg1"},
		HcloudItsh:         {"fsn1"},
		UpCloud:            {"de-fra1"},
		OVH:                {"GRA11"},
		Vultr:              {"ewr"},
	}
	for id, want := range cases {
		a := mustAdapter(t, id)
		got, ok := a.RegionsFromNodeClass(nodeClasses[id], zoneIx)
		if ok != (want != nil) || !slices.Equal(got, want) {
			t.Errorf("%s: %v %v, want %v", id, got, ok, want)
		}
		if _, ok := a.RegionsFromNodeClass(obj(map[string]any{}), zoneIx); ok {
			t.Errorf("%s: regions from an empty NodeClass", id)
		}
		if _, ok := a.RegionsFromNodeClass(nil, zoneIx); ok {
			t.Errorf("%s: regions from no NodeClass", id)
		}
		if a.NodeClassRegionsAuthoritative() == (id == OVH) {
			t.Errorf("%s: authoritative %v", id, a.NodeClassRegionsAuthoritative())
		}
		if a.MultiRegion() != (id == Hcloud || id == HcloudItsh || id == UpCloud) {
			t.Errorf("%s: multi-region %v", id, a.MultiRegion())
		}
	}
	// Zones missing from the Spare Cores table are ignored, never parsed.
	unknown := obj(map[string]any{"status": map[string]any{"subnets": list(map[string]any{"zone": "us-east-9z", "zoneID": "use9-az9"})}})
	if got, ok := mustAdapter(t, AWS).RegionsFromNodeClass(unknown, zoneIx); ok {
		t.Errorf("unknown zone mapped to %v", got)
	}
}

func TestKeeperRegions(t *testing.T) {
	cases := []struct {
		id   ID
		k8s  []string
		want []string
	}{
		{AWS, []string{"us-east-1"}, []string{"us-east-1"}},
		{AWSAutoMode, []string{"us-east-1"}, []string{"us-east-1"}},
		{Azure, []string{"westeurope"}, []string{"westeurope"}},
		{GCP, []string{"us-central1"}, []string{"1000"}},
		{AlicloudACK, []string{"eu-central-1"}, []string{"eu-central-1"}},
		{AlicloudCloudpilot, []string{"eu-central-1"}, []string{"eu-central-1"}},
		{Hcloud, []string{"nbg1", "fsn1", "fsn1"}, []string{"2", "4"}},
		{HcloudItsh, []string{"fsn1"}, []string{"4"}},
		{UpCloud, []string{"de-fra1", "fi-hel1"}, []string{"de-fra1", "fi-hel1"}},
		{OVH, []string{"GRA11"}, []string{"GRA11"}},
		{OVH, []string{"gra11"}, []string{"GRA11"}},
		{Vultr, []string{"ewr"}, []string{"ewr"}},
	}
	for _, c := range cases {
		got, err := KeeperRegions(mustAdapter(t, c.id), c.k8s, regionIx)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%s %v: %v %v, want %v", c.id, c.k8s, got, err, c.want)
		}
	}
	unknown := []struct {
		id  ID
		k8s string
	}{
		{AWS, "us-east-9"},
		{GCP, "1000"},          // numeric ids are not Kubernetes values
		{Hcloud, "fsn1-dc14"},  // the datacenter is not the location
		{Hcloud, "4"},          // nor the numeric id
		{Azure, "West Europe"}, // display names do not map
		{Vultr, "New Jersey"},  // zone api_reference is a city name
		{AlicloudACK, "eu-central-9"},
		{UpCloud, ""},
	}
	for _, c := range unknown {
		if got, err := KeeperRegions(mustAdapter(t, c.id), []string{c.k8s}, regionIx); !errors.Is(err, ErrRegionUnknown) {
			t.Errorf("%s %q: %v %v", c.id, c.k8s, got, err)
		}
	}
}

func regionLabels(region string, extra ...string) map[string]string {
	l := map[string]string{LabelRegion: region}
	for i := 0; i+1 < len(extra); i += 2 {
		l[extra[i]] = extra[i+1]
	}
	return l
}

func TestDetectRegions(t *testing.T) {
	awsRef := NodeClassRef{Group: "karpenter.k8s.aws", Version: "v1", Kind: "EC2NodeClass", Name: "default"}
	azureRef := NodeClassRef{Group: "karpenter.azure.com", Version: "v1beta1", Kind: "AKSNodeClass", Name: "default"}
	ovhRef := NodeClassRef{Group: "karpenter.ovhcloud.sh", Version: "v1alpha1", Kind: "OVHNodeClass", Name: "default"}
	hcloudRef := NodeClassRef{Group: "karpenter.hetzner.cloud", Version: "v1", Kind: "HCloudNodeClass", Name: "default"}
	own := func(ref NodeClassRef) []string { return []string{ref.NodeClaimLabel(), ref.Name} }
	other := []string{"karpenter.azure.com/aksnodeclass", "other"}

	cases := []struct {
		name     string
		id       ID
		ref      NodeClassRef
		reader   *fakeReader
		want     []string
		source   Source
		conflict bool
		pending  bool
		calls    []string
	}{
		{
			name: "aws NodeClass", id: AWS, ref: awsRef,
			reader: &fakeReader{nodeClass: nodeClasses[AWS], nodes: []map[string]string{regionLabels("us-east-1")}},
			want:   []string{"us-east-1"}, source: SourceNodeClass,
			calls: []string{"NodeClass default", "NodeClaims", "Nodes"},
		},
		{
			name: "aws NodeClass wins over NodeClaims", id: AWS, ref: awsRef,
			reader: &fakeReader{
				nodeClass:  nodeClasses[AWS],
				nodeClaims: []map[string]string{regionLabels("us-west-2", own(awsRef)...), regionLabels("us-east-1", own(awsRef)...)},
			},
			want: []string{"us-east-1"}, source: SourceNodeClass, conflict: true,
		},
		{
			name: "aws NodeClass status still empty", id: AWS, ref: awsRef,
			reader: &fakeReader{
				nodeClass: obj(map[string]any{"spec": map[string]any{}}),
				nodes:     []map[string]string{regionLabels("us-east-1"), regionLabels("us-east-1")},
			},
			want: []string{"us-east-1"}, source: SourceAllNodes,
			calls: []string{"NodeClass default", "NodeClaims", "Nodes", "AllNodes"},
		},
		{
			name: "azure NodeClaims", id: Azure, ref: azureRef,
			reader: &fakeReader{
				nodeClass:  nodeClasses[Azure],
				nodeClaims: []map[string]string{regionLabels("westeurope", own(azureRef)...)},
				nodes:      []map[string]string{regionLabels("westeurope", own(azureRef)...), regionLabels("westeurope")},
			},
			want: []string{"westeurope"}, source: SourceNodeClaims,
			calls: []string{"NodeClass default", "NodeClaims", "Nodes"},
		},
		{
			name: "azure nodes of the NodeClass", id: Azure, ref: azureRef,
			reader: &fakeReader{nodes: []map[string]string{regionLabels("westeurope", own(azureRef)...), regionLabels("northeurope")}},
			want:   []string{"westeurope"}, source: SourceNodes,
		},
		{
			name: "azure all nodes", id: Azure, ref: azureRef,
			reader: &fakeReader{nodes: []map[string]string{regionLabels("westeurope", other...), {"kubernetes.io/os": "linux"}}},
			want:   []string{"westeurope"}, source: SourceAllNodes,
		},
		{
			name: "azure all nodes, majority", id: Azure, ref: azureRef,
			reader: &fakeReader{nodes: []map[string]string{
				regionLabels("westeurope"), regionLabels("northeurope"), regionLabels("WestEurope"),
			}},
			want: []string{"westeurope"}, source: SourceAllNodes, conflict: true,
		},
		{
			name: "azure NodeClaims disagree with the NodeClass's nodes: node majority", id: Azure, ref: azureRef,
			reader: &fakeReader{
				nodeClaims: []map[string]string{regionLabels("westeurope", own(azureRef)...)},
				nodes: []map[string]string{
					regionLabels("northeurope", own(azureRef)...), regionLabels("northeurope", own(azureRef)...),
					regionLabels("westeurope", own(azureRef)...),
				},
			},
			want: []string{"northeurope"}, source: SourceNodes, conflict: true,
		},
		{
			name: "azure majority tie: smallest name", id: Azure, ref: azureRef,
			reader: &fakeReader{nodeClaims: []map[string]string{
				regionLabels("westeurope", own(azureRef)...), regionLabels("northeurope", own(azureRef)...),
			}},
			want: []string{"northeurope"}, source: SourceNodeClaims, conflict: true,
		},
		{
			name: "pending: nothing anywhere", id: Azure, ref: azureRef,
			reader: &fakeReader{nodes: []map[string]string{{"kubernetes.io/os": "linux"}}}, pending: true,
		},
		{
			name: "ovh: nodes, NodeClass agrees case-insensitively", id: OVH, ref: ovhRef,
			reader: &fakeReader{
				nodeClass: obj(map[string]any{"spec": map[string]any{"region": "gra11"}}),
				nodes:     []map[string]string{regionLabels("GRA11")},
			},
			want: []string{"GRA11"}, source: SourceAllNodes,
		},
		{
			name: "ovh: NodeClass is only a cross-check", id: OVH, ref: ovhRef,
			reader: &fakeReader{nodeClass: obj(map[string]any{"spec": map[string]any{"region": "GRA9"}}), nodes: []map[string]string{regionLabels("GRA11")}},
			want:   []string{"GRA11"}, source: SourceAllNodes, conflict: true,
		},
		{
			name: "ovh: no node region yet", id: OVH, ref: ovhRef,
			reader: &fakeReader{nodeClass: nodeClasses[OVH]}, pending: true,
		},
		{
			name: "hcloud: several locations, nodes in a subset", id: Hcloud, ref: hcloudRef,
			reader: &fakeReader{nodeClass: nodeClasses[Hcloud], nodes: []map[string]string{regionLabels("fsn1", own(hcloudRef)...)}},
			want:   []string{"fsn1", "nbg1"}, source: SourceNodeClass,
		},
		{
			name: "hcloud: node outside the locations", id: Hcloud, ref: hcloudRef,
			reader: &fakeReader{nodeClass: nodeClasses[Hcloud], nodes: []map[string]string{regionLabels("hel1", own(hcloudRef)...)}},
			want:   []string{"fsn1", "nbg1"}, source: SourceNodeClass, conflict: true,
		},
		{
			name: "hcloud: missing NodeClass, nodes span several locations", id: Hcloud, ref: hcloudRef,
			reader: &fakeReader{nodes: []map[string]string{regionLabels("hel1"), regionLabels("fsn1")}},
			want:   []string{"fsn1", "hel1"}, source: SourceAllNodes,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := DetectRegions(context.Background(), c.reader, mustAdapter(t, c.id), c.ref, zoneIx)
			if c.pending {
				if !errors.Is(err, ErrRegionPending) {
					t.Fatalf("got %+v, %v", d, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(d.Regions, c.want) || d.Source != c.source || d.Conflict != c.conflict {
				t.Errorf("got %v from %s (conflict %v), want %v from %s (conflict %v); seen %+v",
					d.Regions, d.Source, d.Conflict, c.want, c.source, c.conflict, d.Seen)
			}
			if c.calls != nil && !slices.Equal(c.reader.calls, c.calls) {
				t.Errorf("calls %v, want %v", c.reader.calls, c.calls)
			}
			if c.conflict {
				msg := d.ConflictMessage()
				for _, s := range d.Seen {
					if !strings.Contains(msg, s.Region) {
						t.Errorf("conflict message %q lacks %s", msg, s.Region)
					}
				}
			}
		})
	}
}

func TestDetectRegionsSeen(t *testing.T) {
	ref := NodeClassRef{Group: "karpenter.k8s.aws", Version: "v1", Kind: "EC2NodeClass", Name: "default"}
	own := []string{ref.NodeClaimLabel(), ref.Name}
	r := &fakeReader{
		nodeClass:  nodeClasses[AWS],
		nodeClaims: []map[string]string{regionLabels("us-east-1", own...), regionLabels("us-west-2", own...)},
		nodes:      []map[string]string{regionLabels("us-east-1", own...), regionLabels("us-east-1", own...)},
	}
	d, err := DetectRegions(context.Background(), r, mustAdapter(t, AWS), ref, zoneIx)
	if err != nil {
		t.Fatal(err)
	}
	want := []SeenRegion{
		{SourceNodeClass, "us-east-1", 1},
		{SourceNodeClaims, "us-east-1", 1},
		{SourceNodeClaims, "us-west-2", 1},
		{SourceNodes, "us-east-1", 2},
	}
	if !slices.Equal(d.Seen, want) || !d.Conflict {
		t.Errorf("seen %+v (conflict %v)", d.Seen, d.Conflict)
	}
	if ref.NodeClaimLabel() != "karpenter.k8s.aws/ec2nodeclass" {
		t.Errorf("label %s", ref.NodeClaimLabel())
	}
}

func TestDetectRegionsReadError(t *testing.T) {
	ref := NodeClassRef{Group: "karpenter.k8s.aws", Version: "v1", Kind: "EC2NodeClass", Name: "default"}
	boom := errors.New("boom")
	_, err := DetectRegions(context.Background(), &fakeReader{err: boom}, mustAdapter(t, AWS), ref, zoneIx)
	if !errors.Is(err, boom) {
		t.Errorf("got %v", err)
	}
}

func TestSourceAPISource(t *testing.T) {
	cases := map[Source]v1alpha1.RegionSource{
		SourceNodeClass:  v1alpha1.RegionSourceNodeClass,
		SourceNodeClaims: v1alpha1.RegionSourceNodeLabels,
		SourceNodes:      v1alpha1.RegionSourceNodeLabels,
		SourceAllNodes:   v1alpha1.RegionSourceNodeLabels,
	}
	for s, want := range cases {
		if got := s.APISource(); got != want {
			t.Errorf("%s: %s", s, got)
		}
	}
}
