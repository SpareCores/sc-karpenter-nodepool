/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SpareCores/sc-karpenter-nodepool/internal/keeper/keepertest"
)

func offerJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseAutoModeFees(t *testing.T) {
	fees := fixtureFees(t)
	got, err := ParseAutoModeFees(bytes.NewReader(offerJSON(t, keepertest.OfferFile("us-east-1", fees))))
	if err != nil || !maps.Equal(got, fees) {
		t.Fatalf("round trip: %v %v", got, err)
	}

	// The real format, abridged: an Auto Mode product, a product of another
	// kind, and lower-case attribute names.
	abridged := `{"formatVersion": "v1.0", "offerCode": "AmazonEKS",
	  "products": {
	    "AAA": {"sku": "AAA", "productFamily": "Compute", "attributes": {"eksproducttype": "AutoMode", "instancetype": "c7a.4xlarge", "usagetype": "EKS-Auto:c7a.4xlarge-management-hours"}},
	    "BBB": {"sku": "BBB", "attributes": {"eksproducttype": "HybridNodes"}},
	    "CCC": {"sku": "CCC", "attributes": {"operation": "CreateOperation"}}},
	  "terms": {"OnDemand": {
	    "AAA": {"AAA.T": {"priceDimensions": {"AAA.T.R": {"unit": "hours", "pricePerUnit": {"USD": "0.0123400000"}}}}},
	    "BBB": {"BBB.T": {"priceDimensions": {"BBB.T.R": {"unit": "vCPU-hours", "pricePerUnit": {"USD": "0.1"}}}}}}}}`
	got, err = ParseAutoModeFees(strings.NewReader(abridged))
	if err != nil || !maps.Equal(got, map[string]float64{"c7a.4xlarge": 0.01234}) {
		t.Errorf("real format: %v %v", got, err)
	}

	broken := []struct{ name, body, msg string }{
		{"not JSON", `<html>`, "decoding"},
		{"no instance type", `{"products": {"A": {"attributes": {"eksproducttype": "AutoMode"}}}}`, "without instancetype"},
		{"no price", `{"products": {"A": {"attributes": {"eksproducttype": "AutoMode", "instancetype": "m5.large"}}}}`, "0 on-demand prices"},
		{"other unit", `{"products": {"A": {"attributes": {"eksproducttype": "AutoMode", "instancetype": "m5.large"}}},
		  "terms": {"OnDemand": {"A": {"A.T": {"priceDimensions": {"R": {"unit": "GB-Mo", "pricePerUnit": {"USD": "1"}}}}}}}}`, "unit"},
		{"bad number", `{"products": {"A": {"attributes": {"eksproducttype": "AutoMode", "instancetype": "m5.large"}}},
		  "terms": {"OnDemand": {"A": {"A.T": {"priceDimensions": {"R": {"unit": "Hrs", "pricePerUnit": {"EUR": "1"}}}}}}}}`, "bad USD price"},
		{"two prices", `{"products": {"A": {"attributes": {"eksproducttype": "AutoMode", "instancetype": "m5.large"}}},
		  "terms": {"OnDemand": {"A": {"A.T": {"priceDimensions": {
		    "R1": {"unit": "hours", "pricePerUnit": {"USD": "1"}}, "R2": {"unit": "hours", "pricePerUnit": {"USD": "2"}}}}}}}}`, "2 on-demand prices"},
	}
	for _, b := range broken {
		if got, err := ParseAutoModeFees(strings.NewReader(b.body)); err == nil || !strings.Contains(err.Error(), b.msg) {
			t.Errorf("%s: %v %v", b.name, got, err)
		}
	}
}

func TestPriceListClient(t *testing.T) {
	fees := fixtureFees(t)
	srv := keepertest.NewPriceList(t, map[string]map[string]float64{"us-east-1": fees, "eu-west-9": {}})
	c := NewPriceListClient(srv.URL, WithPriceListRetries(3, time.Millisecond))
	ctx := context.Background()
	got, err := c.AutoModeFees(ctx, "us-east-1")
	if err != nil || !maps.Equal(got, fees) {
		t.Fatalf("us-east-1: %v %v", got, err)
	}
	if _, err := c.AutoModeFees(ctx, "eu-west-9"); !errors.Is(err, ErrNoAutoModeFees) {
		t.Errorf("region without Auto Mode: %v", err)
	}
	if _, err := c.AutoModeFees(ctx, "us-west-9"); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("unknown region: %v", err)
	}
	if PriceListPath("us-east-1") != "/offers/v1.0/aws/AmazonEKS/current/us-east-1/index.json" {
		t.Errorf("path %s", PriceListPath("us-east-1"))
	}
	if NewPriceListClient("").baseURL != DefaultPriceListURL {
		t.Error("default URL")
	}
}

func TestPriceListClientRetries(t *testing.T) {
	var calls atomic.Int32
	offer := offerJSON(t, keepertest.OfferFile("us-east-1", map[string]float64{"m5.large": 0.01}))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch calls.Add(1) {
		case 1:
			http.Error(w, "slow down", http.StatusServiceUnavailable)
		case 2:
			_, _ = w.Write(offer[:len(offer)/2]) // truncated
		default:
			_, _ = w.Write(offer)
		}
	}))
	defer srv.Close()
	c := NewPriceListClient(srv.URL, WithPriceListRetries(3, time.Millisecond), WithPriceListHTTPClient(srv.Client()))
	got, err := c.AutoModeFees(context.Background(), "us-east-1")
	if err != nil || got["m5.large"] != 0.01 || calls.Load() != 3 {
		t.Errorf("got %v %v after %d calls", got, err, calls.Load())
	}

	calls.Store(0)
	c = NewPriceListClient(srv.URL, WithPriceListRetries(2, time.Millisecond))
	if _, err := c.AutoModeFees(context.Background(), "us-east-1"); err == nil || calls.Load() != 2 {
		t.Errorf("out of retries: %v after %d calls", err, calls.Load())
	}

	// 4xx is not retried; a cancelled context stops the retries.
	var forbidden atomic.Int32
	srv403 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		forbidden.Add(1)
		http.Error(w, "no", http.StatusForbidden)
	}))
	defer srv403.Close()
	if _, err := NewPriceListClient(srv403.URL).AutoModeFees(context.Background(), "us-east-1"); err == nil || forbidden.Load() != 1 {
		t.Errorf("403: %v after %d calls", err, forbidden.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewPriceListClient(srv.URL).AutoModeFees(ctx, "us-east-1"); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}

// countingSource counts downloads per region.
type countingSource struct {
	mu    sync.Mutex
	calls map[string]int
	fees  map[string]float64
	err   error
}

func (s *countingSource) AutoModeFees(ctx context.Context, region string) (map[string]float64, error) {
	s.mu.Lock()
	s.calls[region]++
	s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.fees, s.err
}

func TestFeeCache(t *testing.T) {
	src := &countingSource{calls: map[string]int{}, fees: map[string]float64{"m5.large": 0.01}}
	cache := NewFeeCache(src)
	ctx := context.Background()
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			if got, err := cache.AutoModeFees(ctx, "us-east-1"); err != nil || got["m5.large"] != 0.01 {
				t.Errorf("got %v %v", got, err)
			}
		})
	}
	wg.Wait()
	if _, err := cache.AutoModeFees(ctx, "us-east-2"); err != nil {
		t.Fatal(err)
	}
	if src.calls["us-east-1"] != 1 || src.calls["us-east-2"] != 1 {
		t.Errorf("calls %v", src.calls)
	}

	// Errors are memoised for the cycle, cancellations are not.
	failing := &countingSource{calls: map[string]int{}, err: errors.New("boom")}
	cache = NewFeeCache(failing)
	for range 2 {
		if _, err := cache.AutoModeFees(ctx, "us-east-1"); err == nil {
			t.Error("error not returned")
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, _ = cache.AutoModeFees(cancelled, "eu-west-1")
	_, _ = cache.AutoModeFees(ctx, "eu-west-1")
	if failing.calls["us-east-1"] != 1 || failing.calls["eu-west-1"] != 2 {
		t.Errorf("calls %v", failing.calls)
	}
}

func TestAdapterManagedFees(t *testing.T) {
	fees := fixtureFees(t)
	srv := keepertest.NewPriceList(t, map[string]map[string]float64{"us-east-1": fees})
	src := NewFeeCache(NewPriceListClient(srv.URL))
	ctx := context.Background()
	got, err := mustAdapter(t, AWSAutoMode).ManagedFees(ctx, src, "us-east-1")
	if err != nil || !maps.Equal(got, fees) {
		t.Errorf("aws-auto: %v %v", got, err)
	}
	if _, err := mustAdapter(t, AWSAutoMode).ManagedFees(ctx, nil, "us-east-1"); err == nil {
		t.Error("aws-auto without a fee source")
	}
	for _, a := range registry.All() {
		if a.ID() == AWSAutoMode {
			continue
		}
		if got, err := a.ManagedFees(ctx, nil, "us-east-1"); got != nil || err != nil {
			t.Errorf("%s: %v %v", a.ID(), got, err)
		}
	}
}

// TestSnapshotPriceList: the snapshot replay server answers price list
// requests from the snapshot's fee table, or with HTTP 404 when the
// snapshot has none.
func TestSnapshotPriceList(t *testing.T) {
	c := NewPriceListClient(snapshotURL, WithPriceListRetries(1, 0))
	region, _ := manifest.KeeperRegion("aws")
	got, err := c.AutoModeFees(context.Background(), region)
	if manifest.EKSAutoModeFees == nil {
		if err == nil || !strings.Contains(err.Error(), "no EKS Auto Mode fee table") {
			t.Errorf("snapshot without fees: %v %v", got, err)
		}
		return
	}
	want, _ := autoModeFees(t)
	if err != nil || !maps.Equal(got, want) {
		t.Errorf("snapshot fees: %d entries, %v", len(got), err)
	}
}
