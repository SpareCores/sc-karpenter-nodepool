/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Command main is the entrypoint of the sc-karpenter-nodepool manager.
package main

import (
	"crypto/tls"
	"flag"
	"os"
	"strconv"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	karpenterv1alpha1 "github.com/SpareCores/sc-karpenter-nodepool/api/v1alpha1"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/controller"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(karpenterv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// envString returns the value of the environment variable key, or def if it is
// unset or empty. Every flag falls back to an SCKN_* variable this way, so the
// Deployment can be configured with either.
func envString(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envBool is envString for booleans; an unparsable value yields def.
func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func main() {
	var metricsAddr, probeAddr string
	var enableLeaderElection, secureMetrics, enableHTTP2 bool

	flag.StringVar(&metricsAddr, "metrics-bind-address", envString("SCKN_METRICS_BIND_ADDRESS", "0"),
		"The address the metrics endpoint binds to; 0 disables it. [env SCKN_METRICS_BIND_ADDRESS]")
	flag.StringVar(&probeAddr, "health-probe-bind-address", envString("SCKN_HEALTH_PROBE_BIND_ADDRESS", ":8081"),
		"The address the probe endpoint binds to. [env SCKN_HEALTH_PROBE_BIND_ADDRESS]")
	flag.BoolVar(&enableLeaderElection, "leader-elect", envBool("SCKN_LEADER_ELECT", false),
		"Enable leader election, so only one manager is active. [env SCKN_LEADER_ELECT]")
	flag.BoolVar(&secureMetrics, "metrics-secure", envBool("SCKN_METRICS_SECURE", true),
		"Serve metrics over HTTPS with authn/authz; use --metrics-secure=false for plain HTTP. [env SCKN_METRICS_SECURE]")
	flag.BoolVar(&enableHTTP2, "enable-http2", envBool("SCKN_ENABLE_HTTP2", false),
		"Enable HTTP/2 for the metrics server. [env SCKN_ENABLE_HTTP2]")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	var tlsOpts []func(*tls.Config)
	if !enableHTTP2 {
		// HTTP/2 is off by default because of the Stream Cancellation and
		// Rapid Reset CVEs (GHSA-qppj-fm5r-hxr3, GHSA-4374-p667-p6c8).
		tlsOpts = append(tlsOpts, func(c *tls.Config) {
			setupLog.Info("Disabling HTTP/2")
			c.NextProtos = []string{"http/1.1"}
		})
	}

	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}
	if secureMetrics {
		// Only authorised users and service accounts may read the metrics;
		// the RBAC for this is in config/rbac.
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "sc-karpenter-nodepool.sparecores.com",
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	if err := (&controller.SCNodePoolReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "scnodepool")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}
