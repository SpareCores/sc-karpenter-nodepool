/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package providers

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// NodeClaimListGVK is Karpenter core's NodeClaim list.
var NodeClaimListGVK = schema.GroupVersionKind{Group: "karpenter.sh", Version: "v1", Kind: "NodeClaimList"}

var nodeListGVK = schema.GroupVersionKind{Version: "v1", Kind: "NodeList"}

// ClientReader implements ClusterReader with a controller-runtime client.
// NodeClasses and NodeClaims are read as unstructured objects (any provider,
// any served version); nodes as metadata only.
type ClientReader struct {
	Client client.Reader
}

var _ ClusterReader = ClientReader{}

// NodeClass gets the NodeClass; a missing object (or a missing CRD) is nil.
func (r ClientReader) NodeClass(ctx context.Context, ref NodeClassRef) (*unstructured.Unstructured, error) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(ref.GroupVersionKind())
	err := r.Client.Get(ctx, client.ObjectKey{Name: ref.Name}, u)
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// NodeClaimLabels lists NodeClaims by label.
func (r ClientReader) NodeClaimLabels(ctx context.Context, selector map[string]string) ([]map[string]string, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(NodeClaimListGVK)
	if err := r.Client.List(ctx, list, client.MatchingLabels(selector)); err != nil {
		if meta.IsNoMatchError(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]map[string]string, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, list.Items[i].GetLabels())
	}
	return out, nil
}

// NodeLabels lists nodes by label (nil selector: all nodes).
func (r ClientReader) NodeLabels(ctx context.Context, selector map[string]string) ([]map[string]string, error) {
	list := &metav1.PartialObjectMetadataList{}
	list.SetGroupVersionKind(nodeListGVK)
	if err := r.Client.List(ctx, list, client.MatchingLabels(selector)); err != nil {
		return nil, err
	}
	out := make([]map[string]string, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, list.Items[i].GetLabels())
	}
	return out, nil
}
