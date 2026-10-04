package adapter

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	dockcontainer "github.com/docker/docker/api/types/container"
	corev1 "k8s.io/api/core/v1"

	"github.com/portainer/d2k/internal/types"
)

const (
	dockerDefaultHealthInterval = 30 * time.Second
	dockerDefaultHealthTimeout  = 30 * time.Second
	dockerDefaultHealthRetries  = 3
)

// ErrInvalidHealthcheck identifies Docker health-check configuration that
// cannot be represented safely by d2k.
var ErrInvalidHealthcheck = errors.New("invalid Docker healthcheck")

// buildHealthProbes translates an explicit Docker healthcheck into Kubernetes
// exec probes. Standalone containers get readiness only because Docker Engine
// health status does not itself restart a container. Swarm services also get a
// liveness probe because Swarm replaces tasks that fail health checks.
func buildHealthProbes(hc *dockcontainer.HealthConfig, swarm bool) (readiness, liveness *corev1.Probe, warnings []string, err error) {
	if hc == nil {
		return nil, nil, nil, nil
	}
	if err := validateDockerHealthcheck(hc); err != nil {
		return nil, nil, nil, err
	}

	command, disabled, commandWarning, err := dockerHealthCommand(hc.Test)
	if err != nil {
		return nil, nil, nil, err
	}
	if commandWarning != "" {
		warnings = append(warnings, commandWarning)
	}
	if disabled || command == nil {
		return nil, nil, warnings, nil
	}

	period, err := probeSeconds("health interval", hc.Interval, dockerDefaultHealthInterval, false)
	if err != nil {
		return nil, nil, nil, err
	}
	timeout, err := probeSeconds("health timeout", hc.Timeout, dockerDefaultHealthTimeout, false)
	if err != nil {
		return nil, nil, nil, err
	}
	startDelay, err := probeSeconds("health start period", hc.StartPeriod, 0, true)
	if err != nil {
		return nil, nil, nil, err
	}
	initialDelay := period
	if hc.StartPeriod > 0 {
		initialDelay = startDelay
	}

	retries := hc.Retries
	if retries == 0 {
		retries = dockerDefaultHealthRetries
	}
	if retries > math.MaxInt32 {
		return nil, nil, nil, invalidHealthcheckf("health retries exceed Kubernetes maximum %d", math.MaxInt32)
	}

	if hc.StartPeriod > 0 {
		warnings = append(warnings,
			"Docker health start_period is approximated with Kubernetes initialDelaySeconds; checks do not run during the delay")
	}
	if hc.StartPeriod > 0 && hc.StartInterval > 0 {
		warnings = append(warnings,
			"Docker health start_interval has no exact Kubernetes probe equivalent and is retained for API readback only")
	}

	readiness = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{Command: command},
		},
		InitialDelaySeconds: initialDelay,
		PeriodSeconds:       period,
		TimeoutSeconds:      timeout,
		FailureThreshold:    int32(retries),
		SuccessThreshold:    1,
	}

	if swarm {
		liveness = readiness.DeepCopy()
	}
	return readiness, liveness, warnings, nil
}

func healthcheckEnabled(hc *dockcontainer.HealthConfig) bool {
	if hc == nil || len(hc.Test) == 0 {
		return false
	}
	return hc.Test[0] != "NONE"
}

func validateDockerHealthcheck(hc *dockcontainer.HealthConfig) error {
	for _, field := range []struct {
		name  string
		value time.Duration
	}{
		{name: "Interval", value: hc.Interval},
		{name: "Timeout", value: hc.Timeout},
		{name: "StartPeriod", value: hc.StartPeriod},
		{name: "StartInterval", value: hc.StartInterval},
	} {
		if field.value != 0 && field.value < dockcontainer.MinimumDuration {
			return invalidHealthcheckf("%s cannot be less than %s", field.name, dockcontainer.MinimumDuration)
		}
	}
	if hc.Retries < 0 {
		return invalidHealthcheckf("Retries cannot be negative")
	}
	return nil
}

func invalidHealthcheckf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidHealthcheck, fmt.Sprintf(format, args...))
}

func dockerHealthCommand(test []string) (command []string, disabled bool, warning string, err error) {
	if len(test) == 0 {
		return nil, false,
			"image-inherited HEALTHCHECK cannot be translated because d2k does not inspect image metadata",
			nil
	}

	switch test[0] {
	case "NONE":
		return nil, true, "", nil
	case "CMD":
		if len(test) < 2 {
			return nil, false, "", invalidHealthcheckf("Docker healthcheck CMD requires at least one argument")
		}
		command = append([]string{}, test[1:]...)
		return command, false, "", nil
	case "CMD-SHELL":
		if len(test) < 2 {
			return nil, false, "", invalidHealthcheckf("Docker healthcheck CMD-SHELL requires a command")
		}
		command := []string{"/bin/sh", "-c"}
		command = append(command, test[1:]...)
		return command, false, "", nil
	default:
		return nil, false, "", invalidHealthcheckf("unsupported Docker healthcheck test form %q", test[0])
	}
}

func probeSeconds(field string, value, defaultValue time.Duration, allowZero bool) (int32, error) {
	if value < 0 {
		return 0, invalidHealthcheckf("%s cannot be negative", field)
	}
	if value == 0 {
		value = defaultValue
	}
	if value == 0 && allowZero {
		return 0, nil
	}
	if value <= 0 {
		return 0, invalidHealthcheckf("%s must be positive", field)
	}

	seconds := int64(value / time.Second)
	if value%time.Second != 0 {
		seconds++
	}
	if seconds < 1 {
		seconds = 1
	}
	if seconds > math.MaxInt32 {
		return 0, invalidHealthcheckf("%s exceeds Kubernetes maximum probe duration", field)
	}
	return int32(seconds), nil
}

func encodeHealthcheckAnnotation(hc *dockcontainer.HealthConfig) (string, error) {
	if hc == nil {
		return "", nil
	}
	raw, err := json.Marshal(hc)
	if err != nil {
		return "", fmt.Errorf("unable to encode Docker healthcheck: %w", err)
	}
	return string(raw), nil
}

func decodeHealthcheckAnnotation(raw string) (*dockcontainer.HealthConfig, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var hc dockcontainer.HealthConfig
	if err := json.Unmarshal([]byte(raw), &hc); err != nil {
		return nil, fmt.Errorf("invalid stored Docker healthcheck: %w", err)
	}
	return &hc, nil
}

func annotateHealthcheck(annotations map[string]string, hc *dockcontainer.HealthConfig) error {
	if hc == nil {
		delete(annotations, types.AnnotationHealthcheck)
		return nil
	}
	raw, err := encodeHealthcheckAnnotation(hc)
	if err != nil {
		return err
	}
	annotations[types.AnnotationHealthcheck] = raw
	return nil
}
