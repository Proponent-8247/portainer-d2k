package adapter

import (
	"context"
	"errors"
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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	if container.ReadinessProbe != nil || container.LivenessProbe != nil {
		t.Fatalf("standalone healthcheck must be owned by d2k monitor, got readiness=%#v liveness=%#v", container.ReadinessProbe, container.LivenessProbe)
	}

	stored, err := decodeHealthcheckAnnotation(deployment.Annotations[types.AnnotationHealthcheck])
	if err != nil {
		t.Fatalf("decode stored healthcheck: %v", err)
	}
	if !reflect.DeepEqual(stored, hc) {
		t.Fatalf("stored healthcheck = %#v, want %#v", stored, hc)
	}

	health := &dockertypes.Health{Status: dockertypes.Healthy, FailingStreak: 0}
	inspect := deploymentToContainerJSON(*deployment, "", workloadRuntimeState{
		Running: true,
		Ready:   true,
		Health:  health,
	})
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
	if container.ReadinessProbe != nil || container.LivenessProbe != nil {
		t.Fatalf("Swarm healthcheck must be owned by d2k monitor, got readiness=%#v liveness=%#v", container.ReadinessProbe, container.LivenessProbe)
	}
	if len(deployment.Spec.Template.Spec.ReadinessGates) != 1 ||
		deployment.Spec.Template.Spec.ReadinessGates[0].ConditionType != corev1.PodConditionType(types.HealthReadinessGate) {
		t.Fatalf("Swarm healthcheck readiness gate missing: %#v", deployment.Spec.Template.Spec.ReadinessGates)
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
		t.Fatalf("removed healthcheck left legacy probes: readiness=%#v liveness=%#v", container.ReadinessProbe, container.LivenessProbe)
	}
	if len(deployment.Spec.Template.Spec.ReadinessGates) != 0 {
		t.Fatalf("removed healthcheck left readiness gate: %#v", deployment.Spec.Template.Spec.ReadinessGates)
	}
	if _, exists := deployment.Annotations[types.AnnotationHealthcheck]; exists {
		t.Fatalf("removed healthcheck left stale annotation: %#v", deployment.Annotations)
	}
}


func TestSwarmUpdateUsesNativeDockerStartTimingWithoutApproximationWarnings(t *testing.T) {
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
	if len(warnings) != 0 {
		t.Fatalf("d2k-owned monitor should not emit Kubernetes timing approximation warnings: %#v", warnings)
	}

	deployment, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get updated deployment: %v", err)
	}
	if len(deployment.Spec.Template.Spec.ReadinessGates) != 1 ||
		deployment.Spec.Template.Spec.ReadinessGates[0].ConditionType != corev1.PodConditionType(types.HealthReadinessGate) {
		t.Fatalf("healthcheck update did not add readiness gate: %#v", deployment.Spec.Template.Spec.ReadinessGates)
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
		t.Fatalf("NONE healthcheck left legacy probes: readiness=%#v liveness=%#v", container.ReadinessProbe, container.LivenessProbe)
	}
	if len(deployment.Spec.Template.Spec.ReadinessGates) != 0 {
		t.Fatalf("NONE healthcheck left readiness gate: %#v", deployment.Spec.Template.Spec.ReadinessGates)
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
						Name:  "original",
						Image: "busybox",
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
			UID:       "original-pod-uid",
			Labels: map[string]string{
				types.LabelManagedBy:    types.LabelManagedByValue,
				types.LabelWorkloadName: "original",
				"app":                   "original",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:        "original",
				ContainerID: "containerd://abc",
				Ready:       true,
				State: corev1.ContainerState{
					Running: &corev1.ContainerStateRunning{StartedAt: started},
				},
			}},
		},
	}

	a := newHealthcheckTestAdapter()
	a.client = fake.NewSimpleClientset(deployment, pod)
	a.storeDockerHealthState(string(pod.UID), &dockerHealthState{
		Health:    dockertypes.Health{Status: dockertypes.Healthy},
		StartedAt: started.Time,
		UpdatedAt: time.Now(),
	})

	inspect, err := a.InspectContainer(ctx, "renamed")
	if err != nil {
		t.Fatalf("InspectContainer: %v", err)
	}
	if inspect.State == nil || !inspect.State.Running {
		t.Fatalf("renamed running workload was reported stopped: %#v", inspect.State)
	}
	if inspect.State.Health == nil || inspect.State.Health.Status != dockertypes.Healthy {
		t.Fatalf("renamed healthchecked workload lost monitor state: %#v", inspect.State)
	}
}

func int32Ptr(v int32) *int32 { return &v }



func TestSwarmUpdateReappliesHealthcheckAfterConflict(t *testing.T) {
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

	conflicts := 0
	a.client.(*fake.Clientset).Fake.PrependReactor("update", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if conflicts == 0 {
			conflicts++
			return true, nil, apierrors.NewConflict(
				schema.GroupResource{Group: "apps", Resource: "deployments"},
				"web",
				fmt.Errorf("simulated conflict"),
			)
		}
		return false, nil, nil
	})

	updateBody := strings.NewReader(`{
		"Name":"web",
		"TaskTemplate":{"ContainerSpec":{
			"Image":"busybox:latest",
			"Healthcheck":{"Test":["CMD","false"],"Interval":7000000000}
		}},
		"Mode":{"Replicated":{"Replicas":1}}
	}`)
	if _, err := a.SwarmUpdateService(ctx, "web", updateBody); err != nil {
		t.Fatalf("SwarmUpdateService: %v", err)
	}
	if conflicts != 1 {
		t.Fatalf("expected exactly one simulated conflict, got %d", conflicts)
	}

	deployment, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get updated deployment: %v", err)
	}
	hc, err := decodeHealthcheckAnnotation(deployment.Annotations[types.AnnotationHealthcheck])
	if err != nil {
		t.Fatalf("decode healthcheck: %v", err)
	}
	if hc == nil || !reflect.DeepEqual(hc.Test, []string{"CMD", "false"}) || hc.Interval != 7*time.Second {
		t.Fatalf("healthcheck update was lost after conflict: %#v", hc)
	}
	if len(deployment.Spec.Template.Spec.ReadinessGates) != 1 ||
		deployment.Spec.Template.Spec.ReadinessGates[0].ConditionType != corev1.PodConditionType(types.HealthReadinessGate) {
		t.Fatalf("health readiness gate was lost after conflict: %#v", deployment.Spec.Template.Spec.ReadinessGates)
	}
}

func TestInvalidSwarmHealthcheckDoesNotCreateFallbackPVC(t *testing.T) {
	ctx := context.Background()
	a := newHealthcheckTestAdapter()

	body := strings.NewReader(`{
		"Name":"web",
		"TaskTemplate":{"ContainerSpec":{
			"Image":"busybox:latest",
			"Healthcheck":{"Test":["CMD","true"],"Interval":1},
			"Mounts":[{"Type":"volume","Source":"data","Target":"/data"}]
		}},
		"Mode":{"Replicated":{"Replicas":1}}
	}`)

	if _, err := a.SwarmCreateService(ctx, body); err == nil {
		t.Fatal("expected invalid healthcheck to reject service creation")
	} else if !errors.Is(err, ErrInvalidHealthcheck) {
		t.Fatalf("error %v is not classified as invalid healthcheck", err)
	}

	pvcs, err := a.client.CoreV1().PersistentVolumeClaims(a.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list PVCs: %v", err)
	}
	if len(pvcs.Items) != 0 {
		t.Fatalf("invalid healthcheck left PVC side effects: %#v", pvcs.Items)
	}
	deployments, err := a.client.AppsV1().Deployments(a.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list deployments: %v", err)
	}
	if len(deployments.Items) != 0 {
		t.Fatalf("invalid healthcheck left deployment side effects: %#v", deployments.Items)
	}
}


func TestKubePodToSwarmTaskUsesTargetContainerHealthNotSidecar(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID: "pod-uid",
			Labels: map[string]string{
				types.LabelSwarmService: "web",
			},
		},
		Spec: corev1.PodSpec{
			ReadinessGates: []corev1.PodReadinessGate{{
				ConditionType: corev1.PodConditionType(types.HealthReadinessGate),
			}},
			Containers: []corev1.Container{
				{Name: "sidecar"},
				{Name: "web"},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:  "sidecar",
					Ready: true,
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				},
				{
					Name:  "web",
					Ready: false,
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				},
			},
		},
	}

	task := kubePodToSwarmTask(pod, "svc", "node", 1, &dockertypes.Health{Status: dockertypes.Starting})
	status := task["Status"].(map[string]any)
	if status["State"] != "starting" {
		t.Fatalf("ready sidecar incorrectly made healthchecked target task running: %#v", status)
	}
}


func TestKubePodToSwarmTaskWithHealthcheckAndNoContainerStatusIsStarting(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID: "pod-uid",
			Labels: map[string]string{
				types.LabelSwarmService: "web",
			},
		},
		Spec: corev1.PodSpec{
			ReadinessGates: []corev1.PodReadinessGate{{
				ConditionType: corev1.PodConditionType(types.HealthReadinessGate),
			}},
			Containers: []corev1.Container{{Name: "web"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	task := kubePodToSwarmTask(pod, "svc", "node", 1, &dockertypes.Health{Status: dockertypes.Healthy})
	status := task["Status"].(map[string]any)
	if status["State"] != "starting" {
		t.Fatalf("empty container status must not report healthchecked task running: %#v", status)
	}
}


func TestKubePodToSwarmTaskReportsUnhealthyMonitorStateFailed(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID: "pod-uid",
			Labels: map[string]string{
				types.LabelSwarmService: "web",
			},
		},
		Spec: corev1.PodSpec{
			ReadinessGates: []corev1.PodReadinessGate{{
				ConditionType: corev1.PodConditionType(types.HealthReadinessGate),
			}},
			Containers: []corev1.Container{{Name: "web"}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:        "web",
				ContainerID: "containerd://abc",
				State:       corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
	task := kubePodToSwarmTask(pod, "svc", "node", 1, &dockertypes.Health{
		Status:        dockertypes.Unhealthy,
		FailingStreak: 3,
	})
	status := task["Status"].(map[string]any)
	if status["State"] != "failed" {
		t.Fatalf("unhealthy Swarm health state = %#v, want failed", status)
	}
	if status["Message"] != "container unhealthy" {
		t.Fatalf("unhealthy Swarm task message = %#v", status["Message"])
	}
}
