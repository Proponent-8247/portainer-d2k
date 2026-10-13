package adapter

import (
	"context"
	"testing"
	"time"

	dockertypes "github.com/docker/docker/api/types"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/kubernetes/fake"
)

func newPendingHealthFailure(t *testing.T, name string) (*KubernetesDockerAdapter, corev1.Pod, map[string]corev1.Pod) {
	t.Helper()
	ctx := context.Background()
	a := newHealthcheckTestAdapter()
	a.healthLeaseID = "uid-precondition-" + name
	if err := a.acquireHealthManagerLease(ctx); err != nil {
		t.Fatalf("acquire manager lease: %v", err)
	}
	t.Cleanup(func() { a.releaseHealthManagerLease(context.Background()) })

	dep := testSwarmDeployment(name, 1)
	pod := testSwarmPod(name+"-old", "old-uid", name, "containerd://old", time.Now().Add(-time.Minute))
	if _, err := a.client.AppsV1().Deployments(a.namespace).Create(ctx, dep.DeepCopy(), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create Deployment: %v", err)
	}
	if _, err := a.client.CoreV1().Pods(a.namespace).Create(ctx, pod.DeepCopy(), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create old Pod: %v", err)
	}
	if _, err := a.ensureSwarmSlotAssignments(ctx, dep, []corev1.Pod{pod}); err != nil {
		t.Fatalf("assign old Pod slot: %v", err)
	}
	created, err := a.beginSwarmHealthReplacement(ctx, dep, pod, 1,
		swarmHealthRestartPolicy{Condition: "on-failure", MaxAttempts: 1, Window: time.Hour},
		&dockertypes.Health{Status: dockertypes.Unhealthy})
	if err != nil || !created {
		t.Fatalf("persist replacement intent: created=%v error=%v", created, err)
	}
	return a, pod, map[string]corev1.Pod{string(pod.UID): pod}
}

// A stale resource read must not permit a name-only destructive Kubernetes
// deletion. Real API servers enforce DeleteOptions.Preconditions.UID.
func TestSwarmReplacementDeleteRequiresPodUIDPrecondition(t *testing.T) {
	ctx := context.Background()
	a, pod, snapshot := newPendingHealthFailure(t, "web")
	foundUIDPrecondition := false
	a.client.(*fake.Clientset).Fake.PrependReactor("delete", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		options := action.(k8stesting.DeleteAction).GetDeleteOptions()
		if options.Preconditions != nil && options.Preconditions.UID != nil &&
			*options.Preconditions.UID == pod.UID {
			foundUIDPrecondition = true
		}
		return false, nil, nil
	})

	dep := testSwarmDeployment("web", 1)
	if err := a.advancePendingSwarmReplacement(ctx, dep, 1, snapshot); err != nil {
		t.Fatalf("advance pending intent: %v", err)
	}
	if !foundUIDPrecondition {
		t.Fatal("Swarm health replacement deleted the Pod without failed UID precondition")
	}
}

// Kubernetes may have already recreated a Pod with the same name after an
// earlier observation. The stale failure intent must not change its readiness,
// delete it, or charge restart history to that unrelated successor.
func TestSwarmReplacementCannotDeleteReusedPodName(t *testing.T) {
	ctx := context.Background()
	a, original, snapshot := newPendingHealthFailure(t, "web")
	if err := a.client.CoreV1().Pods(a.namespace).Delete(ctx, original.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("remove old Pod: %v", err)
	}
	successor := testSwarmPod(original.Name, "successor-uid", "web", "containerd://successor", time.Now())
	if _, err := a.client.CoreV1().Pods(a.namespace).Create(ctx, successor.DeepCopy(), metav1.CreateOptions{}); err != nil {
		t.Fatalf("recreate same-name Pod with new UID: %v", err)
	}
	dep := testSwarmDeployment("web", 1)
	if err := a.advancePendingSwarmReplacement(ctx, dep, 1, snapshot); err == nil {
		t.Fatal("stale failure intent unexpectedly modified a successor with a different Pod UID")
	}
	current, err := a.client.CoreV1().Pods(a.namespace).Get(ctx, successor.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("successor Pod was deleted by stale intent: %v", err)
	}
	if current.UID != successor.UID {
		t.Fatalf("successor Pod UID changed: got %q want %q", current.UID, successor.UID)
	}
	for _, condition := range current.Status.Conditions {
		if condition.Reason == "HealthUnhealthy" {
			t.Fatalf("stale failure intent changed successor Pod health: %#v", condition)
		}
	}
	state, _, err := a.loadSwarmHealthLifecycleState(ctx, dep)
	if err != nil {
		t.Fatalf("read post-failure lifecycle: %v", err)
	}
	if state.Slots["1"].Pending == nil || state.Slots["1"].Pending.DeleteCommittedAt != 0 ||
		len(state.Slots["1"].RestartHistory) != 0 {
		t.Fatalf("stale identity consumed restart or delete intent: %#v", state.Slots["1"])
	}
}
