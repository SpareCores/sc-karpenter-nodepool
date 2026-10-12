/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package providers

import (
	"context"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

var (
	ec2NodeClassRef = NodeClassRef{Group: "karpenter.k8s.aws", Version: "v1", Kind: "EC2NodeClass", Name: "default"}
	nodeClaimGVK    = schema.GroupVersionKind{Group: "karpenter.sh", Version: "v1", Kind: "NodeClaim"}
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	// Other projects' kinds are only known as unstructured objects.
	for _, gvk := range []schema.GroupVersionKind{ec2NodeClassRef.GroupVersionKind(), nodeClaimGVK} {
		list := gvk
		list.Kind += "List"
		s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		s.AddKnownTypeWithName(list, &unstructured.UnstructuredList{})
	}
	return s
}

func unstructuredObj(gvk schema.GroupVersionKind, name string, labels map[string]string, content map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: content}
	if u.Object == nil {
		u.Object = map[string]any{}
	}
	u.SetGroupVersionKind(gvk)
	u.SetName(name)
	u.SetLabels(labels)
	return u
}

func node(name string, labels map[string]string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

func TestClientReader(t *testing.T) {
	own := map[string]string{ec2NodeClassRef.NodeClaimLabel(): "default", LabelRegion: "us-east-1"}
	nc := unstructuredObj(ec2NodeClassRef.GroupVersionKind(), "default", nil, map[string]any{
		"status": map[string]any{"subnets": []any{map[string]any{"zone": "us-east-1a", "zoneID": "use1-az1"}}},
	})
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		nc,
		unstructuredObj(nodeClaimGVK, "default-abc", own, nil),
		unstructuredObj(nodeClaimGVK, "other-abc", map[string]string{ec2NodeClassRef.NodeClaimLabel(): "other", LabelRegion: "us-west-2"}, nil),
		node("node-a", own),
		node("node-b", map[string]string{LabelRegion: "us-east-1"}),
	).Build()
	r := ClientReader{Client: c}
	ctx := context.Background()

	got, err := r.NodeClass(ctx, ec2NodeClassRef)
	if err != nil || got == nil || got.GetName() != "default" {
		t.Fatalf("NodeClass: %v %v", got, err)
	}
	missing := ec2NodeClassRef
	missing.Name = "missing"
	if got, err := r.NodeClass(ctx, missing); err != nil || got != nil {
		t.Errorf("missing NodeClass: %v %v", got, err)
	}
	selector := map[string]string{ec2NodeClassRef.NodeClaimLabel(): "default"}
	if claims, err := r.NodeClaimLabels(ctx, selector); err != nil || len(claims) != 1 || claims[0][LabelRegion] != "us-east-1" {
		t.Errorf("NodeClaims: %v %v", claims, err)
	}
	if nodes, err := r.NodeLabels(ctx, selector); err != nil || len(nodes) != 1 {
		t.Errorf("nodes of the NodeClass: %v %v", nodes, err)
	}
	if nodes, err := r.NodeLabels(ctx, nil); err != nil || len(nodes) != 2 {
		t.Errorf("all nodes: %v %v", nodes, err)
	}

	d, err := DetectRegions(ctx, r, mustAdapter(t, AWS), ec2NodeClassRef, zoneIx)
	if err != nil || !slices.Equal(d.Regions, []string{"us-east-1"}) || d.Source != SourceNodeClass || d.Conflict {
		t.Errorf("DetectRegions: %+v %v", d, err)
	}
}

// TestClientReaderNoCRD: a NodeClass or NodeClaim kind the API server does
// not know reads as absent.
func TestClientReaderNoCRD(t *testing.T) {
	noMatch := func(gvk schema.GroupVersionKind) error {
		return &meta.NoKindMatchError{GroupKind: gvk.GroupKind(), SearchedVersions: []string{gvk.Version}}
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
			return noMatch(obj.GetObjectKind().GroupVersionKind())
		},
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if gvk := list.GetObjectKind().GroupVersionKind(); gvk == NodeClaimListGVK {
				return noMatch(gvk)
			}
			return cl.List(ctx, list, opts...)
		},
	}).WithObjects(node("node-a", map[string]string{LabelRegion: "eu-central-1"})).Build()
	r := ClientReader{Client: c}
	ctx := context.Background()
	if got, err := r.NodeClass(ctx, ec2NodeClassRef); err != nil || got != nil {
		t.Errorf("NodeClass: %v %v", got, err)
	}
	if got, err := r.NodeClaimLabels(ctx, nil); err != nil || got != nil {
		t.Errorf("NodeClaims: %v %v", got, err)
	}
	d, err := DetectRegions(ctx, r, mustAdapter(t, AlicloudACK), ec2NodeClassRef, zoneIx)
	if err != nil || !slices.Equal(d.Regions, []string{"eu-central-1"}) || d.Source != SourceAllNodes {
		t.Errorf("DetectRegions: %+v %v", d, err)
	}
}
