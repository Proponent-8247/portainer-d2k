package adapter

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	dockertypes "github.com/docker/docker/api/types"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/portainer/d2k/internal/types"
)

func testSwarmDeployment(name string, replicas int32) appsv1.Deployment {
	return appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  "healthcheck-test",
			UID:        k8stypes.UID("deployment-" + name),
			Generation: 1,
			Labels: map[string]string{
				types.LabelManagedBy:      types.LabelManagedByValue,
				types.LabelSwarmManagedBy: types.LabelSwarmManagedByValue,
				types.LabelSwarmService:   name,
			},
			Annotations: map[string]string{},
		},
		Spec: appsv1.DeploymentSpec{Replicas: &replicas},
	}
}

func testSwarmPod(name, uid, service, containerID string, created time.Time) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "healthcheck-test",
			UID:               k8stypes.UID(uid),
			CreationTimestamp: metav1.NewTime(created),
			Labels: map[string]string{
				types.LabelManagedBy:      types.LabelManagedByValue,
				types.LabelSwarmManagedBy: types.LabelSwarmManagedByValue,
				types.LabelSwarmService:   service,
				types.LabelWorkloadName:   service,
				"app":                    service,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: service, Image: "busybox"}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:        service,
				ContainerID: containerID,
				Ready:       true,
				State: corev1.ContainerState{
					Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(created)},
				},
			}},
		},
	}
}

func TestSwarmSlotsRemainStableAcrossReplacement(t *testing.T) {
	ctx := context.Background()
	a := newHealthcheckTestAdapter()
	dep := testSwarmDeployment("web", 3)
	base := time.Now().Add(-time.Minute)
	pods := []corev1.Pod{
		testSwarmPod("web-a", "uid-a", "web", "containerd://a", base),
		testSwarmPod("web-b", "uid-b", "web", "containerd://b", base.Add(time.Second)),
		testSwarmPod("web-c", "uid-c", "web", "containerd://c", base.Add(2*time.Second)),
	}
	for i := range pods {
		if _, err := a.client.CoreV1().Pods(a.namespace).Create(ctx, pods[i].DeepCopy(), metav1.CreateOptions{}); err != nil {
			t.Fatalf("create pod: %v", err)
		}
	}

	state, err := a.ensureSwarmSlotAssignments(ctx, dep, pods)
	if err != nil {
		t.Fatalf("initial slot assignment: %v", err)
	}
	if state.slotForPodUID("uid-a").Slot != 1 || state.slotForPodUID("uid-b").Slot != 2 || state.slotForPodUID("uid-c").Slot != 3 {
		t.Fatalf("unexpected initial slots: %#v", state.Slots)
	}

	if err := a.client.CoreV1().Pods(a.namespace).Delete(ctx, "web-a", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete old pod: %v", err)
	}
	replacement := testSwarmPod("web-d", "uid-d", "web", "containerd://d", base.Add(3*time.Second))
	if _, err := a.client.CoreV1().Pods(a.namespace).Create(ctx, replacement.DeepCopy(), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create replacement: %v", err)
	}
	current, err := a.client.CoreV1().Pods(a.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	state, err = a.ensureSwarmSlotAssignments(ctx, dep, current.Items)
	if err != nil {
		t.Fatalf("replacement slot assignment: %v", err)
	}
	if got := state.slotForPodUID("uid-b").Slot; got != 2 {
		t.Fatalf("surviving pod B renumbered to slot %d", got)
	}
	if got := state.slotForPodUID("uid-c").Slot; got != 3 {
		t.Fatalf("surviving pod C renumbered to slot %d", got)
	}
	if got := state.slotForPodUID("uid-d").Slot; got != 1 {
		t.Fatalf("replacement got slot %d, want 1", got)
	}
}

func TestRestartAccountingIsPerSlot(t *testing.T) {
	now := time.Now()
	policy := swarmHealthRestartPolicy{Condition: "on-failure", MaxAttempts: 1, Window: time.Hour}
	slot1 := &swarmHealthSlotState{Slot: 1, RestartHistory: []int64{now.UnixNano()}}
	slot2 := &swarmHealthSlotState{Slot: 2}

	if restartAllowedForSlot(slot1, policy, now) {
		t.Fatal("slot 1 exceeded MaxAttempts but restart was allowed")
	}
	if !restartAllowedForSlot(slot2, policy, now) {
		t.Fatal("slot 2 inherited slot 1 restart budget")
	}
}

func TestCorruptSwarmLifecycleStateFailsClosed(t *testing.T) {
	ctx := context.Background()
	a := newHealthcheckTestAdapter()
	dep := testSwarmDeployment("web", 1)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      swarmLifecycleConfigMapName(dep.Name),
			Namespace: a.namespace,
		},
		Data: map[string]string{swarmLifecycleStateDataKey: "{"},
	}
	if _, err := a.client.CoreV1().ConfigMaps(a.namespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create corrupt state: %v", err)
	}
	if _, _, err := a.loadSwarmHealthLifecycleState(ctx, dep); !errors.Is(err, errCorruptSwarmHealthState) {
		t.Fatalf("corrupt lifecycle state error = %v", err)
	}
}

func TestHealthManagerLeaseEnforcesSingleActiveInstance(t *testing.T) {
	ctx := context.Background()
	a1 := newHealthcheckTestAdapter()
	a2 := newHealthcheckTestAdapter()
	a2.client = a1.client
	a1.healthLeaseID = "one"
	a2.healthLeaseID = "two"

	if err := a1.acquireHealthManagerLease(ctx); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if err := a2.acquireHealthManagerLease(ctx); err == nil {
		t.Fatal("second health manager acquired live lease")
	}
	a1.releaseHealthManagerLease(ctx)
	if err := a2.acquireHealthManagerLease(ctx); err != nil {
		t.Fatalf("second acquire after release: %v", err)
	}
}

func TestReplacementDeleteFailureDoesNotConsumeAttemptAndRetries(t *testing.T) {
	ctx := context.Background()
	a := newHealthcheckTestAdapter()
	a.healthLeaseID = "test"
	if err := a.acquireHealthManagerLease(ctx); err != nil {
		t.Fatalf("acquire lease: %v", err)
	}
	defer a.releaseHealthManagerLease(ctx)

	dep := testSwarmDeployment("web", 1)
	pod := testSwarmPod("web-a", "uid-a", "web", "containerd://a", time.Now().Add(-time.Minute))
	if _, err := a.client.AppsV1().Deployments(a.namespace).Create(ctx, dep.DeepCopy(), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	if _, err := a.client.CoreV1().Pods(a.namespace).Create(ctx, pod.DeepCopy(), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pod: %v", err)
	}
	state, err := a.ensureSwarmSlotAssignments(ctx, dep, []corev1.Pod{pod})
	if err != nil {
		t.Fatalf("assign slot: %v", err)
	}
	if state.Slots["1"] == nil {
		t.Fatal("slot 1 was not created")
	}

	policy := swarmHealthRestartPolicy{Condition: "on-failure", MaxAttempts: 1, Window: time.Hour}
	created, err := a.beginSwarmHealthReplacement(ctx, dep, pod, 1, policy, &dockertypes.Health{Status: dockertypes.Unhealthy})
	if err != nil || !created {
		t.Fatalf("begin replacement: created=%v err=%v", created, err)
	}

	failOnce := true
	a.client.(*fake.Clientset).Fake.PrependReactor("delete", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if failOnce {
			failOnce = false
			return true, nil, apierrors.NewInternalError(fmt.Errorf("transient delete failure"))
		}
		return false, nil, nil
	})

	if err := a.advancePendingSwarmReplacement(ctx, dep, 1, map[string]corev1.Pod{"uid-a": pod}); err == nil {
		t.Fatal("expected first delete attempt to fail")
	}
	state, _, err = a.loadSwarmHealthLifecycleState(ctx, dep)
	if err != nil {
		t.Fatalf("load state after failed delete: %v", err)
	}
	if len(state.Slots["1"].RestartHistory) != 0 {
		t.Fatalf("failed delete consumed restart attempt: %#v", state.Slots["1"].RestartHistory)
	}

	if err := a.advancePendingSwarmReplacement(ctx, dep, 1, map[string]corev1.Pod{"uid-a": pod}); err != nil {
		t.Fatalf("retry replacement: %v", err)
	}
	state, _, err = a.loadSwarmHealthLifecycleState(ctx, dep)
	if err != nil {
		t.Fatalf("load state after retry: %v", err)
	}
	if len(state.Slots["1"].RestartHistory) != 1 {
		t.Fatalf("successful replacement did not consume exactly one attempt: %#v", state.Slots["1"].RestartHistory)
	}
	if len(state.Slots["1"].TaskHistory) != 1 {
		t.Fatalf("failed task was not retained: %#v", state.Slots["1"].TaskHistory)
	}
}

func TestUnlimitedRestartPolicyDoesNotPersistHistory(t *testing.T) {
	slot := &swarmHealthSlotState{Slot: 1}
	policy := swarmHealthRestartPolicy{Condition: "any", MaxAttempts: 0}
	if !restartAllowedForSlot(slot, policy, time.Now()) {
		t.Fatal("unlimited restart policy unexpectedly denied restart")
	}
	if len(slot.RestartHistory) != 0 {
		t.Fatalf("unlimited restart policy grew history: %#v", slot.RestartHistory)
	}
}

func TestTaskHistoryIsBounded(t *testing.T) {
	slot := &swarmHealthSlotState{Slot: 1}
	for i := 0; i < swarmTaskHistoryLimit+3; i++ {
		appendTaskHistory(slot, swarmHealthTaskRecord{ID: fmt.Sprintf("task-%d", i)})
	}
	if len(slot.TaskHistory) != swarmTaskHistoryLimit {
		t.Fatalf("task history length = %d, want %d", len(slot.TaskHistory), swarmTaskHistoryLimit)
	}
	if slot.TaskHistory[0].ID != "task-3" {
		t.Fatalf("old task history was not trimmed: %#v", slot.TaskHistory)
	}
}

