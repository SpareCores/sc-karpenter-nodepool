/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package testsnapshot replays the committed Spare Cores snapshot of this
// repository (test/data/sc-snapshot) as a fake Keeper API, so tests never
// touch the network. It is keepertest of github.com/SpareCores/sc-keeper-go
// pointed at the repository's own snapshot, plus the AWS public price list
// (EKS Auto Mode fees) the snapshot carries.
//
// When the snapshot has an EKS Auto Mode fee table (manifest
// eks_automode_fees), the same server also answers the AWS price list requests
// for its region (PriceListHandler); without one, they get HTTP 404.
package testsnapshot

import (
	"net/http"
	"path/filepath"
	"runtime"

	"github.com/SpareCores/sc-keeper-go/keepertest"
)

// Dir is the absolute path of the committed snapshot.
func Dir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "test", "data", "sc-snapshot")
}

// NewHandler loads the snapshot in dir (Dir() for the committed one) and
// mounts the price list on it.
func NewHandler(dir string) (*keepertest.Handler, error) {
	h, err := keepertest.NewHandler(dir)
	if err != nil {
		return nil, err
	}
	m, err := LoadManifest(dir)
	if err != nil {
		return nil, err
	}
	region, fees, ok, err := AutoModeFees(dir, m)
	if err != nil {
		return nil, err
	}
	var priceList http.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "testsnapshot: the snapshot has no EKS Auto Mode fee table", http.StatusNotFound)
	})
	if ok {
		priceList = NewPriceListHandler(map[string]map[string]float64{region: fees})
	}
	h.Mount(priceListPrefix, priceList)
	return h, nil
}

// New starts a replay server of the committed snapshot; it is closed when the
// test ends.
func New(t keepertest.TB) *keepertest.Server {
	t.Helper()
	h, err := NewHandler(Dir())
	if err != nil {
		t.Fatalf("testsnapshot: %v", err)
	}
	return keepertest.NewServer(t, h)
}
