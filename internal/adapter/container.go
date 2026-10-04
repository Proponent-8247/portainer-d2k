package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"strconv"
	"time"

	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/go-connections/nat"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/portainer/d2k/internal/types"
	"github.com/portainer/d2k/pkg/portmapper"
)

// RunOptions mirrors the subset of docker run flags that d2k supports.
type RunOptions struct {
	Name         string
	Image        string
	Cmd          []string
	Env          []string
	Labels       map[string]string
	PortBindings []string
	PublishAll   bool
	ExposedPorts map[string]struct{}
	Volumes      []string
	GPUCount     int
	Healthcheck  *container.HealthConfig
}

// ContainerSummary is a Docker-compatible summary row, as returned by docker ps.
type ContainerSummary struct {
	ID        string
	Names     []string
	Image     string
	Status    string
	State     string
	Created   int64
	Ports     []dockertypes.Port
	Labels    map[string]string
	IPAddress string
}

type workloadRuntimeState struct {
	Running   bool
	Ready     bool
	StartedAt time.Time
	Health    *dockertypes.Health
}

// CreateContainer implements docker run: creates a Deployment and, where
// required, a Service in the target namespace.
func (a *KubernetesDockerAdapter) CreateContainer(ctx context.Context, opts RunOptions) (string, []string, error) {
	if opts.Name == "" {
		return "", nil, fmt.Errorf("container name is required")
	}

	// Sanitise the name for Kubernetes: lowercase, underscores to hyphens, max 63 chars.
	opts.Name = strings.ToLower(strings.ReplaceAll(opts.Name, "_", "-"))
	if len(opts.Name) > 63 {
		opts.Name = opts.Name[:63]
	}
	opts.Name = strings.TrimRight(opts.Name, "-")

	// Resolve port mappings and determine Service type.
	pmReq := portmapper.Request{
		PublishAll:   opts.PublishAll,
		ExposedPorts: opts.ExposedPorts,
		PortBindings: opts.PortBindings,
	}

	kind, mappings, warnings, err := portmapper.Resolve(pmReq)
	if err != nil {
		return "", nil, fmt.Errorf("unable to resolve port mappings: %w", err)
	}

	// Build and create the Deployment.
	deployment, healthWarnings, err := a.buildDeployment(ctx, opts, kind, mappings)
	if err != nil {
		return "", nil, fmt.Errorf("unable to build deployment: %w", err)
	}
	warnings = append(warnings, healthWarnings...)

	created, err := a.client.AppsV1().Deployments(a.namespace).Create(ctx, deployment, metav1.CreateOptions{})
	if err != nil {
		if errors.IsAlreadyExists(err) {
			return "", nil, fmt.Errorf("container name %q is already in use", opts.Name)
		}
		if errors.IsForbidden(err) {
			return "", nil, fmt.Errorf("deployment rejected by Kubernetes: %w", err)
		}
		return "", nil, fmt.Errorf("unable to create deployment %q: %w", opts.Name, err)
	}

	// Always create a ClusterIP Service so the container name resolves via
	// Kubernetes DNS from other pods in the namespace. This makes Docker
	// short-name DNS work (e.g. "redis", "postgres") without needing FQDNs.
	// Build ClusterIP ports from container port mappings.
	var clusterPorts []corev1.ServicePort
	for i, m := range mappings {
		clusterPorts = append(clusterPorts, corev1.ServicePort{
			Name:       fmt.Sprintf("port-%d", i),
			Protocol:   corev1.Protocol(m.Protocol),
			Port:       int32(m.ContainerPort),
			TargetPort: intstr.FromInt(m.ContainerPort),
		})
	}
	// Use headless (clusterIP: None) when there are no ports — Kubernetes rejects
	// ClusterIP Services with an empty ports list but headless Services are allowed
	// without ports and still register the DNS name for short-name resolution.
	clusterSpec := corev1.ServiceSpec{
		Selector:                 map[string]string{"app": opts.Name},
		Ports:                    clusterPorts,
		PublishNotReadyAddresses: healthcheckEnabled(opts.Healthcheck),
	}
	if len(clusterPorts) == 0 {
		clusterSpec.ClusterIP = "None"
	} else {
		clusterSpec.Type = corev1.ServiceTypeClusterIP
	}
	clusterSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      opts.Name,
			Namespace: a.namespace,
			Labels:    managedLabels(opts.Name),
			Annotations: map[string]string{
				"d2k.portainer.io/dns-service": "true",
			},
		},
		Spec: clusterSpec,
	}
	if _, svcErr := a.client.CoreV1().Services(a.namespace).Create(ctx, clusterSvc, metav1.CreateOptions{}); svcErr != nil {
		if !errors.IsAlreadyExists(svcErr) {
			_ = a.client.AppsV1().Deployments(a.namespace).Delete(ctx, opts.Name, metav1.DeleteOptions{})
			return "", nil, fmt.Errorf("unable to create ClusterIP service for %q: %w", opts.Name, svcErr)
		}
	}

	// Create a LoadBalancer or NodePort Service for externally published ports.
	if kind != portmapper.NoService && len(mappings) > 0 {
		svc, svcErr := a.buildService(opts.Name, kind, mappings)
		if svcErr != nil {
			return "", nil, fmt.Errorf("unable to build service: %w", svcErr)
		}
		svc.Spec.PublishNotReadyAddresses = healthcheckEnabled(opts.Healthcheck)
		if _, svcErr = a.client.CoreV1().Services(a.namespace).Create(ctx, svc, metav1.CreateOptions{}); svcErr != nil {
			// Roll back the Deployment and ClusterIP service so we don't leave orphans.
			_ = a.client.AppsV1().Deployments(a.namespace).Delete(ctx, opts.Name, metav1.DeleteOptions{})
			_ = a.client.CoreV1().Services(a.namespace).Delete(ctx, opts.Name, metav1.DeleteOptions{})
			return "", nil, fmt.Errorf("unable to create service for %q: %w", opts.Name, svcErr)
		}
	}

	return string(created.UID), warnings, nil
}

// ListContainers implements docker ps.
func (a *KubernetesDockerAdapter) ListContainers(ctx context.Context, all bool) ([]ContainerSummary, error) {
	deployments, err := a.client.AppsV1().Deployments(a.namespace).List(ctx, metav1ListOptions())
	if err != nil {
		return nil, fmt.Errorf("unable to list deployments: %w", err)
	}

	runtimeStates := map[string]workloadRuntimeState{}
	needRuntimeState := false
	for _, d := range deployments.Items {
		if deploymentHasTranslatedHealthcheck(d) {
			needRuntimeState = true
			break
		}
	}
	if needRuntimeState {
		runtimeStates, err = a.workloadRuntimeStates(ctx)
		if err != nil {
			return nil, err
		}
	}

	var summaries []ContainerSummary

	for _, d := range deployments.Items {
		runtimeState := workloadRuntimeState{
			Running: d.Status.ReadyReplicas > 0,
			Ready:   d.Status.ReadyReplicas > 0,
		}
		if deploymentHasTranslatedHealthcheck(d) {
			runtimeState = runtimeStates[deploymentRuntimeStateKey(d)]
			if !runtimeState.Running && d.Status.ReadyReplicas > 0 {
				runtimeState.Running = true
				runtimeState.Ready = true
			}
		}
		if !all && !runtimeState.Running {
			continue
		}
		summary := deploymentToSummary(d, runtimeState)

		// Look up the Service to get the LoadBalancer IP.
		// Service name may have a "svc-" prefix if the deployment name started with a digit.
		svcName := serviceName(d.Name)
		svc, svcErr := a.client.CoreV1().Services(a.namespace).Get(ctx, svcName, metav1GetOptions())
		if svcErr == nil && len(svc.Status.LoadBalancer.Ingress) > 0 {
			summary.IPAddress = svc.Status.LoadBalancer.Ingress[0].IP
			if summary.IPAddress == "" {
				summary.IPAddress = svc.Status.LoadBalancer.Ingress[0].Hostname
			}
		}

		summaries = append(summaries, summary)
	}

	return summaries, nil
}

func (a *KubernetesDockerAdapter) workloadRuntimeStates(ctx context.Context) (map[string]workloadRuntimeState, error) {
	pods, err := a.client.CoreV1().Pods(a.namespace).List(ctx, metav1ListOptions())
	if err != nil {
		return nil, fmt.Errorf("unable to list pods for container state: %w", err)
	}

	states := map[string]workloadRuntimeState{}
	for _, pod := range pods.Items {
		if pod.Labels[types.LabelManagedBy] != types.LabelManagedByValue {
			continue
		}

		name := pod.Labels[types.LabelWorkloadName]
		if name == "" {
			name = pod.Labels[types.LabelSwarmService]
		}
		if name == "" {
			name = pod.Labels["app"]
		}
		if name == "" || pod.Status.Phase != corev1.PodRunning {
			continue
		}

		for _, containerStatus := range pod.Status.ContainerStatuses {
			if containerStatus.Name != name || containerStatus.State.Running == nil {
				continue
			}
			state := states[name]
			state.Running = true
			state.Ready = state.Ready || containerStatus.Ready
			if health := a.healthStateForPod(pod); health != nil {
				state.Health = health
			}
			started := containerStatus.State.Running.StartedAt.Time
			if state.StartedAt.IsZero() || started.Before(state.StartedAt) {
				state.StartedAt = started
			}
			states[name] = state
			break
		}
	}
	return states, nil
}

func (a *KubernetesDockerAdapter) workloadRuntimeState(ctx context.Context, name string) (workloadRuntimeState, error) {
	states, err := a.workloadRuntimeStates(ctx)
	if err != nil {
		return workloadRuntimeState{}, err
	}
	return states[name], nil
}

// StopContainer implements docker stop: scales the Deployment to 0 replicas.
// Returns an error if the Deployment is managed by the Swarm layer.
func (a *KubernetesDockerAdapter) StopContainer(ctx context.Context, name string) error {
	resolved, err := a.resolveDeploymentName(ctx, name)
	if err != nil {
		return err
	}
	d, getErr := a.client.AppsV1().Deployments(a.namespace).Get(ctx, resolved, metav1GetOptions())
	if getErr == nil && d.Labels[types.LabelSwarmManagedBy] == types.LabelSwarmManagedByValue {
		return fmt.Errorf("cannot stop a container that is managed by swarm: use docker service scale instead")
	}
	return a.scaleDeployment(ctx, name, 0)
}

// StartContainer implements docker start: scales the Deployment back to 1 replica.
func (a *KubernetesDockerAdapter) StartContainer(ctx context.Context, name string) error {
	return a.scaleDeployment(ctx, name, 1)
}

// RemoveContainer implements docker rm: deletes the Deployment and its associated Service (if any).
// Returns an error if the Deployment is managed by the Swarm layer — use docker service rm instead.
func (a *KubernetesDockerAdapter) RemoveContainer(ctx context.Context, name string) error {
	resolved, err := a.resolveDeploymentName(ctx, name)
	if err != nil {
		return err
	}

	// Refuse to remove containers that back a swarm service — the same guard
	// Docker Swarm applies: "cannot remove a running container that is managed by swarm".
	d, getErr := a.client.AppsV1().Deployments(a.namespace).Get(ctx, resolved, metav1GetOptions())
	if getErr == nil && d.Labels[types.LabelSwarmManagedBy] == types.LabelSwarmManagedByValue {
		return fmt.Errorf("cannot remove a running container that is managed by swarm: use docker service rm instead")
	}

	if err := a.client.AppsV1().Deployments(a.namespace).Delete(ctx, resolved, metav1.DeleteOptions{}); err != nil {
		return fmt.Errorf("unable to delete deployment %q: %w", resolved, err)
	}

	// Best-effort Service deletion — remove ClusterIP DNS service and LB/NodePort service.
	_ = a.client.CoreV1().Services(a.namespace).Delete(ctx, resolved, metav1.DeleteOptions{})
	_ = a.client.CoreV1().Services(a.namespace).Delete(ctx, serviceName(resolved), metav1.DeleteOptions{})

	return nil
}
// Rename Container
func (a *KubernetesDockerAdapter) RenameContainer(ctx context.Context, nameOrID, newName string) error {
    resolved, err := a.resolveDeploymentName(ctx, nameOrID)
    if err != nil {
        return err
    }

    d, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, resolved, metav1GetOptions())
    if err != nil {
        return fmt.Errorf("unable to get deployment %q: %w", resolved, err)
    }

    // Sanitise the new name.
    newName = strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(newName, "/"), "_", "-"))
    if len(newName) > 63 {
        newName = newName[:63]
    }
    newName = strings.TrimRight(newName, "-")

    // Create a new Deployment with the new name, copying the spec.
    newDeployment := d.DeepCopy()
    newDeployment.Name = newName
    newDeployment.ResourceVersion = ""
    newDeployment.UID = ""

    if _, err := a.client.AppsV1().Deployments(a.namespace).Create(ctx, newDeployment, metav1.CreateOptions{}); err != nil {
        return fmt.Errorf("unable to create renamed deployment %q: %w", newName, err)
    }

    // Delete the old Deployment.
    if err := a.client.AppsV1().Deployments(a.namespace).Delete(ctx, resolved, metav1.DeleteOptions{}); err != nil {
        return fmt.Errorf("unable to delete old deployment %q: %w", resolved, err)
    }

    return nil
}

// InspectContainer implements docker inspect for a single container.
func (a *KubernetesDockerAdapter) InspectContainer(ctx context.Context, name string) (*dockertypes.ContainerJSON, error) {
	resolved, err := a.resolveDeploymentName(ctx, name)
	if err != nil {
		return nil, err
	}

	d, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, resolved, metav1GetOptions())
	if err != nil {
		return nil, fmt.Errorf("unable to get deployment %q: %w", resolved, err)
	}

	lbIP := ""
	svc, svcErr := a.client.CoreV1().Services(a.namespace).Get(ctx, serviceName(resolved), metav1GetOptions())
	if svcErr == nil && len(svc.Status.LoadBalancer.Ingress) > 0 {
		lbIP = svc.Status.LoadBalancer.Ingress[0].IP
		if lbIP == "" {
			lbIP = svc.Status.LoadBalancer.Ingress[0].Hostname
		}
	}

	runtimeState := workloadRuntimeState{
		Running: d.Status.ReadyReplicas > 0,
		Ready:   d.Status.ReadyReplicas > 0,
	}
	if deploymentHasTranslatedHealthcheck(*d) {
		runtimeState, err = a.workloadRuntimeState(ctx, deploymentRuntimeStateKey(*d))
		if err != nil {
			return nil, err
		}
		if !runtimeState.Running && d.Status.ReadyReplicas > 0 {
			runtimeState = workloadRuntimeState{Running: true, Ready: true}
		}
	}

	result := deploymentToContainerJSON(*d, lbIP, runtimeState)
	return &result, nil
}

// --- builders ---

func (a *KubernetesDockerAdapter) buildDeployment(ctx context.Context, opts RunOptions, kind portmapper.MappingKind, mappings []portmapper.PortMapping) (*appsv1.Deployment, []string, error) {
	labels := managedLabels(opts.Name)
	for k, v := range opts.Labels {
		if clean, ok := sanitiseLabelValue(v); ok {
			labels[k] = clean
		}
	}

	portAnnotation, err := encodePortMappings(opts.PortBindings, opts.PublishAll)
	if err != nil {
		return nil, nil, err
	}
	annotations := map[string]string{
		types.AnnotationPortMappings: portAnnotation,
		types.AnnotationImageRef:     opts.Image,
	}

	healthWarnings, err := validateHealthcheckForMonitor(opts.Healthcheck)
	if err != nil {
		return nil, nil, err
	}
	if err := annotateHealthcheck(annotations, opts.Healthcheck); err != nil {
		return nil, nil, err
	}

	serviceTypeLabel := types.ServiceTypeNone
	switch kind {
	case portmapper.LoadBalancerService:
		serviceTypeLabel = types.ServiceTypeLB
	case portmapper.NodePortService:
		serviceTypeLabel = types.ServiceTypeNodePort
	}
	labels[types.LabelServiceType] = serviceTypeLabel

	var containerPorts []corev1.ContainerPort
	for _, m := range mappings {
		containerPorts = append(containerPorts, corev1.ContainerPort{
			ContainerPort: int32(m.ContainerPort),
			Protocol:      corev1.Protocol(m.Protocol),
		})
	}

	var envVars []corev1.EnvVar
	for _, e := range opts.Env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) != 2 {
			continue
		}
		envVars = append(envVars, corev1.EnvVar{Name: parts[0], Value: parts[1]})
	}

	var resourceLimits corev1.ResourceList
	if opts.GPUCount > 0 && a.gpuResourceName != "" {
		resourceLimits = corev1.ResourceList{
			corev1.ResourceName(a.gpuResourceName): resource.MustParse(fmt.Sprintf("%d", opts.GPUCount)),
		}
	}

	// Inject requests.cpu / requests.memory when the namespace has a ResourceQuota
	// that requires them. Docker has no concept of ResourceQuotas so the caller
	// cannot know they are needed. injectQuotaDefaults is a no-op when no quota exists.
	resourceReqs, err := a.injectQuotaDefaults(ctx, corev1.ResourceRequirements{
		Limits: resourceLimits,
	})
	if err != nil {
		return nil, nil, err
	}

	replicas := int32(1)

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        opts.Name,
			Namespace:   a.namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": opts.Name},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":                   opts.Name,
						types.LabelManagedBy:    types.LabelManagedByValue,
						types.LabelWorkloadName: opts.Name,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:           opts.Name,
							Image:          opts.Image,
							Command:        opts.Cmd,
							Env:            envVars,
							Ports:          containerPorts,
							Resources:      resourceReqs,
						},
					},
				},
			},
		},
	}, healthWarnings, nil
}

func (a *KubernetesDockerAdapter) buildService(name string, kind portmapper.MappingKind, mappings []portmapper.PortMapping) (*corev1.Service, error) {
	var svcType corev1.ServiceType
	switch kind {
	case portmapper.LoadBalancerService:
		svcType = corev1.ServiceTypeLoadBalancer
	case portmapper.NodePortService:
		svcType = corev1.ServiceTypeNodePort
	default:
		return nil, fmt.Errorf("buildService called with kind %d, which requires no service", kind)
	}

	var ports []corev1.ServicePort
	for i, m := range mappings {
		sp := corev1.ServicePort{
			Name:       fmt.Sprintf("port-%d", i),
			Port:       int32(m.HostPort),
			TargetPort: intstr.FromInt(m.ContainerPort),
			Protocol:   corev1.Protocol(m.Protocol),
		}
		if m.HostPort == 0 {
			sp.Port = int32(m.ContainerPort)
		}
		ports = append(ports, sp)
	}

	svcName := serviceName(name)

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svcName,
			Namespace: a.namespace,
			Labels:    managedLabels(name),
		},
		Spec: corev1.ServiceSpec{
			Type:     svcType,
			Selector: map[string]string{"app": name},
			Ports:    ports,
		},
	}, nil
}

// serviceName returns the Kubernetes Service name for a given deployment name.
// Service names must conform to DNS-1035: start with a letter. If the deployment
// name starts with a digit, we prefix with "svc-".
func serviceName(name string) string {
	if len(name) > 0 && name[0] >= '0' && name[0] <= '9' {
		name = "svc-" + name
	}
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.TrimRight(name, "-")
}

// --- name/ID resolution ---

func (a *KubernetesDockerAdapter) resolveDeploymentName(ctx context.Context, nameOrID string) (string, error) {
	_, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, nameOrID, metav1GetOptions())
	if err == nil {
		return nameOrID, nil
	}
	if !errors.IsNotFound(err) {
		return "", fmt.Errorf("unable to look up container %q: %w", nameOrID, err)
	}
	list, err := a.client.AppsV1().Deployments(a.namespace).List(ctx, metav1ListOptions())
	if err != nil {
		return "", fmt.Errorf("unable to list deployments: %w", err)
	}
	for _, d := range list.Items {
		uid := string(d.UID)
		if uid == nameOrID {
			return d.Name, nil
		}
		if strings.HasSuffix(d.Name, "-"+nameOrID) {
			return d.Name, nil
		}
		// Match the truncated UID format returned by docker ps (e.g. "2665dc61-cb7").
		// docker ps shows the first 8 chars of the UID, a hyphen, then chars 9-11.
		// Reconstruct and compare against the full UID with hyphens stripped to be safe,
		// or do a simple prefix match on the normalised form.
		normID := strings.ReplaceAll(nameOrID, "-", "")
		normUID := strings.ReplaceAll(uid, "-", "")
		if strings.HasPrefix(normUID, normID) && len(normID) >= 8 {
			return d.Name, nil
		}
	}
	return "", fmt.Errorf("container %q not found", nameOrID)
}

// --- scale helper ---

func (a *KubernetesDockerAdapter) scaleDeployment(ctx context.Context, name string, replicas int32) error {
	resolved, err := a.resolveDeploymentName(ctx, name)
	if err != nil {
		return err
	}

	for attempt := 0; attempt < 5; attempt++ {
		d, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, resolved, metav1GetOptions())
		if err != nil {
			return fmt.Errorf("unable to get deployment %q: %w", resolved, err)
		}

		d.Spec.Replicas = &replicas
		_, err = a.client.AppsV1().Deployments(a.namespace).Update(ctx, d, metav1.UpdateOptions{})
		if err == nil {
			return nil
		}
		if !errors.IsConflict(err) {
			return fmt.Errorf("unable to scale deployment %q to %d: %w", name, replicas, err)
		}
	}

	return fmt.Errorf("unable to scale deployment %q to %d: too many conflicts", name, replicas)
}

func deploymentRuntimeStateKey(d appsv1.Deployment) string {
	for _, key := range []string{
		types.LabelWorkloadName,
		types.LabelSwarmService,
		"app",
	} {
		if value := d.Spec.Template.Labels[key]; value != "" {
			return value
		}
	}
	return d.Name
}

func deploymentHasTranslatedHealthcheck(d appsv1.Deployment) bool {
	hc, err := decodeHealthcheckAnnotation(d.Annotations[types.AnnotationHealthcheck])
	return err == nil && healthcheckEnabled(hc)
}

func dockerHealthStatus(d appsv1.Deployment, runtimeState workloadRuntimeState) (string, int) {
	hc, err := decodeHealthcheckAnnotation(d.Annotations[types.AnnotationHealthcheck])
	if err != nil || !healthcheckEnabled(hc) || !runtimeState.Running {
		return "", 0
	}
	if runtimeState.Health == nil {
		return dockertypes.Starting, 0
	}
	return string(runtimeState.Health.Status), runtimeState.Health.FailingStreak
}

// --- converters ---

func deploymentToSummary(d appsv1.Deployment, runtimeState workloadRuntimeState) ContainerSummary {
	state := "exited"
	status := "Exited (0)"
	if d.Spec.Replicas != nil && *d.Spec.Replicas > 0 {
		if runtimeState.Running {
			state = "running"
			uptime := time.Since(d.CreationTimestamp.Time)
			status = fmt.Sprintf("Up %s", humanizeDuration(uptime))
			if healthStatus, _ := dockerHealthStatus(d, runtimeState); healthStatus != "" {
				switch healthStatus {
				case dockertypes.Healthy:
					status += " (healthy)"
				case dockertypes.Unhealthy:
					status += " (unhealthy)"
				default:
					status += " (health: starting)"
				}
			}
		} else {
			state = "starting"
			status = "Starting"
		}
	}

	var ports []dockertypes.Port
	rawPorts := d.Annotations[types.AnnotationPortMappings]
	if rawPorts != "" {
		var bindings []string
		if err := json.Unmarshal([]byte(rawPorts), &bindings); err == nil {
			for _, raw := range bindings {
				parts := strings.SplitN(raw, ":", 2)
				if len(parts) == 2 {
					containerPort, _ := strconv.ParseUint(parts[1], 10, 16)
					hostPort, _ := strconv.ParseUint(parts[0], 10, 16)
					ports = append(ports, dockertypes.Port{
						IP:          "0.0.0.0",
						PrivatePort: uint16(containerPort),
						PublicPort:  uint16(hostPort),
						Type:        "tcp",
					})
				}
			}
		}
	}

	return ContainerSummary{
		ID:      string(d.UID),
		Names:   []string{"/" + d.Name},
		Image:   d.Annotations[types.AnnotationImageRef],
		Status:  status,
		State:   state,
		Created: d.CreationTimestamp.Unix(),
		Labels:  d.Labels,
		Ports:   ports,
	}
}

func deploymentToContainerJSON(d appsv1.Deployment, lbIP string, runtimeState workloadRuntimeState) dockertypes.ContainerJSON {
	running := runtimeState.Running

	state := &dockertypes.ContainerState{
		Status:  "exited",
		Running: false,
	}
	if running {
		state.Status = "running"
		state.Running = true
		state.StartedAt = d.CreationTimestamp.Time.Format(time.RFC3339)
	}

	healthcheck, _ := decodeHealthcheckAnnotation(d.Annotations[types.AnnotationHealthcheck])
	if healthcheckEnabled(healthcheck) && running {
		if runtimeState.Health != nil {
			state.Health = runtimeState.Health
		} else {
			state.Health = &dockertypes.Health{
				Status:        dockertypes.Starting,
				FailingStreak: 0,
				Log:           []*container.HealthcheckResult{},
			}
		}
	}

	hostConfig := &container.HostConfig{
		LogConfig: container.LogConfig{
			Type: "json-file",
		},
	}

	portMap := nat.PortMap{}
	rawPorts := d.Annotations[types.AnnotationPortMappings]
	if rawPorts != "" {
		var bindings []string
		if err := json.Unmarshal([]byte(rawPorts), &bindings); err == nil {
			for _, raw := range bindings {
				parts := strings.SplitN(raw, ":", 2)
				if len(parts) == 2 {
					p := nat.Port(parts[1] + "/tcp")
					portMap[p] = []nat.PortBinding{{HostPort: parts[0]}}
				}
			}
		}
		hostConfig.PortBindings = portMap
	}

	return dockertypes.ContainerJSON{
		ContainerJSONBase: &dockertypes.ContainerJSONBase{
			ID:         string(d.UID),
			Name:       "/" + d.Name,
			Image:      d.Annotations[types.AnnotationImageRef],
			Created:    d.CreationTimestamp.Time.Format(time.RFC3339),
			State:      state,
			HostConfig: hostConfig,
		},
		Config: &container.Config{
			Image:        d.Annotations[types.AnnotationImageRef],
			Healthcheck:  healthcheck,
			ExposedPorts: nat.PortSet{},
			Tty:          false,
			AttachStdin:  false,
			AttachStdout: true,
			AttachStderr: true,
			Cmd:          []string{"/bin/sh"},
			Entrypoint:   []string{},
			Env:          []string{},
			Labels:       map[string]string{},
		},
		NetworkSettings: &dockertypes.NetworkSettings{
			NetworkSettingsBase: dockertypes.NetworkSettingsBase{
				Ports: nat.PortMap(portMap),
			},
			DefaultNetworkSettings: dockertypes.DefaultNetworkSettings{
				IPAddress: lbIP,
			},
			Networks: map[string]*network.EndpointSettings{
				"bridge": {
					IPAddress: lbIP,
					NetworkID: "bridge",
				},
			},
		},
	}
}

// humanizeDuration formats a duration the way Docker does in docker ps STATUS:
// seconds, minutes, hours up to 47h, then days, weeks, months, years.
func humanizeDuration(d time.Duration) string {
	seconds := int(d.Seconds())
	if seconds < 60 {
		return fmt.Sprintf("%d seconds", seconds)
	}
	minutes := seconds / 60
	if minutes < 60 {
		return fmt.Sprintf("%d minutes", minutes)
	}
	hours := minutes / 60
	if hours < 48 {
		return fmt.Sprintf("%d hours", hours)
	}
	days := hours / 24
	if days < 14 {
		return fmt.Sprintf("%d days", days)
	}
	weeks := days / 7
	if weeks < 8 {
		return fmt.Sprintf("%d weeks", weeks)
	}
	months := days / 30
	if months < 24 {
		return fmt.Sprintf("%d months", months)
	}
	years := days / 365
	return fmt.Sprintf("%d years", years)
}

// encodePortMappings serialises the raw port binding strings into a JSON label value.
func encodePortMappings(bindings []string, publishAll bool) (string, error) {
	if publishAll {
		b, err := json.Marshal([]string{"-P"})
		return string(b), err
	}
	if len(bindings) == 0 {
		return "", nil
	}
	b, err := json.Marshal(bindings)
	return string(b), err
}

// ResolveContainerName is the public wrapper around resolveDeploymentName.
func (a *KubernetesDockerAdapter) ResolveContainerName(ctx context.Context, name string) (string, error) {
	return a.resolveDeploymentName(ctx, name)
}
