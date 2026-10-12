/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package controller

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	metricsNamespace = "sc_karpenter_nodepool"
	labelSCNodePool  = "scnodepool"
)

var (
	refreshDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: metricsNamespace,
		Name:      "refresh_duration_seconds",
		Help:      "Duration of SCNodePool refreshes (Keeper queries, selection, NodePool apply) by result (the Ready reason).",
		Buckets:   prometheus.ExponentialBuckets(0.05, 2, 12),
	}, []string{"result"})
	keeperRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricsNamespace,
		Name:      "keeper_requests_total",
		Help:      "HTTP requests to the Spare Cores Keeper API by endpoint and status code (0: no response).",
	}, []string{"endpoint", "code"})
	keeperErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricsNamespace,
		Name:      "keeper_request_errors_total",
		Help:      "Keeper API requests that failed (transport error or non-2xx status) by endpoint.",
	}, []string{"endpoint"})
	selectedTypes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: metricsNamespace,
		Name:      "selected_instance_types",
		Help:      "Instance types in the (would-be) NodePool of each SCNodePool.",
	}, []string{labelSCNodePool})
	conditionStatus = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: metricsNamespace,
		Name:      "condition",
		Help:      "Status conditions of each SCNodePool: 1 for the current status of a condition type, 0 for the others.",
	}, []string{labelSCNodePool, "type", "status"})
)

func init() {
	metrics.Registry.MustRegister(refreshDuration, keeperRequests, keeperErrors, selectedTypes, conditionStatus)
}

var conditionStatuses = []metav1.ConditionStatus{metav1.ConditionTrue, metav1.ConditionFalse, metav1.ConditionUnknown}

// recordConditions exports the conditions of one SCNodePool.
func recordConditions(name string, conds []metav1.Condition) {
	conditionStatus.DeletePartialMatch(prometheus.Labels{labelSCNodePool: name})
	for _, c := range conds {
		for _, s := range conditionStatuses {
			v := 0.0
			if c.Status == s {
				v = 1
			}
			conditionStatus.WithLabelValues(name, c.Type, string(s)).Set(v)
		}
	}
}

// forgetMetrics drops every series of a deleted SCNodePool.
func forgetMetrics(name string) {
	conditionStatus.DeletePartialMatch(prometheus.Labels{labelSCNodePool: name})
	selectedTypes.DeleteLabelValues(name)
}

// keeperEndpoints are the paths the client uses; anything else is "other",
// so the label stays bounded.
var keeperEndpoints = map[string]bool{
	"/servers": true, "/benchmark_configs": true, "/table/region": true, "/table/zone": true, "/healthcheck": true,
}

// instrumentedTransport counts Keeper requests and errors.
type instrumentedTransport struct {
	base http.RoundTripper
}

func (t instrumentedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	endpoint := req.URL.Path
	if !keeperEndpoints[endpoint] {
		endpoint = "other"
	}
	resp, err := t.base.RoundTrip(req)
	code := 0
	if resp != nil {
		code = resp.StatusCode
	}
	keeperRequests.WithLabelValues(endpoint, strconv.Itoa(code)).Inc()
	if err != nil || code < 200 || code > 299 {
		keeperErrors.WithLabelValues(endpoint).Inc()
	}
	return resp, err
}

// NewKeeperHTTPClient returns the HTTP client for the Keeper API: the given
// timeout, with request metrics.
func NewKeeperHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: instrumentedTransport{base: http.DefaultTransport}}
}
