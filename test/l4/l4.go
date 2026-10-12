/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package l4 holds the shared parts of the in-process provider tests (L4,
// docs/plans/v1/wip/00-overview.md §8.4): rendering NodePools for synthetic
// SCNodePools with the controller's own internal/nodepool and
// internal/providers code, keeping the tests away from real cloud endpoints,
// and the drift checks of Karpenter core.
//
// The provider tests (subpackages aws and azure) run the providers' real
// GetInstanceTypes and Create against their in-memory cloud fakes and an
// envtest API server; nothing here talks to a cloud.
package l4

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/SpareCores/sc-karpenter-nodepool/api/v1alpha1"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/nodepool"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/providers"
)

// Guard is the network safety net of a test binary (CONVENTIONS rule 8):
// the provider environments use in-memory fakes only, and Guard makes sure
// no code path reaches a real endpoint anyway.
type Guard struct {
	ln       net.Listener
	mu       sync.Mutex
	attempts []string
}

// Isolate must run in TestMain before any HTTP client is used (Go reads the
// proxy environment once):
//
//   - Cloud credentials and profiles are removed from the environment, so no
//     default credential chain can find anything.
//   - The AWS instance metadata service (IMDS) is disabled; the region is the
//     fake one of the AWS test environment (us-west-2, fake.DefaultRegion).
//   - HTTP(S) to anything but loopback goes to a local proxy that refuses
//     every request and records it (Attempts); the envtest API server stays
//     reachable on 127.0.0.1.
func Isolate(awsRegion string) (*Guard, error) {
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "AWS_") || strings.HasPrefix(name, "AZURE_") || strings.HasPrefix(name, "ARM_") ||
			strings.HasPrefix(name, "MSI_") || strings.HasPrefix(name, "IDENTITY_") {
			if err := os.Unsetenv(name); err != nil {
				return nil, err
			}
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	g := &Guard{ln: ln}
	go g.serve()
	proxy := "http://" + ln.Addr().String()
	for name, value := range map[string]string{
		"AWS_EC2_METADATA_DISABLED": "true",
		"AWS_REGION":                awsRegion,
		"AWS_DEFAULT_REGION":        awsRegion,
		// Empty files: no shared config or credentials of the developer.
		"AWS_CONFIG_FILE":             os.DevNull,
		"AWS_SHARED_CREDENTIALS_FILE": os.DevNull,
		"HTTP_PROXY":                  proxy,
		"HTTPS_PROXY":                 proxy,
		"http_proxy":                  proxy,
		"https_proxy":                 proxy,
		"NO_PROXY":                    "127.0.0.1,localhost,::1",
		"no_proxy":                    "127.0.0.1,localhost,::1",
	} {
		if err := os.Setenv(name, value); err != nil {
			return nil, err
		}
	}
	return g, nil
}

// serve records the request line of every proxied request and drops it.
func (g *Guard) serve() {
	for {
		conn, err := g.ln.Accept()
		if err != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		line, _ := bufio.NewReader(conn).ReadString('\n')
		g.mu.Lock()
		g.attempts = append(g.attempts, strings.TrimSpace(line))
		g.mu.Unlock()
		_ = conn.Close()
	}
}

// Attempts returns the requests that tried to leave the machine.
func (g *Guard) Attempts() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.attempts)
}

// Close stops the proxy.
func (g *Guard) Close() error { return g.ln.Close() }

// Logger is the controllers' logger: silent unless L4_LOG is set (then a
// development logger on stderr, for debugging a failing scenario).
func Logger() logr.Logger {
	if os.Getenv("L4_LOG") == "" {
		return logr.Discard()
	}
	return zap.New(zap.UseDevMode(true))
}

// Scenario is one synthetic SCNodePool and the instance types its
// selection produced (the Keeper side is out of scope here: L1 covers it).
type Scenario struct {
	// Name of the SCNodePool and of the generated NodePool.
	Name string
	// Adapter whose requirement rules render the NodePool.
	Adapter providers.ID
	// NodeClassRef is the template's nodeClassRef.
	NodeClassRef nodepool.NodeClassRef
	// Override replaces the rendered nodeClassRef (test mode, F12): renders
	// a NodePool with one adapter's rules for another provider's NodeClass.
	Override *nodepool.NodeClassRef
	// Requirements are template requirements (copied verbatim).
	Requirements []karpv1.NodeSelectorRequirementWithMinValues
	// Labels are the template's spec.template.metadata.labels.
	Labels map[string]string
	// InstanceTypes are the selected instance types.
	InstanceTypes []string
	// Architectures are the architectures of InstanceTypes.
	Architectures []v1alpha1.Architecture
}

// templateSpec is spec.nodePoolTemplate.spec of the scenario.
func (s Scenario) templateSpec() []byte {
	tmplSpec := map[string]any{
		"nodeClassRef": map[string]any{
			"group": s.NodeClassRef.Group, "kind": s.NodeClassRef.Kind, "name": s.NodeClassRef.Name,
		},
		"expireAfter": "720h",
	}
	if len(s.Requirements) > 0 {
		tmplSpec["requirements"] = s.Requirements
	}
	tmpl := map[string]any{"spec": tmplSpec}
	if len(s.Labels) > 0 {
		tmpl["metadata"] = map[string]any{"labels": s.Labels}
	}
	b, err := json.Marshal(map[string]any{
		"template":   tmpl,
		"disruption": map[string]any{"consolidationPolicy": "WhenEmptyOrUnderutilized", "consolidateAfter": "1m"},
		"limits":     map[string]any{"cpu": "1000"},
	})
	if err != nil {
		panic(err)
	}
	return b
}

// SCNodePool returns the scenario's SCNodePool.
func (s Scenario) SCNodePool() *v1alpha1.SCNodePool {
	return &v1alpha1.SCNodePool{
		ObjectMeta: metav1.ObjectMeta{Name: s.Name, UID: "00000000-0000-4000-8000-000000000004"},
		Spec: v1alpha1.SCNodePoolSpec{
			NodePoolTemplate: v1alpha1.NodePoolTemplate{Spec: runtime.RawExtension{Raw: s.templateSpec()}},
			DeletionPolicy:   v1alpha1.DeletionPolicyDelete,
		},
	}
}

// Rules returns the requirement rules of the scenario's adapter.
func (s Scenario) Rules() (providers.RequirementRules, error) {
	a, ok := providers.NewRegistry().Get(s.Adapter)
	if !ok {
		return providers.RequirementRules{}, fmt.Errorf("no adapter %s", s.Adapter)
	}
	return a.RequirementRules(), nil
}

// Render renders and validates the NodePool the controller would apply,
// with internal/nodepool and the adapter's rules (overview §3.4 steps 7–8).
// dataTime is the volatile data timestamp (metadata.annotations only).
func (s Scenario) Render(ctx context.Context, dataTime time.Time) (*unstructured.Unstructured, error) {
	rules, err := s.Rules()
	if err != nil {
		return nil, err
	}
	sc := s.SCNodePool()
	tmpl, err := nodepool.ParseTemplate(sc.Spec.NodePoolTemplate)
	if err != nil {
		return nil, err
	}
	out, err := nodepool.Render(nodepool.Input{
		SCNodePool:           sc,
		Template:             tmpl,
		InstanceTypes:        s.InstanceTypes,
		Architectures:        s.Architectures,
		Rules:                rules,
		DataTimestamp:        &dataTime,
		NodeClassRefOverride: s.Override,
	})
	if err != nil {
		return nil, err
	}
	if len(out.Invalid) > 0 {
		return nil, fmt.Errorf("invalid instance-type names %v", out.Invalid)
	}
	if err := nodepool.Validate(ctx, out.NodePool, rules); err != nil {
		return nil, err
	}
	return out.NodePool, nil
}

// MissingKeys returns the requirement keys of the NodePool that are not
// labels of the NodeClaim, sorted. Every such key on a multi-valued In
// requirement causes the drift loop of overview §5.1.5.
func MissingKeys(np *karpv1.NodePool, labels map[string]string) []string {
	var missing []string
	for _, r := range np.Spec.Template.Spec.Requirements {
		if _, ok := labels[r.Key]; !ok {
			missing = append(missing, r.Key)
		}
	}
	slices.Sort(missing)
	return slices.Compact(missing)
}

// RequirementsDrifted is Karpenter core's requirement-drift check
// (pkg/controllers/nodeclaim/disruption/drift.go, areRequirementsDrifted, at
// the core version of the provider module): every NodePool requirement must
// be compatible with the NodeClaim's labels, undefined keys not allowed.
func RequirementsDrifted(np *karpv1.NodePool, nc *karpv1.NodeClaim) error {
	nodePoolReq := scheduling.NewNodeSelectorRequirementsWithMinValues(np.Spec.Template.Spec.Requirements...)
	return scheduling.NewLabelRequirements(nc.Labels).Compatible(nodePoolReq)
}

// InstanceTypesOf returns the values of the NodePool's
// node.kubernetes.io/instance-type In requirement.
func InstanceTypesOf(np *karpv1.NodePool) []string {
	for _, r := range np.Spec.Template.Spec.Requirements {
		if r.Key == providers.LabelInstanceType {
			return r.Values
		}
	}
	return nil
}
