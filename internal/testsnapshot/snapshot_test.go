/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package testsnapshot

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SpareCores/sc-keeper-go/keepertest"
)

func get(t *testing.T, s *keepertest.Server, method, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, s.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func manifest(t *testing.T) *Manifest {
	t.Helper()
	m, err := LoadManifest(Dir())
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestSnapshotReplay(t *testing.T) {
	s := New(t)
	for vendor, region := range map[string]string{"aws": "us-east-1", "gcp": "1000", "hcloud": "4"} {
		if got, ok := s.Manifest().KeeperRegion(vendor); !ok || got != region {
			t.Errorf("KeeperRegion(%s) = %q, %v", vendor, got, ok)
		}
	}
	if code, body := get(t, s, http.MethodGet, "/table/zone"); code != http.StatusOK || !strings.HasPrefix(body, "[") {
		t.Errorf("zones: %d %.100s", code, body)
	}
}

func TestPriceListReplay(t *testing.T) {
	// The committed snapshot, with or without a fee table.
	s := New(t)
	code, body := get(t, s, http.MethodGet, "/offers/v1.0/aws/AmazonEKS/current/us-east-1/index.json")
	if manifest(t).EKSAutoModeFees == nil {
		if code != http.StatusNotFound || !strings.Contains(body, "no EKS Auto Mode fee table") {
			t.Errorf("snapshot without fees: %d %.100s", code, body)
		}
	} else if code != http.StatusOK || !strings.Contains(body, `"eksproducttype":"AutoMode"`) {
		t.Errorf("snapshot with fees: %d %.100s", code, body)
	}

	// A snapshot with a (gzipped) fee table.
	dir := t.TempDir()
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte(`{"m5.large": 0.01, "c7g.xlarge": 0.02}`))
	_ = zw.Close()
	if err := os.MkdirAll(filepath.Join(dir, "aws"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "aws", "eks-automode-fees.json.gz"), gz.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := `{"keeper": {"database_last_updated": "2026-01-01T00:00:00+00:00"}, "queries": [],
		"eks_automode_fees": {"region": "us-east-1", "file": "aws/eks-automode-fees.json.gz", "rows": 2}}`
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := NewHandler(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := keepertest.NewServer(t, h)
	code, body = get(t, srv, http.MethodGet, "/offers/v1.0/aws/AmazonEKS/current/us-east-1/index.json")
	var offer struct {
		Products map[string]struct {
			Attributes map[string]string `json:"attributes"`
		} `json:"products"`
		Terms struct {
			OnDemand map[string]json.RawMessage `json:"OnDemand"`
		} `json:"terms"`
	}
	if err := json.Unmarshal([]byte(body), &offer); err != nil || code != http.StatusOK {
		t.Fatalf("%d %v %.200s", code, err, body)
	}
	var types []string
	for sku, p := range offer.Products {
		if p.Attributes["eksproducttype"] == "AutoMode" {
			types = append(types, p.Attributes["instancetype"])
		}
		if _, ok := offer.Terms.OnDemand[sku]; !ok {
			t.Errorf("product %s has no on-demand term", sku)
		}
	}
	slices.Sort(types)
	if len(offer.Products) != 3 || !slices.Equal(types, []string{"c7g.xlarge", "m5.large"}) {
		t.Errorf("products %v, Auto Mode types %v", offer.Products, types)
	}
	for _, path := range []string{
		"/offers/v1.0/aws/AmazonEKS/current/us-west-2/index.json",
		"/offers/v1.0/aws/AmazonEKS/current/us-east-1/other.json",
	} {
		if code, _ := get(t, srv, http.MethodGet, path); code != http.StatusNotFound {
			t.Errorf("%s: %d", path, code)
		}
	}
	if code, _ := get(t, srv, http.MethodPost, "/offers/v1.0/aws/AmazonEKS/current/us-east-1/index.json"); code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", code)
	}

	// A manifest naming a missing fee file is broken.
	if err := os.Remove(filepath.Join(dir, "aws", "eks-automode-fees.json.gz")); err != nil {
		t.Fatal(err)
	}
	if _, err := NewHandler(dir); err == nil {
		t.Error("missing fee file accepted")
	}
}
