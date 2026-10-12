/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// L4 for Azure/karpenter-provider-azure: the provider's real CloudProvider
// (GetInstanceTypes, Create, IsDrifted) on its pkg/test environment, with
// the in-memory Azure API fakes of pkg/fake (southcentralus) and an envtest
// API server. Provision mode: AKS scriptless (VMs created by the provider).
package azure_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	clock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	corecloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/dynamicresources/deviceallocation"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	coreoptions "sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/state/virtualpods"
	coretest "sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/pkg/test/expectations"
	coretestv1alpha1 "sigs.k8s.io/karpenter/pkg/test/v1alpha1"

	"github.com/Azure/karpenter-provider-azure/pkg/apis"
	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	"github.com/Azure/karpenter-provider-azure/pkg/cloudprovider"
	"github.com/Azure/karpenter-provider-azure/pkg/consts"
	"github.com/Azure/karpenter-provider-azure/pkg/fake"
	"github.com/Azure/karpenter-provider-azure/pkg/operator/options"
	"github.com/Azure/karpenter-provider-azure/pkg/test"

	"github.com/SpareCores/sc-karpenter-nodepool/api/v1alpha1"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/keeper"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/nodepool"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/providers"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/selection"
	"github.com/SpareCores/sc-karpenter-nodepool/test/l4"
)

const nodeClassName = "sc-l4"

// fakeAWSRegion only feeds l4.Isolate; nothing AWS runs here.
const fakeAWSRegion = "us-west-2"

var (
	ctx           context.Context
	env           *coretest.Environment
	azureEnv      *test.Environment
	cloudProvider *cloudprovider.CloudProvider
	h             *l4.Harness
	dataTime      = time.Date(2026, 10, 5, 10, 2, 57, 0, time.UTC)

	aksNodeClassRef = nodepool.NodeClassRef{Group: "karpenter.azure.com", Kind: "AKSNodeClass", Name: nodeClassName}
	amd64           = []v1alpha1.Architecture{v1alpha1.ArchitectureAMD64}
	bothArchs       = []v1alpha1.Architecture{v1alpha1.ArchitectureAMD64, v1alpha1.ArchitectureARM64}

	zone1, zone2 = fake.Region + "-1", fake.Region + "-2"
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) (code int) {
	guard, err := l4.Isolate(fakeAWSRegion)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() {
		_ = guard.Close()
		if attempts := guard.Attempts(); len(attempts) > 0 {
			fmt.Fprintf(os.Stderr, "requests to non-local endpoints (blocked): %q\n", attempts)
			code = 1
		}
	}()
	logger := l4.Logger()
	ctrllog.SetLogger(logger)
	ctx = ctrllog.IntoContext(context.Background(), logger)

	// The suite setup of the provider's pkg/cloudprovider tests (AKS
	// scriptless mode), with the provider's CRDs.
	env = coretest.NewEnvironment(
		coretest.WithCRDs(apis.CRDs...),
		coretest.WithCRDs(coretestv1alpha1.CRDs...),
		coretest.WithFieldIndexers(coretest.NodeProviderIDFieldIndexer(ctx)),
	)
	defer func() {
		if err := env.Stop(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			code = 1
		}
	}()
	ctx = coreoptions.ToContext(ctx, coretest.Options())
	ctx = options.ToContext(ctx, test.Options(test.OptionsFields{
		ProvisionMode:             new(consts.ProvisionModeAKSScriptless),
		ManageExistingAKSMachines: new(false),
	}))
	var stop context.CancelFunc
	ctx, stop = context.WithCancel(ctx)
	defer stop()

	azureEnv = test.NewEnvironment(ctx, env)
	fakeClock := clock.NewFakeClock(time.Now())
	recorder := events.NewRecorder(&record.FakeRecorder{})
	cloudProvider = cloudprovider.New(azureEnv.InstanceTypesProvider, azureEnv.VMInstanceProvider,
		azureEnv.AKSMachineProvider, recorder, env.Client, azureEnv.ImageProvider, azureEnv.InstanceTypeStore)
	cluster := state.NewCluster(fakeClock, env.Client, cloudProvider)
	reader, err := client.New(env.Config, client.Options{Scheme: env.Client.Scheme()})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	h = &l4.Harness{
		Client:        env.Client,
		Reader:        reader,
		Cluster:       cluster,
		CloudProvider: cloudProvider,
		Provisioner: provisioning.NewProvisioner(env.Client, recorder, cloudProvider, cluster, fakeClock,
			deviceallocation.NewController(env.Client), virtualpods.NewVirtualPodCache(env.Client)),
		Clock: fakeClock,
		AfterLaunch: func() {
			// VMs are created asynchronously after Create returns.
			cloudProvider.WaitForInstancePromises()
			completeFakeNodes()
		},
		Reset: func(ctx context.Context) {
			cloudProvider.WaitForInstancePromises()
			azureEnv.Reset(ctx)
		},
	}
	return m.Run()
}

// setup creates the AKSNodeClass (the provider test default: Ubuntu 22.04
// community gallery images for amd64 and arm64, Ready) and returns a
// cleanup.
func setup(t *testing.T) func() {
	t.Helper()
	l4.Gomega(t)
	nodeClass := test.AKSNodeClass(v1beta1.AKSNodeClass{ObjectMeta: metav1.ObjectMeta{Name: nodeClassName}})
	test.ApplyDefaultStatus(nodeClass, env, false)
	expectations.ExpectApplied(ctx, env.Client, nodeClass)
	return func() {
		h.Cleanup(ctx)
		expectations.ExpectDeleted(ctx, env.Client, nodeClass)
	}
}

// completeFakeNodes gives the nodes core's test expectations register for
// launched NodeClaims the kubelet version a real node reports: the AKSNodeClass's
// Kubernetes version the provider bootstrapped it with. The provider's
// IsDrifted compares the two (K8sVersionDrift: NodeClass drift, not
// influenced by NodePools).
func completeFakeNodes() {
	nodeClass := &v1beta1.AKSNodeClass{}
	lo.Must0(h.Reader.Get(ctx, client.ObjectKey{Name: nodeClassName}, nodeClass))
	version := "v" + lo.FromPtr(nodeClass.Status.KubernetesVersion)
	nodes := &corev1.NodeList{}
	lo.Must0(h.Reader.List(ctx, nodes))
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if node.Status.NodeInfo.KubeletVersion == "" {
			node.Status.NodeInfo.KubeletVersion = version
			expectations.ExpectApplied(ctx, env.Client, node)
		}
	}
}

// vmSizes are the sizes of every VM the provider created so far.
func vmSizes() []string {
	var out []string
	for in := range azureEnv.VirtualMachinesAPI.VirtualMachineCreateOrUpdateBehavior.CalledWithInput.All() {
		out = append(out, string(lo.FromPtr(in.VM.Properties.HardwareProfile.VMSize)))
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func subset(sub, of []string) []string {
	var extra []string
	for _, s := range sub {
		if !slices.Contains(of, s) {
			extra = append(extra, s)
		}
	}
	return extra
}

func req(key string, values ...string) karpv1.NodeSelectorRequirementWithMinValues {
	return karpv1.NodeSelectorRequirementWithMinValues{Key: key, Operator: corev1.NodeSelectorOpIn, Values: values}
}

type launch struct {
	selector map[string]string
	// want, when set, are the instance types the launch may end up on.
	want []string
}

type scenario struct {
	l4.Scenario
	launches []launch
	// check runs provider-specific assertions on the launched NodeClaims.
	check func(t *testing.T, ncs []*karpv1.NodeClaim)
}

// The scenarios use SKUs of the provider's fake catalog
// (pkg/fake/zz_generated.sku.southcentralus.go).
func scenarios() []scenario {
	return []scenario{
		{
			Scenario: l4.Scenario{
				Name: "azure-general", Adapter: providers.Azure, NodeClassRef: aksNodeClassRef,
				Labels: map[string]string{"sparecores.com/tier": "general"},
				InstanceTypes: []string{
					"Standard_D16plds_v5", "Standard_D2_v3", "Standard_D2_v5", "Standard_D2s_v3", "Standard_D4s_v3",
				},
				Architectures: bothArchs,
			},
			launches: []launch{
				{},
				{selector: map[string]string{corev1.LabelArchStable: "arm64"}, want: []string{"Standard_D16plds_v5"}},
			},
		},
		{
			// Zone pinning with the provider's zone values (<region>-<n>)
			// and both capacity types.
			Scenario: l4.Scenario{
				Name: "azure-spot-zones", Adapter: providers.Azure, NodeClassRef: aksNodeClassRef,
				Requirements: []karpv1.NodeSelectorRequirementWithMinValues{
					req(karpv1.CapacityTypeLabelKey, karpv1.CapacityTypeSpot, karpv1.CapacityTypeOnDemand),
					req(corev1.LabelTopologyZone, zone1, zone2),
				},
				InstanceTypes: []string{"Standard_D2_v3", "Standard_D2s_v3", "Standard_D4s_v3", "Standard_E4d_v5"},
				Architectures: amd64,
			},
			launches: []launch{
				{},
				{selector: map[string]string{karpv1.CapacityTypeLabelKey: karpv1.CapacityTypeSpot}},
				{selector: map[string]string{karpv1.CapacityTypeLabelKey: karpv1.CapacityTypeOnDemand}},
			},
			check: func(t *testing.T, ncs []*karpv1.NodeClaim) {
				for _, nc := range ncs {
					if z := nc.Labels[corev1.LabelTopologyZone]; z != zone1 && z != zone2 {
						t.Errorf("NodeClaim %s: zone %q outside the template's zones", nc.Name, z)
					}
				}
			},
		},
		{
			// A GPU SKU the provider supports next to general-purpose ones.
			Scenario: l4.Scenario{
				Name: "azure-gpu", Adapter: providers.Azure, NodeClassRef: aksNodeClassRef,
				InstanceTypes: []string{"Standard_NC16as_T4_v3", "Standard_NC6s_v3"},
				Architectures: amd64,
			},
			launches: []launch{{}},
		},
	}
}

// TestLaunchesStayInListAndStampEveryKey is the main L4 assertion set
// (overview §8.4) on the real Azure provider.
func TestLaunchesStayInListAndStampEveryKey(t *testing.T) {
	for _, sc := range scenarios() {
		t.Run(sc.Name, func(t *testing.T) {
			defer setup(t)()
			np := h.Apply(ctx, t, sc.Scenario, dataTime)

			// Every selected type is known to the provider, so the
			// scenario exercises the whole list.
			its, err := cloudProvider.GetInstanceTypes(ctx, np)
			if err != nil {
				t.Fatal(err)
			}
			names := lo.Map(its, func(it *corecloudprovider.InstanceType, _ int) string { return it.Name })
			if missing := subset(sc.InstanceTypes, names); len(missing) > 0 {
				t.Fatalf("types missing from the provider's instance types: %v", missing)
			}

			var ncs []*karpv1.NodeClaim
			for i, l := range sc.launches {
				pod := l4.Pod(fmt.Sprintf("%s-%d", sc.Name, i), l.selector)
				nc := h.Launch(ctx, t, pod)[0]
				if slices.ContainsFunc(ncs, func(o *karpv1.NodeClaim) bool { return o.Name == nc.Name }) {
					t.Fatalf("pod %s reused NodeClaim %s", pod.Name, nc.Name)
				}
				it := nc.Labels[corev1.LabelInstanceTypeStable]
				if l.want != nil && !slices.Contains(l.want, it) {
					t.Errorf("pod %s launched %s, want one of %v", pod.Name, it, l.want)
				}
				for k, v := range l.selector {
					if nc.Labels[k] != v {
						t.Errorf("pod %s: NodeClaim label %s=%q, want %q", pod.Name, k, nc.Labels[k], v)
					}
				}
				ncs = append(ncs, nc)
			}
			sizes := vmSizes()
			if len(sizes) == 0 {
				t.Fatal("no VM was created")
			}
			if extra := subset(sizes, l4.InstanceTypesOf(np)); len(extra) > 0 {
				t.Errorf("VMs created with sizes outside the NodePool's list: %v", extra)
			}
			for _, nc := range ncs {
				h.CheckNodeClaim(ctx, t, np, nc)
			}
			if sc.check != nil {
				sc.check(t, ncs)
			}
		})
	}
}

// TestDriftControls shows that the drift checks are live and that our
// NodePool updates behave as overview §5.1.5 says.
func TestDriftControls(t *testing.T) {
	defer setup(t)()
	sc := scenarios()[0].Scenario
	np := h.Apply(ctx, t, sc, dataTime)
	nc := h.Launch(ctx, t, l4.Pod("drift", nil))[0]
	h.CheckNodeClaim(ctx, t, np, nc)
	launched := nc.Labels[corev1.LabelInstanceTypeStable]

	refreshed := sc
	refreshed.InstanceTypes = append(slices.Clone(sc.InstanceTypes), "Standard_F16s_v2")
	np2 := h.Apply(ctx, t, refreshed, dataTime.Add(24*time.Hour))
	if np2.Annotations[karpv1.NodePoolHashAnnotationKey] != np.Annotations[karpv1.NodePoolHashAnnotationKey] {
		t.Errorf("NodePool hash changed on a data refresh: %s -> %s",
			np.Annotations[karpv1.NodePoolHashAnnotationKey], np2.Annotations[karpv1.NodePoolHashAnnotationKey])
	}
	if reason := h.DriftReason(ctx, t, nc); reason != "" {
		t.Errorf("drifted after adding a type and refreshing the data timestamp: %s", reason)
	}

	removed := sc
	removed.InstanceTypes = slices.DeleteFunc(slices.Clone(sc.InstanceTypes), func(s string) bool { return s == launched })
	h.Apply(ctx, t, removed, dataTime.Add(48*time.Hour))
	if reason := h.DriftReason(ctx, t, nc); reason != "RequirementsDrifted" {
		t.Errorf("after removing the launched type %s: drift reason %q, want RequirementsDrifted", launched, reason)
	}
}

// TestOfferingsWithinAdapterRules checks the adapter's capacity-type and
// architecture rules against what the provider offers: in particular no
// reserved offerings, which the adapter rejects (overview §5.4).
func TestOfferingsWithinAdapterRules(t *testing.T) {
	defer setup(t)()
	a, _ := providers.NewRegistry().Get(providers.Azure)
	rules := a.RequirementRules()
	for _, it := range providerInstanceTypes(t) {
		for _, arch := range it.Requirements.Get(corev1.LabelArchStable).Values() {
			if !slices.Contains(rules.AllowedArchitectures, v1alpha1.Architecture(arch)) {
				t.Errorf("%s offers architecture %s outside the adapter's %v", it.Name, arch, rules.AllowedArchitectures)
			}
		}
		for _, o := range it.Offerings {
			if ct := o.CapacityType(); !slices.Contains(rules.AllowedCapacityTypes, ct) {
				t.Errorf("%s offers capacity type %s outside the adapter's %v", it.Name, ct, rules.AllowedCapacityTypes)
			}
		}
	}

	sc := l4.Scenario{
		Name: "azure-reserved", Adapter: providers.Azure, NodeClassRef: aksNodeClassRef,
		Requirements: []karpv1.NodeSelectorRequirementWithMinValues{
			req(karpv1.CapacityTypeLabelKey, karpv1.CapacityTypeReserved),
		},
		InstanceTypes: []string{"Standard_D2_v3"}, Architectures: amd64,
	}
	if _, err := sc.Render(ctx, dataTime); !errors.Is(err, providers.ErrCapacityTypeUnsupported) {
		t.Errorf("template asking for reserved capacity: err %v, want ErrCapacityTypeUnsupported", err)
	}
}

// TestEligibilityMatchesProvider compares the adapter's mirror of the
// provider's static exclusions (overview §5.4, §6.2) with the provider's
// own instance-type filtering, SKU by SKU over the fake catalog.
func TestEligibilityMatchesProvider(t *testing.T) {
	defer setup(t)()
	a, _ := providers.NewRegistry().Get(providers.Azure)
	offered := lo.SliceToMap(providerInstanceTypes(t), func(it *corecloudprovider.InstanceType) (string, bool) {
		return it.Name, true
	})
	sixMonths := time.Now().AddDate(0, 6, 0)
	checked := 0
	for _, raw := range fake.ResourceSkus[fake.Region] {
		name := lo.FromPtr(raw.Name)
		sku := fake.MakeSKU(name)
		vcpus, err := sku.VCPU()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		memGiB, err := sku.Memory()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		gpus, _ := sku.GPU()
		arch, _ := sku.GetCPUArchitectureType()
		// A synthetic Spare Cores row with the SKU's figures; the
		// retirement date stands in for Spare Cores' status.
		srv := keeper.Server{
			APIReference: name, VCPUs: int(vcpus), MemoryAmount: memGiB * 1024,
			AcceleratorCount: float64(gpus), Status: "active", CPUArchitecture: "x86_64",
		}
		c := &selection.Candidate{Server: srv, Architecture: v1alpha1.ArchitectureAMD64}
		if strings.EqualFold(arch, "Arm64") {
			c.Server.CPUArchitecture, c.Architecture = "arm64", v1alpha1.ArchitectureARM64
		}
		if d, err := sku.GetRetirementDate(); err == nil && d != nil && d.Before(sixMonths) {
			c.Server.Status = "planned-for-retirement"
		}
		ok, why := a.Eligible(c, providers.Env{})
		if ok != offered[name] {
			t.Errorf("%s: adapter eligible=%v (%s), provider offers it=%v", name, ok, why, offered[name])
		}
		checked++
	}
	if checked < 10 {
		t.Fatalf("only %d SKUs in the fake catalog", checked)
	}
}

func providerInstanceTypes(t *testing.T) []*corecloudprovider.InstanceType {
	t.Helper()
	np := coretest.NodePool(karpv1.NodePool{Spec: karpv1.NodePoolSpec{Template: karpv1.NodeClaimTemplate{
		Spec: karpv1.NodeClaimTemplateSpec{NodeClassRef: &karpv1.NodeClassReference{
			Group: aksNodeClassRef.Group, Kind: aksNodeClassRef.Kind, Name: aksNodeClassRef.Name,
		}},
	}}})
	its, err := cloudProvider.GetInstanceTypes(ctx, np)
	if err != nil {
		t.Fatal(err)
	}
	return its
}
