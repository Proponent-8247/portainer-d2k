# Docker health checks — independent post-remediation review

**Review status:** IN PROGRESS — blind phase. **Do not use prior audit, review, remediation history or discussions before findings are frozen.**

## Immutable review target and verification (2026-10-09)

- Repository: `Proponent-8247/portainer-d2k`
- Candidate reviewed: `96d3f9209ac48dacacb5dd8330e6d86129eb674c` **(pinned; do not substitute later branch HEAD)**
- Source upstream: `portainer/d2k`, `develop` at `2508dde0d06476029be6aaee999b01c8262b6cc0`
- **Before this review-ledger commit**, the feature branch HEAD exactly matched the pinned candidate SHA.
- Upstream `portainer/d2k:develop` also exactly matched the stated upstream base SHA.
- GitHub compare `2508dde..96d3f92` in fork: **ahead, 148 commits ahead, 0 behind**; `merge_base_commit == base_commit == 2508dde0d06476029be6aaee999b01c8262b6cc0`. No divergence/behind condition at verification.
- Writing this ledger to the requested feature branch necessarily advances the branch HEAD without changing the pinned source snapshot. All source inspection/validation must explicitly use pinned SHA or its immutable local checkout.

## Blind review protocol

Independent from previous findings. No access to `AUDIT-HEALTHCHECK.md`, `BLIND-REVIEW-HEALTHCHECK.md`, feature commit messages, issues or previous review discussion until all independent findings have been frozen and committed. No product fixes in this phase.

## Scope and progress

- [x] Pinned SHA/base/branch verification
- [ ] Reconstruction of candidate lifecycle architecture from source
- [ ] Docker health semantics and exec security
- [ ] Health monitor ownership, concurrency, cancellation and cross-process Lease fencing
- [ ] Persisted Swarm slots/task history/restart accounting and all replacement transitions
- [ ] Standalone/list/inspect and Swarm Docker API paths
- [ ] Routing, reachability and update atomicity
- [ ] Kubernetes controller interaction, upgrade behavior and API 1.44 negotiation
- [ ] Adversarial test coverage and explicit HIGH-risk race timelines
- [ ] Freeze independent findings
- [ ] Validation gates: gofmt, go mod tidy clean, build, test, vet, race, amd64/arm64 OCI
- [ ] Prior-ledger reconciliation **only after freeze**

## Architectural reconstruction

Pending source inspection.

## Independent findings (append before freeze)

Not yet assessed; zero findings logged **does not mean zero defects**.

## Validation

Not yet executed. Static review and independent finding freeze must precede validation per review instructions.

## Independent upstream submission verdict

**NOT YET ASSESSED.**

## Reconciliation with prior work (only after freeze)

Strictly embargoed during blind phase.


## Source-derived architectural reconstruction (partial, source-only)

At the pinned candidate, `health_manager.go` acquires a namespace Lease at startup, launches a Lease-renewal loop and periodic Swarm lifecycle/monitor reconciliations, and tracks monitor ownership by Pod UID, container ID and registration token. `health_lease.go` implements a 15-second lease/5-second renewal model. `swarm_health_lifecycle.go` persists per-service UID and Deployment generation, stable slot mappings, bounded failed-task records, restart history, and replacement intent in lifecycle ConfigMaps. The replacement flow records intent, marks the Pod unready, checks the Lease, deletes the Pod, then commits deletion/restart/task history and later adopts an unassigned replacement Pod. Readback/API path inspection, health command semantics, and full architecture remain incomplete.

## Independently observed defects (pre-validation; no prior audit consulted)

### HC-PRR-001 — HIGH — Pod delete is not guarded by the failed Pod's UID

**Evidence:** `internal/adapter/swarm_health_lifecycle.go:815-845` reads a Pod from a previously listed map, calls `setPodHealthCondition(ctx, pod.Name, ...)`, then `Pods.Delete(ctx, pod.Name, metav1.DeleteOptions{})` with no UID/resourceVersion preconditions. A Pod with the same name can be replaced between the list/get/condition and deletion; the deletion can affect the new Pod rather than the intended failed Pod. `setPodHealthCondition` also acts by name and must be checked for stale-generation side effects.

**Impact/root cause:** TOCTOU/name-only destructive mutation despite intent identifying the failed Pod by UID.

**Suggested remediation:** Fenced delete with Kubernetes UID precondition; fence condition update against Pod UID and correct generation; treat precondition failure as stale intent and re-evaluate. Add race test for replacement between list and delete, including Kubernetes timeout-after-success.

### HC-PRR-002 — HIGH — Newly unassigned Pods can be incorrectly bound to a pending Swarm replacement slot

**Evidence:** `internal/adapter/swarm_health_lifecycle.go:377-409` chooses the first unassigned Pod, sorting candidates only by creation timestamp (`283-300`), and unconditionally binds the Pod to the lowest pending slot with a committed delete. The code shown does not establish that the candidate was created as a replacement for the failed Pod or even after its replacement intent; multiple simultaneous pending replacements and a concurrent scale/rollout cannot be reliably distinguished.

**Impact/root cause:** Cross-slot task ID/slot identity mismatch and potentially incorrect delay/restart history attribution after concurrent reconcile/controller events.

**Suggested remediation:** Establish a durable, verifiable replacement association or rigorously handle ambiguous multi-slot replacement as an explicit limited-support case; test multiple pending slots with simultaneous Pod creation, scale-up and rollout.

### HC-PRR-003 — HIGH — Lease release can delete a successor's Lease via check-then-delete race

**Evidence:** `internal/adapter/health_lease.go:175-187` checks current `HolderIdentity` using GET, then deletes the Lease by name with empty `DeleteOptions`. Another process can acquire/update the same Lease after the GET and before the DELETE; deleting by name alone removes the successor's Lease. The old owner need not hold the Lease at mutation time.

**Impact/root cause:** Loss of fencing/duplicate-leadership opportunity during shutdown or handoff.

**Suggested remediation:** Supply Lease UID/resourceVersion preconditions on deletion and treat precondition failure as successful non-release of a lease no longer owned. Test interleaving: owner A GET; owner B acquires; owner A DELETE.

### HC-PRR-004 — MEDIUM — Lease loss does not immediately stop manager execution on observed renewal failure

**Evidence:** `internal/adapter/health_lease.go:149-169` logs all failed renewal attempts, including a holder-identity mismatch from `renewHealthManagerLease`, but waits until `time.Since(lastSuccess) >= 15s` to cancel. The local reconciliation loop in `health_manager.go:178-195` continues in that interval. Several destructive paths re-check Lease separately, but not all monitor-side effects or mutation routes have been audited yet.

**Impact/root cause:** Potential stale-owner activity during an explicit leadership-loss interval. Scope of concrete downstream effects remains to be proven.

**Suggested remediation:** Cancel immediately on definitive ownership loss or Lease expiration, distinguish from transient API errors, and thoroughly fence every side effect. Test holder replacement during a long-running health probe.

## Further work

This is **not a completed review**. No full test suite, race validation, cross-architecture image build, API contract review, or prior-ledger reconciliation has been performed. Findings are source-derived, not reproduced via live Kubernetes. Do not freeze the list until all required subsystems are reviewed.
