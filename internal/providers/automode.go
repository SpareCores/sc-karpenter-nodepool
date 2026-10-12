/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FeeSource returns the EKS Auto Mode management fees of an AWS region
// (instance type -> USD per instance-hour).
type FeeSource interface {
	AutoModeFees(ctx context.Context, region string) (map[string]float64, error)
}

// DefaultPriceListURL is the AWS public price list endpoint.
const DefaultPriceListURL = "https://pricing.us-east-1.amazonaws.com"

// PriceListPath is the AmazonEKS offer file of a region, relative to the
// price list endpoint.
func PriceListPath(region string) string {
	return "/offers/v1.0/aws/AmazonEKS/current/" + url.PathEscape(region) + "/index.json"
}

// PriceListClient downloads Auto Mode fees from the AWS public price list:
// the AmazonEKS offer file of the region lists one product per Auto
// Mode-eligible instance type (attribute eksproducttype=AutoMode), and its
// on-demand term is the hourly fee (overview §5.4). It does not cache; wrap
// it in a FeeCache per refresh cycle.
type PriceListClient struct {
	baseURL  string
	http     *http.Client
	attempts int
	delay    time.Duration
}

// PriceListOption configures a PriceListClient.
type PriceListOption func(*PriceListClient)

// WithPriceListHTTPClient replaces the HTTP client (default: 2 minute
// timeout).
func WithPriceListHTTPClient(h *http.Client) PriceListOption {
	return func(c *PriceListClient) { c.http = h }
}

// WithPriceListRetries sets the number of attempts and the delay between
// them (default 3 attempts, 5s apart).
func WithPriceListRetries(attempts int, delay time.Duration) PriceListOption {
	return func(c *PriceListClient) { c.attempts, c.delay = max(attempts, 1), delay }
}

// NewPriceListClient returns a client of the price list at baseURL
// (DefaultPriceListURL if empty).
func NewPriceListClient(baseURL string, opts ...PriceListOption) *PriceListClient {
	if baseURL == "" {
		baseURL = DefaultPriceListURL
	}
	c := &PriceListClient{
		baseURL:  strings.TrimRight(baseURL, "/"),
		http:     &http.Client{Timeout: 2 * time.Minute},
		attempts: 3,
		delay:    5 * time.Second,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// AutoModeFees downloads and parses the region's offer file. A region
// without Auto Mode products fails with ErrNoAutoModeFees rather than
// returning an empty table, which would make every type ineligible.
func (c *PriceListClient) AutoModeFees(ctx context.Context, region string) (map[string]float64, error) {
	u := c.baseURL + PriceListPath(region)
	var lastErr error
	for attempt := range c.attempts {
		if attempt > 0 {
			t := time.NewTimer(c.delay)
			select {
			case <-ctx.Done():
				t.Stop()
				return nil, fmt.Errorf("%w (last error: %w)", ctx.Err(), lastErr)
			case <-t.C:
			}
		}
		fees, retry, err := c.fetch(ctx, u)
		if err == nil {
			if len(fees) == 0 {
				return nil, fmt.Errorf("%w: %s lists no Auto Mode product", ErrNoAutoModeFees, u)
			}
			return fees, nil
		}
		lastErr = err
		if !retry || ctx.Err() != nil {
			break
		}
	}
	return nil, lastErr
}

func (c *PriceListClient) fetch(ctx context.Context, u string) (map[string]float64, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, false, fmt.Errorf("building request %s: %w", u, err)
	}
	req.Header.Set("User-Agent", "sc-karpenter-nodepool")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, true, fmt.Errorf("GET %s: %w", u, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		retry := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return nil, retry, fmt.Errorf("GET %s: HTTP %d: %s", u, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	fees, err := ParseAutoModeFees(resp.Body)
	if err != nil {
		// A truncated body is worth another try.
		return nil, true, fmt.Errorf("GET %s: %w", u, err)
	}
	return fees, false, nil
}

// offerFile is the subset of an AWS price list offer file the parser reads.
type offerFile struct {
	Products map[string]struct {
		Attributes map[string]string `json:"attributes"`
	} `json:"products"`
	Terms struct {
		// OnDemand is keyed by SKU, then by offer term code.
		OnDemand map[string]map[string]offerTerm `json:"OnDemand"`
	} `json:"terms"`
}

type offerTerm struct {
	PriceDimensions map[string]struct {
		Unit         string            `json:"unit"`
		PricePerUnit map[string]string `json:"pricePerUnit"`
	} `json:"priceDimensions"`
}

// ParseAutoModeFees extracts {instance type: hourly USD fee} from an
// AmazonEKS offer file. Products other than eksproducttype=AutoMode are
// ignored; an Auto Mode product without a usable hourly USD price is an
// error, as the format would have changed.
func ParseAutoModeFees(r io.Reader) (map[string]float64, error) {
	var f offerFile
	if err := json.NewDecoder(r).Decode(&f); err != nil {
		return nil, fmt.Errorf("decoding price list: %w", err)
	}
	fees := map[string]float64{}
	var errs []error
	for sku, p := range f.Products {
		if p.Attributes["eksproducttype"] != "AutoMode" {
			continue
		}
		it := p.Attributes["instancetype"]
		if it == "" {
			errs = append(errs, fmt.Errorf("product %s: Auto Mode without instancetype", sku))
			continue
		}
		fee, err := hourlyUSD(f.Terms.OnDemand[sku])
		if err != nil {
			errs = append(errs, fmt.Errorf("product %s (Auto Mode, %s): %w", sku, it, err))
			continue
		}
		fees[it] = fee
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("parsing price list: %w", errors.Join(errs...))
	}
	return fees, nil
}

// hourlyUSD returns the single hourly USD price of a product's on-demand
// terms.
func hourlyUSD(terms map[string]offerTerm) (float64, error) {
	var prices []float64
	for _, t := range terms {
		for _, d := range t.PriceDimensions {
			if u := strings.ToLower(d.Unit); u != "hours" && u != "hrs" {
				return 0, fmt.Errorf("unexpected price unit %q", d.Unit)
			}
			v, err := strconv.ParseFloat(d.PricePerUnit["USD"], 64)
			if err != nil {
				return 0, fmt.Errorf("bad USD price %q: %w", d.PricePerUnit["USD"], err)
			}
			prices = append(prices, v)
		}
	}
	if len(prices) != 1 {
		return 0, fmt.Errorf("%d on-demand prices, want 1", len(prices))
	}
	return prices[0], nil
}

// FeeCache memoises a FeeSource for one refresh cycle (errors included,
// except those of a cancelled caller); create a new one per cycle. It is
// safe for concurrent use and concurrent calls for one region share one
// download.
type FeeCache struct {
	src FeeSource

	mu      sync.Mutex
	entries map[string]*feeEntry
}

type feeEntry struct {
	done chan struct{}
	fees map[string]float64
	err  error
}

// NewFeeCache wraps src.
func NewFeeCache(src FeeSource) *FeeCache {
	return &FeeCache{src: src, entries: map[string]*feeEntry{}}
}

// AutoModeFees returns (or reuses) the fees of a region. The map is shared
// and must not be modified.
func (c *FeeCache) AutoModeFees(ctx context.Context, region string) (map[string]float64, error) {
	c.mu.Lock()
	e, ok := c.entries[region]
	if !ok {
		e = &feeEntry{done: make(chan struct{})}
		c.entries[region] = e
		c.mu.Unlock()
		e.fees, e.err = c.src.AutoModeFees(ctx, region)
		if e.err != nil && ctx.Err() != nil {
			c.mu.Lock()
			delete(c.entries, region)
			c.mu.Unlock()
		}
		close(e.done)
		return e.fees, e.err
	}
	c.mu.Unlock()
	select {
	case <-e.done:
		return e.fees, e.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
