package adapter

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/portainer/d2k/internal/types"
	"github.com/portainer/d2k/pkg/portmapper"
)

func parseCIDRList(raw, field string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	seen := map[string]bool{}
	out := make([]string, 0)
	for _, part := range strings.Split(raw, ",") {
		value := strings.TrimSpace(part)
		if value == "" {
			continue
		}
		_, network, err := net.ParseCIDR(value)
		if err != nil {
			return nil, fmt.Errorf("%s contains invalid CIDR %q: %w", field, value, err)
		}
		canonical := network.String()
		if seen[canonical] {
			continue
		}
		seen[canonical] = true
		out = append(out, canonical)
	}
	sort.Strings(out)
	return out, nil
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", sum[:8])
}

func networkLabelKey(id string) string {
	return types.LabelNetworkPrefix + shortHash(id)
}

func networkStateName(id string) string {
	return "d2k-network-" + shortHash(id)
}

func networkPolicyName(id string) string {
	return "d2k-net-" + shortHash(id)
}

func publishedPolicyName(name string) string {
	return "d2k-published-" + shortHash(name)
}

func encodeNetworkIDs(ids []string) string {
	ids = append([]string{}, ids...)
	sort.Strings(ids)
	b, _ := json.Marshal(ids)
	return string(b)
}

func decodeNetworkIDs(raw string) []string {
	var ids []string
	if strings.TrimSpace(raw) == "" {
		return ids
	}
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil
	}
	return ids
}

func parseNetworkIDs(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("network membership annotation is missing")
	}
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil, fmt.Errorf("invalid network membership annotation: %w", err)
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			return nil, fmt.Errorf("network membership annotation contains an empty network ID")
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func (a *KubernetesDockerAdapter) reconcileIsolationState(ctx context.Context) error {
	if !a.networkIsolation {
		return nil
	}
	if err := a.restorePersistedNetworks(ctx); err != nil {
		return err
	}
	if err := a.validateAndReconcileManagedWorkloads(ctx); err != nil {
		return err
	}
	if err := a.ensureIsolationBaseline(ctx); err != nil {
		return err
	}
	a.isolationReady = true
	return nil
}

func (a *KubernetesDockerAdapter) validateAndReconcileManagedWorkloads(ctx context.Context) error {
	deployments, err := a.client.AppsV1().Deployments(a.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: types.LabelManagedBy + "=" + types.LabelManagedByValue,
	})
	if err != nil {
		return fmt.Errorf("unable to inspect existing d2k workloads before enabling isolation: %w", err)
	}

	for i := range deployments.Items {
		deployment := &deployments.Items[i]
		ids, err := parseNetworkIDs(deployment.Annotations[types.AnnotationNetworkIDs])
		if err != nil {
			return fmt.Errorf(
				"existing d2k workload %q has no valid network-isolation metadata; "+
					"refusing to install the default-deny policy because doing so would blackhole a legacy workload: %w",
				deployment.Name,
				err,
			)
		}

		expectedLabels := map[string]bool{}
		for _, id := range ids {
			expectedLabels[networkLabelKey(id)] = true
			network, ok := a.lookupNetwork(id)
			if !ok {
				return fmt.Errorf(
					"existing d2k workload %q references unknown Docker network %q; "+
						"restore the network state or recreate the workload before enabling isolation",
					deployment.Name,
					id,
				)
			}

			label := networkLabelKey(id)
			if deployment.Labels[label] != "true" || deployment.Spec.Template.Labels[label] != "true" {
				return fmt.Errorf(
					"existing d2k workload %q has inconsistent network metadata for %q; "+
						"refusing to install the default-deny policy until the workload is recreated",
					deployment.Name,
					network.Name,
				)
			}

			if err := a.ensureNetworkIsolationPolicy(ctx, network); err != nil {
				return fmt.Errorf("unable to reconcile policy for Docker network %q: %w", network.Name, err)
			}
		}

		for key := range deployment.Labels {
			if strings.HasPrefix(key, types.LabelNetworkPrefix) && !expectedLabels[key] {
				return fmt.Errorf("existing d2k workload %q has stale Deployment network label %q", deployment.Name, key)
			}
		}
		for key := range deployment.Spec.Template.Labels {
			if strings.HasPrefix(key, types.LabelNetworkPrefix) && !expectedLabels[key] {
				return fmt.Errorf("existing d2k workload %q has stale Pod-template network label %q", deployment.Name, key)
			}
		}

		// The Deployment/template annotation is authoritative. Reconcile live
		// Pod metadata before installing/confirming the baseline policy so a
		// stale Pod identity cannot survive a d2k restart.
		if err := a.patchExistingPodNetworks(ctx, deployment.Name, ids); err != nil {
			return fmt.Errorf("unable to reconcile live Pod network membership for %q: %w", deployment.Name, err)
		}
	}

	return nil
}

func (a *KubernetesDockerAdapter) ensureIsolationBaseline(ctx context.Context) error {
	if !a.networkIsolation {
		return nil
	}
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "d2k-workloads-default-deny",
			Namespace: a.namespace,
			Labels: map[string]string{
				types.LabelManagedBy: types.LabelManagedByValue,
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					types.LabelManagedBy: types.LabelManagedByValue,
				},
			},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
		},
	}
	return a.applyNetworkPolicy(ctx, np)
}

func (a *KubernetesDockerAdapter) persistNetwork(ctx context.Context, n *NetworkSummary) error {
	if !a.networkIsolation || n == nil {
		return nil
	}
	labelsJSON, err := json.Marshal(n.Labels)
	if err != nil {
		return fmt.Errorf("unable to encode Docker network labels: %w", err)
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      networkStateName(n.ID),
			Namespace: a.namespace,
			Labels: map[string]string{
				types.LabelNetworkState: "true",
				types.LabelManagedBy:    types.LabelManagedByValue,
			},
		},
		Data: map[string]string{
			"id":         n.ID,
			"name":       n.Name,
			"driver":     n.Driver,
			"scope":      n.Scope,
			"internal":   strconv.FormatBool(n.Internal),
			"attachable": strconv.FormatBool(n.Attachable),
			"labels":     string(labelsJSON),
		},
	}
	api := a.client.CoreV1().ConfigMaps(a.namespace)
	current, err := api.Get(ctx, cm.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = api.Create(ctx, cm, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	cm.ResourceVersion = current.ResourceVersion
	_, err = api.Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

func (a *KubernetesDockerAdapter) deletePersistedNetwork(ctx context.Context, n *NetworkSummary) error {
	if n == nil {
		return nil
	}
	err := a.client.CoreV1().ConfigMaps(a.namespace).Delete(ctx, networkStateName(n.ID), metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (a *KubernetesDockerAdapter) restorePersistedNetworks(ctx context.Context) error {
	if !a.networkIsolation {
		return nil
	}
	cms, err := a.client.CoreV1().ConfigMaps(a.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: types.LabelNetworkState + "=true",
	})
	if err != nil {
		return err
	}

	restored := map[string]*NetworkSummary{}
	for _, cm := range cms.Items {
		id := strings.TrimSpace(cm.Data["id"])
		name := strings.TrimSpace(cm.Data["name"])
		if id == "" || name == "" {
			return fmt.Errorf("persisted Docker network state %q is missing id or name", cm.Name)
		}

		labels := map[string]string{}
		if raw := strings.TrimSpace(cm.Data["labels"]); raw != "" {
			if err := json.Unmarshal([]byte(raw), &labels); err != nil {
				return fmt.Errorf("persisted Docker network state %q has invalid labels: %w", cm.Name, err)
			}
		}
		internal, err := strconv.ParseBool(cm.Data["internal"])
		if err != nil && cm.Data["internal"] != "" {
			return fmt.Errorf("persisted Docker network state %q has invalid internal flag: %w", cm.Name, err)
		}
		attachable, err := strconv.ParseBool(cm.Data["attachable"])
		if err != nil && cm.Data["attachable"] != "" {
			return fmt.Errorf("persisted Docker network state %q has invalid attachable flag: %w", cm.Name, err)
		}

		driver := cm.Data["driver"]
		if driver == "" {
			driver = "overlay"
		}
		scope := cm.Data["scope"]
		if scope == "" {
			scope = "swarm"
		}
		restored[name] = &NetworkSummary{
			ID:         id,
			Name:       name,
			Driver:     driver,
			Scope:      scope,
			Internal:   internal,
			Attachable: attachable,
			IPAM:       NetworkIPAM{Driver: "default", Config: []IPAMConfig{}},
			Labels:     labels,
		}
	}

	a.networksMu.Lock()
	a.networks = restored
	a.networksMu.Unlock()

	// Network definitions are persistent source-of-truth state. Reconcile their
	// policies on every restore so a deleted/drifted policy is repaired.
	for _, network := range restored {
		if err := a.ensureNetworkIsolationPolicy(ctx, network); err != nil {
			return fmt.Errorf("unable to reconcile policy for persisted Docker network %q: %w", network.Name, err)
		}
	}
	if a.isolationReady {
		if err := a.ensureIsolationBaseline(ctx); err != nil {
			return fmt.Errorf("unable to reconcile d2k workload default-deny policy: %w", err)
		}
	}
	return nil
}

func (a *KubernetesDockerAdapter) findNetwork(ctx context.Context, ref string) (*NetworkSummary, error) {
	if a.networkIsolation {
		if err := a.restorePersistedNetworks(ctx); err != nil {
			return nil, err
		}
	}
	if network, ok := a.lookupNetwork(ref); ok {
		return network, nil
	}
	return nil, fmt.Errorf("Docker network %q does not exist", ref)
}

func (a *KubernetesDockerAdapter) validateManualNetworkRefs(ctx context.Context, refs []string, defaultRef string) error {
	if len(refs) == 0 && defaultRef != "" {
		refs = []string{defaultRef}
	}
	for _, ref := range refs {
		network, err := a.findNetwork(ctx, ref)
		if err != nil {
			return err
		}
		if network.Scope == "swarm" && !network.Attachable {
			return fmt.Errorf("network %q is not manually attachable", network.Name)
		}
	}
	return nil
}

func (a *KubernetesDockerAdapter) networkMembership(ctx context.Context, refs []string, defaultRef string) (map[string]string, []string, error) {
	if len(refs) == 0 && defaultRef != "" {
		refs = []string{defaultRef}
	}

	labels := map[string]string{}
	ids := make([]string, 0, len(refs))
	seen := map[string]bool{}
	hasNone := false

	for _, ref := range refs {
		network, err := a.findNetwork(ctx, ref)
		if err != nil {
			return nil, nil, err
		}
		if a.rejectHostNetwork && network.Name == "host" {
			return nil, nil, fmt.Errorf("host network mode is rejected while Docker-network isolation is enabled")
		}
		if network.Name == "none" {
			hasNone = true
		}
		if seen[network.ID] {
			continue
		}
		seen[network.ID] = true
		if err := a.ensureNetworkIsolationPolicy(ctx, network); err != nil {
			return nil, nil, err
		}
		labels[networkLabelKey(network.ID)] = "true"
		ids = append(ids, network.ID)
	}

	if hasNone && len(ids) > 1 {
		return nil, nil, fmt.Errorf("Docker network none cannot be combined with another network")
	}

	sort.Strings(ids)
	return labels, ids, nil
}

func stripNetworkLabels(labels map[string]string) {
	for key := range labels {
		if strings.HasPrefix(key, types.LabelNetworkPrefix) {
			delete(labels, key)
		}
	}
}

func (a *KubernetesDockerAdapter) applyDeploymentNetworks(ctx context.Context, dep *appsv1.Deployment, refs []string, defaultRef string) error {
	labels, ids, err := a.networkMembership(ctx, refs, defaultRef)
	if err != nil {
		return err
	}
	if dep.Labels == nil {
		dep.Labels = map[string]string{}
	}
	if dep.Annotations == nil {
		dep.Annotations = map[string]string{}
	}
	if dep.Spec.Template.Labels == nil {
		dep.Spec.Template.Labels = map[string]string{}
	}
	if dep.Spec.Template.Annotations == nil {
		dep.Spec.Template.Annotations = map[string]string{}
	}

	stripNetworkLabels(dep.Labels)
	stripNetworkLabels(dep.Spec.Template.Labels)
	for key, value := range labels {
		dep.Labels[key] = value
		dep.Spec.Template.Labels[key] = value
	}

	raw := encodeNetworkIDs(ids)
	dep.Annotations[types.AnnotationNetworkIDs] = raw
	dep.Spec.Template.Annotations[types.AnnotationNetworkIDs] = raw
	return nil
}

func (a *KubernetesDockerAdapter) deploymentUsesNetwork(dep appsv1.Deployment, networkID string) (bool, error) {
	raw := dep.Annotations[types.AnnotationNetworkIDs]
	if strings.TrimSpace(raw) != "" {
		ids, err := parseNetworkIDs(raw)
		if err != nil {
			return false, err
		}
		if containsString(ids, networkID) {
			return true, nil
		}
	}
	return dep.Spec.Template.Labels[networkLabelKey(networkID)] == "true", nil
}

func (a *KubernetesDockerAdapter) attachedContainersForNetwork(ctx context.Context, networkID string) (map[string]any, error) {
	deployments, err := a.client.AppsV1().Deployments(a.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: types.LabelManagedBy + "=" + types.LabelManagedByValue,
	})
	if err != nil {
		return nil, err
	}

	containers := map[string]any{}
	for _, deployment := range deployments.Items {
		used, err := a.deploymentUsesNetwork(deployment, networkID)
		if err != nil {
			return nil, fmt.Errorf("workload %q has invalid network metadata: %w", deployment.Name, err)
		}
		if !used {
			continue
		}
		id := string(deployment.UID)
		if id == "" {
			id = deployment.Name
		}
		containers[id] = map[string]any{
			"Name":        deployment.Name,
			"EndpointID":  shortHash(networkID + ":" + id),
			"MacAddress":  "",
			"IPv4Address": "",
			"IPv6Address": "",
		}
	}
	return containers, nil
}

func (a *KubernetesDockerAdapter) networkInUse(ctx context.Context, networkID string) ([]string, error) {
	deployments, err := a.client.AppsV1().Deployments(a.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: types.LabelManagedBy + "=" + types.LabelManagedByValue,
	})
	if err != nil {
		return nil, err
	}
	var users []string
	for _, deployment := range deployments.Items {
		used, err := a.deploymentUsesNetwork(deployment, networkID)
		if err != nil {
			return nil, fmt.Errorf("workload %q has invalid network metadata: %w", deployment.Name, err)
		}
		if used {
			users = append(users, deployment.Name)
		}
	}
	sort.Strings(users)
	return users, nil
}

func (a *KubernetesDockerAdapter) sameNetworkPeer(id string) networkingv1.NetworkPolicyPeer {
	return networkingv1.NetworkPolicyPeer{
		PodSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{
				types.LabelManagedBy: types.LabelManagedByValue,
				networkLabelKey(id):  "true",
			},
		},
	}
}

func (a *KubernetesDockerAdapter) dnsEgressRule() networkingv1.NetworkPolicyEgressRule {
	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP
	port := intstr.FromInt(53)
	return networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"},
			},
			PodSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"k8s-app": "kube-dns"},
			},
		}},
		Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: &udp, Port: &port},
			{Protocol: &tcp, Port: &port},
		},
	}
}

func familyCIDRs(cidrs []string, wantV6 bool) []string {
	var out []string
	for _, raw := range cidrs {
		ip, _, err := net.ParseCIDR(raw)
		if err != nil {
			continue
		}
		if (ip.To4() == nil) == wantV6 {
			out = append(out, raw)
		}
	}
	return out
}

func (a *KubernetesDockerAdapter) worldPeers() []networkingv1.NetworkPolicyPeer {
	all := append(append([]string{}, a.podCIDRs...), a.serviceCIDRs...)
	peers := make([]networkingv1.NetworkPolicyPeer, 0, 2)

	v4Except := familyCIDRs(all, false)
	if len(v4Except) > 0 {
		peers = append(peers, networkingv1.NetworkPolicyPeer{
			IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: v4Except},
		})
	}
	v6Except := familyCIDRs(all, true)
	if len(v6Except) > 0 {
		peers = append(peers, networkingv1.NetworkPolicyPeer{
			IPBlock: &networkingv1.IPBlock{CIDR: "::/0", Except: v6Except},
		})
	}
	return peers
}

func (a *KubernetesDockerAdapter) ensureNetworkIsolationPolicy(ctx context.Context, n *NetworkSummary) error {
	if !a.networkIsolation || n == nil {
		return nil
	}

	key := networkLabelKey(n.ID)
	spec := networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{
			MatchLabels: map[string]string{
				types.LabelManagedBy: types.LabelManagedByValue,
				key:                  "true",
			},
		},
		PolicyTypes: []networkingv1.PolicyType{
			networkingv1.PolicyTypeIngress,
			networkingv1.PolicyTypeEgress,
		},
	}

	if n.Name != "none" {
		spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{
			From: []networkingv1.NetworkPolicyPeer{a.sameNetworkPeer(n.ID)},
		}}
		spec.Egress = []networkingv1.NetworkPolicyEgressRule{
			{To: []networkingv1.NetworkPolicyPeer{a.sameNetworkPeer(n.ID)}},
			a.dnsEgressRule(),
		}
		if !n.Internal {
			world := a.worldPeers()
			if len(world) > 0 {
				spec.Egress = append(spec.Egress, networkingv1.NetworkPolicyEgressRule{To: world})
			}
		}
	}

	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      networkPolicyName(n.ID),
			Namespace: a.namespace,
			Labels: map[string]string{
				types.LabelManagedBy: types.LabelManagedByValue,
			},
		},
		Spec: spec,
	}
	return a.applyNetworkPolicy(ctx, np)
}

func (a *KubernetesDockerAdapter) deleteNetworkIsolationPolicy(ctx context.Context, id string) error {
	err := a.client.NetworkingV1().NetworkPolicies(a.namespace).Delete(ctx, networkPolicyName(id), metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (a *KubernetesDockerAdapter) applyNetworkPolicy(ctx context.Context, desired *networkingv1.NetworkPolicy) error {
	api := a.client.NetworkingV1().NetworkPolicies(a.namespace)
	current, err := api.Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = api.Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	desired.ResourceVersion = current.ResourceVersion
	_, err = api.Update(ctx, desired, metav1.UpdateOptions{})
	return err
}

func mappingsToNetworkPolicyPorts(mappings []portmapper.PortMapping) []networkingv1.NetworkPolicyPort {
	var ports []networkingv1.NetworkPolicyPort
	seen := map[string]bool{}
	for _, mapping := range mappings {
		proto := corev1.ProtocolTCP
		if strings.EqualFold(string(mapping.Protocol), "UDP") {
			proto = corev1.ProtocolUDP
		}
		key := fmt.Sprintf("%s/%d", proto, mapping.ContainerPort)
		if seen[key] {
			continue
		}
		seen[key] = true
		port := intstr.FromInt(mapping.ContainerPort)
		ports = append(ports, networkingv1.NetworkPolicyPort{Protocol: &proto, Port: &port})
	}
	return ports
}

func (a *KubernetesDockerAdapter) ensurePublishedIngressPolicy(ctx context.Context, name string, ports []networkingv1.NetworkPolicyPort) error {
	if !a.networkIsolation {
		return nil
	}
	if len(ports) == 0 {
		return a.deletePublishedIngressPolicy(ctx, name)
	}
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      publishedPolicyName(name),
			Namespace: a.namespace,
			Labels: map[string]string{
				types.LabelManagedBy: types.LabelManagedByValue,
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: managedSelector(name),
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From:  a.worldPeers(),
				Ports: ports,
			}},
		},
	}
	return a.applyNetworkPolicy(ctx, np)
}

func (a *KubernetesDockerAdapter) deletePublishedIngressPolicy(ctx context.Context, name string) error {
	err := a.client.NetworkingV1().NetworkPolicies(a.namespace).Delete(ctx, publishedPolicyName(name), metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func removedNetworkMembership(before, after []string) bool {
	desired := map[string]bool{}
	for _, id := range after {
		desired[id] = true
	}
	for _, id := range before {
		if !desired[id] {
			return true
		}
	}
	return false
}

func (a *KubernetesDockerAdapter) patchExistingPodNetworks(ctx context.Context, deploymentName string, ids []string) error {
	pods, err := a.client.CoreV1().Pods(a.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s,%s=%s", deploymentName, types.LabelManagedBy, types.LabelManagedByValue),
	})
	if err != nil {
		return fmt.Errorf("unable to list running pods for %q: %w", deploymentName, err)
	}

	for _, pod := range pods.Items {
		labelPatch := map[string]any{}
		for key := range pod.Labels {
			if strings.HasPrefix(key, types.LabelNetworkPrefix) {
				labelPatch[key] = nil
			}
		}
		for _, id := range ids {
			labelPatch[networkLabelKey(id)] = "true"
		}
		patch := map[string]any{
			"metadata": map[string]any{
				"labels": labelPatch,
				"annotations": map[string]any{
					types.AnnotationNetworkIDs: encodeNetworkIDs(ids),
				},
			},
		}
		raw, err := json.Marshal(patch)
		if err != nil {
			return err
		}
		if _, err := a.client.CoreV1().Pods(a.namespace).Patch(ctx, pod.Name, k8stypes.MergePatchType, raw, metav1.PatchOptions{}); err != nil {
			return fmt.Errorf("unable to update live network membership for pod %q: %w", pod.Name, err)
		}
	}
	return nil
}

func (a *KubernetesDockerAdapter) ConnectNetwork(ctx context.Context, networkRef, containerName string) error {
	if !a.networkIsolation {
		return nil
	}

	if err := a.validateManualNetworkRefs(ctx, []string{networkRef}, ""); err != nil {
		return err
	}
	network, err := a.findNetwork(ctx, networkRef)
	if err != nil {
		return err
	}

	resolved, err := a.resolveDeploymentName(ctx, containerName)
	if err != nil {
		return err
	}
	deployment, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, resolved, metav1.GetOptions{})
	if err != nil {
		return err
	}

	ids, err := parseNetworkIDs(deployment.Annotations[types.AnnotationNetworkIDs])
	if err != nil {
		return fmt.Errorf("container %q has invalid network metadata: %w", resolved, err)
	}
	if containsString(ids, network.ID) {
		return nil
	}
	ids = append(ids, network.ID)
	if err := a.applyDeploymentNetworks(ctx, deployment, ids, ""); err != nil {
		return err
	}
	if _, err = a.client.AppsV1().Deployments(a.namespace).Update(ctx, deployment, metav1.UpdateOptions{}); err != nil {
		return err
	}
	return a.patchExistingPodNetworks(ctx, resolved, ids)
}

func (a *KubernetesDockerAdapter) DisconnectNetwork(ctx context.Context, networkRef, containerName string) error {
	if !a.networkIsolation {
		return nil
	}

	resolved, err := a.resolveDeploymentName(ctx, containerName)
	if err != nil {
		return err
	}
	deployment, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, resolved, metav1.GetOptions{})
	if err != nil {
		return err
	}

	network, err := a.findNetwork(ctx, networkRef)
	if err != nil {
		return err
	}
	current, err := parseNetworkIDs(deployment.Annotations[types.AnnotationNetworkIDs])
	if err != nil {
		return fmt.Errorf("container %q has invalid network metadata: %w", resolved, err)
	}
	if !containsString(current, network.ID) {
		return fmt.Errorf("container %q is not connected to network %q", resolved, network.Name)
	}

	ids := make([]string, 0, len(current)-1)
	for _, id := range current {
		if id != network.ID {
			ids = append(ids, id)
		}
	}
	// A disconnect is security-reducing membership. Remove the live Pod
	// identity first so policy enforcement closes before the Deployment
	// controller begins its rollout.
	if err := a.patchExistingPodNetworks(ctx, resolved, ids); err != nil {
		return err
	}
	if err := a.applyDeploymentNetworks(ctx, deployment, ids, ""); err != nil {
		return err
	}
	_, err = a.client.AppsV1().Deployments(a.namespace).Update(ctx, deployment, metav1.UpdateOptions{})
	return err
}
