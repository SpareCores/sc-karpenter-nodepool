/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package providers

import (
	"fmt"
	"slices"
	"strings"

	"github.com/SpareCores/sc-karpenter-nodepool/api/v1alpha1"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/keeper"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/selection"
)

// Spare Cores server status values (lower case, as Keeper returns them).
const statusPlannedForRetirement = "planned-for-retirement"

// eligibilityRule is one provider-specific drop rule; it returns a reason
// when the candidate is dropped.
type eligibilityRule func(c *selection.Candidate, env Env) string

// commonEligibility drops what no provider can launch: Spare Cores
// architectures without a Kubernetes equivalent (i386, *_mac).
//
// Rows without a price on the basis in the target regions (the other "all"
// row of §6.2) are dropped by selection.Evaluate itself, as missing data.
func commonEligibility(c *selection.Candidate, _ Env) string {
	if c.Architecture == "" {
		return fmt.Sprintf("architecture %s has no Kubernetes equivalent", c.Server.CPUArchitecture)
	}
	return ""
}

func onlyArchitectures(archs ...v1alpha1.Architecture) eligibilityRule {
	return func(c *selection.Candidate, _ Env) string {
		if !slices.Contains(archs, c.Architecture) {
			return fmt.Sprintf("architecture %s is not supported by the provider", c.Architecture)
		}
		return ""
	}
}

func excludePrefixes(why string, prefixes ...string) eligibilityRule {
	return func(c *selection.Candidate, _ Env) string {
		for _, p := range prefixes {
			if strings.HasPrefix(c.Name(), p) {
				return fmt.Sprintf("%s (%s*)", why, p)
			}
		}
		return ""
	}
}

func excludeNames(why string, names ...string) eligibilityRule {
	return func(c *selection.Candidate, _ Env) string {
		if slices.Contains(names, c.Name()) {
			return why
		}
		return ""
	}
}

// onDemandPrice: the candidate's on-demand price in the target regions when
// the evaluation queried it, else Keeper's on-demand minimum of the row (a
// spot-basis evaluation only queries spot prices).
func onDemandPrice(c *selection.Candidate) (float64, bool) {
	if p, ok := c.Prices[keeper.AllocationOnDemand]; ok && p > 0 {
		return p, true
	}
	if p := c.Server.MinPriceOnDemand; p != nil && *p > 0 {
		return *p, true
	}
	return 0, false
}

// requireOnDemandPrice drops types without an on-demand price: GCP's
// provider takes prices from a file that lacks them (no offerings), and
// Alibaba's providers rank by on-demand price.
func requireOnDemandPrice(c *selection.Candidate, _ Env) string {
	if _, ok := onDemandPrice(c); !ok {
		return "no on-demand price"
	}
	return ""
}

// excludeTPUs: TPUs are not schedulable as GCE instances by the GCP provider.
func excludeTPUs(c *selection.Candidate, _ Env) string {
	if c.AcceleratorType == v1alpha1.AcceleratorTypeTPU {
		return "TPU types are not supported by the provider"
	}
	return ""
}

// excludeGPUsUnlessAsked drops accelerator flavors unless the selector asks
// for accelerators (OVH: GPU flavors take 40–80 minutes to launch, beyond
// core's registration timeout).
func excludeGPUsUnlessAsked(c *selection.Candidate, env Env) string {
	if c.Server.AcceleratorCount > 0 && !env.AllowGPUs {
		return "GPU flavor (select accelerators explicitly to keep it)"
	}
	return ""
}

// requireAutoModeSKU: EKS Auto Mode launches only the types its price list
// has a management fee for, per region.
func requireAutoModeSKU(c *selection.Candidate, env Env) string {
	if _, ok := env.ManagedFees[c.Name()]; !ok {
		return "no EKS Auto Mode SKU in the region"
	}
	return ""
}

// autoModeMinimumSize applies the documented Auto Mode size rule (more than
// 1 vCPU; no nano, micro or small sizes) on top of the price list, which
// also lists 1-vCPU .medium types (us-east-1, price list of 2026-09-28).
func autoModeMinimumSize(c *selection.Candidate, _ Env) string {
	_, size, _ := strings.Cut(c.Name(), ".")
	switch {
	case c.VCPUs() < 2:
		return "fewer than 2 vCPUs (EKS Auto Mode minimum)"
	case size == "nano" || size == "micro" || size == "small":
		return "nano, micro and small sizes are not supported by EKS Auto Mode"
	}
	return ""
}

// eligible runs the common rule and then the adapter's rules; the first
// reason wins.
func eligible(rules []eligibilityRule, c *selection.Candidate, env Env) (bool, string) {
	if r := commonEligibility(c, env); r != "" {
		return false, r
	}
	for _, rule := range rules {
		if r := rule(c, env); r != "" {
			return false, r
		}
	}
	return true, ""
}
