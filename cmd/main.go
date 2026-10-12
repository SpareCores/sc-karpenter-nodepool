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
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	karpenterv1alpha1 "github.com/SpareCores/sc-karpenter-nodepool/api/v1alpha1"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/controller"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/keeper"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/nodepool"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/providers"
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

// envInt is envString for integers; an unparsable value yields def.
func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return def
}

// envDuration is envString for durations; an unparsable value yields def.
func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// keeperAPIKey returns the Keeper API key: the content of keyFile if set,
// else SCKN_KEEPER_API_KEY (empty: anonymous access).
func keeperAPIKey(keyFile string) (string, error) {
	if keyFile == "" {
		return os.Getenv("SCKN_KEEPER_API_KEY"), nil
	}
	b, err := os.ReadFile(keyFile)
	if err != nil {
		return "", fmt.Errorf("reading the Keeper API key: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
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
	var keeperURL, keeperKeyFile, priceListURL, nodeClassRefOverride string
	var keeperTimeout, dataTTL, discoveryInterval time.Duration
	var maxConcurrent, regionsPerRequest int

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
	flag.StringVar(&keeperURL, "keeper-url", envString("SCKN_KEEPER_URL", keeper.DefaultURL),
		"Base URL of the Spare Cores Keeper API. [env SCKN_KEEPER_URL]")
	flag.StringVar(&keeperKeyFile, "keeper-api-key-file", envString("SCKN_KEEPER_API_KEY_FILE", ""),
		"File holding an optional Keeper API key (for example a mounted Secret); "+
			"without it the key is read from SCKN_KEEPER_API_KEY, else access is anonymous. [env SCKN_KEEPER_API_KEY_FILE]")
	flag.DurationVar(&keeperTimeout, "keeper-timeout", envDuration("SCKN_KEEPER_TIMEOUT", 2*time.Minute),
		"Timeout of one Keeper HTTP request. [env SCKN_KEEPER_TIMEOUT]")
	flag.IntVar(&regionsPerRequest, "keeper-regions-per-request", envInt("SCKN_KEEPER_REGIONS_PER_REQUEST", -1),
		"Regions per Keeper /servers request; -1: 3 anonymously (Keeper's limit), unlimited with an API key. "+
			"[env SCKN_KEEPER_REGIONS_PER_REQUEST]")
	flag.DurationVar(&dataTTL, "data-cache-ttl", envDuration("SCKN_DATA_CACHE_TTL", controller.DefaultDataTTL),
		"How long Keeper responses and EKS Auto Mode fees are shared between SCNodePool refreshes. [env SCKN_DATA_CACHE_TTL]")
	flag.StringVar(&priceListURL, "aws-price-list-url",
		envString("SCKN_AWS_PRICE_LIST_URL", providers.DefaultPriceListURL),
		"AWS public price list endpoint (EKS Auto Mode fees). [env SCKN_AWS_PRICE_LIST_URL]")
	flag.IntVar(&maxConcurrent, "max-concurrent-reconciles", envInt("SCKN_MAX_CONCURRENT_RECONCILES", 4),
		"SCNodePools reconciled (refreshed) concurrently. [env SCKN_MAX_CONCURRENT_RECONCILES]")
	flag.DurationVar(&discoveryInterval, "nodeclass-discovery-interval",
		envDuration("SCKN_NODECLASS_DISCOVERY_INTERVAL", controller.DefaultNodeClassDiscoveryInterval),
		"How often to look for newly installed NodeClass CRDs to watch. [env SCKN_NODECLASS_DISCOVERY_INTERVAL]")
	flag.StringVar(&nodeClassRefOverride, "node-class-ref-override", envString("SCKN_NODE_CLASS_REF_OVERRIDE", ""),
		"Test mode only: group/kind/name replacing the nodeClassRef of every generated NodePool "+
			"(the provider adapter is still chosen from the template). [env SCKN_NODE_CLASS_REF_OVERRIDE]")
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

	var override *nodepool.NodeClassRef
	if nodeClassRefOverride != "" {
		ref, err := nodepool.ParseNodeClassRef(nodeClassRefOverride)
		if err != nil {
			setupLog.Error(err, "Invalid --node-class-ref-override")
			os.Exit(1)
		}
		override = &ref
		setupLog.Info("Test mode: overriding the nodeClassRef of generated NodePools", "nodeClassRef", ref.String())
	}
	apiKey, err := keeperAPIKey(keeperKeyFile)
	if err != nil {
		setupLog.Error(err, "Failed to read the Keeper API key")
		os.Exit(1)
	}
	keeperOpts := []keeper.Option{keeper.WithHTTPClient(controller.NewKeeperHTTPClient(keeperTimeout))}
	if apiKey != "" {
		keeperOpts = append(keeperOpts, keeper.WithAPIKey(apiKey))
		if regionsPerRequest < 0 {
			regionsPerRequest = 0
		}
	}
	if regionsPerRequest >= 0 {
		keeperOpts = append(keeperOpts, keeper.WithRegionsPerRequest(regionsPerRequest))
	}
	data := controller.NewDataSource(keeper.New(keeperURL, keeperOpts...),
		providers.NewPriceListClient(priceListURL), dataTTL)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Cache:  controller.CacheOptions(),
		// NodePools, NodeClaims and NodeClasses are read as unstructured
		// objects; serve them from the informers the controller watches.
		Client:                 client.Options{Cache: &client.CacheOptions{Unstructured: true}},
		Metrics:                metricsServerOptions,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "sc-karpenter-nodepool.sparecores.com",
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()
	if err := (&controller.SCNodePoolReconciler{
		Client:               mgr.GetClient(),
		Scheme:               mgr.GetScheme(),
		Registry:             providers.NewRegistry(),
		Data:                 data,
		NodeClassRefOverride: override,
	}).SetupWithManager(ctx, mgr, controller.SetupOptions{
		MaxConcurrentReconciles:    maxConcurrent,
		NodeClassDiscoveryInterval: discoveryInterval,
	}); err != nil {
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
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}
