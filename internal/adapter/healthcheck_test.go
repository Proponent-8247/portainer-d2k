package adapter

import (
	"reflect"
	"strings"
	"testing"
	"time"

	dockcontainer "github.com/docker/docker/api/types/container"
)

func TestBuildHealthProbesStandaloneUsesReadinessOnly(t *testing.T) {
	hc := &dockcontainer.HealthConfig{
		Test:     []string{"CMD", "curl", "-f", "http://localhost/health"},
		Interval: 5 * time.Second,
		Timeout:  2 * time.Second,
		Retries:  4,
	}

	readiness, liveness, warnings, err := buildHealthProbes(hc, false)
	if err != nil {
		t.Fatalf("buildHealthProbes: %v", err)
	}
	if liveness != nil {
		t.Fatal("standalone Docker healthcheck must not create a liveness probe")
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}
	if readiness == nil || readiness.Exec == nil {
		t.Fatal("readiness exec probe was not created")
	}
	wantCommand := []string{"curl", "-f", "http://localhost/health"}
	if !reflect.DeepEqual(readiness.Exec.Command, wantCommand) {
		t.Fatalf("command = %#v, want %#v", readiness.Exec.Command, wantCommand)
	}
	if readiness.PeriodSeconds != 5 || readiness.TimeoutSeconds != 2 || readiness.FailureThreshold != 4 {
		t.Fatalf("unexpected probe timing: %#v", readiness)
	}
}

func TestBuildHealthProbesSwarmAddsLiveness(t *testing.T) {
	hc := &dockcontainer.HealthConfig{
		Test: []string{"CMD-SHELL", "wget -qO- http://localhost || exit 1"},
	}

	readiness, liveness, _, err := buildHealthProbes(hc, true)
	if err != nil {
		t.Fatalf("buildHealthProbes: %v", err)
	}
	if readiness == nil || liveness == nil {
		t.Fatalf("expected readiness and liveness probes, got %#v %#v", readiness, liveness)
	}
	want := []string{"/bin/sh", "-c", "wget -qO- http://localhost || exit 1"}
	if !reflect.DeepEqual(readiness.Exec.Command, want) || !reflect.DeepEqual(liveness.Exec.Command, want) {
		t.Fatalf("unexpected shell command: readiness=%#v liveness=%#v", readiness.Exec.Command, liveness.Exec.Command)
	}
	if readiness.PeriodSeconds != 30 || readiness.TimeoutSeconds != 30 || readiness.FailureThreshold != 3 {
		t.Fatalf("Docker defaults were not applied: %#v", readiness)
	}
}

func TestBuildHealthProbesNoneDisablesHealthcheck(t *testing.T) {
	hc := &dockcontainer.HealthConfig{Test: []string{"NONE"}}
	readiness, liveness, warnings, err := buildHealthProbes(hc, true)
	if err != nil {
		t.Fatalf("buildHealthProbes: %v", err)
	}
	if readiness != nil || liveness != nil || len(warnings) != 0 {
		t.Fatalf("NONE should disable probes, got readiness=%#v liveness=%#v warnings=%#v", readiness, liveness, warnings)
	}
}

func TestBuildHealthProbesStartPeriodAndRounding(t *testing.T) {
	hc := &dockcontainer.HealthConfig{
		Test:          []string{"CMD", "check"},
		Interval:      1500 * time.Millisecond,
		Timeout:       250 * time.Millisecond,
		StartPeriod:   2500 * time.Millisecond,
		StartInterval: 500 * time.Millisecond,
		Retries:       2,
	}
	readiness, _, warnings, err := buildHealthProbes(hc, false)
	if err != nil {
		t.Fatalf("buildHealthProbes: %v", err)
	}
	if readiness.PeriodSeconds != 2 || readiness.TimeoutSeconds != 1 || readiness.InitialDelaySeconds != 3 {
		t.Fatalf("unexpected rounded timing: %#v", readiness)
	}
	if len(warnings) != 2 {
		t.Fatalf("expected start-period/start-interval warnings, got %#v", warnings)
	}
}

func TestBuildHealthProbesInheritedHealthcheckWarns(t *testing.T) {
	readiness, liveness, warnings, err := buildHealthProbes(&dockcontainer.HealthConfig{}, false)
	if err != nil {
		t.Fatalf("buildHealthProbes: %v", err)
	}
	if readiness != nil || liveness != nil {
		t.Fatalf("inherited healthcheck should not invent a probe: %#v %#v", readiness, liveness)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "image-inherited") {
		t.Fatalf("expected image metadata warning, got %#v", warnings)
	}
}

func TestHealthcheckAnnotationRoundTrip(t *testing.T) {
	want := &dockcontainer.HealthConfig{
		Test:          []string{"CMD-SHELL", "test -f /tmp/ready"},
		Interval:      9 * time.Second,
		Timeout:       3 * time.Second,
		StartPeriod:   11 * time.Second,
		StartInterval: 2 * time.Second,
		Retries:       5,
	}
	raw, err := encodeHealthcheckAnnotation(want)
	if err != nil {
		t.Fatalf("encodeHealthcheckAnnotation: %v", err)
	}
	got, err := decodeHealthcheckAnnotation(raw)
	if err != nil {
		t.Fatalf("decodeHealthcheckAnnotation: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip = %#v, want %#v", got, want)
	}
}

func TestBuildHealthProbesRejectsInvalidTest(t *testing.T) {
	for _, hc := range []*dockcontainer.HealthConfig{
		{Test: []string{"CMD"}},
		{Test: []string{"CMD-SHELL"}},
		{Test: []string{"BOGUS", "check"}},
		{Test: []string{"CMD", "check"}, Retries: -1},
	} {
		if _, _, _, err := buildHealthProbes(hc, false); err == nil {
			t.Fatalf("expected error for %#v", hc)
		}
	}
}
