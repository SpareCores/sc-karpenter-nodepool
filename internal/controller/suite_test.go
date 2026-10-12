/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package controller

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/SpareCores/sc-karpenter-nodepool/api/v1alpha1"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/keeper"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/keeper/keepertest"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/nodepool"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/providers"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/testutil"
)

// The envtest control plane has the SCNodePool CRD, Karpenter core's
// NodePool and NodeClaim CRDs (v1.14.1, from the sigs.k8s.io/karpenter
// module) and minimal stand-in NodeClass CRDs (testdata/crds). Keeper is the
// snapshot replay server.
var (
	restConfig *rest.Config
	k8sClient  client.Client
	keeperURL  string
)

func TestMain(m *testing.M) {
	os.Exit(func() int {
		cfg, stop, err := testutil.StartEnvWith(testutil.EnvOptions{
			CRDDirs: []string{filepath.Join("testdata", "crds")},
			CRDs:    testutil.KarpenterCRDs(),
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer func() {
			if err := stop(); err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		}()
		restConfig = cfg
		scheme := runtime.NewScheme()
		if err := clientgoscheme.AddToScheme(scheme); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err := v1alpha1.AddToScheme(scheme); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if k8sClient, err = client.New(cfg, client.Options{Scheme: scheme}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		h, err := keepertest.NewHandler(keepertest.SnapshotDir())
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		srv := httptest.NewServer(h)
		defer srv.Close()
		keeperURL = srv.URL
		return m.Run()
	}())
}

// newDataSource reads the replay server at url without retries.
func newDataSource(url string) *DataSource {
	kc := keeper.New(url, keeper.WithRetries(0, time.Millisecond, time.Millisecond))
	return NewDataSource(kc, providers.NewPriceListClient(url, providers.WithPriceListRetries(1, 0)), 0)
}

// newReconciler returns a reconciler with a fresh memory, reading the API
// server directly and recording Events in memory.
func newReconciler() (*SCNodePoolReconciler, *events.FakeRecorder) {
	rec := events.NewFakeRecorder(1000)
	return &SCNodePoolReconciler{
		Client:   k8sClient,
		Scheme:   k8sClient.Scheme(),
		Recorder: rec,
		Registry: providers.NewRegistry(),
		Data:     newDataSource(keeperURL),
	}, rec
}

func drain(rec *events.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func hasEvent(evs []string, substr ...string) bool {
	return slices.ContainsFunc(evs, func(e string) bool {
		for _, s := range substr {
			if !strings.Contains(e, s) {
				return false
			}
		}
		return true
	})
}

var ec2NodeClassGVK = schema.GroupVersionKind{Group: "karpenter.k8s.aws", Version: "v1", Kind: "EC2NodeClass"}

// createNodeClass creates a stand-in EC2NodeClass whose status lists
// subnets in the given AWS zone IDs.
func createNodeClass(t *testing.T, name string, zoneIDs ...string) *unstructured.Unstructured {
	t.Helper()
	nc := &unstructured.Unstructured{}
	nc.SetGroupVersionKind(ec2NodeClassGVK)
	nc.SetName(name)
	subnets := make([]any, 0, len(zoneIDs))
	for i, z := range zoneIDs {
		subnets = append(subnets, map[string]any{"id": fmt.Sprintf("subnet-%d", i), "zoneID": z})
	}
	if len(subnets) > 0 {
		nc.Object["status"] = map[string]any{"subnets": subnets}
	}
	mustCreate(t, nc)
	return nc
}

func mustCreate(t *testing.T, obj client.Object) {
	t.Helper()
	if err := k8sClient.Create(context.Background(), obj); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), obj) })
}

// familiesSelector selects the 2..maxVCPUs sizes of c7a, m7a (amd64) and
// c7g (arm64) from the AWS snapshot.
func familiesSelector(minVCPUs, maxVCPUs int32) *v1alpha1.Selector {
	return &v1alpha1.Selector{
		VCPUs:    &v1alpha1.IntRange{Min: &minVCPUs, Max: &maxVCPUs},
		Families: &v1alpha1.GlobFilter{Include: []string{"c7a", "m7a", "c7g"}},
	}
}

func templateJSON(group, kind, name, extra string) []byte {
	s := fmt.Sprintf(`{"template":{"spec":{"nodeClassRef":{"group":%q,"kind":%q,"name":%q}%s}}}`, group, kind, name, extra)
	return []byte(s)
}

func newPool(name, nodeClass string, sel *v1alpha1.Selector) *v1alpha1.SCNodePool {
	return &v1alpha1.SCNodePool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.SCNodePoolSpec{
			NodePoolTemplate: v1alpha1.NodePoolTemplate{
				Spec: runtime.RawExtension{Raw: templateJSON("karpenter.k8s.aws", "EC2NodeClass", nodeClass, "")},
			},
			Selector: sel,
		},
	}
}

// createPool creates the SCNodePool; cleanup removes it (finalizers
// included) and its NodePool (envtest runs no garbage collector).
func createPool(t *testing.T, p *v1alpha1.SCNodePool) {
	t.Helper()
	ctx := context.Background()
	if err := k8sClient.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	name := p.Name
	t.Cleanup(func() {
		var cur v1alpha1.SCNodePool
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: name}, &cur); err == nil {
			cur.Finalizers = nil
			_ = k8sClient.Update(ctx, &cur)
			_ = k8sClient.Delete(ctx, &cur)
		}
		np := nodepool.New()
		np.SetName(name)
		_ = k8sClient.Delete(ctx, np)
	})
}

func reconcileOnce(t *testing.T, r *SCNodePoolReconciler, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: name}})
	if err != nil {
		t.Fatalf("reconcile %s: %v", name, err)
	}
	return res
}

func getPool(t *testing.T, name string) *v1alpha1.SCNodePool {
	t.Helper()
	var p v1alpha1.SCNodePool
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: name}, &p); err != nil {
		t.Fatal(err)
	}
	return &p
}

// updatePool applies mutate to the current SCNodePool and updates it.
func updatePool(t *testing.T, name string, mutate func(*v1alpha1.SCNodePool)) {
	t.Helper()
	p := getPool(t, name)
	mutate(p)
	if err := k8sClient.Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

func getNodePool(t *testing.T, name string) *unstructured.Unstructured {
	t.Helper()
	np := nodepool.New()
	err := k8sClient.Get(context.Background(), client.ObjectKey{Name: name}, np)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return np
}

func cond(p *v1alpha1.SCNodePool, t string) metav1.Condition {
	if c := meta.FindStatusCondition(p.Status.Conditions, t); c != nil {
		return *c
	}
	return metav1.Condition{}
}

func expectCondition(t *testing.T, p *v1alpha1.SCNodePool, typ string, status metav1.ConditionStatus, reason string) {
	t.Helper()
	c := cond(p, typ)
	if c.Status != status || (reason != "" && c.Reason != reason) {
		t.Errorf("%s = %s/%s (%s), want %s/%s", typ, c.Status, c.Reason, c.Message, status, reason)
	}
}

// createNode creates a node with the given labels.
func createNode(t *testing.T, name string, labels map[string]string) {
	t.Helper()
	mustCreate(t, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}})
}
