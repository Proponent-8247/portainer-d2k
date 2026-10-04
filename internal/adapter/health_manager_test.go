package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"testing"
	"time"

	dockertypes "github.com/docker/docker/api/types"
	dockcontainer "github.com/docker/docker/api/types/container"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	utilexec "k8s.io/client-go/util/exec"

	"github.com/portainer/d2k/internal/types"
)

func TestDockerHealthStateTracksExactFailingStreak(t *testing.T) {
	started := time.Now().Add(-time.Minute)
	state := &dockerHealthState{
		Health: dockertypes.Health{
			Status: dockertypes.Starting,
			Log:    []*dockertypes.HealthcheckResult{},
		},
		StartedAt: started,
	}
	hc := &dockcontainer.HealthConfig{
		Test:    []string{"CMD", "check"},
		Retries: 3,
	}

	first := &dockertypes.HealthcheckResult{Start: time.Now(), End: time.Now(), ExitCode: 1}
	applyDockerHealthResult(state, hc, first)
	if state.Health.Status != dockertypes.Starting || state.Health.FailingStreak != 1 {
		t.Fatalf("first failure = %s/%d, want starting/1", state.Health.Status, state.Health.FailingStreak)
	}

	second := &dockertypes.HealthcheckResult{Start: time.Now(), End: time.Now(), ExitCode: 1}
	applyDockerHealthResult(state, hc, second)
	if state.Health.Status != dockertypes.Starting || state.Health.FailingStreak != 2 {
		t.Fatalf("second failure = %s/%d, want starting/2", state.Health.Status, state.Health.FailingStreak)
	}

	third := &dockertypes.HealthcheckResult{Start: time.Now(), End: time.Now(), ExitCode: 1}
	applyDockerHealthResult(state, hc, third)
	if state.Health.Status != dockertypes.Unhealthy || state.Health.FailingStreak != 3 {
		t.Fatalf("third failure = %s/%d, want unhealthy/3", state.Health.Status, state.Health.FailingStreak)
	}

	success := &dockertypes.HealthcheckResult{Start: time.Now(), End: time.Now(), ExitCode: 0}
	applyDockerHealthResult(state, hc, success)
	if state.Health.Status != dockertypes.Healthy || state.Health.FailingStreak != 0 {
		t.Fatalf("success = %s/%d, want healthy/0", state.Health.Status, state.Health.FailingStreak)
	}
}

func TestDockerHealthStartPeriodAndStartInterval(t *testing.T) {
	started := time.Now()
	hc := &dockcontainer.HealthConfig{
		Test:          []string{"CMD", "check"},
		Interval:      30 * time.Second,
		StartPeriod:   20 * time.Second,
		StartInterval: 2 * time.Second,
		Retries:       2,
	}
	state := &dockerHealthState{
		Health:    dockertypes.Health{Status: dockertypes.Starting},
		StartedAt: started,
	}

	if got := nextDockerHealthInterval(hc, dockertypes.Starting, started, started.Add(3*time.Second)); got != 2*time.Second {
		t.Fatalf("start interval = %s, want 2s", got)
	}
	if got := nextDockerHealthInterval(hc, dockertypes.Starting, started, started.Add(19*time.Second)); got != time.Second {
		t.Fatalf("capped start interval = %s, want 1s", got)
	}

	graceFailure := &dockertypes.HealthcheckResult{
		Start:    started.Add(5 * time.Second),
		End:      started.Add(5*time.Second + time.Millisecond),
		ExitCode: 1,
	}
	applyDockerHealthResult(state, hc, graceFailure)
	if state.Health.FailingStreak != 0 || state.Health.Status != dockertypes.Starting {
		t.Fatalf("start-period failure = %s/%d, want starting/0", state.Health.Status, state.Health.FailingStreak)
	}

	success := &dockertypes.HealthcheckResult{
		Start:    started.Add(6 * time.Second),
		End:      started.Add(6*time.Second + time.Millisecond),
		ExitCode: 0,
	}
	applyDockerHealthResult(state, hc, success)
	if got := nextDockerHealthInterval(hc, state.Health.Status, started, started.Add(7*time.Second)); got != 30*time.Second {
		t.Fatalf("healthy interval during former start period = %s, want normal 30s", got)
	}
}

func TestHealthCommandCountsInfrastructureErrorsAndPreservesDirectArgv(t *testing.T) {
	a := newHealthcheckTestAdapter()
	var gotCommand []string
	a.healthExec = func(
		ctx context.Context,
		podName string,
		containerName string,
		command []string,
		stdout, stderr io.Writer,
	) error {
		gotCommand = append([]string{}, command...)
		_, _ = fmt.Fprint(stderr, "exec infrastructure failure")
		return errors.New("unable to start exec")
	}

	spec := dockerHealthMonitorSpec{
		PodName:       "web-pod",
		ContainerName: "web",
		Healthcheck: dockcontainer.HealthConfig{
			Test:    []string{"CMD", "check", "$(FOO)", "$BAR"},
			Timeout: time.Second,
			Retries: 2,
		},
	}
	result := a.runDockerHealthCommand(context.Background(), spec)
	if !reflect.DeepEqual(gotCommand, []string{"check", "$(FOO)", "$BAR"}) {
		t.Fatalf("direct Docker argv changed before pod exec: %#v", gotCommand)
	}
	if result.ExitCode != -1 {
		t.Fatalf("infrastructure error exit code = %d, want -1", result.ExitCode)
	}
	if result.Output == "" {
		t.Fatal("infrastructure error output was not retained")
	}

	state := &dockerHealthState{
		Health:    dockertypes.Health{Status: dockertypes.Starting},
		StartedAt: time.Now().Add(-time.Minute),
	}
	applyDockerHealthResult(state, &spec.Healthcheck, result)
	if state.Health.FailingStreak != 1 {
		t.Fatalf("infrastructure error did not increment failing streak: %#v", state.Health)
	}
}

func TestHealthCommandPreservesRemoteExitCode(t *testing.T) {
	a := newHealthcheckTestAdapter()
	a.healthExec = func(
		ctx context.Context,
		podName string,
		containerName string,
		command []string,
		stdout, stderr io.Writer,
	) error {
		return utilexec.CodeExitError{Err: errors.New("exit 7"), Code: 7}
	}

	result := a.runDockerHealthCommand(context.Background(), dockerHealthMonitorSpec{
		PodName:       "web-pod",
		ContainerName: "web",
		Healthcheck: dockcontainer.HealthConfig{
			Test: []string{"CMD", "false"},
		},
	})
	if result.ExitCode != 7 {
		t.Fatalf("remote exit code = %d, want 7", result.ExitCode)
	}
}

func TestSwarmHealthRestartMaxAttemptsAndWindow(t *testing.T) {
	ctx := context.Background()
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web",
			Namespace: "healthcheck-test",
		},
	}
	a := newHealthcheckTestAdapter()
	a.client = fake.NewSimpleClientset(deployment)

	policy := swarmHealthRestartPolicy{
		Condition:   "on-failure",
		MaxAttempts: 2,
		Window:      time.Hour,
	}
	for i := 0; i < 2; i++ {
		allowed, err := a.reserveHealthRestart(ctx, "web", policy)
		if err != nil {
			t.Fatalf("reserve restart %d: %v", i+1, err)
		}
		if !allowed {
			t.Fatalf("restart %d unexpectedly rejected", i+1)
		}
	}
	allowed, err := a.reserveHealthRestart(ctx, "web", policy)
	if err != nil {
		t.Fatalf("reserve third restart: %v", err)
	}
	if allowed {
		t.Fatal("third restart exceeded MaxAttempts")
	}

	current, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	old := time.Now().Add(-2 * time.Hour).UnixNano()
	current.Annotations[types.AnnotationHealthRestartHistory] = fmt.Sprintf("[%d,%d]", old, old)
	if _, err := a.client.AppsV1().Deployments(a.namespace).Update(ctx, current, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("age restart history: %v", err)
	}
	allowed, err = a.reserveHealthRestart(ctx, "web", policy)
	if err != nil {
		t.Fatalf("reserve restart after window: %v", err)
	}
	if !allowed {
		t.Fatal("expired restart attempts were not discarded by Window")
	}
}

func TestSwarmRestartConditionNoneNeverReservesRestart(t *testing.T) {
	a := newHealthcheckTestAdapter()
	allowed, err := a.reserveHealthRestart(context.Background(), "missing", swarmHealthRestartPolicy{Condition: "none"})
	if err != nil {
		t.Fatalf("condition none should not query Kubernetes: %v", err)
	}
	if allowed {
		t.Fatal("restart condition none allowed a health restart")
	}
}

func TestSwarmHealthFailureDeletesPodWhenRestartAllowed(t *testing.T) {
	ctx := context.Background()
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web",
			Namespace: "healthcheck-test",
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-pod",
			Namespace: "healthcheck-test",
		},
	}
	a := newHealthcheckTestAdapter()
	a.client = fake.NewSimpleClientset(deployment, pod)

	a.handleSwarmHealthFailure(ctx, dockerHealthMonitorSpec{
		PodName:        "web-pod",
		DeploymentName: "web",
		RestartPolicy: swarmHealthRestartPolicy{
			Condition: "on-failure",
			Delay:     0,
		},
	})

	if _, err := a.client.CoreV1().Pods(a.namespace).Get(ctx, "web-pod", metav1.GetOptions{}); err == nil {
		t.Fatal("unhealthy Swarm pod was not deleted for replacement")
	}

	current, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if current.Annotations[types.AnnotationHealthRestartHistory] == "" {
		t.Fatal("health-triggered restart was not persisted in restart history")
	}
}

func TestSwarmHealthFailureDoesNotDeletePodWhenRestartDisabledOrExhausted(t *testing.T) {
	ctx := context.Background()

	t.Run("restart condition none", func(t *testing.T) {
		deployment := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "healthcheck-test"},
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "web-pod", Namespace: "healthcheck-test"},
		}
		a := newHealthcheckTestAdapter()
		a.client = fake.NewSimpleClientset(deployment, pod)

		a.handleSwarmHealthFailure(ctx, dockerHealthMonitorSpec{
			PodName:        "web-pod",
			DeploymentName: "web",
			RestartPolicy:  swarmHealthRestartPolicy{Condition: "none"},
		})

		if _, err := a.client.CoreV1().Pods(a.namespace).Get(ctx, "web-pod", metav1.GetOptions{}); err != nil {
			t.Fatalf("restart condition none deleted pod: %v", err)
		}
	})

	t.Run("max attempts exhausted", func(t *testing.T) {
		history, err := json.Marshal([]int64{time.Now().UnixNano()})
		if err != nil {
			t.Fatalf("marshal history: %v", err)
		}
		deployment := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "web",
				Namespace: "healthcheck-test",
				Annotations: map[string]string{
					types.AnnotationHealthRestartHistory: string(history),
				},
			},
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "web-pod", Namespace: "healthcheck-test"},
		}
		a := newHealthcheckTestAdapter()
		a.client = fake.NewSimpleClientset(deployment, pod)

		a.handleSwarmHealthFailure(ctx, dockerHealthMonitorSpec{
			PodName:        "web-pod",
			DeploymentName: "web",
			RestartPolicy: swarmHealthRestartPolicy{
				Condition:   "on-failure",
				MaxAttempts: 1,
				Window:      time.Hour,
			},
		})

		if _, err := a.client.CoreV1().Pods(a.namespace).Get(ctx, "web-pod", metav1.GetOptions{}); err != nil {
			t.Fatalf("exhausted MaxAttempts deleted pod: %v", err)
		}
	})
}

func TestSetPodHealthCondition(t *testing.T) {
	ctx := context.Background()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web",
			Namespace: "healthcheck-test",
		},
	}
	a := newHealthcheckTestAdapter()
	a.client = fake.NewSimpleClientset(pod)

	if err := a.setPodHealthCondition(ctx, "web", corev1.ConditionFalse, "HealthStarting", "starting"); err != nil {
		t.Fatalf("set starting condition: %v", err)
	}
	if err := a.setPodHealthCondition(ctx, "web", corev1.ConditionTrue, "HealthHealthy", "healthy"); err != nil {
		t.Fatalf("set healthy condition: %v", err)
	}
	updated, err := a.client.CoreV1().Pods(a.namespace).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if !podHealthConditionReady(*updated) {
		t.Fatalf("custom health readiness gate was not set true: %#v", updated.Status.Conditions)
	}
}
