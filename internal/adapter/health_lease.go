package adapter

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	healthManagerLeaseName       = "d2k-health-manager"
	healthManagerLeaseDuration   = 15 * time.Second
	healthManagerLeaseRenewEvery = 5 * time.Second
)

var healthManagerInstanceCounter atomic.Uint64

func newHealthManagerIdentity() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "d2k"
	}
	return fmt.Sprintf("%s-%d-%d", host, time.Now().UnixNano(), healthManagerInstanceCounter.Add(1))
}

func leaseExpired(lease *coordinationv1.Lease, now time.Time) bool {
	if lease == nil || lease.Spec.RenewTime == nil || lease.Spec.LeaseDurationSeconds == nil {
		return true
	}
	return lease.Spec.RenewTime.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second).Before(now)
}

func (a *KubernetesDockerAdapter) acquireHealthManagerLease(ctx context.Context) error {
	if a.healthLeaseID == "" {
		a.healthLeaseID = newHealthManagerIdentity()
	}
	durationSeconds := int32(healthManagerLeaseDuration / time.Second)

	for attempt := 0; attempt < 5; attempt++ {
		now := metav1.NewMicroTime(time.Now())
		lease, err := a.client.CoordinationV1().Leases(a.namespace).Get(ctx, healthManagerLeaseName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			holder := a.healthLeaseID
			created, createErr := a.client.CoordinationV1().Leases(a.namespace).Create(ctx, &coordinationv1.Lease{
				ObjectMeta: metav1.ObjectMeta{
					Name:      healthManagerLeaseName,
					Namespace: a.namespace,
				},
				Spec: coordinationv1.LeaseSpec{
					HolderIdentity:       &holder,
					LeaseDurationSeconds: &durationSeconds,
					AcquireTime:          &now,
					RenewTime:            &now,
				},
			}, metav1.CreateOptions{})
			if createErr == nil {
				a.healthLeaseResourceVersion = created.ResourceVersion
				return nil
			}
			if apierrors.IsAlreadyExists(createErr) || apierrors.IsConflict(createErr) {
				continue
			}
			return createErr
		}
		if err != nil {
			return err
		}

		holder := ""
		if lease.Spec.HolderIdentity != nil {
			holder = *lease.Spec.HolderIdentity
		}
		if holder != "" && holder != a.healthLeaseID && !leaseExpired(lease, time.Now()) {
			return fmt.Errorf("health manager lease %q is held by %q; d2k health management supports one active instance per namespace", healthManagerLeaseName, holder)
		}

		copyLease := lease.DeepCopy()
		if holder != a.healthLeaseID {
			transitions := int32(1)
			if copyLease.Spec.LeaseTransitions != nil {
				transitions = *copyLease.Spec.LeaseTransitions + 1
			}
			copyLease.Spec.LeaseTransitions = &transitions
			copyLease.Spec.AcquireTime = &now
		}
		copyLease.Spec.HolderIdentity = &a.healthLeaseID
		copyLease.Spec.LeaseDurationSeconds = &durationSeconds
		copyLease.Spec.RenewTime = &now

		updated, updateErr := a.client.CoordinationV1().Leases(a.namespace).Update(ctx, copyLease, metav1.UpdateOptions{})
		if updateErr == nil {
			a.healthLeaseResourceVersion = updated.ResourceVersion
			return nil
		}
		if apierrors.IsConflict(updateErr) {
			continue
		}
		return updateErr
	}
	return fmt.Errorf("unable to acquire health manager lease after conflicts")
}

func (a *KubernetesDockerAdapter) healthManagerLeaseOwned(ctx context.Context) error {
	lease, err := a.client.CoordinationV1().Leases(a.namespace).Get(ctx, healthManagerLeaseName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != a.healthLeaseID {
		return fmt.Errorf("health manager lease ownership lost")
	}
	if leaseExpired(lease, time.Now()) {
		return fmt.Errorf("health manager lease expired")
	}
	return nil
}

func (a *KubernetesDockerAdapter) renewHealthManagerLease(ctx context.Context) error {
	for attempt := 0; attempt < 5; attempt++ {
		lease, err := a.client.CoordinationV1().Leases(a.namespace).Get(ctx, healthManagerLeaseName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != a.healthLeaseID {
			return fmt.Errorf("health manager lease ownership lost")
		}
		copyLease := lease.DeepCopy()
		now := metav1.NewMicroTime(time.Now())
		durationSeconds := int32(healthManagerLeaseDuration / time.Second)
		copyLease.Spec.LeaseDurationSeconds = &durationSeconds
		copyLease.Spec.RenewTime = &now
		updated, err := a.client.CoordinationV1().Leases(a.namespace).Update(ctx, copyLease, metav1.UpdateOptions{})
		if err == nil {
			a.healthLeaseResourceVersion = updated.ResourceVersion
			return nil
		}
		if apierrors.IsConflict(err) {
			continue
		}
		return err
	}
	return fmt.Errorf("unable to renew health manager lease after conflicts")
}

func (a *KubernetesDockerAdapter) runHealthManagerLease(ctx context.Context, cancel context.CancelFunc) {
	ticker := time.NewTicker(healthManagerLeaseRenewEvery)
	defer ticker.Stop()
	lastSuccess := time.Now()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if err := a.renewHealthManagerLease(ctx); err != nil {
			if a.logger != nil {
				a.logger.Warnw("health manager lease renewal failed", "error", err)
			}
			if time.Since(lastSuccess) >= healthManagerLeaseDuration {
				cancel()
				return
			}
			continue
		}
		lastSuccess = time.Now()
	}
}

func (a *KubernetesDockerAdapter) releaseHealthManagerLease(ctx context.Context) {
	if a.healthLeaseID == "" {
		return
	}
	lease, err := a.client.CoordinationV1().Leases(a.namespace).Get(ctx, healthManagerLeaseName, metav1.GetOptions{})
	if err != nil {
		return
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != a.healthLeaseID {
		return
	}
	_ = a.client.CoordinationV1().Leases(a.namespace).Delete(ctx, healthManagerLeaseName, metav1.DeleteOptions{})
}
