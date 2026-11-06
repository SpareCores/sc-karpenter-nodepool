/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package testsnapshot

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SpareCores/sc-keeper-go/snapshot"
)

// hack/sc-snapshot/queries.yaml is what the refresh workflow fetches. Run
// against the replay of the committed snapshot, it must reproduce the query
// set of the committed manifest: names, files, paths, parameters, row counts
// and SHA-256 of the data.
func TestQueryListReproducesTheCommittedManifest(t *testing.T) {
	list, err := snapshot.LoadQueryList(filepath.Join("..", "..", "hack", "sc-snapshot", "queries.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	srv := New(t)
	snap, err := snapshot.Fetch(context.Background(), list, snapshot.Options{
		KeeperURL: srv.URL,
		Sleep:     func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(Dir(), "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var committed snapshot.Manifest
	if err := json.Unmarshal(raw, &committed); err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(struct {
		Keeper  snapshot.ManifestKeeper
		Vendors map[string]snapshot.ManifestVendor
		Queries []snapshot.ManifestQuery
	}{committed.Keeper, committed.Vendors, committed.Queries})
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(struct {
		Keeper  snapshot.ManifestKeeper
		Vendors map[string]snapshot.ManifestVendor
		Queries []snapshot.ManifestQuery
	}{snap.Manifest.Keeper, snap.Manifest.Vendors, snap.Manifest.Queries})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("the query list does not reproduce the committed manifest:\n got %s\nwant %s", got, want)
	}
}
