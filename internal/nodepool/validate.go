/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package nodepool

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/SpareCores/sc-karpenter-nodepool/internal/providers"
)

// karpenterDomain is the label domain the NodePool CRD reserves, except for
// karpenter.sh/capacity-type (overview §5.1.1).
const karpenterDomain = "karpenter.sh"

// Bounds of minValues in the NodePool CRD (overview §5.1.2).
const (
	minValuesMin = 1
	minValuesMax = 50
)

// legacyRestrictedDomains and legacyDomainExceptions are the label domains
// Karpenter core v1.8 and older reserve for well-known labels (§5.1.3).
var (
	legacyRestrictedDomains = []string{"kubernetes.io", "k8s.io"}
	legacyDomainExceptions  = []string{"kops.k8s.io", "node.kubernetes.io", "node-restriction.kubernetes.io"}
)

// Validate checks a rendered NodePool before it is applied (overview §3.6):
// at most 100 requirements; Karpenter core's runtime requirement validation
// (supported operators, restricted keys including kubernetes.io/hostname,
// label-value syntax and length, non-empty In, minValues not above the
// number of values); no karpenter.sh key other than capacity-type; minValues
// within the CRD bounds; and, for providers on core v1.8 or older, no
// Gte/Lte and no non-well-known keys in the kubernetes.io and k8s.io
// domains. The provider's own CRD rules are left to the server-side dry run.
func Validate(ctx context.Context, np *unstructured.Unstructured, rules providers.RequirementRules) error {
	var errs []error
	if ref, ok := NodeClassRefOf(np); !ok || ref.Group == "" || ref.Kind == "" || ref.Name == "" {
		errs = append(errs, errors.New("spec.template.spec.nodeClassRef needs group, kind and name"))
	}
	if _, ok, _ := unstructured.NestedFieldNoCopy(np.Object, "spec", "disruption", "consolidateAfter"); !ok {
		errs = append(errs, errors.New("spec.disruption.consolidateAfter is required"))
	}
	reqs := RequirementsOf(np)
	if len(reqs) > MaxRequirements {
		errs = append(errs, fmt.Errorf("%d requirements, at most %d are allowed", len(reqs), MaxRequirements))
	}
	for i, r := range reqs {
		if err := validateRequirement(ctx, r, rules); err != nil {
			errs = append(errs, fmt.Errorf("requirements[%d] (%s): %w", i, r.Key, err))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrInvalidNodePool, errors.Join(errs...))
}

func validateRequirement(ctx context.Context, r Requirement, rules providers.RequirementRules) error {
	var errs []error
	err := karpv1.ValidateRequirement(ctx, karpv1.NodeSelectorRequirementWithMinValues{
		Key:       r.Key,
		Operator:  corev1.NodeSelectorOperator(r.Operator),
		Values:    r.Values,
		MinValues: r.MinValues,
	})
	if err != nil {
		errs = append(errs, err)
	}
	if inDomain(karpv1.GetLabelDomain(r.Key), karpenterDomain) && r.Key != providers.LabelCapacityType {
		errs = append(errs, fmt.Errorf("the %s domain is reserved (only %s is allowed)", karpenterDomain, providers.LabelCapacityType))
	}
	if r.MinValues != nil && (*r.MinValues < minValuesMin || *r.MinValues > minValuesMax) {
		errs = append(errs, fmt.Errorf("minValues %d is outside %d..%d", *r.MinValues, minValuesMin, minValuesMax))
	}
	if rules.LegacyCore {
		if r.Operator == OpGte || r.Operator == OpLte {
			errs = append(errs, fmt.Errorf("operator %s needs Karpenter core v1.9 or newer, the provider pins an older one", r.Operator))
		}
		if legacyRestricted(r.Key) {
			errs = append(errs, fmt.Errorf("core v1.8 and older reserve the %s domain for well-known labels",
				karpv1.GetLabelDomain(r.Key)))
		}
	}
	return errors.Join(errs...)
}

func legacyRestricted(key string) bool {
	if karpv1.WellKnownLabels.Has(key) {
		return false
	}
	domain := karpv1.GetLabelDomain(key)
	for _, e := range legacyDomainExceptions {
		if inDomain(domain, e) {
			return false
		}
	}
	for _, d := range legacyRestrictedDomains {
		if inDomain(domain, d) {
			return true
		}
	}
	return false
}

// inDomain reports whether a label domain is d or a subdomain of it.
func inDomain(domain, d string) bool {
	return domain == d || strings.HasSuffix(domain, "."+d)
}
