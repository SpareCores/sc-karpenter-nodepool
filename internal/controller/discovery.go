/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package controller

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/SpareCores/sc-karpenter-nodepool/api/v1alpha1"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/nodepool"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/providers"
)

// nodeClassRefIndex indexes SCNodePools by the group/kind/name of their
// template's nodeClassRef, to map NodeClass events to SCNodePools.
const nodeClassRefIndex = "spec.nodePoolTemplate.nodeClassRef"

// DefaultNodeClassDiscoveryInterval is how often the controller looks for
// newly installed NodeClass CRDs.
const DefaultNodeClassDiscoveryInterval = 5 * time.Minute

func indexNodeClassRef(obj client.Object) []string {
	p, ok := obj.(*v1alpha1.SCNodePool)
	if !ok {
		return nil
	}
	t, err := nodepool.ParseTemplate(p.Spec.NodePoolTemplate)
	if err != nil {
		return nil
	}
	return []string{t.NodeClassRef.String()}
}

// servedVersions returns the served versions of a kind, preferred first; nil
// when its CRD is not installed.
func servedVersions(mapper meta.RESTMapper, gk schema.GroupKind) ([]string, error) {
	mappings, err := mapper.RESTMappings(gk)
	if meta.IsNoMatchError(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("looking up %s: %w", gk, err)
	}
	out := make([]string, 0, len(mappings))
	for _, m := range mappings {
		out = append(out, m.GroupVersionKind.Version)
	}
	return out, nil
}

// detectAdapter resolves the adapter of a nodeClassRef, looking up the served
// version of the NodeClass CRD by discovery (overview §6.1: UpCloud's
// kubekanvas provider serves v1alpha1, the unsupported upcloud-tools one
// v1alpha2 under the same group and kind). It returns the version to read
// the NodeClass with ("" when the CRD is not installed).
func detectAdapter(reg *providers.Registry, mapper meta.RESTMapper, group, kind string) (providers.Adapter, string, error) {
	versions, err := servedVersions(mapper, schema.GroupKind{Group: group, Kind: kind})
	if err != nil {
		return nil, "", err
	}
	if len(versions) == 0 {
		a, err := reg.Detect(group, kind, "")
		if err != nil {
			return nil, "", fmt.Errorf("%w (the %s.%s CRD is not installed)", err, kind, group)
		}
		return a, "", nil
	}
	for _, v := range versions {
		if a, err := reg.Detect(group, kind, v); err == nil {
			return a, v, nil
		}
	}
	_, err = reg.Detect(group, kind, versions[0])
	return nil, "", err
}

// nodeClassWatcher adds a watch for the NodeClass kind of every registered
// adapter whose CRD is installed, at start-up and periodically, so that
// providers installed later are picked up; missing CRDs are fine.
type nodeClassWatcher struct {
	controller controller.Controller
	cache      cache.Cache
	mapper     meta.RESTMapper
	reader     client.Reader
	registry   *providers.Registry
	interval   time.Duration
	watched    map[schema.GroupKind]bool
}

// NeedLeaderElection: watches only matter while the controller runs.
func (w *nodeClassWatcher) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable.
func (w *nodeClassWatcher) Start(ctx context.Context) error {
	w.sync(ctx)
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			w.sync(ctx)
		}
	}
}

func (w *nodeClassWatcher) sync(ctx context.Context) {
	log := logf.FromContext(ctx).WithName("nodeclass-discovery")
	for _, a := range w.registry.All() {
		group, kind := a.NodeClass()
		gk := schema.GroupKind{Group: group, Kind: kind}
		if w.watched[gk] {
			continue
		}
		mapping, err := w.mapper.RESTMapping(gk)
		if meta.IsNoMatchError(err) {
			continue
		}
		if err != nil {
			log.Error(err, "Looking up NodeClass kind", "kind", gk.String())
			continue
		}
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(mapping.GroupVersionKind)
		src := source.Kind[client.Object](w.cache, u, handler.EnqueueRequestsFromMapFunc(w.mapNodeClass(gk)))
		if err := w.controller.Watch(src); err != nil {
			log.Error(err, "Watching NodeClass kind", "kind", gk.String())
			continue
		}
		w.watched[gk] = true
		log.Info("Watching NodeClass kind", "kind", gk.String(), "version", mapping.GroupVersionKind.Version)
	}
}

func (w *nodeClassWatcher) mapNodeClass(gk schema.GroupKind) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		var list v1alpha1.SCNodePoolList
		key := nodepool.NodeClassRef{Group: gk.Group, Kind: gk.Kind, Name: obj.GetName()}.String()
		if err := w.reader.List(ctx, &list, client.MatchingFields{nodeClassRefIndex: key}); err != nil {
			logf.FromContext(ctx).Error(err, "Listing SCNodePools of a NodeClass", "nodeClass", key)
			return nil
		}
		out := make([]reconcile.Request, 0, len(list.Items))
		for i := range list.Items {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
		return out
	}
}
