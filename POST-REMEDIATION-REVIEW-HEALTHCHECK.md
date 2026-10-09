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

---

# Restarted independent source pass — 2026-10-09

**Restart scope:** Complete fresh source inspection at immutable candidate `96d3f9209ac48dacacb5dd8330e6d86129eb674c`; source tree and upstream base verified again. Feature-branch HEAD at restart was `2c3634e68e5b376e1e0dcee19633eb61bf55efc5` (review-ledger updates after candidate); upstream `portainer/d2k:develop` remained `2508dde0d06476029be6aaee999b01c8262b6cc0`. Compare candidate against base still reports `ahead=148`, `behind=0`, merge-base equal to base. Do not mistake documentation-only commits on branch HEAD for modifications to pinned code.

## Source reconstruction (expanded)

- `cmd/d2k.go` starts health manager before HTTP server begins; startup fails if another process holds the namespace Lease.
- `healthcheck.go` stores Docker HealthConfig JSON as Deployment annotations, validates durations/command shape; `NONE` disables translation; image HEALTHCHECK inheritance is not implemented and create emits explicit warnings.
- `health_manager.go` periodically reconciles d2k-managed Deployments; it discovers target container status by name, keys monitors by Pod UID + container ID, and fences state publication through registration token and container ID. Memory holds starting/healthy/unhealthy, failing streak and last-five probe records. Health is non-durable across process restart.
- Swarm readiness uses custom Pod readiness gate `d2k.portainer.io/health-ready`; standalone Docker health does not gate endpoints, and standalone Services set `PublishNotReadyAddresses`. `Pods/exec` runs health commands in the named workload container through Kubernetes SPDY.
- `health_lease.go`: namespaced `d2k-health-manager` Lease, 15-second duration and 5-second renewal; failed renewal cancels manager only after 15 seconds since last success; lease release GETs and DELETEs by name.
- `swarm_health_lifecycle.go`: one ConfigMap per Swarm service-name hash, schema version 1; tracks Deployment UID/generation, per-slot Pod/container/task identity, max-attempt restart timestamps, 5-task/slot history and durable Pending replacement intent. Slot IDs are assigned by surviving stored UID, existing Pod label, pending committed replacement, then the next lowest vacant slot.
- Unhealthy Swarm monitor records health and marks Pod unready. The reconciler persists intent, deletes the Pod, records delete commitment + restart history + failed task, then binds a new Pod to its old slot, preserving a preassigned replacement task ID and delayed readiness.
- `swarm.go` creates/updates service Deployments, DNS/LoadBalancer Services, sets readiness gates, and synthesizes Swarm task readback from current Pods plus ConfigMap history. Pod execution and target status are matched by container name to avoid sidecar-first errors.
- Docker Engine 27.3.1 `daemon/health.go` and upstream SwarmKit restart supervisor were inspected independently to check defaults, execution timing, output truncation, failure-streak semantics and per-slot restart accounting.

**High-risk event timelines checked:**
1. replacement DELETE success -> process crash before ConfigMap commit -> new Kubernetes replacement Pod exists -> slot reassignment runs before pending intent advancement;
2. Lease holder A reads itself as owner -> B takes over -> A deletes Lease by name;
3. monitor A verifies Pod UID/container ID -> old Pod disappears -> new Pod with reused name appears -> downstream name-only UpdateStatus operates on new Pod;
4. renewal loop observes identity mismatch but leaves context live until timeout, allowing stale monitor-side activity.

## Further findings from restarted source inspection

### HC-PRR-005 — HIGH — Crash-after-delete before intent commit can permanently poison replacement slot

**Evidence:** `internal/adapter/swarm_health_lifecycle.go:306-319,377-455,815-882`. If the failed Pod has vanished but `slot.Pending.DeleteCommittedAt == 0`, `ensureSwarmSlotAssignments` removes the old current UID but does not reserve the empty slot during `nextVacant()`. If a new Pod has already arrived, the unassigned-Pod fallback adopts it into the same slot with `swarmID(newPodUID)`; `Pending` remains. When `advancePendingSwarmReplacement` subsequently marks the deletion committed, it does not clear `CurrentPodUID` because that UID is no longer the failed UID. Later reconciliation sees a nonempty current slot and cannot clear the pending intent. New unhealthy instances cannot create a new intent.

**Impact:** stuck pending lifecycle, incorrect replacement task identity, suppressed future restarts and incorrect task history across the crash boundary.

**Remediation/test:** reserve every pending slot whether delete committed or not; reconcile old intent before adopting successor; atomically adopt with preassigned task ID once delete is durably recorded; inject process crash after successful API deletion and before ConfigMap write with replacement already visible.

### HC-PRR-006 — MEDIUM — Probe timeout is not enforced from process start

**Evidence:** `health_manager.go:503-561` passes one `context.WithTimeout(ctx, setupBudget + probeTimeout)` to `execInPod` and does not know when remote command starts. With a 3-second Docker Timeout and normal immediate start, a hanging command can run for up to 33 seconds. Moby v27.3.1 `daemon/health.go:111-156` starts separate 30-second setup and then configured command timeout after a process-start notification.

**Impact:** delayed health-failure detection and delayed Swarm replacement by up to the setup allowance.

**Remediation/test:** implement a start-boundary indication if practical, or document and bound timing difference; live test immediate exec start followed by a long-running command.

### HC-PRR-007 — HIGH — Health manager permanently stops after lease renewal expiry while Docker API remains live

**Evidence:** `health_lease.go:149-172` cancels health manager context after 15s without successful renewal. `health_manager.go:122-153` does not reacquire the Lease or restart the manager; `healthCancel` remains non-nil, making a later `StartHealthManager` call return immediately without restarting any goroutine. `cmd/d2k.go` continues serving Docker HTTP API after manager cancellation.

**Impact:** transient Kubernetes control-plane outage or leadership loss can leave otherwise healthy d2k API alive but never resume health probes/replacements on recovery.

**Remediation/test:** explicit liveness/leadership state machine with reacquisition or fail process to be restarted; expose health-manager readiness separately from HTTP ping; test 20s API disruption followed by full recovery.

### HC-PRR-008 — HIGH — Corrupt health annotation bypasses Swarm health gating

**Evidence:** `health_manager.go:208-214` skips deployments where `decodeHealthcheckAnnotation` fails; `swarm_health_lifecycle.go:638-651` calls `deploymentHealthEnabled`, which returns false for invalid JSON; neither path forces previously healthy Pods unready. A previously healthy Pod retains the readiness gate condition `True` if the Deployment health JSON is manually corrupted.

**Impact:** affected Pod remains routable without a working health monitor, violating fail-closed health semantics.

**Remediation/test:** distinguish absent/disabled from invalid health configuration; force readiness false on corruption; test corrupted annotation after readiness became True.

### HC-PRR-009 — HIGH — Old service Pods can be adopted by a newly created service using the same name

**Evidence:** `swarm_health_lifecycle.go:612-638` and `health_manager.go:197-229` select pods by `app=<deployment-name>` and d2k labels, but do not match ReplicaSet ancestry or the current Deployment UID. `ensureSwarmSlotAssignments` can adopt any nonterminating matching Pod into the newly initialized state. `loadSwarmHealthLifecycleState` correctly resets state on service UID change but cannot protect identity when live old-service Pods still match name-based selectors.

**Impact:** during service delete/recreate and Kubernetes garbage-collection delays, old Pods can acquire new service slots, be reported as new tasks and probed as if owned by the new service.

**Remediation/test:** verify Pod owner ReplicaSet ownership/generation against the selected Deployment; test recreate same-named Deployment before old Pods are fully reaped.

### HC-PRR-010 — MEDIUM — Live Swarm task inspect omits ServiceID and NodeID

**Evidence:** `swarm.go:1466-1522` matches live Pods but always calls `kubePodToSwarmTask(p, "", "", slotNumber, ...)`, whereas `SwarmListTasks` passes the actual Swarm service and node IDs at `1360-1397`.

**Impact:** list and inspect disagree for the same task; clients relying on task's associated service/node identity break.

**Remediation/test:** resolve current Deployment's service ID and node's Swarm ID for live inspect; assert list/inspect identity equality.

### HC-PRR-011 — MEDIUM — Bounded per-slot task history does not bound total ConfigMap size after scale churn

**Evidence:** `swarm_health_lifecycle.go:445-457` retires empty slots above desired count only when `TaskHistory` and `RestartHistory` are both empty. Every removed slot with up to five failed tasks survives indefinitely, independent of current desired replicas, and `swarmTaskHistoryLimit` limits per-slot only.

**Impact:** repeated large scale-up/failure/scale-down cycles accumulate stale slots until Kubernetes ConfigMap size limits prevent further lifecycle reconciliation.

**Remediation/test:** enforce bounded aggregate history and retire completed scale-down slots by age/retention; stress tens of thousands of historical slots.

### HC-PRR-012 — MEDIUM — Same-Pod Kubernetes container restarts are not represented as new Swarm task generations

**Evidence:** `swarm_health_lifecycle.go:329-348` updates `CurrentContainerID` when the kubelet restarts the target container in the same Pod but does not replace `CurrentTaskID`, record the previous failed task, or apply Swarm restart accounting. `health_manager.go:322-367` correctly starts a new health monitor, but task identity remains unchanged.

**Impact:** divergence from Docker/Swarm task lifecycle, especially `none`/`MaxAttempts` restart policies. This may be a pre-existing Deployment/Kubelet abstraction limitation, not remediation-specific; investigate scope before final verdict.

**Remediation/test:** establish explicit documented boundaries or per-container-restart task generation tracking; test same-Pod restart with each Swarm policy.

## Notes on earlier provisional findings, assessed from pinned source (without prior ledgers)

- HC-PRR-001: name-only Pod deletion confirmed in `advancePendingSwarmReplacement`; UID fencing also falls through in `setPodHealthConditionForMonitor` because it re-fetches by name after checking UID but ultimately invokes a second name-only GET/UpdateStatus.
- HC-PRR-002: pending assignment chooses first available new Pod by creation timestamp, without a causally verified failed-task replacement relationship.
- HC-PRR-003: check-then-delete Lease release confirmed, without delete preconditions.
- HC-PRR-004: definitive holder mismatch is treated like a transient renewal failure until last-success timeout; monitor Pod readiness mutation is not Lease-fenced.

## Not yet full proof

Static source inspection demonstrates the listed paths. No concurrent live Kubernetes reproduction, full compilation, race test, or multi-arch OCI build was performed in this restart. Previous review files and remediation commit messages remain unread. Finding list is **not frozen**.

## Additional findings — rollout, policy validation and API-state coherence

### HC-PRR-013 — HIGH — Negative Swarm restart-policy fields are accepted, then prevent health-manager operation

**Evidence:** `swarm.go:306-337` copies `Delay`, `MaxAttempts` and `Window` from the user-supplied Swarm ServiceSpec directly to the annotation without checking for negative values. `SwarmCreateService` and `applySwarmServiceUpdate` persist this policy. `health_manager.go:92-113` rejects negative values on decode, and `reconcileHealthMonitors:229-242` sets readiness false/does not monitor. Caller has already received a successful create/update response.

**Impact:** an invalid policy produces a successful API response but a service that can remain permanently unroutable because its readiness gate never turns true.

**Remediation/test:** validate normalized policy before any Kubernetes mutation; return a Docker-compatible bad-request response; test `Delay<0`, `Window<0`, `MaxAttempts<0` on create and update without side effects.

### HC-PRR-014 — HIGH — Swarm restart delay gates readiness but does not delay application execution

**Evidence:** `swarm_health_lifecycle.go:831-881` deletes the failed Pod and records `NotBefore` as an activation timestamp. Deployment/ReplicaSet creates and starts the replacement Pod immediately. `health_manager.go:429-438` and `swarm_health_lifecycle.go:740-756` only hold custom Pod readiness false before `NotBefore`. There is no mechanism to prevent the replacement application process from starting.

**Impact:** a workload configured with a long Swarm restart delay may execute and produce side effects immediately, despite appearing unavailable via ClusterIP; it violates actual delayed *start* semantics and may defeat operational cooldown/backoff.

**Remediation/test:** delay replacement Pod creation/start or explicitly decline to claim Swarm restart-delay equivalence; test that process execution itself cannot begin before delay elapses.

### HC-PRR-015 — MEDIUM — Terminating Pods can still appear as RUNNING Swarm tasks

**Evidence:** `swarm.go:2403-2485` converts Pod phase and health status to task state without inspecting `DeletionTimestamp`. `SwarmListTasks:1358-1397` maps labeled Pods into slots without filtering terminating Pods, so a healthy-but-terminating Pod can continue to be exposed as a live running task.

**Impact:** task list/inspect may report stale running instances during scale-down, update and deletion, distorting service-convergence decisions.

**Remediation/test:** include deletion/desired-state transition in task conversion; test Pod Running with DeletionTimestamp and previously healthy readiness.

### HC-PRR-016 — MEDIUM — Concurrent StartHealthManager invocations can start two manager loops in one adapter

**Evidence:** `health_manager.go:124-151` checks `healthCancel` under mutex, unlocks, calls `acquireHealthManagerLease`, then locks again to store `healthCancel` and launch goroutines. Two callers can both observe nil; because they share `healthLeaseID`, Lease acquisition with the same holder identity may succeed for both. There is no second guard before launching goroutines.

**Impact:** duplicate reconcile/lease-renewal loops within one process despite single-manager intention. Current `cmd/d2k.go` invokes this only once, so impact depends on library/test concurrency or future lifecycle control.

**Remediation/test:** serialize start across Lease acquisition and goroutine activation using explicit state; concurrent-start unit/race test.

### HC-PRR-017 — HIGH — Default rolling update can strand real Pods above the desired Swarm slot range

**Evidence:** `swarm.go:687-707` defaults Swarm-backed Deployments to RollingUpdate with `MaxSurge=1`. `swarm_health_lifecycle.go:413-455` gives unassigned surge Pods new slots above desired count and explicitly never renumbers surviving Pods. `SwarmListTasks:1360-1425` returns tasks only for slots `1..max(status.replicas,spec.replicas)`.

**Deterministic timeline:** a one-replica service starts with old Pod in slot 1; during image rolling update the new surge Pod appears while old is still present and gets slot 2; Kubernetes deletes old Pod, leaving the new running Pod permanently in slot 2; Deployment desired/status settle to one replica; task API reports synthetic `preparing` slot 1 and omits the actual healthy slot-2 Pod. No future unassigned Pod exists to correct the mapping.

**Impact:** ordinary image updates can make `docker service ps`, task inspect and Docker CLI convergence incorrect permanently, even while the new workload is running.

**Remediation/test:** bind rollout successors to retired old slots based on Deployment/ReplicaSet rollout generation and enforce valid active slot range, potentially via an explicit rollout state machine; test 1->1 and 2->2 image updates with MaxSurge=1 and overlap.

### HC-PRR-018 — MEDIUM — PodRunning with a crashed target container may stay reported as STARTING indefinitely

**Evidence:** `swarm.go:2410-2455` reports `starting` when target container is not Running even if Pod phase is Running because an injected sidecar remains alive. `health_manager.go:249-258` does not start or continue probes when target container is absent or waiting. No failed-task history/replacement transition is triggered for such a target crash.

**Impact:** target CrashLoopBackOff/Terminated while sidecar runs can produce misleading perpetual task-starting behavior; Kubelet restarts bypass Swarm policy accounting.

**Remediation/test:** model actual target container lifecycle and distinguish waiting/restarting from terminal failure, including sidecar-present test and simulated kubelet restart.

## Coverage checkpoint

Source inspections: full `health_manager.go`, `health_lease.go`, `healthcheck.go`, `swarm_health_lifecycle.go`, relevant `swarm.go`, `container.go`, `exec.go`, Docker API handlers, `cmd/d2k.go`, deployment RBAC, Docker API version, CI workflow, relevant test suites, and baseline Moby v27.3.1/SwarmKit health/restart logic. Ancillary small changed files were compared against upstream; changes in events/helpers/metrics/volume/exec/images/networks/middleware were predominantly gofmt/import-order and not a source of identified behavior change. Remaining: validation availability, fixed finding list/independent verdict, and prior-ledger reconciliation after freeze.
