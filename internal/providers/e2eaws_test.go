/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package providers

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/SpareCores/sc-karpenter-nodepool/api/v1alpha1"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/keeper"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/selection"
)

// TestE2EAWSSelection keeps the L5 lane's SCNodePool and its expected list
// (test/e2e-aws) in step with each other: against the committed snapshot,
// the selector yields exactly expected-instance-types.txt for both AWS
// adapters, which the real e2e run then asserts on a live cluster. The
// snapshot is us-east-1, the lane runs in us-east-2: the families are old and
// offered in every region.
func TestE2EAWSSelection(t *testing.T) {
	dir := filepath.Join("..", "..", "test", "e2e-aws")
	raw, err := os.ReadFile(filepath.Join(dir, "manifests", "scnodepool.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{
		"E2E_SCNODEPOOL":      "sckn-e2e",
		"E2E_NODECLASS_GROUP": "karpenter.k8s.aws",
		"E2E_NODECLASS_KIND":  "EC2NodeClass",
		"E2E_CAPACITY_TYPE":   "on-demand",
	}
	var pool v1alpha1.SCNodePool
	if err := yaml.UnmarshalStrict([]byte(os.Expand(string(raw), func(k string) string { return vars[k] })), &pool); err != nil {
		t.Fatal(err)
	}

	list, err := os.ReadFile(filepath.Join(dir, "expected-instance-types.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for l := range strings.SplitSeq(string(list), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			want = append(want, l)
		}
	}
	slices.Sort(want)

	fees, _ := autoModeFees(t)
	for _, tc := range []struct {
		id  ID
		env Env
	}{
		{AWS, Env{}},
		{AWSAutoMode, Env{ManagedFees: fees}},
	} {
		t.Run(string(tc.id), func(t *testing.T) {
			a := mustAdapter(t, tc.id)
			cands := snapshotCandidates(t, "aws", keeper.AllocationOnDemand)
			res, err := selection.Evaluate(&pool.Spec, cands, selection.Options{
				Eligible:    EligibleFunc(a, tc.env),
				ManagedFees: tc.env.ManagedFees,
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := res.Names(); !slices.Equal(got, want) {
				t.Errorf("selected %v, expected-instance-types.txt has %v", got, want)
			}
		})
	}
}
