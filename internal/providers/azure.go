/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package providers

import (
	"slices"
	"strings"

	"github.com/SpareCores/sc-karpenter-nodepool/internal/selection"
)

// Azure's static exclusions, mirrored from the provider at the commit of
// overview §5.2 (Azure/karpenter-provider-azure 8878fa43:
// pkg/providers/instancetype/instancetypes.go isSupported,
// pkg/providers/instancetype/skus.go, pkg/utils/gpu.go and
// pkg/utils/supported-gpus.yaml). Per-NodeClass filters (image family vs
// GPU, artifact streaming, encryption at host) are not mirrored.

// azureRestrictedSizes: AKS-restricted low-performance sizes and the SKUs
// known to be problematic with the provider.
var azureRestrictedSizes = []string{
	"Standard_A0", "Standard_A1", "Standard_A1_v2",
	"Standard_B1s", "Standard_B1ms",
	"Standard_F1", "Standard_F1s",
	"Basic_A0", "Basic_A1", "Basic_A2", "Basic_A3", "Basic_A4",
	"Standard_E64i_v3", "Standard_E64is_v3",
}

// azureSupportedGPUSizes are the GPU SKUs the provider can set up (lower
// case, without the optional _promo suffix); other GPU SKUs are dropped.
var azureSupportedGPUSizes = map[string]bool{}

func init() {
	for _, s := range []string{
		"standard_nc12",
		"standard_nc12s_v2",
		"standard_nc12s_v3",
		"standard_nc144ds_xl_rtxpro6000bse_v6",
		"standard_nc144lds_xl_rtxpro6000bse_v6",
		"standard_nc16ads_a10_v4",
		"standard_nc16as_t4_v3",
		"standard_nc24",
		"standard_nc24ads_a100_v4",
		"standard_nc24lds_xl_rtxpro6000bse_v6",
		"standard_nc24r",
		"standard_nc24rs_v2",
		"standard_nc24rs_v3",
		"standard_nc24s_v2",
		"standard_nc24s_v3",
		"standard_nc288ds_xl_rtxpro6000bse_v6",
		"standard_nc288lds_xl_rtxpro6000bse_v6",
		"standard_nc32ads_a10_v4",
		"standard_nc36ds_xl_rtxpro6000bse_v6",
		"standard_nc36lds_xl_rtxpro6000bse_v6",
		"standard_nc40ads_h100_v5",
		"standard_nc48ads_a100_v4",
		"standard_nc4as_t4_v3",
		"standard_nc6",
		"standard_nc64as_t4_v3",
		"standard_nc6s_v2",
		"standard_nc6s_v3",
		"standard_nc72ds_xl_rtxpro6000bse_v6",
		"standard_nc72lds_xl_rtxpro6000bse_v6",
		"standard_nc80adis_h100_v5",
		"standard_nc8ads_a10_v4",
		"standard_nc8as_t4_v3",
		"standard_nc96ads_a100_v4",
		"standard_nd112amsr_a100_v4",
		"standard_nd112asr_a100_v4",
		"standard_nd120amsr_a100_v4",
		"standard_nd120asr_a100_v4",
		"standard_nd128isr_gb300_v6",
		"standard_nd128isr_ndr_gb200_v6",
		"standard_nd12s",
		"standard_nd24rs",
		"standard_nd24s",
		"standard_nd40rs_v2",
		"standard_nd40s_v3",
		"standard_nd6s",
		"standard_nd96ams_a100_flex_v4",
		"standard_nd96ams_a100_v4",
		"standard_nd96amsf_a100_v4",
		"standard_nd96amsr_a100_v4",
		"standard_nd96amsrf_a100_v4",
		"standard_nd96ams_v4",
		"standard_nd96asr_v4",
		"standard_nd96isf_h100_v5",
		"standard_nd96isf_h200_v5",
		"standard_nd96is_flex_h100_v5",
		"standard_nd96is_h100_v5",
		"standard_nd96is_h200_v5",
		"standard_nd96is_mi300x_v5",
		"standard_nd96is_noib_h100_v5",
		"standard_nd96isrf_h100_v5",
		"standard_nd96isrf_h200_v5",
		"standard_nd96isr_h100_v5",
		"standard_nd96isr_h200_v5",
		"standard_nd96isr_mi300x_v5",
		"standard_nv12",
		"standard_nv12ads_a10_v5",
		"standard_nv12ads_v710_v5",
		"standard_nv12s_v3",
		"standard_nv18ads_a10_v5",
		"standard_nv24",
		"standard_nv24ads_v710_v5",
		"standard_nv24r",
		"standard_nv24s_v3",
		"standard_nv28adms_v710_v5",
		"standard_nv36adms_a10_v5",
		"standard_nv36ads_a10_v5",
		"standard_nv48s_v3",
		"standard_nv4ads_v710_v5",
		"standard_nv6",
		"standard_nv6ads_a10_v5",
		"standard_nv72ads_a10_v5",
		"standard_nv8ads_v710_v5",
	} {
		azureSupportedGPUSizes[s] = true
	}
}

// azureEligibility returns the reason Azure's provider would not offer the
// type ("" if it would).
func azureEligibility(c *selection.Candidate, _ Env) string {
	name := c.Name()
	// The provider checks confidential and constrained sizes on the size
	// without the tier prefix (D4s_v5 of Standard_D4s_v5).
	size := name
	if _, after, ok := strings.Cut(name, "_"); ok {
		size = after
	}
	switch {
	case slices.Contains(azureRestrictedSizes, name):
		return "size restricted by AKS"
	case c.VCPUs() < 2:
		return "fewer than 2 vCPUs"
	case c.MemoryGiB() < 3.5:
		return "less than 3.5 GiB memory"
	case strings.Contains(size, "-"):
		return "constrained vCPU size"
	case strings.HasPrefix(size, "DC") || strings.HasPrefix(size, "EC"):
		return "confidential VM size"
	case c.Server.AcceleratorCount > 0 && !azureSupportedGPUSizes[azureNormalizeSize(name)]:
		return "GPU size not supported by the provider"
	case c.Server.Status == statusPlannedForRetirement:
		// Proxy for the provider dropping SKUs retiring within 6 months.
		return "planned for retirement"
	}
	return ""
}

func azureNormalizeSize(name string) string {
	return strings.TrimSuffix(strings.ToLower(name), "_promo")
}
