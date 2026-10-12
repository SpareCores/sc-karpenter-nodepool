/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package keepertest

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// FeeFile is the manifest entry of the EKS Auto Mode fee table the snapshot
// fetcher extracts from the AWS public price list ({instance type: hourly
// USD fee} of one region).
type FeeFile struct {
	Region          string `json:"region"`
	File            string `json:"file"`
	Rows            int    `json:"rows"`
	SHA256          string `json:"sha256"`
	URL             string `json:"url"`
	PublicationDate string `json:"publication_date"`
	Version         string `json:"version"`
}

// priceListPrefix and priceListSuffix frame the region of an AmazonEKS offer
// file path.
const (
	priceListPrefix = "/offers/v1.0/aws/AmazonEKS/current/"
	priceListSuffix = "/index.json"
)

// ReadFeeTable reads a fee table file (JSON, gzipped if the name ends in
// .gz).
func ReadFeeTable(path string) (map[string]float64, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("keepertest: %w", err)
	}
	if strings.HasSuffix(path, ".gz") {
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("keepertest: %s: %w", path, err)
		}
		if raw, err = io.ReadAll(zr); err != nil {
			return nil, fmt.Errorf("keepertest: %s: %w", path, err)
		}
	}
	var fees map[string]float64
	if err := json.Unmarshal(raw, &fees); err != nil {
		return nil, fmt.Errorf("keepertest: %s: %w", path, err)
	}
	return fees, nil
}

// AutoModeFees returns the snapshot's Auto Mode fee table and its region;
// ok is false when the snapshot has none (snapshots fetched before the fee
// table was added).
func AutoModeFees(dir string, m *Manifest) (region string, fees map[string]float64, ok bool, err error) {
	if m.EKSAutoModeFees == nil {
		return "", nil, false, nil
	}
	fees, err = ReadFeeTable(filepath.Join(dir, m.EKSAutoModeFees.File))
	if err != nil {
		return "", nil, false, err
	}
	return m.EKSAutoModeFees.Region, fees, true, nil
}

// PriceListHandler serves AmazonEKS offer files of the AWS public price list
// (/offers/v1.0/aws/AmazonEKS/current/<region>/index.json), synthesised from
// fee tables per region. Besides one Auto Mode product per instance type,
// each file has one product of another kind, as the real files do.
type PriceListHandler struct {
	fees map[string]map[string]float64
}

// NewPriceListHandler serves the fee tables (region -> instance type -> fee).
func NewPriceListHandler(fees map[string]map[string]float64) *PriceListHandler {
	return &PriceListHandler{fees: fees}
}

func (h *PriceListHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	region, ok := strings.CutPrefix(r.URL.Path, priceListPrefix)
	if ok {
		region, ok = strings.CutSuffix(region, priceListSuffix)
	}
	fees, known := h.fees[region]
	if r.Method != http.MethodGet || !ok || !known {
		http.Error(w, fmt.Sprintf("keepertest: no price list for %s %s", r.Method, r.URL.Path), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(OfferFile(region, fees))
}

// OfferFile synthesises an AmazonEKS offer file in the price list format.
func OfferFile(region string, fees map[string]float64) map[string]any {
	products := map[string]any{}
	onDemand := map[string]any{}
	add := func(sku string, attrs map[string]string, usd float64) {
		attrs["regionCode"] = region
		attrs["servicecode"] = "AmazonEKS"
		products[sku] = map[string]any{"sku": sku, "productFamily": "Compute", "attributes": attrs}
		term := sku + ".JRTCKXETXF"
		onDemand[sku] = map[string]any{term: map[string]any{
			"offerTermCode": "JRTCKXETXF",
			"sku":           sku,
			"priceDimensions": map[string]any{term + ".6YS6EN2CT7": map[string]any{
				"unit":         "hours",
				"pricePerUnit": map[string]string{"USD": strconv.FormatFloat(usd, 'f', 10, 64)},
			}},
		}}
	}
	for i, it := range slices.Sorted(maps.Keys(fees)) {
		add(fmt.Sprintf("SKU%06d", i), map[string]string{
			"eksproducttype": "AutoMode",
			"instancetype":   it,
			"operation":      "EKSAutoUsage",
		}, fees[it])
	}
	add("SKUCLUSTER", map[string]string{"operation": "CreateOperation"}, 0.1)
	return map[string]any{
		"formatVersion": "v1.0",
		"offerCode":     "AmazonEKS",
		"products":      products,
		"terms":         map[string]any{"OnDemand": onDemand},
	}
}

// NewPriceList starts a price list server of the given fee tables; it is
// closed when the test ends.
func NewPriceList(t TB, fees map[string]map[string]float64) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(NewPriceListHandler(fees))
	t.Cleanup(s.Close)
	return s
}
