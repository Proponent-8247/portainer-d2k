package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

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
	network.Labels["com.docker.stack.namespace"] = "demo"
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

func TestCreateContainerRejectsNonAttachableOverlay(t *testing.T) {
	ctx := context.Background()
	network := testNetwork("locked-overlay", false, false)
	a := newIsolationTestAdapter()
	a.networks[network.Name] = network
	if err := a.persistNetwork(ctx, network); err != nil {
		t.Fatalf("persistNetwork: %v", err)
	}

	_, _, err := a.CreateContainer(ctx, RunOptions{
		Name:     "standalone",
		Image:    "example.invalid/test:1",
		Networks: []string{network.Name},
	})
	if err == nil || !strings.Contains(err.Error(), "not manually attachable") {
		t.Fatalf("expected initial non-attachable overlay rejection, got %v", err)
	}
}

func TestConnectNetworkRejectsCorruptMembership(t *testing.T) {
	ctx := context.Background()
	network := testNetwork("front", false, true)
	deployment := managedDeployment("web", network.ID)
	deployment.Annotations[types.AnnotationNetworkIDs] = "{broken"
	deployment.Spec.Template.Annotations[types.AnnotationNetworkIDs] = "{broken"
	a := newIsolationTestAdapter(deployment)
	a.networks[network.Name] = network
	if err := a.persistNetwork(ctx, network); err != nil {
		t.Fatalf("persistNetwork: %v", err)
	}

	err := a.ConnectNetwork(ctx, network.Name, deployment.Name)
	if err == nil || !strings.Contains(err.Error(), "invalid network metadata") {
		t.Fatalf("expected corrupt membership rejection, got %v", err)
	}
}

func TestSwarmReadbackPreservesExplicitEmptyMembership(t *testing.T) {
	a := newIsolationTestAdapter()
	deployment := managedDeployment("disconnected")
	deployment.Annotations[types.AnnotationNetworkIDs] = "[]"
	deployment.Spec.Template.Annotations[types.AnnotationNetworkIDs] = "[]"

	networks := a.swarmNetworksForDeployment(*deployment)
	if len(networks) != 0 {
		t.Fatalf("explicitly disconnected service gained synthetic network readback: %#v", networks)
	}
}

func TestStackRemovalKeepsExternalUnderscoreNetwork(t *testing.T) {
	ctx := context.Background()
	stackNetwork := testNetwork("demo_default", false, false)
	stackNetwork.Labels["com.docker.stack.namespace"] = "demo"
	external := testNetwork("demo_shared", false, true)

	deployment := managedDeployment("demo-web", stackNetwork.ID)
	deployment.UID = "service-uid"
	deployment.Labels[types.LabelSwarmManagedBy] = types.LabelSwarmManagedByValue
	deployment.Labels[types.LabelSwarmStack] = "demo"
	deployment.Labels[types.LabelSwarmService] = "demo-web"
	deployment.Spec.Template.Labels[types.LabelSwarmManagedBy] = types.LabelSwarmManagedByValue
	deployment.Spec.Template.Labels[types.LabelSwarmStack] = "demo"

	a := newIsolationTestAdapter(deployment)
	for _, network := range []*NetworkSummary{stackNetwork, external} {
		a.networks[network.Name] = network
		if err := a.persistNetwork(ctx, network); err != nil {
			t.Fatalf("persistNetwork(%s): %v", network.Name, err)
		}
		if err := a.ensureNetworkIsolationPolicy(ctx, network); err != nil {
			t.Fatalf("ensureNetworkIsolationPolicy(%s): %v", network.Name, err)
		}
	}

	if err := a.SwarmDeleteStack(ctx, "demo"); err != nil {
		t.Fatalf("SwarmDeleteStack: %v", err)
	}

	if _, err := a.client.CoreV1().ConfigMaps(a.namespace).Get(ctx, networkStateName(stackNetwork.ID), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("stack-owned network state still exists: %v", err)
	}
	if _, err := a.client.CoreV1().ConfigMaps(a.namespace).Get(ctx, networkStateName(external.ID), metav1.GetOptions{}); err != nil {
		t.Fatalf("external underscore network was incorrectly removed: %v", err)
	}
}

func TestIsolationRejectsMacvlanAndIPvlan(t *testing.T) {
	ctx := context.Background()
	a := newIsolationTestAdapter()
	for _, driver := range []string{"macvlan", "ipvlan"} {
		if _, _, err := a.CreateNetwork(ctx, CreateNetworkOptions{Name: "bad-" + driver, Driver: driver}); err == nil {
			t.Fatalf("unsupported driver %q was accepted", driver)
		}
	}
}

func TestSwarmUpdateReappliesSpecAfterConflict(t *testing.T) {
	ctx := context.Background()
	network := testNetwork("front", false, true)
	deployment := managedDeployment("svc", network.ID)
	deployment.UID = "service-uid"
	deployment.Labels[types.LabelSwarmManagedBy] = types.LabelSwarmManagedByValue
	deployment.Labels[types.LabelSwarmService] = "svc"
	deployment.Spec.Template.Labels[types.LabelSwarmManagedBy] = types.LabelSwarmManagedByValue
	deployment.Spec.Template.Labels[types.LabelSwarmService] = "svc"
	deployment.Spec.Template.Spec.Containers = []corev1.Container{{Name: "svc", Image: "old:1"}}
	a := newIsolationTestAdapter(deployment)
	a.networks[network.Name] = network
	if err := a.persistNetwork(ctx, network); err != nil {
		t.Fatalf("persistNetwork: %v", err)
	}

	client := a.client.(*fake.Clientset)
	conflicted := false
	client.Fake.PrependReactor("update", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if conflicted {
			return false, nil, nil
		}
		conflicted = true
		return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, "svc", fmt.Errorf("test conflict"))
	})

	body := strings.NewReader(`{"TaskTemplate":{"ContainerSpec":{"Image":"new:2"},"Networks":[{"Target":"` + network.ID + `"}]},"Mode":{"Replicated":{"Replicas":2}}}`)
	if err := a.SwarmUpdateService(ctx, "svc", body); err != nil {
		t.Fatalf("SwarmUpdateService: %v", err)
	}
	got, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, "svc", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get updated service: %v", err)
	}
	if got.Spec.Template.Spec.Containers[0].Image != "new:2" {
		t.Fatalf("conflict retry lost image update: %q", got.Spec.Template.Spec.Containers[0].Image)
	}
	if got.Spec.Replicas == nil || *got.Spec.Replicas != 2 {
		t.Fatalf("conflict retry lost replica update: %#v", got.Spec.Replicas)
	}
}

func TestSwarmUpdateKeepsExplicitEmptyNetworkMembership(t *testing.T) {
	ctx := context.Background()
	network := testNetwork("front", false, true)
	deployment := managedDeployment("svc", network.ID)
	deployment.UID = "service-uid"
	deployment.Labels[types.LabelSwarmManagedBy] = types.LabelSwarmManagedByValue
	deployment.Labels[types.LabelSwarmService] = "svc"
	deployment.Spec.Template.Labels[types.LabelSwarmManagedBy] = types.LabelSwarmManagedByValue
	deployment.Spec.Template.Labels[types.LabelSwarmService] = "svc"
	deployment.Spec.Template.Spec.Containers = []corev1.Container{{Name: "svc", Image: "old:1"}}
	a := newIsolationTestAdapter(deployment)
	a.networks[network.Name] = network
	if err := a.persistNetwork(ctx, network); err != nil {
		t.Fatalf("persistNetwork: %v", err)
	}

	body := strings.NewReader(`{"TaskTemplate":{"ContainerSpec":{"Image":"old:1"},"Networks":[]}}`)
	if err := a.SwarmUpdateService(ctx, "svc", body); err != nil {
		t.Fatalf("SwarmUpdateService: %v", err)
	}
	got, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, "svc", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get updated service: %v", err)
	}
	if got.Annotations[types.AnnotationNetworkIDs] != "[]" {
		t.Fatalf("empty membership was rewritten: %q", got.Annotations[types.AnnotationNetworkIDs])
	}
	if networks := a.swarmNetworksForDeployment(*got); len(networks) != 0 {
		t.Fatalf("empty membership readback is not empty: %#v", networks)
	}
}

func TestSwarmDeleteServiceUsesDNSOwnershipAnnotation(t *testing.T) {
	ctx := context.Background()
	deployment := managedDeployment("demo-stack-api-v2")
	deployment.UID = "service-uid"
	deployment.Labels[types.LabelSwarmManagedBy] = types.LabelSwarmManagedByValue
	deployment.Labels[types.LabelSwarmService] = "demo-stack-api-v2"
	deployment.Spec.Template.Labels[types.LabelSwarmManagedBy] = types.LabelSwarmManagedByValue
	dns := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "api-v2", Namespace: "d2k-workloads",
			Annotations: map[string]string{
				"d2k.portainer.io/dns-service":    "true",
				"d2k.portainer.io/dns-for-deploy": "demo-stack-api-v2",
			},
		},
	}
	a := newIsolationTestAdapter(deployment, dns)
	if err := a.SwarmDeleteService(ctx, deployment.Name); err != nil {
		t.Fatalf("SwarmDeleteService: %v", err)
	}
	if _, err := a.client.CoreV1().Services(a.namespace).Get(ctx, dns.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("owned DNS service still exists after service removal: %v", err)
	}
}

func TestSwarmUpdateReconcilesServicesAndPublishedPolicy(t *testing.T) {
	ctx := context.Background()
	network := testNetwork("front", false, true)
	deployment := managedDeployment("svc", network.ID)
	deployment.UID = "service-uid"
	deployment.Labels[types.LabelSwarmManagedBy] = types.LabelSwarmManagedByValue
	deployment.Labels[types.LabelSwarmService] = "svc"
	deployment.Spec.Template.Labels[types.LabelSwarmManagedBy] = types.LabelSwarmManagedByValue
	deployment.Spec.Template.Labels[types.LabelSwarmService] = "svc"
	deployment.Spec.Template.Spec.Containers = []corev1.Container{{Name: "svc", Image: "image:1"}}
	dns := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "svc", Namespace: "d2k-workloads",
			Annotations: map[string]string{
				"d2k.portainer.io/dns-service":    "true",
				"d2k.portainer.io/dns-for-deploy": "svc",
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "svc"},
			Ports:    []corev1.ServicePort{{Name: "port-0", Port: 80, TargetPort: intstr.FromInt(80)}},
		},
	}
	lb := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: publishedServiceName("svc"), Namespace: "d2k-workloads"},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeLoadBalancer, Selector: map[string]string{"app": "svc"},
			Ports: []corev1.ServicePort{{Name: "port-0", Port: 8080, TargetPort: intstr.FromInt(80)}},
		},
	}
	a := newIsolationTestAdapter(deployment, dns, lb)
	a.networks[network.Name] = network
	if err := a.persistNetwork(ctx, network); err != nil {
		t.Fatalf("persistNetwork: %v", err)
	}

	body := strings.NewReader(`{"TaskTemplate":{"ContainerSpec":{"Image":"image:1"},"Networks":[{"Target":"` + network.ID + `"}]},"EndpointSpec":{"Mode":"vip","Ports":[{"Protocol":"tcp","TargetPort":8443,"PublishedPort":9443,"PublishMode":"ingress"}]}}`)
	if err := a.SwarmUpdateService(ctx, "svc", body); err != nil {
		t.Fatalf("SwarmUpdateService: %v", err)
	}

	gotDNS, err := a.client.CoreV1().Services(a.namespace).Get(ctx, "svc", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get DNS service: %v", err)
	}
	if len(gotDNS.Spec.Ports) != 1 || gotDNS.Spec.Ports[0].Port != 8443 {
		t.Fatalf("DNS service ports not reconciled: %#v", gotDNS.Spec.Ports)
	}
	gotLB, err := a.client.CoreV1().Services(a.namespace).Get(ctx, publishedServiceName("svc"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get published service: %v", err)
	}
	if len(gotLB.Spec.Ports) != 1 || gotLB.Spec.Ports[0].Port != 9443 || gotLB.Spec.Ports[0].TargetPort.IntVal != 8443 {
		t.Fatalf("published service ports not reconciled: %#v", gotLB.Spec.Ports)
	}
	policy, err := a.client.NetworkingV1().NetworkPolicies(a.namespace).Get(ctx, publishedPolicyName("svc"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get published policy: %v", err)
	}
	if len(policy.Spec.Ingress) != 1 || len(policy.Spec.Ingress[0].Ports) != 1 || policy.Spec.Ingress[0].Ports[0].Port.IntVal != 8443 {
		t.Fatalf("published policy ports not reconciled: %#v", policy.Spec.Ingress)
	}
}

func TestSwarmUpdateClearsPublishedServiceAndPreservesHeadlessDNS(t *testing.T) {
	ctx := context.Background()
	network := testNetwork("front", false, true)
	deployment := managedDeployment("svc", network.ID)
	deployment.UID = "service-uid"
	deployment.Labels[types.LabelSwarmManagedBy] = types.LabelSwarmManagedByValue
	deployment.Labels[types.LabelSwarmService] = "svc"
	deployment.Spec.Template.Labels[types.LabelSwarmManagedBy] = types.LabelSwarmManagedByValue
	deployment.Spec.Template.Spec.Containers = []corev1.Container{{Name: "svc", Image: "image:1"}}
	dns := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "svc", Namespace: "d2k-workloads",
			Annotations: map[string]string{
				"d2k.portainer.io/dns-service":    "true",
				"d2k.portainer.io/dns-for-deploy": "svc",
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "svc"},
			Ports:    []corev1.ServicePort{{Name: "port-0", Port: 80, TargetPort: intstr.FromInt(80)}},
		},
	}
	lb := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: publishedServiceName("svc"), Namespace: "d2k-workloads"},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeLoadBalancer, Selector: map[string]string{"app": "svc"},
			Ports: []corev1.ServicePort{{Name: "port-0", Port: 8080, TargetPort: intstr.FromInt(80)}},
		},
	}
	a := newIsolationTestAdapter(deployment, dns, lb)
	a.networks[network.Name] = network
	if err := a.persistNetwork(ctx, network); err != nil {
		t.Fatalf("persistNetwork: %v", err)
	}
	if err := a.ensurePublishedIngressPolicy(ctx, "svc", []networkingv1.NetworkPolicyPort{{Port: intstrPtr(intstr.FromInt(80))}}); err != nil {
		t.Fatalf("ensurePublishedIngressPolicy: %v", err)
	}

	body := strings.NewReader(`{"TaskTemplate":{"ContainerSpec":{"Image":"image:1"},"Networks":[{"Target":"` + network.ID + `"}]},"EndpointSpec":{"Mode":"vip","Ports":[]}}`)
	if err := a.SwarmUpdateService(ctx, "svc", body); err != nil {
		t.Fatalf("SwarmUpdateService: %v", err)
	}
	if _, err := a.client.CoreV1().Services(a.namespace).Get(ctx, publishedServiceName("svc"), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("published service still exists after clearing ports: %v", err)
	}
	if _, err := a.client.NetworkingV1().NetworkPolicies(a.namespace).Get(ctx, publishedPolicyName("svc"), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("published policy still exists after clearing ports: %v", err)
	}
	gotDNS, err := a.client.CoreV1().Services(a.namespace).Get(ctx, "svc", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get replacement DNS service: %v", err)
	}
	if gotDNS.Spec.ClusterIP != "None" || len(gotDNS.Spec.Ports) != 0 {
		t.Fatalf("DNS service was not converted to headless: %#v", gotDNS.Spec)
	}
}

func TestSwarmUpdateHostPublishingRemovesLoadBalancer(t *testing.T) {
	ctx := context.Background()
	network := testNetwork("front", false, true)
	deployment := managedDeployment("svc", network.ID)
	deployment.UID = "service-uid"
	deployment.Labels[types.LabelSwarmManagedBy] = types.LabelSwarmManagedByValue
	deployment.Labels[types.LabelSwarmService] = "svc"
	deployment.Spec.Template.Labels[types.LabelSwarmManagedBy] = types.LabelSwarmManagedByValue
	deployment.Spec.Template.Spec.Containers = []corev1.Container{{Name: "svc", Image: "image:1"}}
	dns := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "svc", Namespace: "d2k-workloads",
			Annotations: map[string]string{
				"d2k.portainer.io/dns-service":    "true",
				"d2k.portainer.io/dns-for-deploy": "svc",
			},
		},
		Spec: corev1.ServiceSpec{ClusterIP: "None", Selector: map[string]string{"app": "svc"}},
	}
	lb := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: publishedServiceName("svc"), Namespace: "d2k-workloads"},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeLoadBalancer, Selector: map[string]string{"app": "svc"},
			Ports: []corev1.ServicePort{{Name: "port-0", Port: 8080, TargetPort: intstr.FromInt(80)}},
		},
	}
	a := newIsolationTestAdapter(deployment, dns, lb)
	a.networks[network.Name] = network
	if err := a.persistNetwork(ctx, network); err != nil {
		t.Fatalf("persistNetwork: %v", err)
	}

	body := strings.NewReader(`{"TaskTemplate":{"ContainerSpec":{"Image":"image:1"},"Networks":[{"Target":"` + network.ID + `"}]},"EndpointSpec":{"Mode":"dnsrr","Ports":[{"Protocol":"tcp","TargetPort":80,"PublishedPort":8080,"PublishMode":"host"}]}}`)
	if err := a.SwarmUpdateService(ctx, "svc", body); err != nil {
		t.Fatalf("SwarmUpdateService: %v", err)
	}
	got, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, "svc", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get updated deployment: %v", err)
	}
	if got.Annotations[types.AnnotationEndpointMode] != "dnsrr" {
		t.Fatalf("endpoint mode annotation not updated: %#v", got.Annotations)
	}
	if len(got.Spec.Template.Spec.Containers[0].Ports) != 1 || got.Spec.Template.Spec.Containers[0].Ports[0].HostPort != 8080 {
		t.Fatalf("hostPort was not reconciled: %#v", got.Spec.Template.Spec.Containers[0].Ports)
	}
	if _, err := a.client.CoreV1().Services(a.namespace).Get(ctx, publishedServiceName("svc"), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("LoadBalancer service still exists in host-publish mode: %v", err)
	}
}

func TestSwarmCreateFailsClosedWhenPublishedServiceCreationFails(t *testing.T) {
	ctx := context.Background()
	network := testNetwork("front", false, true)
	a := newIsolationTestAdapter()
	a.networks[network.Name] = network
	if err := a.persistNetwork(ctx, network); err != nil {
		t.Fatalf("persistNetwork: %v", err)
	}

	client := a.client.(*fake.Clientset)
	client.Fake.PrependReactor("create", "services", func(action k8stesting.Action) (bool, runtime.Object, error) {
		create := action.(k8stesting.CreateAction)
		service := create.GetObject().(*corev1.Service)
		if service.Name == publishedServiceName("svc") {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "services"}, service.Name, fmt.Errorf("test denial"))
		}
		return false, nil, nil
	})

	body := strings.NewReader(`{"Name":"svc","TaskTemplate":{"ContainerSpec":{"Image":"image:1"},"Networks":[{"Target":"` + network.ID + `"}]},"EndpointSpec":{"Mode":"vip","Ports":[{"Protocol":"tcp","TargetPort":80,"PublishedPort":8080,"PublishMode":"ingress"}]}}`)
	if _, err := a.SwarmCreateService(ctx, body); err == nil || !strings.Contains(err.Error(), "network isolation is enabled") {
		t.Fatalf("expected fail-closed published Service error, got %v", err)
	}
	if _, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, "svc", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("deployment remained after publication failure: %v", err)
	}
	if _, err := a.client.NetworkingV1().NetworkPolicies(a.namespace).Get(ctx, publishedPolicyName("svc"), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("published allow policy exists despite publication failure: %v", err)
	}
}

func TestDisconnectPatchesLivePodMembershipBeforeRollout(t *testing.T) {
	ctx := context.Background()
	front := testNetwork("front", false, true)
	back := testNetwork("back", false, true)
	deployment := managedDeployment("web", front.ID, back.ID)
	deployment.UID = "container-uid"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web-old-pod", Namespace: "d2k-workloads",
			Labels: map[string]string{
				"app":                     "web",
				types.LabelManagedBy:      types.LabelManagedByValue,
				networkLabelKey(front.ID): "true",
				networkLabelKey(back.ID):  "true",
			},
			Annotations: map[string]string{types.AnnotationNetworkIDs: encodeNetworkIDs([]string{front.ID, back.ID})},
		},
	}
	a := newIsolationTestAdapter(deployment, pod)
	for _, network := range []*NetworkSummary{front, back} {
		a.networks[network.Name] = network
		if err := a.persistNetwork(ctx, network); err != nil {
			t.Fatalf("persistNetwork(%s): %v", network.Name, err)
		}
	}

	if err := a.DisconnectNetwork(ctx, back.Name, deployment.Name); err != nil {
		t.Fatalf("DisconnectNetwork: %v", err)
	}
	got, err := a.client.CoreV1().Pods(a.namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get patched pod: %v", err)
	}
	if got.Labels[networkLabelKey(back.ID)] != "" {
		t.Fatalf("removed network label remains on live pod: %#v", got.Labels)
	}
	if got.Labels[networkLabelKey(front.ID)] != "true" {
		t.Fatalf("remaining network label was lost: %#v", got.Labels)
	}
	if got.Annotations[types.AnnotationNetworkIDs] != encodeNetworkIDs([]string{front.ID}) {
		t.Fatalf("live pod membership annotation is wrong: %q", got.Annotations[types.AnnotationNetworkIDs])
	}
}

func TestConnectPatchesLivePodMembershipAfterDeploymentUpdate(t *testing.T) {
	ctx := context.Background()
	front := testNetwork("front", false, true)
	back := testNetwork("back", false, true)
	deployment := managedDeployment("web", front.ID)
	deployment.UID = "container-uid"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web-pod", Namespace: "d2k-workloads",
			Labels: map[string]string{
				"app":                     "web",
				types.LabelManagedBy:      types.LabelManagedByValue,
				networkLabelKey(front.ID): "true",
			},
			Annotations: map[string]string{types.AnnotationNetworkIDs: encodeNetworkIDs([]string{front.ID})},
		},
	}
	a := newIsolationTestAdapter(deployment, pod)
	for _, network := range []*NetworkSummary{front, back} {
		a.networks[network.Name] = network
		if err := a.persistNetwork(ctx, network); err != nil {
			t.Fatalf("persistNetwork(%s): %v", network.Name, err)
		}
	}

	if err := a.ConnectNetwork(ctx, back.Name, deployment.Name); err != nil {
		t.Fatalf("ConnectNetwork: %v", err)
	}
	got, err := a.client.CoreV1().Pods(a.namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get patched pod: %v", err)
	}
	if got.Labels[networkLabelKey(front.ID)] != "true" || got.Labels[networkLabelKey(back.ID)] != "true" {
		t.Fatalf("live pod did not gain additive network membership: %#v", got.Labels)
	}
}

func TestStartupReconcilesLivePodMembershipFromDeployment(t *testing.T) {
	ctx := context.Background()
	front := testNetwork("front", false, true)
	back := testNetwork("back", false, true)
	deployment := managedDeployment("web", front.ID)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web-stale", Namespace: "d2k-workloads",
			Labels: map[string]string{
				"app":                    "web",
				types.LabelManagedBy:     types.LabelManagedByValue,
				networkLabelKey(back.ID): "true",
			},
			Annotations: map[string]string{
				types.AnnotationNetworkIDs: encodeNetworkIDs([]string{back.ID}),
			},
		},
	}
	a := newIsolationTestAdapter(deployment, pod)
	for _, network := range []*NetworkSummary{front, back} {
		a.networks[network.Name] = network
		if err := a.persistNetwork(ctx, network); err != nil {
			t.Fatalf("persistNetwork(%s): %v", network.Name, err)
		}
	}

	if err := a.reconcileIsolationState(ctx); err != nil {
		t.Fatalf("reconcileIsolationState: %v", err)
	}
	got, err := a.client.CoreV1().Pods(a.namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get reconciled pod: %v", err)
	}
	if got.Labels[networkLabelKey(front.ID)] != "true" || got.Labels[networkLabelKey(back.ID)] != "" {
		t.Fatalf("live Pod labels were not reconciled from Deployment membership: %#v", got.Labels)
	}
	if got.Annotations[types.AnnotationNetworkIDs] != encodeNetworkIDs([]string{front.ID}) {
		t.Fatalf("live Pod membership annotation was not reconciled: %q", got.Annotations[types.AnnotationNetworkIDs])
	}
}

func TestRenameContainerRewritesOwnedResourcesAndIsolationPolicy(t *testing.T) {
	ctx := context.Background()
	network := testNetwork("front", false, true)
	deployment := managedDeployment("old-name", network.ID)
	deployment.UID = "old-uid"
	deployment.Spec.Template.Spec.Containers = []corev1.Container{{Name: "old-name", Image: "image:1"}}
	dns := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "old-name", Namespace: "d2k-workloads", Labels: managedLabels("old-name")},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "old-name"},
			Ports:    []corev1.ServicePort{{Name: "port-0", Port: 80, TargetPort: intstr.FromInt(80)}},
		},
	}
	published := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: publishedServiceName("old-name"), Namespace: "d2k-workloads", Labels: managedLabels("old-name"), Finalizers: []string{"service.kubernetes.io/load-balancer-cleanup"}},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeLoadBalancer,
			Selector: map[string]string{"app": "old-name"},
			Ports:    []corev1.ServicePort{{Name: "port-0", Port: 8080, TargetPort: intstr.FromInt(80), Protocol: corev1.ProtocolTCP}},
		},
	}
	a := newIsolationTestAdapter(deployment, dns, published)
	a.networks[network.Name] = network
	if err := a.persistNetwork(ctx, network); err != nil {
		t.Fatalf("persistNetwork: %v", err)
	}
	tcp := corev1.ProtocolTCP
	p := intstr.FromInt(80)
	if err := a.ensurePublishedIngressPolicy(ctx, "old-name", []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &p}}); err != nil {
		t.Fatalf("old published policy: %v", err)
	}

	if err := a.RenameContainer(ctx, "old-name", "new_name"); err != nil {
		t.Fatalf("RenameContainer: %v", err)
	}

	if _, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, "old-name", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("old deployment still exists: %v", err)
	}
	got, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, "new-name", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("new deployment missing: %v", err)
	}
	if got.Spec.Selector.MatchLabels["app"] != "new-name" || got.Spec.Template.Labels["app"] != "new-name" {
		t.Fatalf("renamed deployment selectors were not rewritten: %#v %#v", got.Spec.Selector.MatchLabels, got.Spec.Template.Labels)
	}
	if got.Labels[types.LabelWorkloadName] != "new-name" || got.Spec.Template.Labels[types.LabelWorkloadName] != "new-name" {
		t.Fatalf("renamed workload-name labels were not rewritten")
	}
	if got.Spec.Template.Labels[networkLabelKey(network.ID)] != "true" || got.Annotations[types.AnnotationNetworkIDs] != encodeNetworkIDs([]string{network.ID}) {
		t.Fatalf("network isolation metadata was not preserved across rename")
	}
	if len(got.Spec.Template.Spec.Containers) != 1 || got.Spec.Template.Spec.Containers[0].Name != "new-name" {
		t.Fatalf("pod container identity was not rewritten: %#v", got.Spec.Template.Spec.Containers)
	}

	newDNS, err := a.client.CoreV1().Services(a.namespace).Get(ctx, "new-name", metav1.GetOptions{})
	if err != nil || newDNS.Spec.Selector["app"] != "new-name" {
		t.Fatalf("renamed DNS service wrong: %#v %v", newDNS, err)
	}
	newPublishedName := publishedServiceName("new-name")
	newPublished, err := a.client.CoreV1().Services(a.namespace).Get(ctx, newPublishedName, metav1.GetOptions{})
	if err != nil || newPublished.Spec.Selector["app"] != "new-name" {
		t.Fatalf("renamed published service wrong: %#v %v", newPublished, err)
	}
	if len(newPublished.Finalizers) != 0 {
		t.Fatalf("renamed service copied controller finalizers from the old Service: %#v", newPublished.Finalizers)
	}
	if _, err := a.client.NetworkingV1().NetworkPolicies(a.namespace).Get(ctx, publishedPolicyName("new-name"), metav1.GetOptions{}); err != nil {
		t.Fatalf("renamed published policy missing: %v", err)
	}
	if _, err := a.client.NetworkingV1().NetworkPolicies(a.namespace).Get(ctx, publishedPolicyName("old-name"), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("old published policy still exists: %v", err)
	}
	if _, err := a.client.CoreV1().Services(a.namespace).Get(ctx, "old-name", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("old DNS service still exists: %v", err)
	}
	if _, err := a.client.CoreV1().Services(a.namespace).Get(ctx, publishedServiceName("old-name"), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("old published service still exists: %v", err)
	}
}

func TestStartContainerRejectsSwarmManagedDeployment(t *testing.T) {
	ctx := context.Background()
	deployment := managedDeployment("swarm-owned")
	replicas := int32(0)
	deployment.Spec.Replicas = &replicas
	deployment.Labels[types.LabelSwarmManagedBy] = types.LabelSwarmManagedByValue
	a := newIsolationTestAdapter(deployment)

	err := a.StartContainer(ctx, deployment.Name)
	if err == nil || !strings.Contains(err.Error(), "managed by swarm") {
		t.Fatalf("expected Swarm ownership rejection, got %v", err)
	}
	got, getErr := a.client.AppsV1().Deployments(a.namespace).Get(ctx, deployment.Name, metav1.GetOptions{})
	if getErr != nil {
		t.Fatalf("get deployment: %v", getErr)
	}
	if got.Spec.Replicas == nil || *got.Spec.Replicas != 0 {
		t.Fatalf("Swarm-owned deployment was scaled by docker start")
	}
}

func TestRenameContainerRejectsSwarmManagedDeployment(t *testing.T) {
	ctx := context.Background()
	deployment := managedDeployment("swarm-owned")
	deployment.Labels[types.LabelSwarmManagedBy] = types.LabelSwarmManagedByValue
	a := newIsolationTestAdapter(deployment)

	err := a.RenameContainer(ctx, deployment.Name, "renamed")
	if err == nil || !strings.Contains(err.Error(), "managed by swarm") {
		t.Fatalf("expected Swarm ownership rejection, got %v", err)
	}
	if _, getErr := a.client.AppsV1().Deployments(a.namespace).Get(ctx, "renamed", metav1.GetOptions{}); !apierrors.IsNotFound(getErr) {
		t.Fatalf("renamed Swarm deployment unexpectedly exists: %v", getErr)
	}
}

func TestStopAndRemoveFailClosedOnOwnershipLookupError(t *testing.T) {
	ctx := context.Background()

	for _, operation := range []string{"stop", "remove"} {
		t.Run(operation, func(t *testing.T) {
			deployment := managedDeployment("standalone")
			replicas := int32(1)
			deployment.Spec.Replicas = &replicas
			a := newIsolationTestAdapter(deployment)
			gets := 0
			a.client.(*fake.Clientset).PrependReactor("get", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
				gets++
				if gets == 2 {
					return true, nil, fmt.Errorf("simulated ownership lookup failure")
				}
				return false, nil, nil
			})

			var err error
			if operation == "stop" {
				err = a.StopContainer(ctx, deployment.Name)
			} else {
				err = a.RemoveContainer(ctx, deployment.Name)
			}
			if err == nil || !strings.Contains(err.Error(), "verify ownership") {
				t.Fatalf("%s should fail closed on ownership lookup error, got %v", operation, err)
			}

			got, getErr := a.client.AppsV1().Deployments(a.namespace).Get(ctx, deployment.Name, metav1.GetOptions{})
			if getErr != nil {
				t.Fatalf("deployment was deleted after failed ownership verification: %v", getErr)
			}
			if got.Spec.Replicas == nil || *got.Spec.Replicas != 1 {
				t.Fatalf("deployment was scaled after failed ownership verification")
			}
		})
	}
}

func TestIsolationPoliciesSelectOnlyD2KManagedPods(t *testing.T) {
	ctx := context.Background()
	a := newIsolationTestAdapter()
	network := testNetwork("frontend", false, true)

	if err := a.ensureNetworkIsolationPolicy(ctx, network); err != nil {
		t.Fatalf("ensureNetworkIsolationPolicy: %v", err)
	}
	policy, err := a.client.NetworkingV1().NetworkPolicies(a.namespace).Get(ctx, networkPolicyName(network.ID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get network policy: %v", err)
	}
	if policy.Spec.PodSelector.MatchLabels[types.LabelManagedBy] != types.LabelManagedByValue {
		t.Fatalf("network policy does not scope selected pods to d2k ownership: %#v", policy.Spec.PodSelector.MatchLabels)
	}
	if len(policy.Spec.Ingress) == 0 || len(policy.Spec.Ingress[0].From) == 0 ||
		policy.Spec.Ingress[0].From[0].PodSelector == nil ||
		policy.Spec.Ingress[0].From[0].PodSelector.MatchLabels[types.LabelManagedBy] != types.LabelManagedByValue {
		t.Fatalf("same-network peer does not require d2k ownership: %#v", policy.Spec.Ingress)
	}

	tcp := corev1.ProtocolTCP
	port := intstr.FromInt(80)
	if err := a.ensurePublishedIngressPolicy(ctx, "web", []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}}); err != nil {
		t.Fatalf("ensurePublishedIngressPolicy: %v", err)
	}
	published, err := a.client.NetworkingV1().NetworkPolicies(a.namespace).Get(ctx, publishedPolicyName("web"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get published policy: %v", err)
	}
	if published.Spec.PodSelector.MatchLabels["app"] != "web" ||
		published.Spec.PodSelector.MatchLabels[types.LabelManagedBy] != types.LabelManagedByValue {
		t.Fatalf("published policy selector is not ownership-scoped: %#v", published.Spec.PodSelector.MatchLabels)
	}
}

func TestSwarmCreateExistingReconcilesNetworksServicesAndPolicy(t *testing.T) {
	ctx := context.Background()
	front := testNetwork("front", false, true)
	back := testNetwork("back", false, true)

	existing := managedDeployment("svc", front.ID)
	existing.UID = "existing-uid"
	existing.Annotations[types.AnnotationSwarmServiceID] = "stable-service-id"
	existing.Labels[types.LabelSwarmManagedBy] = types.LabelSwarmManagedByValue
	existing.Labels[types.LabelSwarmService] = "svc"
	existing.Spec.Template.Labels[types.LabelSwarmManagedBy] = types.LabelSwarmManagedByValue
	existing.Spec.Template.Labels[types.LabelSwarmService] = "svc"
	existing.Spec.Template.Spec.Containers = []corev1.Container{{Name: "svc", Image: "image:old"}}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "svc-old-pod", Namespace: "d2k-workloads",
			Labels: map[string]string{
				"app":                     "svc",
				types.LabelManagedBy:      types.LabelManagedByValue,
				networkLabelKey(front.ID): "true",
			},
			Annotations: map[string]string{types.AnnotationNetworkIDs: encodeNetworkIDs([]string{front.ID})},
		},
	}
	dns := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "svc", Namespace: "d2k-workloads",
			Annotations: map[string]string{
				"d2k.portainer.io/dns-service":    "true",
				"d2k.portainer.io/dns-for-deploy": "svc",
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "svc"},
			Ports:    []corev1.ServicePort{{Name: "port-0", Port: 80, TargetPort: intstr.FromInt(80)}},
		},
	}
	lb := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: publishedServiceName("svc"), Namespace: "d2k-workloads"},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeLoadBalancer,
			Selector: map[string]string{"app": "svc"},
			Ports:    []corev1.ServicePort{{Name: "port-0", Port: 8080, TargetPort: intstr.FromInt(80)}},
		},
	}

	a := newIsolationTestAdapter(existing, pod, dns, lb)
	for _, network := range []*NetworkSummary{front, back} {
		a.networks[network.Name] = network
		if err := a.persistNetwork(ctx, network); err != nil {
			t.Fatalf("persistNetwork(%s): %v", network.Name, err)
		}
	}
	tcp := corev1.ProtocolTCP
	oldPort := intstr.FromInt(80)
	if err := a.ensurePublishedIngressPolicy(ctx, "svc", []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &oldPort}}); err != nil {
		t.Fatalf("old published policy: %v", err)
	}

	body := strings.NewReader(`{
		"Name":"svc",
		"TaskTemplate":{"ContainerSpec":{"Image":"image:new"},"Networks":[{"Target":"` + back.ID + `"}]},
		"Mode":{"Replicated":{"Replicas":1}},
		"EndpointSpec":{"Mode":"vip","Ports":[{"Protocol":"tcp","TargetPort":8443,"PublishedPort":9443,"PublishMode":"ingress"}]}
	}`)
	result, err := a.SwarmCreateService(ctx, body)
	if err != nil {
		t.Fatalf("SwarmCreateService existing path: %v", err)
	}
	if result["ID"] != "stable-service-id" {
		t.Fatalf("stable service ID not preserved: %#v", result)
	}

	got, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, "svc", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get updated deployment: %v", err)
	}
	if got.Annotations[types.AnnotationNetworkIDs] != encodeNetworkIDs([]string{back.ID}) {
		t.Fatalf("deployment network membership was not replaced: %q", got.Annotations[types.AnnotationNetworkIDs])
	}
	livePod, err := a.client.CoreV1().Pods(a.namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get live pod: %v", err)
	}
	if livePod.Labels[networkLabelKey(front.ID)] != "" || livePod.Labels[networkLabelKey(back.ID)] != "true" {
		t.Fatalf("live pod membership was not reconciled: %#v", livePod.Labels)
	}

	gotDNS, err := a.client.CoreV1().Services(a.namespace).Get(ctx, "svc", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get DNS service: %v", err)
	}
	if len(gotDNS.Spec.Ports) != 1 || gotDNS.Spec.Ports[0].Port != 8443 ||
		gotDNS.Spec.Selector[types.LabelManagedBy] != types.LabelManagedByValue {
		t.Fatalf("DNS service was not fully reconciled: %#v", gotDNS.Spec)
	}

	gotLB, err := a.client.CoreV1().Services(a.namespace).Get(ctx, publishedServiceName("svc"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get published service: %v", err)
	}
	if len(gotLB.Spec.Ports) != 1 || gotLB.Spec.Ports[0].Port != 9443 ||
		gotLB.Spec.Ports[0].TargetPort.IntVal != 8443 ||
		gotLB.Spec.Selector[types.LabelManagedBy] != types.LabelManagedByValue {
		t.Fatalf("published service was not fully reconciled: %#v", gotLB.Spec)
	}

	policy, err := a.client.NetworkingV1().NetworkPolicies(a.namespace).Get(ctx, publishedPolicyName("svc"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get published policy: %v", err)
	}
	if len(policy.Spec.Ingress) != 1 || len(policy.Spec.Ingress[0].Ports) != 1 ||
		policy.Spec.Ingress[0].Ports[0].Port.IntVal != 8443 {
		t.Fatalf("published policy was not reconciled: %#v", policy.Spec.Ingress)
	}
}
