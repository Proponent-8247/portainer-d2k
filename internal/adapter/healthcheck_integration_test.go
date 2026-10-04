package adapter

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	dockertypes "github.com/docker/docker/api/types"
	dockcontainer "github.com/docker/docker/api/types/container"
	"go.uber.org/zap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

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
	if err := a.SwarmUpdateService(ctx, "web", updateBody); err != nil {
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
