# HC-PRR-017 / HC-PRR-002: Swarm slot architecture investigation

This is an engineering design note, not a claim that all post-remediation findings
are resolved. Frozen review evidence remains in `POST-REMEDIATION-REVIEW-HEALTHCHECK.md`;
current remediation status is tracked in `POST-REMEDIATION-REMEDIATION-HEALTHCHECK.md`.

## Requirements and invariants

1. Every user-visible, currently desired Swarm replica slot 1..N must eventually
   have a real Pod/task or a truthful pending/failed task, never a permanently
   synthetic task while a healthy replacement exists outside the range.
2. Existing in-range, live tasks retain their slot. A provisional rollout-surge
   identity above N may be promoted into a vacated real slot.
3. A durable Pending health-replacement intent reserves its slot **before and
   after** Kubernetes Pod deletion is committed. It must not become a generic
   scale-up vacancy if d2k crashes before persisting the delete outcome.
4. A Pod present before replacement-intent creation must never inherit the
   pending task's restart history or reserved replacement task ID.
5. Candidate admission must account for ReplicaSet revision ancestry.
6. If several new Pods are indistinguishable products of a single ReplicaSet,
   assignment must be deterministic and unique. **No source-visible causal
   lineage exists** in a standard ReplicaSet mapping each newly created Pod to
   one particular failed Pod.
7. Normal `maxSurge=1,maxUnavailable=0` rolling updates must keep health-checked
   surge Pods eligible for health monitoring. Simply hiding all surplus Pods
   prevents readiness-gated new Pods from becoming ready and can deadlock the
   Deployment rollout.
8. Kubernetes Pod creation timestamps can be serialized at second precision;
   nanosecond comparisons against d2k failure timestamps are not trustworthy.
9. A candidate's ownerReferences/ReplicaSet ancestry must eventually be
   validated against the current Deployment UID, not merely its `app` labels.
   This remains part of HC-PRR-009, independently open.

## Considered options

| Workload model | Stable physical slot provenance | Rolling update model | Migration impact | Decision |
|---|---|---|---|---|
| **One Deployment**, reconciled slot ledger | Logical assignment only, with UID snapshot/revision/timestamp guards; cannot prove causal pairing of simultaneous same-ReplicaSet replacements | Existing configurable `MaxSurge`/`MaxUnavailable`; preserves start-first rollout and Service routing | Low; repair/migrate provisional slots in existing ConfigMap | **Selected for compatibility remediation** |
| **One StatefulSet** per Swarm service, one ordinal per slot | Native stable ordinal, e.g. `apps.kubernetes.io/pod-index`; same name recreated for failed ordinal | OrderedReady / RollingUpdate / OnDelete, not Deployment surge. Start-first parity requires additional orchestration | High: controller resource kind changes, selector, RBAC, Service name/headless DNS, history migration, PVC and upgrade orchestration | Candidate for a separate opt-in backend if strict slot identity is required |
| **One Deployment per replica slot** | Strong ownership via controller UID and unique per-slot selector | Independent per-slot rollouts; can approximate start-first without renumbering other slots | Very high: N workload controllers, service aggregation, per-slot scale/update/delete/inspect/RBAC/migration | Viable long-term exact backend, but excessive change for upstream health-check patch |

### Why a shared Deployment cannot give strict causal identity

A ReplicaSet creates Pods to satisfy a replica count, not to replace a named
specific Pod. A new Pod contains an ownerReference to the ReplicaSet, not a
reference to which failed Pod or Swarm slot caused the ReplicaSet to create it.
Even two new Pods created simultaneously for two failed slots are identical
with respect to causal provenance. A deterministic permutation preserves
logical consistency, but is not proof of actual slot-specific lineage.

Therefore **HC-PRR-002 must not be marked fully validated** merely because
fake-client tests prove a stable deterministic permutation. If upstream
requires strict per-slot physical identity, choose StatefulSet/per-slot
controllers and plan an explicit, opt-in migration.

## Selected compatibility-path mechanics

- Preserve the existing Swarm single-Deployment behavior.
- Permit provisional over-desired slot identities during a `MaxSurge` rollout
  so d2k can monitor health and unblock Kubernetes readiness gates.
- When an in-range slot becomes vacant, promote a surviving provisional Pod
  into that real slot; leave healthy in-range survivors untouched.
- Preserve pending replacement reservations across the deletion/persistence
  crash window, including `Pending.DeleteCommittedAt == 0`.
- At health-failure time, persist a snapshot of already-visible Pod UIDs.
  Replacement candidates exclude those UIDs, even if Kubernetes timestamps
  round the candidate and failure to the same second.
- Prefer candidates belonging to the failed task's ReplicaSet UID;
  reject Pods from other ReplicaSet revisions for that pending intent.
- Apply stable ordering by CreationTimestamp then Pod UID; never depend on
  Kubernetes LIST order or Go map iteration.
- For existing pending records that lack new snapshot/provenance fields,
  preserve compatibility, but classify matching as less certain; do not
  silently claim that historic state has stronger evidence than it does.
- Fail closed when no eligible replacement is yet visible.

## Migration / rollback strategy

The compatibility remediation changes only the adapter slot ledger, not
the kind/namespace/name of existing workloads:

1. Read the existing lifecycle ConfigMap by Deployment UID.
2. Identify valid current in-range slots and any provisional over-desired slots.
3. Never replace a current in-range occupant during migration.
4. Promote a real, surviving over-desired Pod **only when** an in-range slot
   has become vacant and is not reserved by Pending.
5. Patch the Pod's slot label/deletion-cost after the durable ledger CAS update.
6. Reconcile after interruption or patch failure; do not rewrite Git history.

For a future StatefulSet/per-slot controller migration, an in-place controller
kind swap is unsafe. Implement a separate opt-in migration with a planned
handoff of Service selectors, task IDs, slot histories and persistent storage;
verify that old and new owners do not claim the same Pod. A rollback must retain
the original Deployment and state until convergence is proven.

## Acceptance gates

1. Red then green regression tests for ordinary single-replica surge,
   uncommitted delete intent, preexisting surge theft, revision isolation,
   equal-timestamp dual replacement, and timestamp precision.
2. Adapter suite, formatting, tidy, build, test, vet, and race detection.
3. **Real controller**: Kubernetes Deployment default/start-first rolling
   update with health-ready gate; prove no rollout deadlock and the final
   task list maps slots 1..desired to live Pods.
4. **Real controller**: concurrent two-slot replacement, scale/rollout
   interleaving, and crash-after-delete-before-ConfigMap-commit.
5. **Real controller**: owner lineage, service deletion/recreation and
   Lease handoff fencing (tracked separately under HC-PRR-009 and 003/007).

Do not promote HC-PRR-017 or HC-PRR-002 to final FIXED until relevant live
controller/failure-injection acceptance succeeds.
