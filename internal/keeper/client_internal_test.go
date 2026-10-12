/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package keeper

import (
	"net/http"
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	tests := map[string]time.Duration{
		"":     0,
		"7":    7 * time.Second,
		"-3":   0,
		"soon": 0,
		now.Add(90 * time.Second).Format(http.TimeFormat): 90 * time.Second,
		now.Add(-time.Hour).Format(http.TimeFormat):       0,
	}
	for in, want := range tests {
		if got := parseRetryAfter(in, now); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestBackoffCapped(t *testing.T) {
	c := New("", WithRetries(3, time.Second, 5*time.Second))
	if c.baseURL != DefaultURL {
		t.Errorf("default URL = %q", c.baseURL)
	}
	for attempt, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second} {
		if got := c.backoff(attempt); got != want {
			t.Errorf("backoff(%d) = %v, want %v", attempt, got, want)
		}
	}
	if got := c.backoff(100); got != 5*time.Second {
		t.Errorf("backoff(100) = %v", got)
	}
}

func TestBenchmarkKeyString(t *testing.T) {
	if got := (BenchmarkKey{ID: "a", Config: "{}"}).String(); got != "a {}" {
		t.Errorf("String() = %q", got)
	}
}
