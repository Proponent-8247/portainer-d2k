package adapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	dockertypes "github.com/docker/docker/api/types"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/portainer/d2k/internal/types"
)

const (
	swarmHealthLifecycleVersion = 1
	swarmTaskHistoryLimit       = 5
	swarmLifecycleStateDataKey  = "state.json"
	podDeletionCostAnnotation   = "controller.kubernetes.io/pod-deletion-cost"
)

var errCorruptSwarmHealthState = errors.New("corrupt d2k Swarm health lifecycle state")

type swarmHealthLifecycleState struct {
	Version           int                              `json:"version"`
	ServiceUID        string                           `json:"serviceUID"`
	ServiceGeneration int64                            `json:"serviceGeneration"`
	Slots             map[string]*swarmHealthSlotState `json:"slots"`
}

type swarmHealthSlotState struct {
	Slot                int                           `json:"slot"`
	CurrentPodUID       string                        `json:"currentPodUID,omitempty"`
	CurrentPodName      string                        `json:"currentPodName,omitempty"`
	CurrentContainerID  string                        `json:"currentContainerID,omitempty"`
	ActivationNotBefore int64                         `json:"activationNotBefore,omitempty"`
	RestartHistory      []int64                       `json:"restartHistory,omitempty"`
	Pending             *swarmHealthReplacementIntent `json:"pending,omitempty"`
	TaskHistory         []swarmHealthTaskRecord       `json:"taskHistory,omitempty"`
}

type swarmHealthReplacementIntent struct {
	FailedPodUID      string                   `json:"failedPodUID"`
	FailedPodName     string                   `json:"failedPodName"`
	FailedTaskID      string                   `json:"failedTaskID"`
	FailedContainerID string                   `json:"failedContainerID,omitempty"`
	FailedNodeName    string                   `json:"failedNodeName,omitempty"`
	FailedAt          int64                    `json:"failedAt"`
	DeleteCommittedAt int64                    `json:"deleteCommittedAt,omitempty"`
	NotBefore         int64                    `json:"notBefore,omitempty"`
	Error             string                   `json:"error,omitempty"`
	Policy            swarmHealthRestartPolicy `json:"policy"`
}

type swarmHealthTaskRecord struct {
	ID          string `json:"id"`
	ServiceID   string `json:"serviceID"`
	Slot        int    `json:"slot"`
	PodUID      string `json:"podUID"`
	PodName     string `json:"podName"`
	NodeName    string `json:"nodeName,omitempty"`
	NodeID      string `json:"nodeID,omitempty"`
	ContainerID string `json:"containerID,omitempty"`
	Image       string `json:"image,omitempty"`
	State       string `json:"state"`
	Error       string `json:"error,omitempty"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
}

func swarmLifecycleConfigMapName(deploymentName string) string {
	base := sanitiseResourceName(deploymentName)
	if len(base) > 40 {
		base = base[:40]
	}
	sum := sha256.Sum256([]byte(deploymentName))
	return "d2k-health-" + strings.Trim(base, "-") + "-" + hex.EncodeToString(sum[:4])
}

func newSwarmHealthLifecycleState(deployment appsv1.Deployment) *swarmHealthLifecycleState {
	return &swarmHealthLifecycleState{
		Version:           swarmHealthLifecycleVersion,
		ServiceUID:        string(deployment.UID),
		ServiceGeneration: deployment.Generation,
		Slots:             map[string]*swarmHealthSlotState{},
	}
}

func slotKey(slot int) string {
	return strconv.Itoa(slot)
}

func (s *swarmHealthLifecycleState) ensureSlot(slot int) *swarmHealthSlotState {
	if s.Slots == nil {
		s.Slots = map[string]*swarmHealthSlotState{}
	}
	key := slotKey(slot)
	current := s.Slots[key]
	if current == nil {
		current = &swarmHealthSlotState{Slot: slot}
		s.Slots[key] = current
	}
	return current
}

func (s *swarmHealthLifecycleState) slotForPodUID(uid string) *swarmHealthSlotState {
	if uid == "" {
		return nil
	}
	for _, slot := range s.Slots {
		if slot != nil && slot.CurrentPodUID == uid {
			return slot
		}
	}
	return nil
}

func (a *KubernetesDockerAdapter) loadSwarmHealthLifecycleState(
	ctx context.Context,
	deployment appsv1.Deployment,
) (*swarmHealthLifecycleState, *corev1.ConfigMap, error) {
	name := swarmLifecycleConfigMapName(deployment.Name)
	cm, err := a.client.CoreV1().ConfigMaps(a.namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return newSwarmHealthLifecycleState(deployment), nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	raw := cm.Data[swarmLifecycleStateDataKey]
	if strings.TrimSpace(raw) == "" {
		return nil, cm, fmt.Errorf("%w: ConfigMap %q has no %s", errCorruptSwarmHealthState, name, swarmLifecycleStateDataKey)
	}
	var state swarmHealthLifecycleState
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return nil, cm, fmt.Errorf("%w: ConfigMap %q: %v", errCorruptSwarmHealthState, name, err)
	}
	if state.Version != swarmHealthLifecycleVersion {
		return nil, cm, fmt.Errorf("%w: ConfigMap %q version %d is unsupported", errCorruptSwarmHealthState, name, state.Version)
	}
	if state.ServiceUID != "" && state.ServiceUID != string(deployment.UID) {
		// The Deployment name was reused for a different service identity. Old
		// slot/restart state must not leak into the new service.
		return newSwarmHealthLifecycleState(deployment), cm, nil
	}
	if state.Slots == nil {
		state.Slots = map[string]*swarmHealthSlotState{}
	}
	state.ServiceUID = string(deployment.UID)
	if state.ServiceGeneration != deployment.Generation {
		state.ServiceGeneration = deployment.Generation
		for _, slot := range state.Slots {
			if slot == nil {
				continue
			}
			slot.RestartHistory = nil
			slot.Pending = nil
			slot.ActivationNotBefore = 0
		}
	}
	return &state, cm, nil
}

func (a *KubernetesDockerAdapter) saveSwarmHealthLifecycleState(
	ctx context.Context,
	deployment appsv1.Deployment,
	state *swarmHealthLifecycleState,
	cm *corev1.ConfigMap,
) (*corev1.ConfigMap, error) {
	raw, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	name := swarmLifecycleConfigMapName(deployment.Name)
	if cm == nil {
		created, err := a.client.CoreV1().ConfigMaps(a.namespace).Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: a.namespace,
				Labels: map[string]string{
					types.LabelManagedBy:      types.LabelManagedByValue,
					types.LabelSwarmManagedBy: types.LabelSwarmManagedByValue,
					types.LabelSwarmService:   deployment.Name,
				},
			},
			Data: map[string]string{swarmLifecycleStateDataKey: string(raw)},
		}, metav1.CreateOptions{})
		return created, err
	}
	copyCM := cm.DeepCopy()
	if copyCM.Data == nil {
		copyCM.Data = map[string]string{}
	}
	copyCM.Data[swarmLifecycleStateDataKey] = string(raw)
	return a.client.CoreV1().ConfigMaps(a.namespace).Update(ctx, copyCM, metav1.UpdateOptions{})
}

func (a *KubernetesDockerAdapter) mutateSwarmHealthLifecycleState(
	ctx context.Context,
	deployment appsv1.Deployment,
	mutate func(*swarmHealthLifecycleState) (bool, error),
) (*swarmHealthLifecycleState, error) {
	for attempt := 0; attempt < 5; attempt++ {
		state, cm, err := a.loadSwarmHealthLifecycleState(ctx, deployment)
		if err != nil {
			return nil, err
		}
		changed, err := mutate(state)
		if err != nil {
			return nil, err
		}
		if !changed {
			return state, nil
		}
		if _, err := a.saveSwarmHealthLifecycleState(ctx, deployment, state, cm); err != nil {
			if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) {
				continue
			}
			return nil, err
		}
		return state, nil
	}
	return nil, fmt.Errorf("unable to update Swarm health lifecycle state for %q after conflicts", deployment.Name)
}

func desiredReplicaCount(deployment appsv1.Deployment) int {
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas < 0 {
		return 0
	}
	return int(*deployment.Spec.Replicas)
}

func parseSwarmSlotLabel(pod corev1.Pod) int {
	raw := pod.Labels[types.LabelSwarmSlot]
	slot, err := strconv.Atoi(raw)
	if err != nil || slot <= 0 {
		return 0
	}
	return slot
}

func targetContainerID(pod corev1.Pod, workloadName string) string {
	status, ok := containerStatusByName(pod, workloadName)
	if !ok {
		return ""
	}
	return status.ContainerID
}

func (a *KubernetesDockerAdapter) patchPodSwarmSlot(ctx context.Context, pod corev1.Pod, slot int) error {
	cost := strconv.Itoa(-slot)
	patch := map[string]any{
		"metadata": map[string]any{
			"labels": map[string]string{
				types.LabelSwarmSlot: strconv.Itoa(slot),
			},
			"annotations": map[string]string{
				podDeletionCostAnnotation: cost,
			},
		},
	}
	raw, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = a.client.CoreV1().Pods(a.namespace).Patch(ctx, pod.Name, k8stypes.MergePatchType, raw, metav1.PatchOptions{})
	return err
}

func (a *KubernetesDockerAdapter) ensureSwarmSlotAssignments(
	ctx context.Context,
	deployment appsv1.Deployment,
	pods []corev1.Pod,
) (*swarmHealthLifecycleState, error) {
	sort.Slice(pods, func(i, j int) bool {
		return pods[i].CreationTimestamp.Before(&pods[j].CreationTimestamp)
	})
	live := map[string]corev1.Pod{}
	for _, pod := range pods {
		if pod.DeletionTimestamp != nil {
			continue
		}
		live[string(pod.UID)] = pod
	}

	state, err := a.mutateSwarmHealthLifecycleState(ctx, deployment, func(state *swarmHealthLifecycleState) (bool, error) {
		changed := false
		desired := desiredReplicaCount(deployment)

		// Clear vanished current Pods. Pending health replacements keep their slot
		// reserved so the replacement inherits the failed task's slot.
		for _, slot := range state.Slots {
			if slot == nil || slot.CurrentPodUID == "" {
				continue
			}
			if _, ok := live[slot.CurrentPodUID]; ok {
				continue
			}
			slot.CurrentPodUID = ""
			slot.CurrentPodName = ""
			slot.CurrentContainerID = ""
			changed = true
		}

		assigned := map[string]int{}
		usedSlots := map[int]string{}

		// First preserve persisted slot ownership for surviving Pods.
		for _, slot := range state.Slots {
			if slot == nil || slot.CurrentPodUID == "" {
				continue
			}
			if _, ok := live[slot.CurrentPodUID]; !ok {
				continue
			}
			assigned[slot.CurrentPodUID] = slot.Slot
			usedSlots[slot.Slot] = slot.CurrentPodUID
		}

		// Next adopt valid on-Pod slot labels when they do not conflict with
		// persisted ownership. This survives d2k restarts before the ConfigMap
		// state has been reconstructed.
		for uid, pod := range live {
			if assigned[uid] != 0 {
				continue
			}
			slotNumber := parseSwarmSlotLabel(pod)
			if slotNumber <= 0 {
				continue
			}
			if owner := usedSlots[slotNumber]; owner != "" && owner != uid {
				continue
			}
			slot := state.ensureSlot(slotNumber)
			slot.CurrentPodUID = uid
			slot.CurrentPodName = pod.Name
			slot.CurrentContainerID = targetContainerID(pod, deploymentRuntimeStateKey(deployment))
			assigned[uid] = slotNumber
			usedSlots[slotNumber] = uid
			changed = true
		}

		// A pending health replacement gets first claim on the next unassigned
		// Pod so it inherits the failed task's stable Swarm slot.
		pendingSlots := make([]int, 0)
		for _, slot := range state.Slots {
			if slot != nil && slot.Pending != nil && slot.Pending.DeleteCommittedAt != 0 && slot.CurrentPodUID == "" {
				pendingSlots = append(pendingSlots, slot.Slot)
			}
		}
		sort.Ints(pendingSlots)
		for _, slotNumber := range pendingSlots {
			var podUID string
			for uid := range live {
				if assigned[uid] == 0 {
					podUID = uid
					break
				}
			}
			if podUID == "" {
				break
			}
			pod := live[podUID]
			slot := state.ensureSlot(slotNumber)
			slot.CurrentPodUID = podUID
			slot.CurrentPodName = pod.Name
			slot.CurrentContainerID = targetContainerID(pod, deploymentRuntimeStateKey(deployment))
			slot.ActivationNotBefore = slot.Pending.NotBefore
			slot.Pending = nil
			assigned[podUID] = slotNumber
			usedSlots[slotNumber] = podUID
			changed = true
		}

		// Assign scale-up / initially created Pods to the lowest vacant stable
		// slots. Existing Pods are never renumbered.
		nextVacant := func() int {
			upper := desired
			if len(live) > upper {
				upper = len(live)
			}
			for slot := 1; slot <= upper; slot++ {
				if usedSlots[slot] == "" {
					return slot
				}
			}
			return 0
		}
		for uid, pod := range live {
			if assigned[uid] != 0 {
				continue
			}
			slotNumber := nextVacant()
			if slotNumber == 0 {
				continue
			}
			slot := state.ensureSlot(slotNumber)
			slot.CurrentPodUID = uid
			slot.CurrentPodName = pod.Name
			slot.CurrentContainerID = targetContainerID(pod, deploymentRuntimeStateKey(deployment))
			assigned[uid] = slotNumber
			usedSlots[slotNumber] = uid
			changed = true
		}

		// Retire empty slots above the desired replica count once no restart or
		// historical state needs them.
		for key, slot := range state.Slots {
			if slot == nil {
				delete(state.Slots, key)
				changed = true
				continue
			}
			if slot.Slot > desired && slot.CurrentPodUID == "" && slot.Pending == nil && len(slot.TaskHistory) == 0 && len(slot.RestartHistory) == 0 {
				delete(state.Slots, key)
				changed = true
			}
		}
		return changed, nil
	})
	if err != nil {
		return nil, err
	}

	// Slot labels make task identity visible to API reads and Pod deletion cost
	// biases Deployment scale-down toward removing the highest slots first.
	for _, pod := range pods {
		slot := state.slotForPodUID(string(pod.UID))
		if slot == nil {
			continue
		}
		if parseSwarmSlotLabel(pod) == slot.Slot && pod.Annotations[podDeletionCostAnnotation] == strconv.Itoa(-slot.Slot) {
			continue
		}
		if err := a.patchPodSwarmSlot(ctx, pod, slot.Slot); err != nil && !apierrors.IsNotFound(err) {
			return state, err
		}
	}
	return state, nil
}

func pruneRestartHistory(history []int64, window time.Duration, now time.Time) []int64 {
	if window <= 0 {
		return history
	}
	cutoff := now.Add(-window).UnixNano()
	out := history[:0]
	for _, ts := range history {
		if ts > cutoff {
			out = append(out, ts)
		}
	}
	return out
}

func restartAllowedForSlot(slot *swarmHealthSlotState, policy swarmHealthRestartPolicy, now time.Time) bool {
	if policy.Condition == "none" {
		return false
	}
	if policy.MaxAttempts <= 0 {
		return true
	}
	slot.RestartHistory = pruneRestartHistory(slot.RestartHistory, policy.Window, now)
	return int64(len(slot.RestartHistory)) < policy.MaxAttempts
}

func appendTaskHistory(slot *swarmHealthSlotState, record swarmHealthTaskRecord) {
	slot.TaskHistory = append(slot.TaskHistory, record)
	if len(slot.TaskHistory) > swarmTaskHistoryLimit {
		slot.TaskHistory = append([]swarmHealthTaskRecord{}, slot.TaskHistory[len(slot.TaskHistory)-swarmTaskHistoryLimit:]...)
	}
}

func failedTaskRecord(
	deployment appsv1.Deployment,
	pod corev1.Pod,
	slot int,
	containerID string,
	now time.Time,
) swarmHealthTaskRecord {
	serviceID := deployment.Annotations[types.AnnotationSwarmServiceID]
	if serviceID == "" {
		serviceID = swarmID(string(deployment.UID))
	}
	return swarmHealthTaskRecord{
		ID:          swarmID(string(pod.UID)),
		ServiceID:   serviceID,
		Slot:        slot,
		PodUID:      string(pod.UID),
		PodName:     pod.Name,
		NodeName:    pod.Spec.NodeName,
		NodeID:      swarmID(string(pod.Spec.NodeName)),
		ContainerID: containerID,
		Image:       podImage(pod),
		State:       "failed",
		Error:       "container unhealthy",
		CreatedAt:   pod.CreationTimestamp.UTC().Format(time.RFC3339Nano),
		UpdatedAt:   now.UTC().Format(time.RFC3339Nano),
	}
}

func lifecycleTaskToSwarmTask(record swarmHealthTaskRecord) map[string]any {
	return map[string]any{
		"ID":        record.ID,
		"Version":   map[string]any{"Index": uint64(1)},
		"CreatedAt": record.CreatedAt,
		"UpdatedAt": record.UpdatedAt,
		"Spec": map[string]any{
			"ContainerSpec": map[string]any{
				"Image": record.Image,
			},
		},
		"ServiceID": record.ServiceID,
		"Slot":      record.Slot,
		"NodeID":    record.NodeID,
		"Status": map[string]any{
			"State":     record.State,
			"Message":   record.Error,
			"Err":       record.Error,
			"Timestamp": record.UpdatedAt,
			"ContainerStatus": map[string]any{
				"ContainerID": record.ContainerID,
				"PID":         0,
				"ExitCode":    0,
			},
		},
		"DesiredState": "shutdown",
	}
}

func (a *KubernetesDockerAdapter) deleteSwarmLifecycleState(ctx context.Context, deploymentName string) {
	_ = a.client.CoreV1().ConfigMaps(a.namespace).Delete(
		ctx,
		swarmLifecycleConfigMapName(deploymentName),
		metav1.DeleteOptions{},
	)
}

func (a *KubernetesDockerAdapter) cleanupOrphanSwarmLifecycleStates(ctx context.Context, active map[string]struct{}) {
	cms, err := a.client.CoreV1().ConfigMaps(a.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: types.LabelSwarmManagedBy + "=" + types.LabelSwarmManagedByValue,
	})
	if err != nil {
		return
	}
	for _, cm := range cms.Items {
		service := cm.Labels[types.LabelSwarmService]
		if service == "" {
			continue
		}
		if _, ok := active[service]; ok {
			continue
		}
		if strings.HasPrefix(cm.Name, "d2k-health-") {
			_ = a.client.CoreV1().ConfigMaps(a.namespace).Delete(ctx, cm.Name, metav1.DeleteOptions{})
		}
	}
}

func healthStateIsUnhealthy(health *dockertypes.Health) bool {
	return health != nil && health.Status == dockertypes.Unhealthy
}
