# Post-remediation lifecycle remediation — HC-PRR

## Authority and baseline
- Repository: `Proponent-8247/portainer-d2k`
- Working branch: `feature/docker-healthchecks-upstream`
- Immutable reviewed source: `96d3f9209ac48dacacb5dd8330e6d86129eb674c`
- Final independent review: `POST-REMEDIATION-REVIEW-HEALTHCHECK.md` at `4609b1aa5fed1e119f770a38f2eeb30c850d35e7`
- Upstream base: `portainer/d2k@2508dde0d06476029be6aaee999b01c8262b6cc0`
- Preserve frozen source/review history. No rebases or history rewrites.

## Status summary
Open: **15** · In progress: **2** · Fixed: **1** · Accepted with documented limitation: **0**

Of the 18 frozen findings, 15 remain OPEN, 2 are IN PROGRESS and 1 is FIXED with exact-SHA full Go/race/OCI and live Kubernetes controller proof. Source-level fixes are not promoted to FIXED solely on fake-client tests.

| ID | Severity | Status | Scope |
|---|---|---|---|
| HC-PRR-001 | HIGH | IN PROGRESS | UID-fenced destructive Pod status and delete; red run 38466415572, source 414f4a804ba36a28d1ec8403b8b1284d970f479e; exact-head full gates pending |
| HC-PRR-002 | HIGH | IN PROGRESS | Concurrent replacement slot association and ambiguity |
| HC-PRR-003 | HIGH | OPEN | Lease release race |
| HC-PRR-004 | MEDIUM | OPEN | Lease loss fencing |
| HC-PRR-005 | HIGH | OPEN | Crash after delete before durable commit |
| HC-PRR-006 | MEDIUM | OPEN | Documented exec timeout approximation |
| HC-PRR-007 | HIGH | OPEN | Lease reacquisition / fail-process lifecycle |
| HC-PRR-008 | HIGH | OPEN | Invalid health annotation fail closed |
| HC-PRR-009 | HIGH | OPEN | Pod ownership across recreated same-name service |
| HC-PRR-010 | MEDIUM | OPEN | Live task inspect IDs |
| HC-PRR-011 | MEDIUM | OPEN | Aggregate history retention |
| HC-PRR-012 | MEDIUM | OPEN | Same-Pod target-container restarts |
| HC-PRR-013 | HIGH | OPEN | Negative restart policy prevalidation |
| HC-PRR-014 | HIGH | OPEN | Restart delay controls process start vs readiness |
| HC-PRR-015 | MEDIUM | OPEN | Terminating task state |
| HC-PRR-016 | MEDIUM | OPEN | Concurrent manager start |
| HC-PRR-017 | HIGH | FIXED | Ordinary Deployment rolling-update surge slot drift; exact-source validation: 38465636577, 38465636594 and 38465636607 |
| HC-PRR-018 | MEDIUM | OPEN | Crashloop target with healthy sidecar |

## Current investigation

### Root causes
- HC-PRR-017: slot allocator uses `max(desiredReplicas, livePods)` as
  the slot ceiling. During ordinary Deployment `MaxSurge`, a newly created
  Pod receives an extra slot beyond desired replicas. When the old Pod
  disappears, the survivor is never moved back, yet task list iterates only
  slots 1..desired, producing an indefinitely synthetic task.
- HC-PRR-002: a pending deleted task claims the oldest unassigned Pod without
  verifying that it is eligible as a replacement. Multiple pending replacements,
  rolling-update surges, scale changes and mixed ReplicaSets make assignment
  ambiguous. Simple creation-time sorting is not causal provenance.

### Candidate architecture options
1. **Retain one Deployment and constrain the allocator:** never assign permanent
   Swarm slot numbers above desired replicas; treat surge Pods as candidates,
   not permanent new slots; require eligible replacement ancestry/generation
   and avoid adoption of pre-existing/ambiguous Pods. Minimal migration impact.
   Limitation: a single ReplicaSet generates interchangeable Pods, so it
   cannot prove exact one-to-one causal assignment for simultaneous losses.
2. **One StatefulSet per Swarm service:** ordinal-indexed stable Pod identities
   provide a native slot mapping. Requires managed migration, StatefulSet
   serviceName/headless-DNS semantics, update-strategy parity, new RBAC and
   lifecycle/API readback refactor; cannot be silently substituted for existing
   Deployments.
3. **One single-replica Deployment per Swarm slot:** unique controller/selector
   per logical replica solves causal identity and per-slot rolling updates, but
   massively changes Swarm service create/update/scale/list/readback, PVC and
   network policy handling. Separate feature/migration design required.

### Decision gate
Start with a focused one-Deployment compatibility fix for HC-PRR-017
and a fail-safe allocator for HC-PRR-002; add failing tests before
claiming remediation. Do **not** claim strict causal affinity between
simultaneous Replacement Pods while using a shared Deployment.
If exact slot-to-Pod causality is a hard requirement, escalate to a
StatefulSet/per-slot controller design rather than inventing a mapping.

## Acceptance / kill tests
- 1-replica Deployment, `maxSurge=1,maxUnavailable=0`: old Pod slot 1,
  new surge Pod appears while old lives; any slot 2 is provisional, not permanent; old terminates;
  new Pod inherits slot 1 and task listing remains real, not synthetic.
- N-replica Deployment, overlapping rolling update and scale-up/down.
- Two simultaneous unhealthy slots, old Pods deleted and two new Pods arrive
  with equal timestamps and mixed ordering; assignment repeatable, unique and
  strictly within desired slots; no pre-existing Pod can steal pending identity.
- Crash after deletion before ConfigMap commit; pending slots remain reserved
  and no new Pod consumes the slot as a fresh task before retry.
- Old ReplicaSet / old same-name service Pods cannot be adopted by new owner.
- Run gofmt, tidy, build, tests, vet, race, OCI and live-Kubernetes controller
  rollout tests. Fake client tests do not substitute for live controller proof.

## Timeline
- **STARTED** source recovery/current branch verification and immutable snapshot checks.
- **STARTED** HC-PRR-017/002 root-cause and architectural alternatives.
- **COMPLETED** Red-test run `38450191087` reproduced the rollout slot drift, uncommitted-delete slot leak, and preexisting-surge theft.
- **COMPLETED** Allocator guard/promotion/reservation commit `e6300038fff18c21bfd3c19bb6c6d1f8d086639c`; focused test run `38450538839` green.
- **COMPLETED** Additional revision-aware, stable tie-break and same-second Pod UIDs tests; run `38451414502` green.
- **COMPLETED** Time-precision/known-failure-time Pod snapshot commit `b9a1fcb51bf6b763993436202dae940e834310f8`; run `38451202073` green.
- **COMPLETED** Kind real-controller run `38452483770` proved rolling-update surge and promotion but exposed a task-readback defect: a terminating former slot owner can override the active Pod in `SwarmListTasks` (HC-PRR-015 interaction). New focused test reproduces that bug, and product fix is committed as `7d8c659fce8faba9e8b6eb3434b5b4809c24b55a`.
- **IN PROGRESS** Real-controller repeat: both one-replica rolling-update and two simultaneous replacement-Pod tests in disposable Kind; not yet green on final code.
- **IN PROGRESS** Full gofmt/tidy/build/test/vet/race/OCI exact-head gates and supplemental controller fault injection.
- **BLOCKED ON PROOF** HC-PRR-002 cannot claim exact physical slot causality for two indistinguishable Pod replacements under a single ReplicaSet; architecture tradeoff is documented in `HEALTHCHECK-SLOT-ARCHITECTURE.md`.

### New regression / validation evidence

- `internal/adapter/swarm_slot_reconciliation_test.go`: ordinary surge promotion, pending reservation before delete commit, preexisting surge exclusion, cross-revision concurrent replacement, deterministic same-revision assignment, Kubernetes CreationTimestamp precision, failure-time UID snapshot, and current-vs-terminating task readback.
- `internal/adapter/swarm_slot_live_test.go`: opt-in `livek8s` real Deployment-controller rollout and concurrent two-slot replacement scenarios.
- GitHub temporary workflows `healthcheck-prr-slot-validation.yml` and `healthcheck-prr-kind-validation.yml` are validation-only and must be removed before upstream submission.
- **No finding closed based solely on source inspection or fake-client success.**

## HC-PRR-017 remediation closure — independently reproducible evidence

**Reviewed source baseline:** `96d3f9209ac48dacacb5dd8330e6d86129eb674c`.
**Validated functional head:** `666c783c9ce256721d5f22e964ded29d27dec7d3`.
**Result:** **FIXED** for the specific ordinary rolling-update slot-drift/task-readback defect; this does **not** close HC-PRR-002 or the overall 18-finding review.

- Red source-target regressions: run `38450191087` reproduced desired-slot drift and replacement misattribution.
- Repaired provisional surge-slot promotion, reserved pending replacement identity, timestamp rounding and pre-existing UID snapshots, with deterministic/revision-aware adoption.
- Corrected `SwarmListTasks` to use desired `Deployment.Spec.Replicas`, never the transient `Status.Replicas` inflated by MaxSurge; filtered terminating Pods and favored the durable current slot UID for task readback.
- **Focused Go regression matrix:** [run 38465636577](https://github.com/Proponent-8247/portainer-d2k/actions/runs/38465636577) **PASS** on exact source.
- **Real Kind Kubernetes 1.35 controllers:** [run 38465636594](https://github.com/Proponent-8247/portainer-d2k/actions/runs/38465636594) **PASS** on exact source. Acceptance covers 1->1 start-first rolling update, two simultaneous replacement Pods, 2->2 rolling update and native Service EndpointSlice health admission/withdrawal.
- **Strict full validation:** [run 38465636607](https://github.com/Proponent-8247/portainer-d2k/actions/runs/38465636607) **PASS** on exact source: gofmt, strict go mod tidy diff, build, all Go tests, go vet, `go test -race ./...`, and `linux/amd64,linux/arm64` Buildx OCI local-context build.
- Journal and native compatibility design remain separate working documents. Temporary validation workflows are not part of intended upstream submission.

### HC-PRR-002 — partial acceptance, not closed

- Simultaneous replacement tests pass with deterministic and unique **logical** slot assignments on a shared Deployment.
- There is no Kubernetes ReplicaSet API metadata linking a newly generated Pod to a single causally corresponding failed Pod. It is therefore not possible to prove **strict physical predecessor->replacement slot identity** for two indistinguishable replacements using only existing ReplicaSet evidence.
- Status stays **IN PROGRESS**. If strict physical slot identity is required for Docker/Swarm parity, design an explicit opt-in native StatefulSet or per-slot-controller backend, including upgrade/rollback and start-first compatibility gates. Do not silently claim these stronger guarantees from a sorted-Pod implementation.

## HC-PRR-001 — red/green UID-fencing remediation in progress

- Regression source: `internal/adapter/swarm_health_uid_fencing_test.go`, created at `5d00fef08ee3259e5f380202ded4cb6b427d9e25`. Test 1 asserts `DeleteOptions.Preconditions.UID` equals the failed Pod UID; test 2 replaces a Pod with another UID under the same name and checks a stale health intent neither changes successor readiness nor deletes it/consumes restart attempts.
- **RED** focused CI [38466415572](https://github.com/Proponent-8247/portainer-d2k/actions/runs/38466415572), head `86034c20f69489f8df43f6c47c7f08571466d8ee`: both UID race tests failed, original slot tests passed.
- **Product fix** single atomic commit `414f4a804ba36a28d1ec8403b8b1284d970f479e`:
  - UID-aware Pod status mutation with conflict retry and identity verification;
  - monitoring/Swarm reconciler paths pass observed Pod UID to fenced helper;
  - destructive Pods.Delete uses native Kubernetes `DeleteOptions.Preconditions.UID` from durable failed-task identity.
- **GREEN behavioral tests** on run `38466970659` at an intermediate head; full workflow itself failed only gofmt import ordering for the new test.
- Format fix `93b1dee1f0b94a864e895483c0405f77b5a4a431`. **Pending:** exact-functional-head focused `38467235235`, live Kind `38467235234`, full Go/race/multiarch `38467235317`.
- Do not mark HC-PRR-001 FIXED until final gates pass; native API preconditions are security-relevant and failure modes must remain fail-closed.

## Mutation policy
- One conceptual fix/commit where practical, plus independent ledger checkpoints.
- Never rewrite published Git history.
- Preserve default behavior outside Swarm health lifecycle.
- New findings must be appended with evidence; do not erase frozen review text.
