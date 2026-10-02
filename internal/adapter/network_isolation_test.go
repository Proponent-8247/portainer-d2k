package adapter

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/portainer/d2k/internal/types"
)

func newIsolationTestAdapter(objects ...runtime.Object) *KubernetesDockerAdapter {
	return &KubernetesDockerAdapter{
		client:            fake.NewSimpleClientset(objects...),
		namespace:         "d2k-workloads",
		networkIsolation:  true,
		rejectHostNetwork: true,
		podCIDRs:          []string{"10.240.0.0/16"},
		serviceCIDRs:      []string{"10.241.0.0/16"},
		logger:            zap.NewNop().Sugar(),
		prevCPU:           map[string]int64{},
		networks:          map[string]*NetworkSummary{},
		nfsStorageClasses: map[string]string{},
	}
}

func testNetwork(name string, internal, attachable bool) *NetworkSummary {
	return &NetworkSummary{
		ID:         networkIDForName(name, "d2k-workloads"),
		Name:       name,
		Driver:     "overlay",
		Scope:      "swarm",
		Internal:   internal,
		Attachable: attachable,
		IPAM:       NetworkIPAM{Driver: "default"},
		Labels:     map[string]string{types.LabelManagedBy: types.LabelManagedByValue},
	}
}

func managedDeployment(name string, networkIDs ...string) *appsv1.Deployment {
	labels := map[string]string{
		"app":                   name,
		types.LabelManagedBy:    types.LabelManagedByValue,
		types.LabelWorkloadName: name,
	}
	for _, id := range networkIDs {
		labels[networkLabelKey(id)] = "true"
	}
	annotations := map[string]string{}
	if len(networkIDs) > 0 {
		annotations[types.AnnotationNetworkIDs] = encodeNetworkIDs(networkIDs)
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "d2k-workloads",
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: annotations,
				},
			},
		},
	}
}

func TestParseCIDRListRejectsInvalidValues(t *testing.T) {
	got, err := parseCIDRList("10.240.0.1/16, 10.241.0.0/16,10.241.0.0/16", "TEST")
	if err != nil {
		t.Fatalf("parseCIDRList returned unexpected error: %v", err)
	}
	if len(got) != 2 || got[0] != "10.240.0.0/16" || got[1] != "10.241.0.0/16" {
		t.Fatalf("unexpected canonical CIDRs: %#v", got)
	}
	if _, err := parseCIDRList("10.240.0.0/16,not-a-cidr", "TEST"); err == nil {
		t.Fatal("invalid CIDR was accepted")
	}
}

func TestIsolationStartupRefusesLegacyWorkloadsBeforeDefaultDeny(t *testing.T) {
	ctx := context.Background()
	legacy := managedDeployment("legacy")
	a := newIsolationTestAdapter(legacy)

	err := a.reconcileIsolationState(ctx)
	if err == nil || !strings.Contains(err.Error(), "blackhole a legacy workload") {
		t.Fatalf("expected migration-safety error, got %v", err)
	}

	_, err = a.client.NetworkingV1().NetworkPolicies(a.namespace).Get(ctx, "d2k-workloads-default-deny", metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("default-deny policy should not be installed for an unadopted legacy workload, got %v", err)
	}
}

func TestRestorePersistedNetworkReconcilesPolicy(t *testing.T) {
	ctx := context.Background()
	a := newIsolationTestAdapter()
	network := testNetwork("frontend", false, true)

	if err := a.persistNetwork(ctx, network); err != nil {
		t.Fatalf("persistNetwork: %v", err)
	}
	if err := a.restorePersistedNetworks(ctx); err != nil {
		t.Fatalf("restorePersistedNetworks: %v", err)
	}

	if _, err := a.client.NetworkingV1().NetworkPolicies(a.namespace).Get(ctx, networkPolicyName(network.ID), metav1.GetOptions{}); err != nil {
		t.Fatalf("network policy was not reconciled: %v", err)
	}
}

func TestNetworkPolicyInternalAndWorldEgress(t *testing.T) {
	ctx := context.Background()
	a := newIsolationTestAdapter()

	internal := testNetwork("backend", true, true)
	if err := a.ensureNetworkIsolationPolicy(ctx, internal); err != nil {
		t.Fatalf("internal policy: %v", err)
	}
	policy, err := a.client.NetworkingV1().NetworkPolicies(a.namespace).Get(ctx, networkPolicyName(internal.ID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get internal policy: %v", err)
	}
	for _, rule := range policy.Spec.Egress {
		for _, peer := range rule.To {
			if peer.IPBlock != nil && (peer.IPBlock.CIDR == "0.0.0.0/0" || peer.IPBlock.CIDR == "::/0") {
				t.Fatalf("internal network unexpectedly has world egress: %#v", peer.IPBlock)
			}
		}
	}

	normal := testNetwork("frontend", false, true)
	if err := a.ensureNetworkIsolationPolicy(ctx, normal); err != nil {
		t.Fatalf("normal policy: %v", err)
	}
	policy, err = a.client.NetworkingV1().NetworkPolicies(a.namespace).Get(ctx, networkPolicyName(normal.ID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get normal policy: %v", err)
	}
	foundWorld := false
	for _, rule := range policy.Spec.Egress {
		for _, peer := range rule.To {
			if peer.IPBlock != nil && peer.IPBlock.CIDR == "0.0.0.0/0" {
				foundWorld = true
				if len(peer.IPBlock.Except) != 2 {
					t.Fatalf("world rule should exclude pod/service CIDRs, got %#v", peer.IPBlock.Except)
				}
			}
		}
	}
	if !foundWorld {
		t.Fatal("normal network is missing world egress")
	}
}

func TestRemoveNetworkRejectsActiveEndpoints(t *testing.T) {
	ctx := context.Background()
	network := testNetwork("frontend", false, true)
	deployment := managedDeployment("web", network.ID)
	a := newIsolationTestAdapter(deployment)
	a.networks[network.Name] = network
	if err := a.persistNetwork(ctx, network); err != nil {
		t.Fatalf("persistNetwork: %v", err)
	}
	if err := a.ensureNetworkIsolationPolicy(ctx, network); err != nil {
		t.Fatalf("ensureNetworkIsolationPolicy: %v", err)
	}

	err := a.RemoveNetwork(ctx, network.Name)
	if err == nil || !strings.Contains(err.Error(), "active endpoints") {
		t.Fatalf("expected in-use network rejection, got %v", err)
	}
	if _, err := a.client.CoreV1().ConfigMaps(a.namespace).Get(ctx, networkStateName(network.ID), metav1.GetOptions{}); err != nil {
		t.Fatalf("network state was deleted despite active endpoint: %v", err)
	}
}

func TestRemoveNetworkCleansPersistentStateAndPolicy(t *testing.T) {
	ctx := context.Background()
	network := testNetwork("frontend", false, true)
	a := newIsolationTestAdapter()
	a.networks[network.Name] = network
	if err := a.persistNetwork(ctx, network); err != nil {
		t.Fatalf("persistNetwork: %v", err)
	}
	if err := a.ensureNetworkIsolationPolicy(ctx, network); err != nil {
		t.Fatalf("ensureNetworkIsolationPolicy: %v", err)
	}

	if err := a.RemoveNetwork(ctx, network.Name); err != nil {
		t.Fatalf("RemoveNetwork: %v", err)
	}
	if _, err := a.client.CoreV1().ConfigMaps(a.namespace).Get(ctx, networkStateName(network.ID), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("persisted network state still exists: %v", err)
	}
	if _, err := a.client.NetworkingV1().NetworkPolicies(a.namespace).Get(ctx, networkPolicyName(network.ID), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("network policy still exists: %v", err)
	}
}

func TestConnectNetworkEnforcesAttachable(t *testing.T) {
	ctx := context.Background()
	network := testNetwork("private-overlay", false, false)
	a := newIsolationTestAdapter()
	a.networks[network.Name] = network
	if err := a.persistNetwork(ctx, network); err != nil {
		t.Fatalf("persistNetwork: %v", err)
	}

	err := a.ConnectNetwork(ctx, network.Name, "standalone")
	if err == nil || !strings.Contains(err.Error(), "not manually attachable") {
		t.Fatalf("expected attachable rejection, got %v", err)
	}
}

func TestNoneCannotBeCombinedWithAnotherNetwork(t *testing.T) {
	ctx := context.Background()
	a := newIsolationTestAdapter()
	_, _, err := a.networkMembership(ctx, []string{"none", "bridge"}, "")
	if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("expected none-network combination rejection, got %v", err)
	}
}

func TestContainerReadbackUsesEnforcedNetworks(t *testing.T) {
	ctx := context.Background()
	front := testNetwork("front", false, true)
	back := testNetwork("back", true, true)
	deployment := managedDeployment("web", front.ID, back.ID)
	deployment.UID = "container-uid"
	a := newIsolationTestAdapter(deployment)
	for _, network := range []*NetworkSummary{front, back} {
		a.networks[network.Name] = network
		if err := a.persistNetwork(ctx, network); err != nil {
			t.Fatalf("persistNetwork(%s): %v", network.Name, err)
		}
	}

	settings := a.containerNetworkSettings(ctx, *deployment, "")
	if len(settings) != 2 {
		t.Fatalf("expected two network settings, got %#v", settings)
	}
	if settings["front"] == nil || settings["front"].NetworkID != front.ID {
		t.Fatalf("front network readback is wrong: %#v", settings["front"])
	}
	if settings["back"] == nil || settings["back"].NetworkID != back.ID {
		t.Fatalf("back network readback is wrong: %#v", settings["back"])
	}

	networks := a.swarmNetworksForDeployment(*deployment)
	if len(networks) != 2 {
		t.Fatalf("expected two Swarm networks, got %#v", networks)
	}
}

func TestStackRemovalCleansNetworkAndPublishedPolicies(t *testing.T) {
	ctx := context.Background()
	network := testNetwork("demo_default", false, true)
	network.Labels["com.docker.compose.project"] = "demo"
	deployment := managedDeployment("demo-web", network.ID)
	deployment.UID = "service-uid"
	deployment.Labels[types.LabelSwarmManagedBy] = types.LabelSwarmManagedByValue
	deployment.Labels[types.LabelSwarmStack] = "demo"
	deployment.Labels[types.LabelSwarmService] = "demo-web"
	deployment.Spec.Template.Labels[types.LabelSwarmManagedBy] = types.LabelSwarmManagedByValue
	deployment.Spec.Template.Labels[types.LabelSwarmStack] = "demo"

	a := newIsolationTestAdapter(deployment)
	a.networks[network.Name] = network
	if err := a.persistNetwork(ctx, network); err != nil {
		t.Fatalf("persistNetwork: %v", err)
	}
	if err := a.ensureNetworkIsolationPolicy(ctx, network); err != nil {
		t.Fatalf("network policy: %v", err)
	}
	if err := a.ensurePublishedIngressPolicy(ctx, deployment.Name, []networkingv1.NetworkPolicyPort{}); err != nil {
		t.Fatalf("published policy setup: %v", err)
	}
	// ensurePublishedIngressPolicy with no ports deletes; create a marker policy
	// to prove the canonical service cleanup path removes it.
	_, err := a.client.NetworkingV1().NetworkPolicies(a.namespace).Create(ctx, &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: publishedPolicyName(deployment.Name), Namespace: a.namespace},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create published policy marker: %v", err)
	}

	if err := a.SwarmDeleteStack(ctx, "demo"); err != nil {
		t.Fatalf("SwarmDeleteStack: %v", err)
	}

	if _, err := a.client.NetworkingV1().NetworkPolicies(a.namespace).Get(ctx, publishedPolicyName(deployment.Name), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("published policy still exists after stack removal: %v", err)
	}
	if _, err := a.client.NetworkingV1().NetworkPolicies(a.namespace).Get(ctx, networkPolicyName(network.ID), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("network policy still exists after stack removal: %v", err)
	}
	if _, err := a.client.CoreV1().ConfigMaps(a.namespace).Get(ctx, networkStateName(network.ID), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("network state still exists after stack removal: %v", err)
	}
}

func TestEmptyNetworkMembershipSurvivesRestartValidation(t *testing.T) {
	ctx := context.Background()
	deployment := managedDeployment("disconnected")
	deployment.Annotations[types.AnnotationNetworkIDs] = "[]"
	deployment.Spec.Template.Annotations[types.AnnotationNetworkIDs] = "[]"
	a := newIsolationTestAdapter(deployment)

	if err := a.reconcileIsolationState(ctx); err != nil {
		t.Fatalf("valid disconnected workload was rejected: %v", err)
	}
	if !a.isolationReady {
		t.Fatal("isolation was not marked ready")
	}
}

func TestRuntimeRestoreRepairsDeletedBaseline(t *testing.T) {
	ctx := context.Background()
	deployment := managedDeployment("disconnected")
	deployment.Annotations[types.AnnotationNetworkIDs] = "[]"
	deployment.Spec.Template.Annotations[types.AnnotationNetworkIDs] = "[]"
	a := newIsolationTestAdapter(deployment)
	if err := a.reconcileIsolationState(ctx); err != nil {
		t.Fatalf("reconcileIsolationState: %v", err)
	}
	if err := a.client.NetworkingV1().NetworkPolicies(a.namespace).Delete(ctx, "d2k-workloads-default-deny", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete baseline: %v", err)
	}
	if err := a.restorePersistedNetworks(ctx); err != nil {
		t.Fatalf("restorePersistedNetworks: %v", err)
	}
	if _, err := a.client.NetworkingV1().NetworkPolicies(a.namespace).Get(ctx, "d2k-workloads-default-deny", metav1.GetOptions{}); err != nil {
		t.Fatalf("baseline was not reconciled: %v", err)
	}
}

func TestPublishedServiceNameDoesNotCollideWithDNSService(t *testing.T) {
	for _, name := range []string{"web", "123-web", strings.Repeat("a", 63)} {
		published := publishedServiceName(name)
		if published == name {
			t.Fatalf("published service name %q collides with DNS service for %q", published, name)
		}
		if len(published) > 63 {
			t.Fatalf("published service name exceeds DNS label limit: %q", published)
		}
	}
}

func TestCreateContainerCanCreateDNSAndPublishedServices(t *testing.T) {
	ctx := context.Background()
	a := newIsolationTestAdapter()
	_, _, err := a.CreateContainer(ctx, RunOptions{
		Name:         "web",
		Image:        "example.invalid/web:1",
		PortBindings: []string{"8080:80"},
	})
	if err != nil {
		t.Fatalf("CreateContainer with published port: %v", err)
	}

	dns, err := a.client.CoreV1().Services(a.namespace).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("internal DNS service missing: %v", err)
	}
	if dns.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Fatalf("internal DNS service has unexpected type: %s", dns.Spec.Type)
	}
	published, err := a.client.CoreV1().Services(a.namespace).Get(ctx, publishedServiceName("web"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("published service missing: %v", err)
	}
	if published.Spec.Type != corev1.ServiceTypeLoadBalancer {
		t.Fatalf("published service has unexpected type: %s", published.Spec.Type)
	}
	if _, err := a.client.NetworkingV1().NetworkPolicies(a.namespace).Get(ctx, publishedPolicyName("web"), metav1.GetOptions{}); err != nil {
		t.Fatalf("published ingress policy missing: %v", err)
	}
}

func TestSwarmPublishedPolicyIgnoresUnpublishedPortEntries(t *testing.T) {
	var spec swarmServiceSpec
	raw := "{\"EndpointSpec\":{\"Ports\":[{\"Protocol\":\"tcp\",\"TargetPort\":8080,\"PublishedPort\":0},{\"Protocol\":\"tcp\",\"TargetPort\":8443,\"PublishedPort\":443}]}}"
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		t.Fatalf("decode test service spec: %v", err)
	}
	ports := swarmNetworkPolicyPorts(spec)
	if len(ports) != 1 || ports[0].Port == nil || ports[0].Port.IntVal != 8443 {
		t.Fatalf("unexpected published policy ports: %#v", ports)
	}
}

func TestValidationRejectsStaleNetworkLabels(t *testing.T) {
	ctx := context.Background()
	deployment := managedDeployment("stale")
	deployment.Annotations[types.AnnotationNetworkIDs] = "[]"
	deployment.Spec.Template.Annotations[types.AnnotationNetworkIDs] = "[]"
	deployment.Labels[types.LabelNetworkPrefix+"deadbeef"] = "true"
	deployment.Spec.Template.Labels[types.LabelNetworkPrefix+"deadbeef"] = "true"
	a := newIsolationTestAdapter(deployment)

	err := a.reconcileIsolationState(ctx)
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("expected stale network metadata rejection, got %v", err)
	}
}
