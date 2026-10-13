package adapter

import (
	"context"
	"testing"
	"time"

	dockertypes "github.com/docker/docker/api/types"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/portainer/d2k/internal/types"
)

// The Deployment controller may create one surge Pod before the original Pod
// disappears. A temporary extra slot must never become a permanent task slot.
func TestSwarmRolloutSurgeSlotPromotedWhenOldPodDisappears(t *testing.T) {
	ctx := context.Background()
	a := newHealthcheckTestAdapter()
	dep := testSwarmDeployment("web", 1)
	base := time.Now().Add(-time.Minute)

	old := testSwarmPod("web-old", "old", "web", "containerd://old", base)
	newPod := testSwarmPod("web-new", "new", "web", "containerd://new", base.Add(time.Second))
	for _, pod := range []corev1.Pod{old, newPod} {
		if _, err := a.client.CoreV1().Pods(a.namespace).Create(ctx, pod.DeepCopy(), metav1.CreateOptions{}); err != nil {
			t.Fatalf("create Pod: %v", err)
		}
	}

	if _, err := a.ensureSwarmSlotAssignments(ctx, dep, []corev1.Pod{old}); err != nil {
		t.Fatalf("assign original slot: %v", err)
	}
	state, err := a.ensureSwarmSlotAssignments(ctx, dep, []corev1.Pod{old, newPod})
	if err != nil {
		t.Fatalf("assign during surge: %v", err)
	}
	if slot := state.slotForPodUID("old"); slot == nil || slot.Slot != 1 {
		t.Fatalf("old Pod lost slot 1 during surge: %#v", state.Slots)
	}

	// A new surge Pod is allowed to obtain provisional identity so its custom
	// health gate can open and the Kubernetes Deployment rollout can progress.
	if err := a.client.CoreV1().Pods(a.namespace).Delete(ctx, old.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete original Pod: %v", err)
	}
	state, err = a.ensureSwarmSlotAssignments(ctx, dep, []corev1.Pod{newPod})
	if err != nil {
		t.Fatalf("assign after original removed: %v", err)
	}
	if slot := state.slotForPodUID("new"); slot == nil || slot.Slot != 1 {
		t.Fatalf("rollout survivor stranded outside desired slot 1: %#v", state.Slots)
	}
	actual, err := a.client.CoreV1().Pods(a.namespace).Get(ctx, newPod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("fetch promoted Pod: %v", err)
	}
	if actual.Labels[types.LabelSwarmSlot] != "1" {
		t.Fatalf("promoted Pod label = %q, want 1", actual.Labels[types.LabelSwarmSlot])
	}
}

// A crash between successful Pod delete and persisted DeleteCommittedAt must
// preserve the slot's pending identity; it must not become a fresh scale-up task.
func TestUncommittedReplacementIntentReservesSlotAfterDeleteCrash(t *testing.T) {
	ctx := context.Background()
	a := newHealthcheckTestAdapter()
	dep := testSwarmDeployment("web", 1)
	old := testSwarmPod("web-old", "old", "web", "containerd://old", time.Now().Add(-time.Minute))
	if _, err := a.client.CoreV1().Pods(a.namespace).Create(ctx, old.DeepCopy(), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create old Pod: %v", err)
	}
	if _, err := a.ensureSwarmSlotAssignments(ctx, dep, []corev1.Pod{old}); err != nil {
		t.Fatalf("assign old slot: %v", err)
	}
	failedAt := time.Now().Add(-10 * time.Second).UnixNano()
	_, err := a.mutateSwarmHealthLifecycleState(ctx, dep, func(state *swarmHealthLifecycleState) (bool, error) {
		slot := state.ensureSlot(1)
		slot.Pending = &swarmHealthReplacementIntent{
			FailedPodUID: "old", FailedPodName: old.Name,
			FailedAt: failedAt, ReplacementTaskID: "reserved-task-id",
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("persist pending: %v", err)
	}
	if err := a.client.CoreV1().Pods(a.namespace).Delete(ctx, old.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("simulate committed Kubernetes delete: %v", err)
	}
	replacement := testSwarmPod("web-replacement", "next", "web", "containerd://next", time.Now())
	if _, err := a.client.CoreV1().Pods(a.namespace).Create(ctx, replacement.DeepCopy(), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create replacement: %v", err)
	}

	state, err := a.ensureSwarmSlotAssignments(ctx, dep, []corev1.Pod{replacement})
	if err != nil {
		t.Fatalf("assignment before commit: %v", err)
	}
	if state.slotForPodUID("next") != nil {
		t.Fatalf("uncommitted deletion allowed replacement adoption: %#v", state.Slots)
	}
	if state.Slots["1"] == nil || state.Slots["1"].Pending == nil {
		t.Fatalf("pending replacement reservation was lost: %#v", state.Slots)
	}

	_, err = a.mutateSwarmHealthLifecycleState(ctx, dep, func(state *swarmHealthLifecycleState) (bool, error) {
		state.Slots["1"].Pending.DeleteCommittedAt = time.Now().UnixNano()
		return true, nil
	})
	if err != nil {
		t.Fatalf("commit deletion: %v", err)
	}
	state, err = a.ensureSwarmSlotAssignments(ctx, dep, []corev1.Pod{replacement})
	if err != nil {
		t.Fatalf("assign committed replacement: %v", err)
	}
	if slot := state.slotForPodUID("next"); slot == nil || slot.Slot != 1 || slot.CurrentTaskID != "reserved-task-id" {
		t.Fatalf("replacement did not inherit reserved task identity: %#v", state.Slots)
	}
}

// A Pod that already existed before a health failure is not evidence of a
// replacement; it may be a concurrent rolling-update surge Pod.
func TestSwarmPendingReplacementDoesNotStealPreexistingSurgePod(t *testing.T) {
	ctx := context.Background()
	a := newHealthcheckTestAdapter()
	dep := testSwarmDeployment("web", 2)
	start := time.Now().Add(-time.Minute)
	old := testSwarmPod("web-old", "old", "web", "containerd://old", start)
	other := testSwarmPod("web-other", "other", "web", "containerd://other", start.Add(time.Second))
	staleSurge := testSwarmPod("web-surge", "surge", "web", "containerd://surge", start.Add(2*time.Second))
	for _, pod := range []corev1.Pod{old, other, staleSurge} {
		if _, err := a.client.CoreV1().Pods(a.namespace).Create(ctx, pod.DeepCopy(), metav1.CreateOptions{}); err != nil {
			t.Fatalf("create Pod: %v", err)
		}
	}
	if _, err := a.ensureSwarmSlotAssignments(ctx, dep, []corev1.Pod{old, other}); err != nil {
		t.Fatalf("original assignments: %v", err)
	}
	failedAt := time.Now().Add(-10 * time.Second).UnixNano()
	_, err := a.mutateSwarmHealthLifecycleState(ctx, dep, func(state *swarmHealthLifecycleState) (bool, error) {
		state.Slots["1"].Pending = &swarmHealthReplacementIntent{
			FailedPodUID: "old", FailedPodName: old.Name, FailedAt: failedAt,
			DeleteCommittedAt: time.Now().Add(-5 * time.Second).UnixNano(), ReplacementTaskID: "reserved-task",
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("store committed intent: %v", err)
	}
	if err := a.client.CoreV1().Pods(a.namespace).Delete(ctx, old.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete old Pod: %v", err)
	}
	newPod := testSwarmPod("web-next", "replacement", "web", "containerd://replacement", time.Now())
	if _, err := a.client.CoreV1().Pods(a.namespace).Create(ctx, newPod.DeepCopy(), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create new Pod: %v", err)
	}
	state, err := a.ensureSwarmSlotAssignments(ctx, dep, []corev1.Pod{staleSurge, other, newPod})
	if err != nil {
		t.Fatalf("assign replacement: %v", err)
	}
	if slot := state.slotForPodUID("replacement"); slot == nil || slot.Slot != 1 {
		t.Fatalf("eligible replacement did not inherit slot 1: %#v", state.Slots)
	}
	if slot := state.slotForPodUID("surge"); slot != nil && slot.Slot <= 2 {
		t.Fatalf("preexisting surge Pod stole an active slot: %#v", state.Slots)
	}
}

func podWithReplicaSetOwner(pod corev1.Pod, uid string) corev1.Pod {
	controller := true
	pod.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "apps/v1",
		Kind:       "ReplicaSet",
		Name:       "rs-" + uid,
		UID:        k8stypes.UID(uid),
		Controller: &controller,
	}}
	return pod
}

// Even if a different ReplicaSet's Pod was created first, its Pod must not
// inherit the restart history/identity reserved by a failed older revision.
func TestConcurrentPendingReplacementPreservesReplicaSetProvenance(t *testing.T) {
	ctx := context.Background()
	a := newHealthcheckTestAdapter()
	dep := testSwarmDeployment("web", 2)
	base := time.Now().Add(-time.Minute)
	oldA := podWithReplicaSetOwner(testSwarmPod("old-a", "old-a", "web", "ctr-a", base), "revision-a")
	oldB := podWithReplicaSetOwner(testSwarmPod("old-b", "old-b", "web", "ctr-b", base.Add(time.Second)), "revision-b")
	for _, pod := range []corev1.Pod{oldA, oldB} {
		if _, err := a.client.CoreV1().Pods(a.namespace).Create(ctx, pod.DeepCopy(), metav1.CreateOptions{}); err != nil {
			t.Fatalf("create old Pod: %v", err)
		}
	}
	if _, err := a.ensureSwarmSlotAssignments(ctx, dep, []corev1.Pod{oldA, oldB}); err != nil {
		t.Fatalf("assign initial slots: %v", err)
	}
	failedAt := time.Now().Add(-10 * time.Second).UnixNano()
	_, err := a.mutateSwarmHealthLifecycleState(ctx, dep, func(state *swarmHealthLifecycleState) (bool, error) {
		for _, entry := range []struct {
			slot          int
			uid, revision string
		}{
			{1, "old-a", "revision-a"}, {2, "old-b", "revision-b"},
		} {
			state.Slots[slotKey(entry.slot)].Pending = &swarmHealthReplacementIntent{
				FailedPodUID: entry.uid, FailedAt: failedAt,
				FailedReplicaSetUID: entry.revision,
				DeleteCommittedAt:   time.Now().UnixNano(),
				ReplacementTaskID:   "task-" + entry.revision,
			}
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("store pending intents: %v", err)
	}
	for _, pod := range []corev1.Pod{oldA, oldB} {
		if err := a.client.CoreV1().Pods(a.namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
			t.Fatalf("delete old Pod: %v", err)
		}
	}
	// Sort order deliberately makes revision B's replacement appear first.
	timestamp := time.Now()
	newB := podWithReplicaSetOwner(testSwarmPod("new-b", "new-00", "web", "ctr-b2", timestamp), "revision-b")
	newA := podWithReplicaSetOwner(testSwarmPod("new-a", "new-99", "web", "ctr-a2", timestamp), "revision-a")
	for _, pod := range []corev1.Pod{newB, newA} {
		if _, err := a.client.CoreV1().Pods(a.namespace).Create(ctx, pod.DeepCopy(), metav1.CreateOptions{}); err != nil {
			t.Fatalf("create new Pod: %v", err)
		}
	}
	state, err := a.ensureSwarmSlotAssignments(ctx, dep, []corev1.Pod{newB, newA})
	if err != nil {
		t.Fatalf("assign new Pods: %v", err)
	}
	if slot := state.slotForPodUID("new-99"); slot == nil || slot.Slot != 1 || slot.CurrentTaskID != "task-revision-a" {
		t.Fatalf("revision A lost its slot/task: %#v", state.Slots)
	}
	if slot := state.slotForPodUID("new-00"); slot == nil || slot.Slot != 2 || slot.CurrentTaskID != "task-revision-b" {
		t.Fatalf("revision B lost its slot/task: %#v", state.Slots)
	}
}

// When ReplicaSet makes multiple interchangeable Pods for identical tasks,
// assignment must at least be unique and deterministic even with identical
// timestamps and order-varying LIST responses.
func TestConcurrentSameRevisionReplacementAssignmentDeterministic(t *testing.T) {
	for _, reversed := range []bool{false, true} {
		ctx := context.Background()
		a := newHealthcheckTestAdapter()
		dep := testSwarmDeployment("web", 2)
		base := time.Now().Add(-time.Minute)
		oldA := podWithReplicaSetOwner(testSwarmPod("old-a", "old-a", "web", "ctr-a", base), "shared")
		oldB := podWithReplicaSetOwner(testSwarmPod("old-b", "old-b", "web", "ctr-b", base.Add(time.Second)), "shared")
		for _, pod := range []corev1.Pod{oldA, oldB} {
			if _, err := a.client.CoreV1().Pods(a.namespace).Create(ctx, pod.DeepCopy(), metav1.CreateOptions{}); err != nil {
				t.Fatalf("create original: %v", err)
			}
		}
		if _, err := a.ensureSwarmSlotAssignments(ctx, dep, []corev1.Pod{oldA, oldB}); err != nil {
			t.Fatalf("initial assign: %v", err)
		}
		failedAt := time.Now().Add(-10 * time.Second).UnixNano()
		_, err := a.mutateSwarmHealthLifecycleState(ctx, dep, func(state *swarmHealthLifecycleState) (bool, error) {
			for i, uid := range []string{"old-a", "old-b"} {
				state.Slots[slotKey(i+1)].Pending = &swarmHealthReplacementIntent{
					FailedPodUID: uid, FailedReplicaSetUID: "shared", FailedAt: failedAt,
					DeleteCommittedAt: time.Now().UnixNano(),
					ReplacementTaskID: "replacement-" + uid,
				}
			}
			return true, nil
		})
		if err != nil {
			t.Fatalf("store pending: %v", err)
		}
		for _, pod := range []corev1.Pod{oldA, oldB} {
			if err := a.client.CoreV1().Pods(a.namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
				t.Fatalf("delete original: %v", err)
			}
		}
		stamp := time.Now()
		c := podWithReplicaSetOwner(testSwarmPod("new-c", "c", "web", "ctr-c", stamp), "shared")
		d := podWithReplicaSetOwner(testSwarmPod("new-d", "d", "web", "ctr-d", stamp), "shared")
		pods := []corev1.Pod{c, d}
		if reversed {
			pods = []corev1.Pod{d, c}
		}
		for _, pod := range pods {
			if _, err := a.client.CoreV1().Pods(a.namespace).Create(ctx, pod.DeepCopy(), metav1.CreateOptions{}); err != nil {
				t.Fatalf("create replacement Pod: %v", err)
			}
		}
		state, err := a.ensureSwarmSlotAssignments(ctx, dep, pods)
		if err != nil {
			t.Fatalf("assign replacements: %v", err)
		}
		if slot := state.slotForPodUID("c"); slot == nil || slot.Slot != 1 {
			t.Fatalf("UID c not stable slot 1 (reversed=%v): %#v", reversed, state.Slots)
		}
		if slot := state.slotForPodUID("d"); slot == nil || slot.Slot != 2 {
			t.Fatalf("UID d not stable slot 2 (reversed=%v): %#v", reversed, state.Slots)
		}
	}
}

func TestReplacementCandidateAllowsKubernetesSecondPrecisionTimestamp(t *testing.T) {
	ctx := context.Background()
	a := newHealthcheckTestAdapter()
	dep := testSwarmDeployment("web", 1)
	base := time.Now().Add(-time.Minute)
	old := testSwarmPod("old", "old", "web", "ctr-old", base)
	if _, err := a.client.CoreV1().Pods(a.namespace).Create(ctx, old.DeepCopy(), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create old: %v", err)
	}
	if _, err := a.ensureSwarmSlotAssignments(ctx, dep, []corev1.Pod{old}); err != nil {
		t.Fatalf("initial slot: %v", err)
	}
	// The API server may serialize CreationTimestamp only to whole seconds.
	// The actual replacement can be created after a failure in that second.
	second := time.Now().Truncate(time.Second)
	failure := second.Add(500 * time.Millisecond).UnixNano()
	_, err := a.mutateSwarmHealthLifecycleState(ctx, dep, func(state *swarmHealthLifecycleState) (bool, error) {
		state.Slots["1"].Pending = &swarmHealthReplacementIntent{
			FailedPodUID: "old", FailedAt: failure, DeleteCommittedAt: failure,
			ReplacementTaskID: "replacement-id",
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("persist pending: %v", err)
	}
	if err := a.client.CoreV1().Pods(a.namespace).Delete(ctx, old.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete old: %v", err)
	}
	candidate := testSwarmPod("new", "new", "web", "ctr-new", second)
	if _, err := a.client.CoreV1().Pods(a.namespace).Create(ctx, candidate.DeepCopy(), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create candidate: %v", err)
	}
	state, err := a.ensureSwarmSlotAssignments(ctx, dep, []corev1.Pod{candidate})
	if err != nil {
		t.Fatalf("assign candidate: %v", err)
	}
	if slot := state.slotForPodUID("new"); slot == nil || slot.Slot != 1 {
		t.Fatalf("second-precision CreationTimestamp stranded replacement: %#v", state.Slots)
	}
}

func TestReplacementExcludesPreviouslyObservedSurgePodInSameSecond(t *testing.T) {
	ctx := context.Background()
	a := newHealthcheckTestAdapter()
	a.healthLeaseID = "known-uid-test"
	if err := a.acquireHealthManagerLease(ctx); err != nil {
		t.Fatalf("acquire lease: %v", err)
	}
	defer a.releaseHealthManagerLease(ctx)
	dep := testSwarmDeployment("web", 1)
	if _, err := a.client.AppsV1().Deployments(a.namespace).Create(ctx, dep.DeepCopy(), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create Deployment: %v", err)
	}
	old := testSwarmPod("old", "old", "web", "ctr-old", time.Now().Add(-time.Minute))
	// The surge Pod already existed before the health failure, but has not
	// yet been seen by the slot reconciler.
	second := time.Now().Truncate(time.Second)
	stale := testSwarmPod("surge", "surge", "web", "ctr-surge", second)
	for _, pod := range []corev1.Pod{old, stale} {
		if _, err := a.client.CoreV1().Pods(a.namespace).Create(ctx, pod.DeepCopy(), metav1.CreateOptions{}); err != nil {
			t.Fatalf("create Pod: %v", err)
		}
	}
	if _, err := a.ensureSwarmSlotAssignments(ctx, dep, []corev1.Pod{old}); err != nil {
		t.Fatalf("assign original: %v", err)
	}
	created, err := a.beginSwarmHealthReplacement(ctx, dep, old, 1,
		swarmHealthRestartPolicy{Condition: "on-failure"},
		&dockertypes.Health{Status: dockertypes.Unhealthy})
	if err != nil || !created {
		t.Fatalf("begin pending: created=%v error=%v", created, err)
	}
	state, _, err := a.loadSwarmHealthLifecycleState(ctx, dep)
	if err != nil {
		t.Fatalf("read pending: %v", err)
	}
	pending := state.Slots["1"].Pending
	if pending == nil {
		t.Fatal("pending replacement missing")
	}
	foundStale := false
	for _, uid := range pending.KnownPodUIDs {
		if uid == "surge" {
			foundStale = true
		}
	}
	if !foundStale {
		t.Fatalf("failure intent did not snapshot surge UID: %#v", pending.KnownPodUIDs)
	}

	if err := a.client.CoreV1().Pods(a.namespace).Delete(ctx, old.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete failed Pod: %v", err)
	}
	_, err = a.mutateSwarmHealthLifecycleState(ctx, dep, func(state *swarmHealthLifecycleState) (bool, error) {
		state.Slots["1"].Pending.DeleteCommittedAt = time.Now().UnixNano()
		return true, nil
	})
	if err != nil {
		t.Fatalf("commit deletion: %v", err)
	}
	// Simulate a new Pod created after failure but with the same second-
	// precision Kubernetes CreationTimestamp as the older surge Pod.
	newPod := testSwarmPod("new", "new", "web", "ctr-new", second)
	if _, err := a.client.CoreV1().Pods(a.namespace).Create(ctx, newPod.DeepCopy(), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create new Pod: %v", err)
	}
	state, err = a.ensureSwarmSlotAssignments(ctx, dep, []corev1.Pod{stale, newPod})
	if err != nil {
		t.Fatalf("assign replacement: %v", err)
	}
	if slot := state.slotForPodUID("new"); slot == nil || slot.Slot != 1 {
		t.Fatalf("new candidate did not inherit pending slot: %#v", state.Slots)
	}
	if slot := state.slotForPodUID("surge"); slot != nil && slot.Slot == 1 {
		t.Fatalf("known preexisting surge stole replacement: %#v", state.Slots)
	}
}
