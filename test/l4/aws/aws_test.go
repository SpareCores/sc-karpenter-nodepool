/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// L4 for aws/karpenter-provider-aws: the provider's real CloudProvider
// (GetInstanceTypes, Create, IsDrifted) on its pkg/test environment, with
// in-memory EC2/SSM/IAM/EKS/pricing fakes and an envtest API server.
package aws_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	opstatus "github.com/awslabs/operatorpkg/status"
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

	"github.com/aws/karpenter-provider-aws/pkg/apis"
	v1 "github.com/aws/karpenter-provider-aws/pkg/apis/v1"
	"github.com/aws/karpenter-provider-aws/pkg/cloudprovider"
	"github.com/aws/karpenter-provider-aws/pkg/fake"
	"github.com/aws/karpenter-provider-aws/pkg/operator/options"
	"github.com/aws/karpenter-provider-aws/pkg/test"

	"github.com/SpareCores/sc-karpenter-nodepool/api/v1alpha1"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/nodepool"
	"github.com/SpareCores/sc-karpenter-nodepool/internal/providers"
	"github.com/SpareCores/sc-karpenter-nodepool/test/l4"
)

const nodeClassName = "sc-l4"

var (
	ctx           context.Context
	env           *coretest.Environment
	awsEnv        *test.Environment
	cloudProvider *cloudprovider.CloudProvider
	h             *l4.Harness
	dataTime      = time.Date(2026, 10, 5, 10, 2, 57, 0, time.UTC)

	ec2NodeClassRef = nodepool.NodeClassRef{Group: "karpenter.k8s.aws", Kind: "EC2NodeClass", Name: nodeClassName}
	amd64           = []v1alpha1.Architecture{v1alpha1.ArchitectureAMD64}
	bothArchs       = []v1alpha1.Architecture{v1alpha1.ArchitectureAMD64, v1alpha1.ArchitectureARM64}
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) (code int) {
	guard, err := l4.Isolate(fake.DefaultRegion)
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

	// The suite setup of the provider's pkg/cloudprovider tests, with the
	// provider's unmodified CRDs (its NodePool CRD validates our NodePools).
	env = coretest.NewEnvironment(
		coretest.WithCRDs(apis.CRDs...),
		coretest.WithCRDs(coretestv1alpha1.CRDs...),
		coretest.WithFieldIndexers(coretest.NodePoolNodeClassRefFieldIndexer(ctx)),
	)
	defer func() {
		if err := env.Stop(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			code = 1
		}
	}()
	ctx = coreoptions.ToContext(ctx, coretest.Options())
	ctx = options.ToContext(ctx, test.Options())
	var stop context.CancelFunc
	ctx, stop = context.WithCancel(ctx)
	defer stop()

	awsEnv = test.NewEnvironment(ctx, env)
	fakeClock := clock.NewFakeClock(time.Now())
	recorder := events.NewRecorder(&record.FakeRecorder{})
	cloudProvider = cloudprovider.New(awsEnv.InstanceTypesProvider, awsEnv.InstanceProvider, recorder,
		env.Client, awsEnv.AMIProvider, awsEnv.SecurityGroupProvider, awsEnv.CapacityReservationProvider,
		awsEnv.PlacementGroupProvider, awsEnv.InstanceTypeStore, new("test-ca-bundle"))
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
		Clock:       fakeClock,
		AfterLaunch: completeFakeInstances,
		Reset:       func(context.Context) { awsEnv.Reset() },
	}
	return m.Run()
}

// setup creates the EC2NodeClass (the provider test default: AL2023 AMIs
// for amd64 and arm64, three subnets in test-zone-1a/b/c = tstz1-1a/b/c)
// and returns a cleanup.
func setup(t *testing.T) func() {
	t.Helper()
	l4.Gomega(t)
	nodeClass := test.EC2NodeClass(v1.EC2NodeClass{ObjectMeta: metav1.ObjectMeta{Name: nodeClassName}})
	nodeClass.StatusConditions().SetTrue(opstatus.ConditionReady)
	expectations.ExpectApplied(ctx, env.Client, nodeClass)
	if _, err := awsEnv.SubnetProvider.List(ctx, nodeClass); err != nil {
		t.Fatal(err)
	}
	if err := awsEnv.InstanceTypesProvider.UpdateInstanceTypes(ctx); err != nil {
		t.Fatal(err)
	}
	if err := awsEnv.InstanceTypesProvider.UpdateInstanceTypeOfferings(ctx); err != nil {
		t.Fatal(err)
	}
	return func() {
		h.Cleanup(ctx)
		expectations.ExpectDeleted(ctx, env.Client, nodeClass)
	}
}

// completeFakeInstances gives the fake's instances the subnet and security
// groups real EC2 would: the fake CreateFleet launches Overrides[0] of the
// first launch template config but records neither its subnet nor the
// launch template's security groups, which the provider's IsDrifted
// compares with the EC2NodeClass (SubnetDrift, SecurityGroupDrift: NodeClass
// drift, not influenced by NodePools).
func completeFakeInstances() {
	groups := map[string][]string{}
	awsEnv.EC2API.CreateLaunchTemplateBehavior.CalledWithInput.ForEach(func(in *ec2.CreateLaunchTemplateInput) {
		data := in.LaunchTemplateData
		ids := slices.Clone(data.SecurityGroupIds)
		for _, ni := range data.NetworkInterfaces {
			ids = append(ids, ni.Groups...)
		}
		groups[lo.FromPtr(in.LaunchTemplateName)] = ids
	})
	var fleets []*ec2.CreateFleetInput
	awsEnv.EC2API.CreateFleetBehavior.CalledWithInput.ForEach(func(in *ec2.CreateFleetInput) {
		fleets = append(fleets, in)
	})
	awsEnv.EC2API.Instances.Range(func(id, v any) bool {
		inst := v.(ec2types.Instance)
		if inst.SubnetId != nil {
			return true
		}
		for _, f := range fleets {
			cfg := f.LaunchTemplateConfigs[0]
			o := cfg.Overrides[0]
			sameZone := lo.FromPtr(o.AvailabilityZone) == lo.FromPtr(inst.Placement.AvailabilityZone)
			if o.InstanceType != inst.InstanceType || !sameZone {
				continue
			}
			inst.SubnetId = o.SubnetId
			for _, g := range groups[lo.FromPtr(cfg.LaunchTemplateSpecification.LaunchTemplateName)] {
				inst.SecurityGroups = append(inst.SecurityGroups, ec2types.GroupIdentifier{GroupId: new(g)})
			}
			awsEnv.EC2API.Instances.Store(id, inst)
			break
		}
		return true
	})
}

// fleetTypes are the instance types of every CreateFleet request so far:
// what the provider asked EC2 to launch.
func fleetTypes() []string {
	var out []string
	awsEnv.EC2API.CreateFleetBehavior.CalledWithInput.ForEach(func(in *ec2.CreateFleetInput) {
		for _, cfg := range in.LaunchTemplateConfigs {
			for _, o := range cfg.Overrides {
				out = append(out, string(o.InstanceType))
			}
		}
	})
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
	check func(t *testing.T, np *karpv1.NodePool, ncs []*karpv1.NodeClaim, fleet []string)
}

// The scenarios use instance types of the provider's fake EC2 catalog
// (pkg/fake/zz_generated.describe_instance_types.go).
func scenarios() []scenario {
	return []scenario{
		{
			Scenario: l4.Scenario{
				Name: "aws-general", Adapter: providers.AWS, NodeClassRef: ec2NodeClassRef,
				Labels:        map[string]string{"sparecores.com/tier": "general"},
				InstanceTypes: []string{"c6g.large", "m5.large", "m5.xlarge", "m7i-flex.large", "t3.large"},
				Architectures: bothArchs,
			},
			launches: []launch{
				{},
				{selector: map[string]string{corev1.LabelArchStable: "arm64"}, want: []string{"c6g.large"}},
				{selector: map[string]string{corev1.LabelArchStable: "amd64"}},
			},
		},
		{
			// Zone pinning by AZ ID (account-independent, = Spare Cores
			// zone_id, overview §5.4) with both capacity types.
			Scenario: l4.Scenario{
				Name: "aws-spot-zone-id", Adapter: providers.AWS, NodeClassRef: ec2NodeClassRef,
				Requirements: []karpv1.NodeSelectorRequirementWithMinValues{
					req(karpv1.CapacityTypeLabelKey, karpv1.CapacityTypeSpot, karpv1.CapacityTypeOnDemand),
					req(v1.LabelTopologyZoneID, "tstz1-1a", "tstz1-1b"),
				},
				InstanceTypes: []string{"m5.large", "m5.xlarge", "t3.large"},
				Architectures: amd64,
			},
			launches: []launch{
				{},
				{selector: map[string]string{karpv1.CapacityTypeLabelKey: karpv1.CapacityTypeSpot}},
				{selector: map[string]string{karpv1.CapacityTypeLabelKey: karpv1.CapacityTypeOnDemand}},
			},
			check: func(t *testing.T, _ *karpv1.NodePool, ncs []*karpv1.NodeClaim, _ []string) {
				for _, nc := range ncs {
					if z := nc.Labels[v1.LabelTopologyZoneID]; z != "tstz1-1a" && z != "tstz1-1b" {
						t.Errorf("NodeClaim %s: zone ID %q outside the template's zone IDs", nc.Name, z)
					}
				}
			},
		},
		{
			Scenario: l4.Scenario{
				Name: "aws-zone-name", Adapter: providers.AWS, NodeClassRef: ec2NodeClassRef,
				Requirements:  []karpv1.NodeSelectorRequirementWithMinValues{req(corev1.LabelTopologyZone, "test-zone-1b")},
				InstanceTypes: []string{"m5.large", "m5.xlarge", "t3.large"},
				Architectures: amd64,
			},
			launches: []launch{{}},
			check: func(t *testing.T, _ *karpv1.NodePool, ncs []*karpv1.NodeClaim, _ []string) {
				for _, nc := range ncs {
					if z := nc.Labels[corev1.LabelTopologyZone]; z != "test-zone-1b" {
						t.Errorf("NodeClaim %s: zone %q, want test-zone-1b", nc.Name, z)
					}
				}
			},
		},
		{
			// The provider's exotic filter (overview §5.4): metal and
			// GPU/accelerator types are dropped while other types are left.
			Scenario: l4.Scenario{
				Name: "aws-exotic", Adapter: providers.AWS, NodeClassRef: ec2NodeClassRef,
				InstanceTypes: []string{"g4dn.8xlarge", "m5.large", "m5.metal", "m5.xlarge"},
				Architectures: amd64,
			},
			launches: []launch{{want: []string{"m5.large", "m5.xlarge"}}},
			check: func(t *testing.T, _ *karpv1.NodePool, _ []*karpv1.NodeClaim, fleet []string) {
				if extra := subset(fleet, []string{"m5.large", "m5.xlarge"}); len(extra) > 0 {
					t.Errorf("CreateFleet included exotic types %v (fleet %v)", extra, fleet)
				}
			},
		},
		{
			// Rendered with the EKS Auto Mode adapter's rules for an
			// Auto Mode NodeClass, launched by the open-source provider
			// (test mode override): the keys Auto Mode pools carry
			// (arch, capacity type, instance type, zone ID) are stamped.
			Scenario: l4.Scenario{
				Name: "aws-auto-mode-style", Adapter: providers.AWSAutoMode,
				NodeClassRef: nodepool.NodeClassRef{Group: "eks.amazonaws.com", Kind: "NodeClass", Name: "sc-l4-auto"},
				Override:     &ec2NodeClassRef,
				Requirements: []karpv1.NodeSelectorRequirementWithMinValues{
					req(v1.LabelTopologyZoneID, "tstz1-1a", "tstz1-1c"),
				},
				InstanceTypes: []string{"m5.large", "m5.xlarge", "m7i-flex.large"},
				Architectures: amd64,
			},
			launches: []launch{{}},
			check: func(t *testing.T, np *karpv1.NodePool, _ []*karpv1.NodeClaim, _ []string) {
				for _, r := range np.Spec.Template.Spec.Requirements {
					if r.Key == corev1.LabelOSStable {
						t.Errorf("Auto Mode NodePool carries %s", r.Key)
					}
				}
			},
		},
	}
}

// TestLaunchesStayInListAndStampEveryKey is the main L4 assertion set
// (overview §8.4) on the real AWS provider.
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
				t.Fatalf("types missing from the provider's catalog: %v", missing)
			}

			var ncs []*karpv1.NodeClaim
			for i, l := range sc.launches {
				pod := l4.Pod(fmt.Sprintf("%s-%d", sc.Name, i), l.selector)
				nc := h.Launch(ctx, t, pod)[0]
				if slices.ContainsFunc(ncs, func(o *karpv1.NodeClaim) bool { return o.Name == nc.Name }) {
					t.Fatalf("pod %s reused NodeClaim %s", pod.Name, nc.Name)
				}
				if l.want != nil && !slices.Contains(l.want, nc.Labels[corev1.LabelInstanceTypeStable]) {
					t.Errorf("pod %s launched %s, want one of %v", pod.Name, nc.Labels[corev1.LabelInstanceTypeStable], l.want)
				}
				for k, v := range l.selector {
					if nc.Labels[k] != v {
						t.Errorf("pod %s: NodeClaim label %s=%q, want %q", pod.Name, k, nc.Labels[k], v)
					}
				}
				ncs = append(ncs, nc)
			}
			fleet := fleetTypes()
			if len(fleet) == 0 {
				t.Fatal("no CreateFleet request")
			}
			if extra := subset(fleet, l4.InstanceTypesOf(np)); len(extra) > 0 {
				t.Errorf("CreateFleet asked for types outside the NodePool's list: %v", extra)
			}
			for _, nc := range ncs {
				h.CheckNodeClaim(ctx, t, np, nc)
			}
			if sc.check != nil {
				sc.check(t, np, ncs, fleet)
			}
		})
	}
}

// TestDriftControls shows that the drift checks above are live and that our
// NodePool updates behave as overview §5.1.5 says: a refresh that only
// changes volatile data or adds types drifts nothing; removing the launched
// type drifts its node.
func TestDriftControls(t *testing.T) {
	defer setup(t)()
	sc := scenarios()[0].Scenario
	np := h.Apply(ctx, t, sc, dataTime)
	nc := h.Launch(ctx, t, l4.Pod("drift", nil))[0]
	h.CheckNodeClaim(ctx, t, np, nc)
	launched := nc.Labels[corev1.LabelInstanceTypeStable]

	refreshed := sc
	refreshed.InstanceTypes = append(slices.Clone(sc.InstanceTypes), "m6idn.32xlarge")
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
// architecture rules against what the provider actually offers.
func TestOfferingsWithinAdapterRules(t *testing.T) {
	defer setup(t)()
	for _, id := range []providers.ID{providers.AWS, providers.AWSAutoMode} {
		a, _ := providers.NewRegistry().Get(id)
		rules := a.RequirementRules()
		np := coretest.NodePool(karpv1.NodePool{Spec: karpv1.NodePoolSpec{Template: karpv1.NodeClaimTemplate{
			Spec: karpv1.NodeClaimTemplateSpec{NodeClassRef: &karpv1.NodeClassReference{
				Group: ec2NodeClassRef.Group, Kind: ec2NodeClassRef.Kind, Name: ec2NodeClassRef.Name,
			}},
		}}})
		its, err := cloudProvider.GetInstanceTypes(ctx, np)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range its {
			for _, arch := range it.Requirements.Get(corev1.LabelArchStable).Values() {
				if !slices.Contains(rules.AllowedArchitectures, v1alpha1.Architecture(arch)) {
					t.Errorf("%s: %s offers architecture %s outside the adapter's %v", id, it.Name, arch, rules.AllowedArchitectures)
				}
			}
			for _, o := range it.Offerings {
				if ct := o.CapacityType(); !slices.Contains(rules.AllowedCapacityTypes, ct) {
					t.Errorf("%s: %s offers capacity type %s outside the adapter's %v", id, it.Name, ct, rules.AllowedCapacityTypes)
				}
			}
		}
	}

	// The Auto Mode adapter never emits kubernetes.io/os and rejects a
	// template that asks for it (AWS documents it as unsupported there).
	sc := l4.Scenario{
		Name: "aws-auto-os", Adapter: providers.AWSAutoMode,
		NodeClassRef:  nodepool.NodeClassRef{Group: "eks.amazonaws.com", Kind: "NodeClass", Name: "sc-l4-auto"},
		Requirements:  []karpv1.NodeSelectorRequirementWithMinValues{req(corev1.LabelOSStable, "linux")},
		InstanceTypes: []string{"m5.large"}, Architectures: amd64,
	}
	if _, err := sc.Render(ctx, dataTime); !errors.Is(err, nodepool.ErrForbiddenKey) {
		t.Errorf("Auto Mode template with %s: err %v, want ErrForbiddenKey", corev1.LabelOSStable, err)
	}
}
