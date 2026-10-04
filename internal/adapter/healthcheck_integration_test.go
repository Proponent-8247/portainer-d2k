package adapter

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	dockertypes "github.com/docker/docker/api/types"
	dockcontainer "github.com/docker/docker/api/types/container"
	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/portainer/d2k/internal/types"
	"github.com/portainer/d2k/pkg/portmapper"
)

func newHealthcheckTestAdapter() *KubernetesDockerAdapter {
	return &KubernetesDockerAdapter{
		client:    fake.NewSimpleClientset(),
		namespace: "healthcheck-test",
		logger:    zap.NewNop().Sugar(),
		networks:  map[string]*NetworkSummary{},
	}
}

func TestStandaloneHealthcheckWiresIntoDeploymentAndInspect(t *testing.T) {
	ctx := context.Background()
	a := newHealthcheckTestAdapter()
	hc := &dockcontainer.HealthConfig{
		Test:     []string{"CMD", "test", "-f", "/tmp/ready"},
		Interval: 7 * time.Second,
		Timeout:  2 * time.Second,
		Retries:  4,
	}

	deployment, warnings, err := a.buildDeployment(ctx, RunOptions{
		Name:        "web",
		Image:       "busybox:latest",
		Healthcheck: hc,
	}, portmapper.NoService, nil)
	if err != nil {
		t.Fatalf("buildDeployment: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}
	container := deployment.Spec.Template.Spec.Containers[0]
	if container.ReadinessProbe == nil {
		t.Fatal("standalone healthcheck did not create readiness probe")
	}
	if container.LivenessProbe != nil {
		t.Fatal("standalone healthcheck must not create liveness probe")
	}
	if !reflect.DeepEqual(container.ReadinessProbe.Exec.Command, []string{"test", "-f", "/tmp/ready"}) {
		t.Fatalf("unexpected readiness command: %#v", container.ReadinessProbe.Exec.Command)
	}

	stored, err := decodeHealthcheckAnnotation(deployment.Annotations[types.AnnotationHealthcheck])
	if err != nil {
		t.Fatalf("decode stored healthcheck: %v", err)
	}
	if !reflect.DeepEqual(stored, hc) {
		t.Fatalf("stored healthcheck = %#v, want %#v", stored, hc)
	}

	inspect := deploymentToContainerJSON(*deployment, "", workloadRuntimeState{Running: true, Ready: true})
	if inspect.Config == nil || !reflect.DeepEqual(inspect.Config.Healthcheck, hc) {
		t.Fatalf("inspect healthcheck = %#v, want %#v", inspect.Config, hc)
	}
	if inspect.State == nil || inspect.State.Health == nil || inspect.State.Health.Status != dockertypes.Healthy {
		t.Fatalf("inspect health state = %#v, want healthy", inspect.State)
	}
}

func TestSwarmHealthcheckWiresIntoCreateAndReadback(t *testing.T) {
	ctx := context.Background()
	a := newHealthcheckTestAdapter()

	body := strings.NewReader(`{
		"Name":"web",
		"TaskTemplate":{
			"ContainerSpec":{
				"Image":"busybox:latest",
				"Healthcheck":{
					"Test":["CMD-SHELL","test -f /tmp/ready"],
					"Interval":5000000000,
					"Timeout":2000000000,
					"Retries":3
				}
			}
		},
		"Mode":{"Replicated":{"Replicas":1}}
	}`)

	if _, err := a.SwarmCreateService(ctx, body); err != nil {
		t.Fatalf("SwarmCreateService: %v", err)
	}

	deployment, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	container := deployment.Spec.Template.Spec.Containers[0]
	if container.ReadinessProbe == nil || container.LivenessProbe == nil {
		t.Fatalf("Swarm healthcheck probes missing: readiness=%#v liveness=%#v", container.ReadinessProbe, container.LivenessProbe)
	}
	wantCommand := []string{"/bin/sh", "-c", "test -f /tmp/ready"}
	if !reflect.DeepEqual(container.ReadinessProbe.Exec.Command, wantCommand) ||
		!reflect.DeepEqual(container.LivenessProbe.Exec.Command, wantCommand) {
		t.Fatalf("unexpected Swarm probe command: readiness=%#v liveness=%#v",
			container.ReadinessProbe.Exec.Command, container.LivenessProbe.Exec.Command)
	}

	service := a.deploymentToSwarmService(ctx, *deployment)
	spec := service["Spec"].(map[string]any)
	taskTemplate := spec["TaskTemplate"].(map[string]any)
	containerSpec := taskTemplate["ContainerSpec"].(map[string]any)
	readback, ok := containerSpec["Healthcheck"].(*dockcontainer.HealthConfig)
	if !ok || readback == nil {
		t.Fatalf("Swarm service inspect did not return healthcheck: %#v", containerSpec["Healthcheck"])
	}
	if !reflect.DeepEqual(readback.Test, []string{"CMD-SHELL", "test -f /tmp/ready"}) ||
		readback.Interval != 5*time.Second || readback.Timeout != 2*time.Second || readback.Retries != 3 {
		t.Fatalf("unexpected Swarm healthcheck readback: %#v", readback)
	}
}

func TestStandaloneHealthcheckDoesNotGateServiceEndpoints(t *testing.T) {
	ctx := context.Background()
	a := newHealthcheckTestAdapter()

	if _, _, err := a.CreateContainer(ctx, RunOptions{
		Name:  "web",
		Image: "busybox:latest",
		Healthcheck: &dockcontainer.HealthConfig{
			Test: []string{"CMD", "true"},
		},
	}); err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}

	svc, err := a.client.CoreV1().Services(a.namespace).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get DNS service: %v", err)
	}
	if !svc.Spec.PublishNotReadyAddresses {
		t.Fatal("standalone healthcheck must not remove unhealthy containers from Docker-style network reachability")
	}
}

func TestNoHealthcheckDoesNotRequirePodList(t *testing.T) {
	ctx := context.Background()
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web",
			Namespace: "healthcheck-test",
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 1},
	}
	client := fake.NewSimpleClientset(deployment)
	podListCalled := false
	client.Fake.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		podListCalled = true
		return true, nil, fmt.Errorf("pod listing intentionally blocked")
	})
	a := &KubernetesDockerAdapter{
		client:    client,
		namespace: "healthcheck-test",
		logger:    zap.NewNop().Sugar(),
		networks:  map[string]*NetworkSummary{},
	}

	if _, err := a.ListContainers(ctx, false); err != nil {
		t.Fatalf("ListContainers without healthcheck unexpectedly required pods: %v", err)
	}
	if _, err := a.InspectContainer(ctx, "web"); err != nil {
		t.Fatalf("InspectContainer without healthcheck unexpectedly required pods: %v", err)
	}
	if podListCalled {
		t.Fatal("pod list was called for workload without translated healthcheck")
	}
}

func TestWorkloadRuntimeStateIgnoresForeignPodsAndSidecars(t *testing.T) {
	ctx := context.Background()
	started := metav1.NewTime(time.Now().Add(-time.Minute))

	foreign := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foreign",
			Namespace: "healthcheck-test",
			Labels:    map[string]string{"app": "web"},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "web",
				Ready: true,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: started}},
			}},
		},
	}
	managed := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "managed",
			Namespace: "healthcheck-test",
			Labels: map[string]string{
				types.LabelManagedBy:    types.LabelManagedByValue,
				types.LabelWorkloadName: "web",
				"app":                   "web",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:  "sidecar",
					Ready: true,
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: started}},
				},
				{
					Name:  "web",
					Ready: false,
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: started}},
				},
			},
		},
	}
	a := newHealthcheckTestAdapter()
	a.client = fake.NewSimpleClientset(foreign, managed)

	states, err := a.workloadRuntimeStates(ctx)
	if err != nil {
		t.Fatalf("workloadRuntimeStates: %v", err)
	}
	state := states["web"]
	if !state.Running {
		t.Fatal("managed target container should be running")
	}
	if state.Ready {
		t.Fatal("foreign Pod or injected sidecar incorrectly made workload healthy")
	}
}

func TestDockerHealthStatusTransitionsToUnhealthy(t *testing.T) {
	hc := &dockcontainer.HealthConfig{
		Test:     []string{"CMD", "false"},
		Interval: 5 * time.Second,
		Retries:  3,
	}
	annotations := map[string]string{}
	if err := annotateHealthcheck(annotations, hc); err != nil {
		t.Fatalf("annotateHealthcheck: %v", err)
	}
	readiness, _, _, err := buildHealthProbes(hc, false)
	if err != nil {
		t.Fatalf("buildHealthProbes: %v", err)
	}
	deployment := appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Annotations: annotations},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "web", ReadinessProbe: readiness}},
				},
			},
		},
	}

	starting, streak := dockerHealthStatus(deployment, workloadRuntimeState{
		Running:   true,
		Ready:     false,
		StartedAt: time.Now().Add(-6 * time.Second),
	})
	if starting != dockertypes.Starting || streak != 0 {
		t.Fatalf("early health status = %q/%d, want starting/0", starting, streak)
	}

	unhealthy, streak := dockerHealthStatus(deployment, workloadRuntimeState{
		Running:   true,
		Ready:     false,
		StartedAt: time.Now().Add(-16 * time.Second),
	})
	if unhealthy != dockertypes.Unhealthy || streak != 3 {
		t.Fatalf("late health status = %q/%d, want unhealthy/3", unhealthy, streak)
	}

	healthy, streak := dockerHealthStatus(deployment, workloadRuntimeState{
		Running:   true,
		Ready:     true,
		StartedAt: time.Now().Add(-time.Minute),
	})
	if healthy != dockertypes.Healthy || streak != 0 {
		t.Fatalf("ready health status = %q/%d, want healthy/0", healthy, streak)
	}
	stopped, streak := dockerHealthStatus(deployment, workloadRuntimeState{})
	if stopped != "" || streak != 0 {
		t.Fatalf("stopped health status = %q/%d, want omitted/0", stopped, streak)
	}
}

func TestSwarmReadbackOmitsAbsentHealthcheck(t *testing.T) {
	ctx := context.Background()
	a := newHealthcheckTestAdapter()
	body := strings.NewReader(`{
		"Name":"web",
		"TaskTemplate":{"ContainerSpec":{"Image":"busybox:latest"}},
		"Mode":{"Replicated":{"Replicas":1}}
	}`)
	if _, err := a.SwarmCreateService(ctx, body); err != nil {
		t.Fatalf("SwarmCreateService: %v", err)
	}
	deployment, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	service := a.deploymentToSwarmService(ctx, *deployment)
	spec := service["Spec"].(map[string]any)
	taskTemplate := spec["TaskTemplate"].(map[string]any)
	containerSpec := taskTemplate["ContainerSpec"].(map[string]any)
	if _, exists := containerSpec["Healthcheck"]; exists {
		t.Fatalf("absent healthcheck should be omitted from service readback: %#v", containerSpec)
	}
}

func TestDockerHealthStatusUsesKubernetesProbeTickSchedule(t *testing.T) {
	hc := &dockcontainer.HealthConfig{
		Test:        []string{"CMD", "false"},
		Interval:    30 * time.Second,
		StartPeriod: 5 * time.Second,
		Retries:     3,
	}
	annotations := map[string]string{}
	if err := annotateHealthcheck(annotations, hc); err != nil {
		t.Fatalf("annotateHealthcheck: %v", err)
	}
	readiness, _, _, err := buildHealthProbes(hc, false)
	if err != nil {
		t.Fatalf("buildHealthProbes: %v", err)
	}
	deployment := appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Annotations: annotations},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "web", ReadinessProbe: readiness}},
				},
			},
		},
	}

	status, _ := dockerHealthStatus(deployment, workloadRuntimeState{
		Running:   true,
		StartedAt: time.Now().Add(-65 * time.Second),
	})
	if status != dockertypes.Starting {
		t.Fatalf("health status at 65s = %q, want starting until third 30s probe tick", status)
	}
	status, streak := dockerHealthStatus(deployment, workloadRuntimeState{
		Running:   true,
		StartedAt: time.Now().Add(-91 * time.Second),
	})
	if status != dockertypes.Unhealthy || streak != 3 {
		t.Fatalf("health status at 91s = %q/%d, want unhealthy/3", status, streak)
	}
}

func TestSwarmUpdateCanRemoveHealthcheck(t *testing.T) {
	ctx := context.Background()
	a := newHealthcheckTestAdapter()

	createBody := strings.NewReader(`{
		"Name":"web",
		"TaskTemplate":{"ContainerSpec":{
			"Image":"busybox:latest",
			"Healthcheck":{"Test":["CMD","true"]}
		}},
		"Mode":{"Replicated":{"Replicas":1}}
	}`)
	if _, err := a.SwarmCreateService(ctx, createBody); err != nil {
		t.Fatalf("SwarmCreateService: %v", err)
	}

	updateBody := strings.NewReader(`{
		"Name":"web",
		"TaskTemplate":{"ContainerSpec":{"Image":"busybox:latest"}},
		"Mode":{"Replicated":{"Replicas":1}}
	}`)
	warnings, err := a.SwarmUpdateService(ctx, "web", updateBody)
	if err != nil {
		t.Fatalf("SwarmUpdateService: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	deployment, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get updated deployment: %v", err)
	}
	container := deployment.Spec.Template.Spec.Containers[0]
	if container.ReadinessProbe != nil || container.LivenessProbe != nil {
		t.Fatalf("removed healthcheck left stale probes: readiness=%#v liveness=%#v", container.ReadinessProbe, container.LivenessProbe)
	}
	if _, exists := deployment.Annotations[types.AnnotationHealthcheck]; exists {
		t.Fatalf("removed healthcheck left stale annotation: %#v", deployment.Annotations)
	}
}

func TestSwarmUpdateReturnsHealthcheckWarnings(t *testing.T) {
	ctx := context.Background()
	a := newHealthcheckTestAdapter()

	createBody := strings.NewReader(`{
		"Name":"web",
		"TaskTemplate":{"ContainerSpec":{"Image":"busybox:latest"}},
		"Mode":{"Replicated":{"Replicas":1}}
	}`)
	if _, err := a.SwarmCreateService(ctx, createBody); err != nil {
		t.Fatalf("SwarmCreateService: %v", err)
	}

	updateBody := strings.NewReader(`{
		"Name":"web",
		"TaskTemplate":{"ContainerSpec":{
			"Image":"busybox:latest",
			"Healthcheck":{
				"Test":["CMD","true"],
				"StartPeriod":10000000000,
				"StartInterval":1000000000
			}
		}},
		"Mode":{"Replicated":{"Replicas":1}}
	}`)
	warnings, err := a.SwarmUpdateService(ctx, "web", updateBody)
	if err != nil {
		t.Fatalf("SwarmUpdateService: %v", err)
	}
	if len(warnings) != 2 {
		t.Fatalf("expected start-period/start-interval warnings, got %#v", warnings)
	}
}

func TestSwarmUpdateCanDisableHealthcheck(t *testing.T) {
	ctx := context.Background()
	a := newHealthcheckTestAdapter()

	createBody := strings.NewReader(`{
		"Name":"web",
		"TaskTemplate":{"ContainerSpec":{
			"Image":"busybox:latest",
			"Healthcheck":{"Test":["CMD","true"]}
		}},
		"Mode":{"Replicated":{"Replicas":1}}
	}`)
	if _, err := a.SwarmCreateService(ctx, createBody); err != nil {
		t.Fatalf("SwarmCreateService: %v", err)
	}

	updateBody := strings.NewReader(`{
		"Name":"web",
		"TaskTemplate":{"ContainerSpec":{
			"Image":"busybox:latest",
			"Healthcheck":{"Test":["NONE"]}
		}},
		"Mode":{"Replicated":{"Replicas":1}}
	}`)
	if _, err := a.SwarmUpdateService(ctx, "web", updateBody); err != nil {
		t.Fatalf("SwarmUpdateService: %v", err)
	}

	deployment, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get updated deployment: %v", err)
	}
	container := deployment.Spec.Template.Spec.Containers[0]
	if container.ReadinessProbe != nil || container.LivenessProbe != nil {
		t.Fatalf("NONE healthcheck did not remove probes: readiness=%#v liveness=%#v", container.ReadinessProbe, container.LivenessProbe)
	}
	hc, err := decodeHealthcheckAnnotation(deployment.Annotations[types.AnnotationHealthcheck])
	if err != nil {
		t.Fatalf("decode healthcheck: %v", err)
	}
	if hc == nil || !reflect.DeepEqual(hc.Test, []string{"NONE"}) {
		t.Fatalf("disabled healthcheck was not preserved for readback: %#v", hc)
	}
}


func TestRenamedDeploymentUsesOriginalWorkloadIdentityForHealthRuntimeState(t *testing.T) {
	ctx := context.Background()
	hc := &dockcontainer.HealthConfig{
		Test:     []string{"CMD", "false"},
		Interval: 5 * time.Second,
		Retries:  3,
	}
	annotations := map[string]string{}
	if err := annotateHealthcheck(annotations, hc); err != nil {
		t.Fatalf("annotate healthcheck: %v", err)
	}
	readiness, _, _, err := buildHealthProbes(hc, false)
	if err != nil {
		t.Fatalf("build health probes: %v", err)
	}

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "renamed",
			Namespace:   "healthcheck-test",
			Annotations: annotations,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						types.LabelManagedBy:    types.LabelManagedByValue,
						types.LabelWorkloadName: "original",
						"app":                   "original",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:           "original",
						Image:          "busybox",
						ReadinessProbe: readiness,
					}},
				},
			},
		},
	}
	started := metav1.NewTime(time.Now().Add(-10 * time.Second))
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "original-pod",
			Namespace: "healthcheck-test",
			Labels: map[string]string{
				types.LabelManagedBy:    types.LabelManagedByValue,
				types.LabelWorkloadName: "original",
				"app":                   "original",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "original",
				Ready: false,
				State: corev1.ContainerState{
					Running: &corev1.ContainerStateRunning{StartedAt: started},
				},
			}},
		},
	}

	a := newHealthcheckTestAdapter()
	a.client = fake.NewSimpleClientset(deployment, pod)

	inspect, err := a.InspectContainer(ctx, "renamed")
	if err != nil {
		t.Fatalf("InspectContainer: %v", err)
	}
	if inspect.State == nil || !inspect.State.Running {
		t.Fatalf("renamed running workload was reported stopped: %#v", inspect.State)
	}
	if inspect.State.Health == nil {
		t.Fatalf("renamed healthchecked workload lost health state: %#v", inspect.State)
	}
}

func int32Ptr(v int32) *int32 { return &v }
