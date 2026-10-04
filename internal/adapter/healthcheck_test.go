package adapter

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	dockcontainer "github.com/docker/docker/api/types/container"

	"github.com/portainer/d2k/internal/types"
)

func TestValidateHealthcheckForMonitorAcceptsExplicitCMD(t *testing.T) {
	hc := &dockcontainer.HealthConfig{
		Test:     []string{"CMD", "curl", "-f", "http://localhost/health"},
		Interval: 5 * time.Second,
		Timeout:  2 * time.Second,
		Retries:  4,
	}
	warnings, err := validateHealthcheckForMonitor(hc)
	if err != nil {
		t.Fatalf("validateHealthcheckForMonitor: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}
}

func TestDockerHealthCommandCMDPreservesLiteralArgv(t *testing.T) {
	command, disabled, warning, err := dockerHealthCommand([]string{"CMD", "check", "$(FOO)", "$BAR"})
	if err != nil {
		t.Fatalf("dockerHealthCommand: %v", err)
	}
	if disabled || warning != "" {
		t.Fatalf("unexpected disabled/warning: %v %q", disabled, warning)
	}
	want := []string{"check", "$(FOO)", "$BAR"}
	if !reflect.DeepEqual(command, want) {
		t.Fatalf("command = %#v, want %#v", command, want)
	}
}

func TestDockerHealthCommandCMDShellPreservesArguments(t *testing.T) {
	command, disabled, warning, err := dockerHealthCommand([]string{"CMD-SHELL", "printf '%s' "$0"", "arg-zero"})
	if err != nil {
		t.Fatalf("dockerHealthCommand: %v", err)
	}
	if disabled || warning != "" {
		t.Fatalf("unexpected disabled/warning: %v %q", disabled, warning)
	}
	want := []string{"/bin/sh", "-c", "printf '%s' "$0"", "arg-zero"}
	if !reflect.DeepEqual(command, want) {
		t.Fatalf("command = %#v, want %#v", command, want)
	}
}

func TestValidateHealthcheckNoneDisablesMonitor(t *testing.T) {
	hc := &dockcontainer.HealthConfig{Test: []string{"NONE"}}
	warnings, err := validateHealthcheckForMonitor(hc)
	if err != nil {
		t.Fatalf("validateHealthcheckForMonitor: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("NONE returned warnings: %#v", warnings)
	}
	if healthcheckEnabled(hc) {
		t.Fatal("NONE must not enable health monitoring")
	}
}

func TestValidateHealthcheckInheritedWarns(t *testing.T) {
	warnings, err := validateHealthcheckForMonitor(&dockcontainer.HealthConfig{})
	if err != nil {
		t.Fatalf("validateHealthcheckForMonitor: %v", err)
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

func TestValidateHealthcheckMatchesDockerMinimumDurationRules(t *testing.T) {
	for _, hc := range []*dockcontainer.HealthConfig{
		{Test: []string{"CMD", "true"}, Interval: time.Nanosecond},
		{Test: []string{"CMD", "true"}, Timeout: time.Nanosecond},
		{Test: []string{"CMD", "true"}, StartPeriod: time.Nanosecond},
		{Test: []string{"CMD", "true"}, StartInterval: time.Nanosecond},
	} {
		if _, err := validateHealthcheckForMonitor(hc); err == nil {
			t.Fatalf("expected Docker minimum-duration error for %#v", hc)
		} else if !errors.Is(err, ErrInvalidHealthcheck) {
			t.Fatalf("error %v is not classified as invalid healthcheck", err)
		}
	}
}

func TestValidateHealthcheckRejectsInvalidCommandAndRetries(t *testing.T) {
	for _, hc := range []*dockcontainer.HealthConfig{
		{Test: []string{"CMD"}},
		{Test: []string{"CMD-SHELL"}},
		{Test: []string{"CMD", "check"}, Retries: -1},
	} {
		if _, err := validateHealthcheckForMonitor(hc); err == nil {
			t.Fatalf("expected error for %#v", hc)
		} else if !errors.Is(err, ErrInvalidHealthcheck) {
			t.Fatalf("error %v is not classified as invalid healthcheck", err)
		}
	}
}

func TestUnknownHealthcheckTypeIsPreservedButNotMonitored(t *testing.T) {
	hc := &dockcontainer.HealthConfig{Test: []string{"cmd", "true"}}
	warnings, err := validateHealthcheckForMonitor(hc)
	if err != nil {
		t.Fatalf("validateHealthcheckForMonitor: %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "unknown Docker healthcheck test type") {
		t.Fatalf("expected unknown-type warning, got %#v", warnings)
	}
	if healthcheckEnabled(hc) {
		t.Fatal("unknown Docker healthcheck type must not enable monitoring")
	}
}

func TestAnnotateHealthcheckRejectsOversizedPayloadWithoutMutation(t *testing.T) {
	annotations := map[string]string{
		"existing": "value",
	}
	hc := &dockcontainer.HealthConfig{
		Test: []string{"CMD", strings.Repeat("x", kubernetesAnnotationSizeLimitByte)},
	}

	err := annotateHealthcheck(annotations, hc)
	if err == nil {
		t.Fatal("expected oversized healthcheck annotation to be rejected")
	}
	if !errors.Is(err, ErrInvalidHealthcheck) {
		t.Fatalf("error %v is not classified as invalid healthcheck", err)
	}
	if _, exists := annotations[types.AnnotationHealthcheck]; exists {
		t.Fatal("oversized healthcheck mutated annotation map before returning error")
	}
	if annotations["existing"] != "value" {
		t.Fatalf("existing annotation changed: %#v", annotations)
	}
}

func TestAnnotateHealthcheckAccountsForExistingAnnotations(t *testing.T) {
	annotations := map[string]string{
		"existing": strings.Repeat("x", kubernetesAnnotationSizeLimitByte-128),
	}
	hc := &dockcontainer.HealthConfig{
		Test: []string{"CMD", strings.Repeat("y", 256)},
	}

	if err := annotateHealthcheck(annotations, hc); err == nil {
		t.Fatal("expected total annotation payload limit to be enforced")
	} else if !errors.Is(err, ErrInvalidHealthcheck) {
		t.Fatalf("error %v is not classified as invalid healthcheck", err)
	}
}
