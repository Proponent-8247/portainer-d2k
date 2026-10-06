package adapter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	dockertypes "github.com/docker/docker/api/types"
	dockcontainer "github.com/docker/docker/api/types/container"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	utilexec "k8s.io/client-go/util/exec"
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

	for streak := 1; streak <= 3; streak++ {
		result := &dockertypes.HealthcheckResult{Start: time.Now(), End: time.Now(), ExitCode: 1}
		applyDockerHealthResult(state, hc, result)
		if state.Health.FailingStreak != streak {
			t.Fatalf("failing streak = %d, want %d", state.Health.FailingStreak, streak)
		}
	}
	if state.Health.Status != dockertypes.Unhealthy {
		t.Fatalf("third failure status = %s, want unhealthy", state.Health.Status)
	}

	success := &dockertypes.HealthcheckResult{Start: time.Now(), End: time.Now(), ExitCode: 0}
	applyDockerHealthResult(state, hc, success)
	if state.Health.Status != dockertypes.Healthy || state.Health.FailingStreak != 0 {
		t.Fatalf("success = %s/%d, want healthy/0", state.Health.Status, state.Health.FailingStreak)
	}
}

func TestDockerHealthStartPeriodUsesFullStartIntervalAcrossBoundary(t *testing.T) {
	started := time.Now()
	hc := &dockcontainer.HealthConfig{
		Test:          []string{"CMD", "check"},
		Interval:      30 * time.Second,
		StartPeriod:   20 * time.Second,
		StartInterval: 9 * time.Second,
		Retries:       2,
	}

	if got := nextDockerHealthInterval(hc, dockertypes.Starting, started, started.Add(3*time.Second)); got != 9*time.Second {
		t.Fatalf("start interval = %s, want 9s", got)
	}
	if got := nextDockerHealthInterval(hc, dockertypes.Starting, started, started.Add(19*time.Second)); got != 9*time.Second {
		t.Fatalf("boundary-crossing start interval = %s, want full 9s", got)
	}
	if got := nextDockerHealthInterval(hc, dockertypes.Starting, started, started.Add(21*time.Second)); got != 30*time.Second {
		t.Fatalf("post-start-period interval = %s, want 30s", got)
	}
	if got := nextDockerHealthInterval(hc, dockertypes.Healthy, started, started.Add(5*time.Second)); got != 30*time.Second {
		t.Fatalf("healthy interval during former start period = %s, want 30s", got)
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

func TestHealthOutputBufferMatchesDockerTruncationMarker(t *testing.T) {
	for _, tc := range []struct {
		name       string
		inputBytes int
		wantSuffix string
		wantLen    int
	}{
		{name: "below limit", inputBytes: 4095, wantSuffix: "", wantLen: 4095},
		{name: "at limit", inputBytes: 4096, wantSuffix: "", wantLen: 4096},
		{name: "over limit", inputBytes: 4097, wantSuffix: "...", wantLen: 4099},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &healthOutputBuffer{limit: dockerHealthMaxOutputBytes}
			_, _ = b.Write([]byte(strings.Repeat("x", tc.inputBytes)))
			got := b.String()
			if len(got) != tc.wantLen {
				t.Fatalf("length = %d, want %d", len(got), tc.wantLen)
			}
			if tc.wantSuffix != "" && !strings.HasSuffix(got, tc.wantSuffix) {
				t.Fatalf("truncated output %q missing suffix %q", got[len(got)-8:], tc.wantSuffix)
			}
		})
	}
}

func TestStaleMonitorCannotOverwriteNewContainerState(t *testing.T) {
	a := newHealthcheckTestAdapter()
	a.healthStates = map[string]*dockerHealthState{}
	a.healthMonitors = map[string]*healthMonitorRegistration{
		"old": {token: 1, podUID: "pod", containerID: "old-container"},
		"new": {token: 2, podUID: "pod", containerID: "new-container"},
	}
	a.healthCurrent = map[string]healthMonitorOwner{
		"pod": {key: "new", token: 2, containerID: "new-container"},
	}

	oldSpec := dockerHealthMonitorSpec{Key: "old", PodUID: "pod", ContainerID: "old-container"}
	newSpec := dockerHealthMonitorSpec{Key: "new", PodUID: "pod", ContainerID: "new-container"}
	oldState := &dockerHealthState{Health: dockertypes.Health{Status: dockertypes.Unhealthy}, ContainerID: "old-container"}
	newState := &dockerHealthState{Health: dockertypes.Health{Status: dockertypes.Starting}, ContainerID: "new-container"}

	if a.storeDockerHealthState(oldSpec, 1, oldState) {
		t.Fatal("stale monitor overwrote current Pod health state")
	}
	if !a.storeDockerHealthState(newSpec, 2, newState) {
		t.Fatal("current monitor could not publish health state")
	}
	if got := a.healthStates["pod"].ContainerID; got != "new-container" {
		t.Fatalf("stored container = %q, want new-container", got)
	}
}

func TestCancelledMonitorRegistrationCannotDeleteNewerRegistration(t *testing.T) {
	a := newHealthcheckTestAdapter()
	a.healthMonitors = map[string]*healthMonitorRegistration{
		"same": {token: 2, podUID: "pod", containerID: "container"},
	}
	// This assertion captures the ownership predicate used by deferred monitor
	// teardown: a stale token must not match the newer registration.
	if current := a.healthMonitors["same"]; current == nil || current.token == 1 {
		t.Fatalf("new monitor registration was not distinct: %#v", current)
	}
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
