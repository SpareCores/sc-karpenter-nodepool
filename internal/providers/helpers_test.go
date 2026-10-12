/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package providers

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/SpareCores/sc-karpenter-nodepool/internal/keeper"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/keeper/keepertest"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/selection"
)

// The snapshot replay server, its region and zone tables and one Fetcher are
// shared by all tests of the package.
var (
	snapshotURL string
	fetcher     *keeper.Fetcher
	manifest    *keepertest.Manifest
	regionIx    *RegionIndex
	zoneIx      *ZoneIndex
	registry    = NewRegistry()
)

func TestMain(m *testing.M) {
	os.Exit(func() int {
		h, err := keepertest.NewHandler(keepertest.SnapshotDir())
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		srv := httptest.NewServer(h)
		defer srv.Close()
		snapshotURL = srv.URL
		manifest = h.Manifest()
		fetcher = keeper.NewFetcher(keeper.New(srv.URL))
		ctx := context.Background()
		regions, err := fetcher.Regions(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		zones, err := fetcher.Zones(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		regionIx, zoneIx = NewRegionIndex(regions), NewZoneIndex(zones)
		return m.Run()
	}())
}

func mustAdapter(t *testing.T, id ID) Adapter {
	t.Helper()
	a, ok := registry.Get(id)
	if !ok {
		t.Fatalf("no adapter %s", id)
	}
	return a
}

// snapshotCandidates returns the candidates of a vendor's base query in
// its snapshot region.
func snapshotCandidates(t *testing.T, vendor string, alloc keeper.Allocation) []*selection.Candidate {
	t.Helper()
	region, ok := manifest.KeeperRegion(vendor)
	if !ok {
		t.Fatalf("snapshot has no region for %s", vendor)
	}
	q := keeper.Query{Vendor: vendor, Regions: []string{region}, Allocation: alloc}
	rows, err := fetcher.Servers(context.Background(), q)
	if err != nil {
		t.Fatalf("fetching %s: %v", vendor, err)
	}
	return selection.BuildCandidates([]keeper.QueryResult{{Query: q, Servers: rows}})
}

// fixtureFees is the synthetic Auto Mode fee table under testdata (values
// are made up, the names are real AWS types).
func fixtureFees(t *testing.T) map[string]float64 {
	t.Helper()
	fees, err := keepertest.ReadFeeTable(filepath.Join("testdata", "eks-automode-fees.json"))
	if err != nil {
		t.Fatal(err)
	}
	return fees
}

// autoModeFees returns the snapshot's Auto Mode fee table of the aws test
// region when the snapshot has one, else the synthetic fixture.
func autoModeFees(t *testing.T) (map[string]float64, bool) {
	t.Helper()
	region, fees, ok, err := keepertest.AutoModeFees(keepertest.SnapshotDir(), manifest)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		if want, _ := manifest.KeeperRegion("aws"); region != want {
			t.Fatalf("fee table region %s, aws snapshot region %s", region, want)
		}
		return fees, true
	}
	return fixtureFees(t), false
}
