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
	dockerHealthExecSetupTimeout   = 30 * time.Second
)

type healthCommandExecFunc func(
	ctx context.Context,
	podName string,
	containerName string,
	command []string,
	stdout io.Writer,
	stderr io.Writer,
) error

type healthMonitorRegistration struct {
	cancel      context.CancelFunc
	token       uint64
	podUID      string
	containerID string
}

type healthMonitorOwner struct {
	key         string
	token       uint64
	containerID string
}

type dockerHealthState struct {
	Health       dockertypes.Health
	StartedAt    time.Time
	UpdatedAt    time.Time
	MonitorKey   string
	MonitorToken uint64
	ContainerID  string
}

type dockerHealthMonitorSpec struct {
	Key                 string
	PodUID              string
	PodName             string
	ContainerName       string
	ContainerID         string
	DeploymentName      string
	StartedAt           time.Time
	Healthcheck         dockcontainer.HealthConfig
	Swarm               bool
	Slot                int
	ActivationNotBefore time.Time
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

func decodeSwarmHealthRestartPolicyStrict(raw string) (swarmHealthRestartPolicy, error) {
	policy := defaultSwarmHealthRestartPolicy()
	if raw == "" {
		return policy, nil
	}
	if err := json.Unmarshal([]byte(raw), &policy); err != nil {
		return swarmHealthRestartPolicy{}, fmt.Errorf("%w: invalid Swarm restart policy: %v", errCorruptSwarmHealthState, err)
	}
	switch policy.Condition {
	case "", "any":
		policy.Condition = "any"
	case "on-failure", "none":
	default:
		return swarmHealthRestartPolicy{}, fmt.Errorf("%w: invalid Swarm restart condition %q", errCorruptSwarmHealthState, policy.Condition)
	}
	if policy.Delay < 0 || policy.MaxAttempts < 0 || policy.Window < 0 {
		return swarmHealthRestartPolicy{}, fmt.Errorf("%w: invalid negative Swarm restart-policy value", errCorruptSwarmHealthState)
	}
	return policy, nil
}

func decodeSwarmHealthRestartPolicy(raw string) swarmHealthRestartPolicy {
	policy, err := decodeSwarmHealthRestartPolicyStrict(raw)
	if err != nil {
		return swarmHealthRestartPolicy{Condition: "none"}
	}
	return policy
}

// StartHealthManager starts d2k's Docker-compatible health monitor. Health
// management is single-active per namespace; a Kubernetes Lease prevents
// multiple d2k processes from running duplicate probes/replacements.
func (a *KubernetesDockerAdapter) StartHealthManager(parent context.Context) error {
	a.healthMu.Lock()
	if a.healthCancel != nil {
		a.healthMu.Unlock()
		return nil
	}
	a.healthMu.Unlock()

	if err := a.acquireHealthManagerLease(parent); err != nil {
		return err
	}

	a.healthMu.Lock()
	ctx, cancel := context.WithCancel(parent)
	a.healthCancel = cancel
	if a.healthStates == nil {
		a.healthStates = map[string]*dockerHealthState{}
	}
	if a.healthMonitors == nil {
		a.healthMonitors = map[string]*healthMonitorRegistration{}
	}
	if a.healthCurrent == nil {
		a.healthCurrent = map[string]healthMonitorOwner{}
	}
	a.healthMu.Unlock()

	go a.runHealthManagerLease(ctx, cancel)
	go a.runHealthManager(ctx)
	return nil
}

func (a *KubernetesDockerAdapter) StopHealthManager() {
	a.healthMu.Lock()
	cancel := a.healthCancel
	a.healthCancel = nil
	registrations := make([]*healthMonitorRegistration, 0, len(a.healthMonitors))
	for _, registration := range a.healthMonitors {
		registrations = append(registrations, registration)
	}
	a.healthMu.Unlock()

	if cancel != nil {
		cancel()
	}
	for _, registration := range registrations {
		if registration != nil && registration.cancel != nil {
			registration.cancel()
		}
	}
	releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer releaseCancel()
	a.releaseHealthManagerLease(releaseCtx)
}

func (a *KubernetesDockerAdapter) runHealthManager(ctx context.Context) {
	ticker := time.NewTicker(healthManagerReconcileInterval)
	defer ticker.Stop()

	for {
		if err := a.reconcileSwarmHealthLifecycle(ctx); err != nil && a.logger != nil {
			a.logger.Warnw("Swarm health lifecycle reconciliation failed", "error", err)
		}
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

		var lifecycle *swarmHealthLifecycleState
		isSwarm := deployment.Labels[types.LabelSwarmManagedBy] == types.LabelSwarmManagedByValue
		if isSwarm {
			lifecycle, _, _ = a.loadSwarmHealthLifecycleState(ctx, deployment)
		}

		for _, pod := range pods.Items {
			if pod.Status.Phase != corev1.PodRunning {
				continue
			}
			status, ok := containerStatusByName(pod, workloadName)
			if !ok || status.State.Running == nil || status.ContainerID == "" {
				continue
			}

			slotNumber := 0
			activationNotBefore := time.Time{}
			if isSwarm {
				slotNumber = parseSwarmSlotLabel(pod)
				if slotNumber <= 0 {
					// Slot assignment is the durable identity boundary for Swarm
					// health management. Do not start an unfenced task monitor.
					continue
				}
				if lifecycle != nil {
					if slot := lifecycle.Slots[slotKey(slotNumber)]; slot != nil && slot.ActivationNotBefore > 0 {
						activationNotBefore = time.Unix(0, slot.ActivationNotBefore)
					}
				}
			}

			key := string(pod.UID) + "/" + status.ContainerID
			desiredMonitors[key] = struct{}{}
			desiredPods[string(pod.UID)] = struct{}{}

			hcCopy := *hc
			hcCopy.Test = append([]string{}, hc.Test...)
			spec := dockerHealthMonitorSpec{
				Key:                 key,
				PodUID:              string(pod.UID),
				PodName:             pod.Name,
				ContainerName:       workloadName,
				ContainerID:         status.ContainerID,
				DeploymentName:      deployment.Name,
				StartedAt:           status.State.Running.StartedAt.Time,
				Healthcheck:         hcCopy,
				Swarm:               isSwarm,
				Slot:                slotNumber,
				ActivationNotBefore: activationNotBefore,
			}
			a.ensureHealthMonitor(ctx, spec)
		}
	}

	a.healthMu.Lock()
	for key, registration := range a.healthMonitors {
		if _, keep := desiredMonitors[key]; keep {
			continue
		}
		if registration != nil && registration.cancel != nil {
			registration.cancel()
		}
		// Ownership-aware deferred teardown removes the registration. Keeping it
		// here prevents a transient desired-set gap from launching an overlapping
		// monitor before the cancelled goroutine has actually exited.
	}
	for podUID := range a.healthStates {
		if _, keep := desiredPods[podUID]; keep {
			continue
		}
		if owner, ok := a.healthCurrent[podUID]; !ok || a.healthMonitors[owner.key] == nil {
			delete(a.healthStates, podUID)
			delete(a.healthCurrent, podUID)
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

	a.healthNextToken++
	token := a.healthNextToken
	if previous, ok := a.healthCurrent[spec.PodUID]; ok && previous.key != spec.Key {
		if registration := a.healthMonitors[previous.key]; registration != nil && registration.cancel != nil {
			registration.cancel()
		}
	}

	ctx, cancel := context.WithCancel(parent)
	a.healthMonitors[spec.Key] = &healthMonitorRegistration{
		cancel:      cancel,
		token:       token,
		podUID:      spec.PodUID,
		containerID: spec.ContainerID,
	}
	a.healthCurrent[spec.PodUID] = healthMonitorOwner{
		key:         spec.Key,
		token:       token,
		containerID: spec.ContainerID,
	}

	state := &dockerHealthState{
		Health: dockertypes.Health{
			Status:        dockertypes.Starting,
			FailingStreak: 0,
			Log:           []*dockertypes.HealthcheckResult{},
		},
		StartedAt:    spec.StartedAt,
		UpdatedAt:    time.Now(),
		MonitorKey:   spec.Key,
		MonitorToken: token,
		ContainerID:  spec.ContainerID,
	}
	a.healthStates[spec.PodUID] = cloneDockerHealthState(state)
	a.healthMu.Unlock()

	go func() {
		defer func() {
			a.healthMu.Lock()
			if current := a.healthMonitors[spec.Key]; current != nil && current.token == token {
				delete(a.healthMonitors, spec.Key)
			}
			if owner, ok := a.healthCurrent[spec.PodUID]; ok && owner.key == spec.Key && owner.token == token {
				delete(a.healthCurrent, spec.PodUID)
			}
			a.healthMu.Unlock()
		}()
		a.monitorDockerHealth(ctx, spec, token, state)
	}()
}

func (a *KubernetesDockerAdapter) monitorOwns(spec dockerHealthMonitorSpec, token uint64) bool {
	a.healthMu.RLock()
	defer a.healthMu.RUnlock()
	registration := a.healthMonitors[spec.Key]
	owner, ok := a.healthCurrent[spec.PodUID]
	return registration != nil &&
		registration.token == token &&
		registration.containerID == spec.ContainerID &&
		ok &&
		owner.key == spec.Key &&
		owner.token == token &&
		owner.containerID == spec.ContainerID
}

func (a *KubernetesDockerAdapter) monitorDockerHealth(
	ctx context.Context,
	spec dockerHealthMonitorSpec,
	token uint64,
	state *dockerHealthState,
) {
	if spec.Swarm {
		_ = a.setPodHealthConditionForMonitor(ctx, spec, token, corev1.ConditionFalse, "HealthStarting", "Docker healthcheck is starting")
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
		if ctx.Err() != nil || !a.monitorOwns(spec, token) {
			return
		}

		applyDockerHealthResult(state, &spec.Healthcheck, result)
		if !a.storeDockerHealthState(spec, token, state) {
			return
		}

		if spec.Swarm {
			switch state.Health.Status {
			case dockertypes.Healthy:
				if !spec.ActivationNotBefore.IsZero() && time.Now().Before(spec.ActivationNotBefore) {
					_ = a.setPodHealthConditionForMonitor(ctx, spec, token, corev1.ConditionFalse, "RestartDelay", "Swarm restart delay has not elapsed")
				} else {
					_ = a.setPodHealthConditionForMonitor(ctx, spec, token, corev1.ConditionTrue, "HealthHealthy", "Docker healthcheck is healthy")
				}
			case dockertypes.Unhealthy:
				_ = a.setPodHealthConditionForMonitor(ctx, spec, token, corev1.ConditionFalse, "HealthUnhealthy", "Docker healthcheck is unhealthy")
				// Replacement is owned by the durable lifecycle reconciler. Stop
				// probing this failed task but keep its final health state until
				// the Pod disappears or this monitor is cancelled.
				<-ctx.Done()
				return
			default:
				_ = a.setPodHealthConditionForMonitor(ctx, spec, token, corev1.ConditionFalse, "HealthStarting", "Docker healthcheck is starting")
			}
		}
	}
}

func nextDockerHealthInterval(hc *dockcontainer.HealthConfig, status string, startedAt, now time.Time) time.Duration {
	interval := hc.Interval
	if interval == 0 {
		interval = dockerDefaultHealthInterval
	}
	startPeriod := hc.StartPeriod
	if startPeriod <= 0 || now.Sub(startedAt) >= startPeriod || status != dockertypes.Starting {
		return interval
	}
	startInterval := hc.StartInterval
	if startInterval == 0 {
		startInterval = dockerDefaultStartInterval
	}
	// Moby chooses the start interval at the end of the preceding probe and
	// does not clamp the timer to the exact start-period boundary.
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

	probeTimeout := spec.Healthcheck.Timeout
	if probeTimeout == 0 {
		probeTimeout = dockerDefaultHealthTimeout
	}

	// Kubernetes remote exec does not expose Moby's process-start notification.
	// Give exec establishment its own 30s budget before the configured command
	// timeout. This avoids charging normal API/kubelet setup latency entirely
	// against Docker's probe timeout, while remaining bounded if setup wedges.
	totalBudget := dockerHealthExecSetupTimeout + probeTimeout
	probeCtx, cancel := context.WithTimeout(ctx, totalBudget)
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

	if errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
		result.ExitCode = -1
		if result.Output != "" {
			result.Output = fmt.Sprintf("Health check exceeded timeout (%v): %s", probeTimeout, result.Output)
		} else {
			result.Output = fmt.Sprintf("Health check exceeded timeout (%v)", probeTimeout)
		}
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
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *healthOutputBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limit > b.buf.Len() {
		remaining := b.limit - b.buf.Len()
		keep := len(p)
		if keep > remaining {
			keep = remaining
		}
		if keep > 0 {
			_, _ = b.buf.Write(p[:keep])
		}
		if keep < len(p) {
			b.truncated = true
		}
	} else if len(p) > 0 {
		b.truncated = true
	}
	return len(p), nil
}

func (b *healthOutputBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.buf.String()
	if b.truncated {
		out += "..."
	}
	return out
}

func (a *KubernetesDockerAdapter) storeDockerHealthState(
	spec dockerHealthMonitorSpec,
	token uint64,
	state *dockerHealthState,
) bool {
	a.healthMu.Lock()
	defer a.healthMu.Unlock()
	registration := a.healthMonitors[spec.Key]
	owner, ok := a.healthCurrent[spec.PodUID]
	if registration == nil || registration.token != token ||
		!ok || owner.key != spec.Key || owner.token != token || owner.containerID != spec.ContainerID {
		return false
	}
	if a.healthStates == nil {
		a.healthStates = map[string]*dockerHealthState{}
	}
	a.healthStates[spec.PodUID] = cloneDockerHealthState(state)
	return true
}

func (a *KubernetesDockerAdapter) healthStateForPod(pod corev1.Pod) *dockertypes.Health {
	target := swarmTargetContainerName(pod)
	if target == "" {
		return nil
	}
	status, ok := containerStatusByName(pod, target)
	if !ok || status.ContainerID == "" {
		return nil
	}

	a.healthMu.RLock()
	state := a.healthStates[string(pod.UID)]
	cloned := cloneDockerHealthState(state)
	a.healthMu.RUnlock()
	if cloned == nil || cloned.ContainerID != status.ContainerID {
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

func (a *KubernetesDockerAdapter) setPodHealthConditionForMonitor(
	ctx context.Context,
	spec dockerHealthMonitorSpec,
	token uint64,
	status corev1.ConditionStatus,
	reason string,
	message string,
) error {
	if !a.monitorOwns(spec, token) {
		return context.Canceled
	}
	pod, err := a.client.CoreV1().Pods(a.namespace).Get(ctx, spec.PodName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if string(pod.UID) != spec.PodUID {
		return context.Canceled
	}
	containerStatus, ok := containerStatusByName(*pod, spec.ContainerName)
	if !ok || containerStatus.ContainerID != spec.ContainerID {
		return context.Canceled
	}
	if !a.monitorOwns(spec, token) {
		return context.Canceled
	}
	return a.setPodHealthCondition(ctx, spec.PodName, status, reason, message)
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

func deploymentHealthEnabled(deployment appsv1.Deployment) bool {
	hc, err := decodeHealthcheckAnnotation(deployment.Annotations[types.AnnotationHealthcheck])
	return err == nil && healthcheckEnabled(hc)
}
