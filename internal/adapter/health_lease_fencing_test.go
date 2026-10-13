package adapter

import (
	"context"
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// A Lease is updated in place during leadership takeover. Its UID remains
// stable, so the former holder's DELETE needs BOTH UID and ResourceVersion
// preconditions to avoid deleting the successor's updated Lease.
func TestHealthManagerLeaseReleaseCannotDeleteSuccessor(t *testing.T) {
	ctx := context.Background()
	a := newHealthcheckTestAdapter()
	a.healthLeaseID = "manager-a"
	if err := a.acquireHealthManagerLease(ctx); err != nil {
		t.Fatalf("acquire original Lease: %v", err)
	}
	lease, err := a.client.CoordinationV1().Leases(a.namespace).Get(ctx, healthManagerLeaseName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read original Lease: %v", err)
	}
	lease.UID = k8stypes.UID("lease-uid")
	lease.ResourceVersion = "version-a"
	if _, err := a.client.CoordinationV1().Leases(a.namespace).Update(ctx, lease, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("record original resource version: %v", err)
	}

	called := false
	a.client.(*fake.Clientset).Fake.PrependReactor("delete", "leases", func(action k8stesting.Action) (bool, runtime.Object, error) {
		called = true
		options := action.(k8stesting.DeleteAction).GetDeleteOptions()
		if options.Preconditions == nil ||
			options.Preconditions.UID == nil || *options.Preconditions.UID != lease.UID ||
			options.Preconditions.ResourceVersion == nil || *options.Preconditions.ResourceVersion != "version-a" {
			t.Error("old health-manager release used name-only or incomplete Lease delete preconditions")
		}

		// Simulate manager B acquiring/updating the same Lease resource after
		// A's prior ownership GET, but before A's DELETE reaches the API.
		updated, err := a.client.CoordinationV1().Leases(a.namespace).Get(ctx, healthManagerLeaseName, metav1.GetOptions{})
		if err != nil {
			return true, nil, err
		}
		holder := "manager-b"
		updated.Spec.HolderIdentity = &holder
		updated.ResourceVersion = "version-b"
		if _, err := a.client.CoordinationV1().Leases(a.namespace).Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
			return true, nil, err
		}

		// Mimic real Kubernetes API deletion preconditions. Fake client
		// trackers do not automatically enforce resource-version conflicts.
		if options.Preconditions != nil &&
			options.Preconditions.ResourceVersion != nil &&
			*options.Preconditions.ResourceVersion != updated.ResourceVersion {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "leases"},
				healthManagerLeaseName, fmt.Errorf("resourceVersion changed"))
		}
		return false, nil, nil
	})

	a.releaseHealthManagerLease(ctx)
	if !called {
		t.Fatal("release did not attempt to delete the Lease")
	}
	current, err := a.client.CoordinationV1().Leases(a.namespace).Get(ctx, healthManagerLeaseName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("former health-manager deleted the successor's Lease: %v", err)
	}
	if current.Spec.HolderIdentity == nil || *current.Spec.HolderIdentity != "manager-b" {
		t.Fatalf("successor Lease holder was not preserved: %#v", current.Spec)
	}
}
