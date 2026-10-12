/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package controller

import (
	"sync"
	"time"

	"github.com/SpareCores/sc-karpenter-nodepool/internal/keeper"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/providers"
)

// DefaultDataTTL is how long Keeper responses and Auto Mode fees are shared
// between refreshes (Keeper responses are cacheable for an hour).
const DefaultDataTTL = 10 * time.Minute

// DataSource hands out the caches of the current refresh cycle: a Keeper
// Fetcher and an Auto Mode fee cache, shared by every SCNodePool refreshed
// within the TTL so that identical queries hit Keeper once. A cycle that saw
// an error is dropped, so a transient failure is not memoised for the rest
// of the TTL. Safe for concurrent use.
type DataSource struct {
	keeper *keeper.Client
	fees   providers.FeeSource
	ttl    time.Duration
	now    func() time.Time

	mu  sync.Mutex
	cur *DataCycle
}

// DataCycle are the caches of one refresh cycle.
type DataCycle struct {
	Fetcher *keeper.Fetcher
	// fees is nil without a fee source.
	fees    *providers.FeeCache
	started time.Time
}

// FeeSource returns the cycle's Auto Mode fee cache, or nil without one.
func (c *DataCycle) FeeSource() providers.FeeSource {
	if c.fees == nil {
		return nil
	}
	return c.fees
}

// NewDataSource reads Keeper through k and Auto Mode fees through fees
// (nil: Auto Mode pools fail with DataStale); ttl <= 0 uses DefaultDataTTL.
func NewDataSource(k *keeper.Client, fees providers.FeeSource, ttl time.Duration) *DataSource {
	if ttl <= 0 {
		ttl = DefaultDataTTL
	}
	return &DataSource{keeper: k, fees: fees, ttl: ttl, now: time.Now}
}

// Cycle returns the current cycle, starting a new one when the TTL expired.
func (d *DataSource) Cycle() *DataCycle {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	if d.cur == nil || now.Sub(d.cur.started) >= d.ttl {
		c := &DataCycle{Fetcher: keeper.NewFetcher(d.keeper), started: now}
		if d.fees != nil {
			c.fees = providers.NewFeeCache(d.fees)
		}
		d.cur = c
	}
	return d.cur
}

// Invalidate drops c if it is still the current cycle.
func (d *DataSource) Invalidate(c *DataCycle) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cur == c {
		d.cur = nil
	}
}
