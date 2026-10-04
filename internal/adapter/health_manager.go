package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	dockertypes "github.com/docker/docker/api/types"
	dockcontainer "github.com/docker/docker/api/types/container"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilexec "k8s.io/client-go/util/exec"

	"github.com/portainer/d2k/internal/types"
)

const (
	healthManagerReconcileInterval = time.Second
	dockerDefaultStartInterval     = 5 * time.Second
	dockerHealthMaxLogEntries      = 5
	dockerHealthMaxOutputBytes     = 4096
	dockerDefaultSwarmRestartDelay = 5 * time.Second
)

type healthCommandExecFunc func(
	ctx context.Context,
	podName string,
	containerName string,
	command []string,
	stdout io.Writer,
	stderr io.Writer,
) error

type dockerHealthState struct {
	Health    dockertypes.Health
	StartedAt time.Time
	UpdatedAt time.Time
}

type dockerHealthMonitorSpec struct {
	Key            string
	PodUID         string
	PodName        string
	ContainerName  string
	ContainerID    string
	DeploymentName string
	StartedAt      time.Time
	Healthcheck    dockcontainer.HealthConfig
	Swarm          bool
	RestartPolicy  swarmHealthRestartPolicy
}

type swarmHealthRestartPolicy struct {
	Condition   string        `json:"condition"`
	Delay       time.Duration `json:"delay"`
	MaxAttempts int64         `json:"maxAttempts"`
	Window      time.Duration `json:"window"`
}

func defaultSwarmHealthRestartPolicy() swarmHealthRestartPolicy {
	return swarmHealthRestartPolicy{
		Condition: "any",
		Delay:     dockerDefaultSwarmRestartDelay,
	}
}

func decodeSwarmHealthRestartPolicy(raw string) swarmHealthRestartPolicy {
	policy := defaultSwarmHealthRestartPolicy()
	if raw == "" {
		return policy
	}
	if err := json.Unmarshal([]byte(raw), &policy); err != nil {
		return defaultSwarmHealthRestartPolicy()
	}
	if policy.Condition == "" {
		policy.Condition = "any"
	}
	if policy.Delay < 0 {
		policy.Delay = 0
	}
	if policy.MaxAttempts < 0 {
		policy.MaxAttempts = 0
	}
	if policy.Window < 0 {
		policy.Window = 0
	}
	return policy
}

// StartHealthManager starts d2k's Docker-compatible health monitor. It is kept
// separate from adapter construction so unit tests using fake clients do not
// accidentally start background pod-exec loops.
func (a *KubernetesDockerAdapter) StartHealthManager(parent context.Context) {
	a.healthMu.Lock()
	if a.healthCancel != nil {
		a.healthMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	a.healthCancel = cancel
	if a.healthStates == nil {
		a.healthStates = map[string]*dockerHealthState{}
	}
	if a.healthMonitors == nil {
		a.healthMonitors = map[string]context.CancelFunc{}
	}
	a.healthMu.Unlock()

	go a.runHealthManager(ctx)
}

func (a *KubernetesDockerAdapter) StopHealthManager() {
	a.healthMu.Lock()
	cancel := a.healthCancel
	a.healthCancel = nil
	a.healthMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (a *KubernetesDockerAdapter) runHealthManager(ctx context.Context) {
	ticker := time.NewTicker(healthManagerReconcileInterval)
	defer ticker.Stop()

	for {
		if err := a.reconcileHealthMonitors(ctx); err != nil && a.logger != nil {
			a.logger.Warnw("health monitor reconciliation failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *KubernetesDockerAdapter) reconcileHealthMonitors(ctx context.Context) error {
	deployments, err := a.client.AppsV1().Deployments(a.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: types.LabelManagedBy + "=" + types.LabelManagedByValue,
	})
	if err != nil {
		return fmt.Errorf("unable to list healthchecked deployments: %w", err)
	}

	desiredMonitors := map[string]struct{}{}
	desiredPods := map[string]struct{}{}

	for _, deployment := range deployments.Items {
		hc, err := decodeHealthcheckAnnotation(deployment.Annotations[types.AnnotationHealthcheck])
		if err != nil || !healthcheckEnabled(hc) {
			continue
		}
		if deployment.Spec.Replicas != nil && *deployment.Spec.Replicas == 0 {
			continue
		}

		workloadName := deploymentRuntimeStateKey(deployment)
		pods, err := a.client.CoreV1().Pods(a.namespace).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("%s=%s,app=%s", types.LabelManagedBy, types.LabelManagedByValue, workloadName),
		})
		if err != nil {
			return fmt.Errorf("unable to list pods for healthchecked deployment %q: %w", deployment.Name, err)
		}

		for _, pod := range pods.Items {
			if pod.Status.Phase != corev1.PodRunning {
				continue
			}
			status, ok := containerStatusByName(pod, workloadName)
			if !ok || status.State.Running == nil || status.ContainerID == "" {
				continue
			}

			key := string(pod.UID) + "/" + status.ContainerID
			desiredMonitors[key] = struct{}{}
			desiredPods[string(pod.UID)] = struct{}{}

			hcCopy := *hc
			hcCopy.Test = append([]string{}, hc.Test...)
			spec := dockerHealthMonitorSpec{
				Key:            key,
				PodUID:         string(pod.UID),
				PodName:        pod.Name,
				ContainerName:  workloadName,
				ContainerID:    status.ContainerID,
				DeploymentName: deployment.Name,
				StartedAt:      status.State.Running.StartedAt.Time,
				Healthcheck:    hcCopy,
				Swarm:          deployment.Labels[types.LabelSwarmManagedBy] == types.LabelSwarmManagedByValue,
				RestartPolicy:  decodeSwarmHealthRestartPolicy(deployment.Annotations[types.AnnotationSwarmRestartPolicy]),
			}
			a.ensureHealthMonitor(ctx, spec)
		}
	}

	a.healthMu.Lock()
	for key, cancel := range a.healthMonitors {
		if _, keep := desiredMonitors[key]; keep {
			continue
		}
		cancel()
		delete(a.healthMonitors, key)
	}
	for podUID := range a.healthStates {
		if _, keep := desiredPods[podUID]; !keep {
			delete(a.healthStates, podUID)
		}
	}
	a.healthMu.Unlock()

	return nil
}

func (a *KubernetesDockerAdapter) ensureHealthMonitor(parent context.Context, spec dockerHealthMonitorSpec) {
	a.healthMu.Lock()
	if _, exists := a.healthMonitors[spec.Key]; exists {
		a.healthMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	a.healthMonitors[spec.Key] = cancel
	state := &dockerHealthState{
		Health: dockertypes.Health{
			Status:        dockertypes.Starting,
			FailingStreak: 0,
			Log:           []*dockertypes.HealthcheckResult{},
		},
		StartedAt: spec.StartedAt,
		UpdatedAt: time.Now(),
	}
	a.healthStates[spec.PodUID] = cloneDockerHealthState(state)
	a.healthMu.Unlock()

	go func() {
		defer func() {
			a.healthMu.Lock()
			if current, ok := a.healthMonitors[spec.Key]; ok && current != nil {
				delete(a.healthMonitors, spec.Key)
			}
			a.healthMu.Unlock()
		}()
		a.monitorDockerHealth(ctx, spec, state)
	}()
}

func (a *KubernetesDockerAdapter) monitorDockerHealth(ctx context.Context, spec dockerHealthMonitorSpec, state *dockerHealthState) {
	if spec.Swarm {
		_ = a.setPodHealthCondition(ctx, spec.PodName, corev1.ConditionFalse, "HealthStarting", "Docker healthcheck is starting")
	}

	for {
		wait := nextDockerHealthInterval(&spec.Healthcheck, state.Health.Status, spec.StartedAt, time.Now())
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		result := a.runDockerHealthCommand(ctx, spec)
		applyDockerHealthResult(state, &spec.Healthcheck, result)
		a.storeDockerHealthState(spec.PodUID, state)

		if spec.Swarm {
			switch state.Health.Status {
			case dockertypes.Healthy:
				_ = a.setPodHealthCondition(ctx, spec.PodName, corev1.ConditionTrue, "HealthHealthy", "Docker healthcheck is healthy")
			case dockertypes.Unhealthy:
				_ = a.setPodHealthCondition(ctx, spec.PodName, corev1.ConditionFalse, "HealthUnhealthy", "Docker healthcheck is unhealthy")
				a.handleSwarmHealthFailure(ctx, spec)
				// A Swarm unhealthy task is terminal. Keep the final state available
				// until the pod disappears or this monitor is cancelled, but do not
				// allow a later probe to resurrect the failed task.
				<-ctx.Done()
				return
			default:
				_ = a.setPodHealthCondition(ctx, spec.PodName, corev1.ConditionFalse, "HealthStarting", "Docker healthcheck is starting")
			}
		}
	}
}

func nextDockerHealthInterval(hc *dockcontainer.HealthConfig, status dockcontainer.HealthStatus, startedAt, now time.Time) time.Duration {
	interval := hc.Interval
	if interval == 0 {
		interval = dockerDefaultHealthInterval
	}
	startPeriod := hc.StartPeriod
	if startPeriod <= 0 || status != dockertypes.Starting {
		return interval
	}

	elapsed := now.Sub(startedAt)
	if elapsed >= startPeriod {
		return interval
	}

	startInterval := hc.StartInterval
	if startInterval == 0 {
		startInterval = dockerDefaultStartInterval
	}
	remaining := startPeriod - elapsed
	if startInterval > remaining {
		return remaining
	}
	return startInterval
}

func applyDockerHealthResult(state *dockerHealthState, hc *dockcontainer.HealthConfig, result *dockertypes.HealthcheckResult) {
	if len(state.Health.Log) >= dockerHealthMaxLogEntries {
		copy(state.Health.Log, state.Health.Log[len(state.Health.Log)+1-dockerHealthMaxLogEntries:])
		state.Health.Log = state.Health.Log[:dockerHealthMaxLogEntries-1]
	}
	state.Health.Log = append(state.Health.Log, result)

	if result.ExitCode == 0 {
		state.Health.FailingStreak = 0
		state.Health.Status = dockertypes.Healthy
		state.UpdatedAt = result.End
		return
	}

	increment := true
	if state.Health.Status == dockertypes.Starting && hc.StartPeriod > 0 {
		if result.Start.Sub(state.StartedAt) < hc.StartPeriod {
			increment = false
		}
	}
	if increment {
		state.Health.FailingStreak++
		retries := hc.Retries
		if retries <= 0 {
			retries = dockerDefaultHealthRetries
		}
		if state.Health.FailingStreak >= retries {
			state.Health.Status = dockertypes.Unhealthy
		}
	}
	state.UpdatedAt = result.End
}

func (a *KubernetesDockerAdapter) runDockerHealthCommand(ctx context.Context, spec dockerHealthMonitorSpec) *dockertypes.HealthcheckResult {
	start := time.Now()
	result := &dockertypes.HealthcheckResult{Start: start}

	command, disabled, _, err := dockerHealthCommand(spec.Healthcheck.Test)
	if err != nil || disabled || len(command) == 0 {
		result.End = time.Now()
		result.ExitCode = -1
		if err != nil {
			result.Output = err.Error()
		} else {
			result.Output = "healthcheck command is unavailable"
		}
		return result
	}

	timeout := spec.Healthcheck.Timeout
	if timeout == 0 {
		timeout = dockerDefaultHealthTimeout
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	output := &healthOutputBuffer{limit: dockerHealthMaxOutputBytes}
	if a.healthExec != nil {
		err = a.healthExec(probeCtx, spec.PodName, spec.ContainerName, command, output, output)
	} else {
		err = a.execInPod(probeCtx, spec.PodName, spec.ContainerName, command, nil, output, output, false)
	}
	result.End = time.Now()
	result.Output = output.String()
	if err == nil {
		result.ExitCode = 0
		return result
	}

	result.ExitCode = -1
	var exitErr utilexec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitStatus()
	}
	if result.Output == "" {
		result.Output = err.Error()
	}
	return result
}

type healthOutputBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
}

func (b *healthOutputBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limit > b.buf.Len() {
		remaining := b.limit - b.buf.Len()
		if len(p) > remaining {
			_, _ = b.buf.Write(p[:remaining])
		} else {
			_, _ = b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *healthOutputBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (a *KubernetesDockerAdapter) storeDockerHealthState(podUID string, state *dockerHealthState) {
	a.healthMu.Lock()
	if a.healthStates == nil {
		a.healthStates = map[string]*dockerHealthState{}
	}
	a.healthStates[podUID] = cloneDockerHealthState(state)
	a.healthMu.Unlock()
}

func (a *KubernetesDockerAdapter) healthStateForPod(pod corev1.Pod) *dockertypes.Health {
	a.healthMu.RLock()
	state := a.healthStates[string(pod.UID)]
	cloned := cloneDockerHealthState(state)
	a.healthMu.RUnlock()
	if cloned == nil {
		return nil
	}
	return &cloned.Health
}

func cloneDockerHealthState(state *dockerHealthState) *dockerHealthState {
	if state == nil {
		return nil
	}
	copyState := *state
	copyState.Health.Log = make([]*dockertypes.HealthcheckResult, 0, len(state.Health.Log))
	for _, entry := range state.Health.Log {
		if entry == nil {
			copyState.Health.Log = append(copyState.Health.Log, nil)
			continue
		}
		entryCopy := *entry
		copyState.Health.Log = append(copyState.Health.Log, &entryCopy)
	}
	return &copyState
}

func containerStatusByName(pod corev1.Pod, name string) (*corev1.ContainerStatus, bool) {
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == name {
			return &pod.Status.ContainerStatuses[i], true
		}
	}
	return nil, false
}

func podHealthConditionReady(pod corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodConditionType(types.HealthReadinessGate) {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func (a *KubernetesDockerAdapter) setPodHealthCondition(
	ctx context.Context,
	podName string,
	status corev1.ConditionStatus,
	reason string,
	message string,
) error {
	for attempt := 0; attempt < 5; attempt++ {
		pod, err := a.client.CoreV1().Pods(a.namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return err
		}

		now := metav1.Now()
		found := false
		changed := false
		for i := range pod.Status.Conditions {
			condition := &pod.Status.Conditions[i]
			if condition.Type != corev1.PodConditionType(types.HealthReadinessGate) {
				continue
			}
			found = true
			if condition.Status != status || condition.Reason != reason || condition.Message != message {
				if condition.Status != status {
					condition.LastTransitionTime = now
				}
				condition.Status = status
				condition.Reason = reason
				condition.Message = message
				condition.LastProbeTime = now
				changed = true
			}
			break
		}
		if !found {
			pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
				Type:               corev1.PodConditionType(types.HealthReadinessGate),
				Status:             status,
				LastProbeTime:      now,
				LastTransitionTime: now,
				Reason:             reason,
				Message:            message,
			})
			changed = true
		}
		if !changed {
			return nil
		}

		if _, err := a.client.CoreV1().Pods(a.namespace).UpdateStatus(ctx, pod, metav1.UpdateOptions{}); err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			return err
		}
		return nil
	}
	return fmt.Errorf("unable to update health readiness condition for pod %q after conflicts", podName)
}

func (a *KubernetesDockerAdapter) handleSwarmHealthFailure(ctx context.Context, spec dockerHealthMonitorSpec) {
	if spec.RestartPolicy.Condition == "none" {
		return
	}

	allowed, err := a.reserveHealthRestart(ctx, spec.DeploymentName, spec.RestartPolicy)
	if err != nil {
		if a.logger != nil {
			a.logger.Warnw("unable to reserve Swarm health restart", "service", spec.DeploymentName, "pod", spec.PodName, "error", err)
		}
		return
	}
	if !allowed {
		return
	}

	delay := spec.RestartPolicy.Delay
	if delay < 0 {
		delay = 0
	}
	if delay > 0 {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}

	if err := a.client.CoreV1().Pods(a.namespace).Delete(ctx, spec.PodName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		if a.logger != nil {
			a.logger.Warnw("unable to replace unhealthy Swarm task", "service", spec.DeploymentName, "pod", spec.PodName, "error", err)
		}
	}
}

func (a *KubernetesDockerAdapter) reserveHealthRestart(
	ctx context.Context,
	deploymentName string,
	policy swarmHealthRestartPolicy,
) (bool, error) {
	if policy.Condition == "none" {
		return false, nil
	}

	for attempt := 0; attempt < 5; attempt++ {
		deployment, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, deploymentName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if deployment.Annotations == nil {
			deployment.Annotations = map[string]string{}
		}

		now := time.Now()
		var history []int64
		if raw := deployment.Annotations[types.AnnotationHealthRestartHistory]; raw != "" {
			_ = json.Unmarshal([]byte(raw), &history)
		}
		if policy.Window > 0 {
			cutoff := now.Add(-policy.Window).UnixNano()
			filtered := history[:0]
			for _, ts := range history {
				if ts > cutoff {
					filtered = append(filtered, ts)
				}
			}
			history = filtered
		}
		if policy.MaxAttempts > 0 && int64(len(history)) >= policy.MaxAttempts {
			return false, nil
		}

		history = append(history, now.UnixNano())
		raw, err := json.Marshal(history)
		if err != nil {
			return false, err
		}
		deployment.Annotations[types.AnnotationHealthRestartHistory] = string(raw)

		if _, err := a.client.AppsV1().Deployments(a.namespace).Update(ctx, deployment, metav1.UpdateOptions{}); err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			return false, err
		}
		return true, nil
	}

	return false, fmt.Errorf("unable to reserve health restart for service %q after conflicts", deploymentName)
}

func deploymentHealthEnabled(deployment appsv1.Deployment) bool {
	hc, err := decodeHealthcheckAnnotation(deployment.Annotations[types.AnnotationHealthcheck])
	return err == nil && healthcheckEnabled(hc)
}
