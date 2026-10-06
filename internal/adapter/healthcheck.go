package adapter

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	dockcontainer "github.com/docker/docker/api/types/container"

	"github.com/portainer/d2k/internal/types"
)

const (
	dockerDefaultHealthInterval       = 30 * time.Second
	dockerDefaultHealthTimeout        = 30 * time.Second
	dockerDefaultHealthRetries        = 3
	kubernetesAnnotationSizeLimitByte = 256 * 1024
)

// ErrInvalidHealthcheck identifies Docker health-check configuration that
// cannot be represented safely by d2k.
var ErrInvalidHealthcheck = errors.New("invalid Docker healthcheck")

func validateHealthcheckForMonitor(hc *dockcontainer.HealthConfig) ([]string, error) {
	if hc == nil {
		return nil, nil
	}
	if err := validateDockerHealthcheck(hc); err != nil {
		return nil, err
	}
	_, disabled, warning, err := dockerHealthCommand(hc.Test)
	if err != nil {
		return nil, err
	}
	if disabled {
		return nil, nil
	}

	if warning != "" {
		return []string{warning}, nil
	}
	return nil, nil
}

func validateHealthcheckForCreate(hc *dockcontainer.HealthConfig) ([]string, error) {
	warnings, err := validateHealthcheckForMonitor(hc)
	if err != nil {
		return nil, err
	}
	if hc == nil {
		warnings = append(warnings,
			"d2k does not inspect image metadata: an image-defined HEALTHCHECK, if present, cannot be inherited when the create request omits Healthcheck",
		)
	}
	return warnings, nil
}

func healthcheckEnabled(hc *dockcontainer.HealthConfig) bool {
	if hc == nil || len(hc.Test) == 0 {
		return false
	}
	return hc.Test[0] == "CMD" || hc.Test[0] == "CMD-SHELL"
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
		return nil, false,
			fmt.Sprintf("unknown Docker healthcheck test type %q is preserved for API readback but not translated", test[0]),
			nil
	}
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

	// Kubernetes limits the total size of all annotation keys and values on an
	// object to 256 KiB. Validate before mutating the caller's map so an
	// oversized Docker health command is rejected as bad input instead of
	// surfacing later as an opaque Kubernetes deployment error.
	total := len(types.AnnotationHealthcheck) + len(raw)
	for k, v := range annotations {
		if k == types.AnnotationHealthcheck {
			continue
		}
		total += len(k) + len(v)
	}
	if total > kubernetesAnnotationSizeLimitByte {
		return invalidHealthcheckf(
			"encoded healthcheck annotations require %d bytes, exceeding Kubernetes maximum %d bytes",
			total,
			kubernetesAnnotationSizeLimitByte,
		)
	}

	annotations[types.AnnotationHealthcheck] = raw
	return nil
}
