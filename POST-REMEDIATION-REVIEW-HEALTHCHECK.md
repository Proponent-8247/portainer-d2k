# Docker health checks — independent post-remediation review

**Review status:** COMPLETE — independent findings frozen, all validation gates passed, prior-ledger reconciliation completed. **Final verdict: NOT READY FOR UPSTREAM.**

**Authoritative final summary:** pinned candidate `96d3f9209ac48dacacb5dd8330e6d86129eb674c`; 18 frozen independent findings (10 HIGH / 8 MEDIUM); one later classified as a documented acceptable approximation; all prescribed gofmt/tidy/build/test/vet/race and amd64+arm64 OCI gates passed via [run 37976451131](https://github.com/Proponent-8247/portainer-d2k/actions/runs/37976451131). Reconciliation and full supporting evidence appear at the end of this ledger. No product fixes made.

**Historical checkpoints:** Earlier in-progress placeholders and interim assessments below document the review sequence; they do not supersede the final conclusion or final validation results.

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
- [x] Reconstruction of candidate lifecycle architecture from source
- [x] Docker health semantics and exec security
- [x] Health monitor ownership, concurrency, cancellation and cross-process Lease fencing
- [x] Persisted Swarm slots/task history/restart accounting and all replacement transitions
- [x] Standalone/list/inspect and Swarm Docker API paths
- [x] Routing, reachability and update atomicity
- [x] Kubernetes controller interaction, upgrade behavior and API 1.44 negotiation
- [x] Adversarial test coverage and explicit HIGH-risk race timelines
- [x] Freeze independent findings
- [x] Validation gates: gofmt, go mod tidy clean, build, test, vet, race, amd64/arm64 OCI
- [x] Prior-ledger reconciliation **only after freeze**

## Architectural reconstruction

*Historical initialization checkpoint; the completed 17-point source reconstruction is recorded below.*

## Independent findings (append before freeze)

*Historical initialization checkpoint; the 18 frozen findings and final reconciliation appear below.*

## Validation

*Historical initialization checkpoint. All final CI gates passed after freeze; see validation results below.*

## Independent upstream submission verdict

**NOT READY** — see pre-reconciliation independent verdict and final reconciled verdict below.

## Reconciliation with prior work (only after freeze)

*Historical embargo checkpoint. No prior ledgers were opened until after freeze and successful CI validation; full reconciliation appears at the end.*


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

---

# Independent findings frozen — 2026-10-09

**Immutable code snapshot:** `96d3f9209ac48dacacb5dd8330e6d86129eb674c`. **Comparison:** `2508dde0d06476029be6aaee999b01c8262b6cc0` (upstream `portainer/d2k:develop`). The restarted static finding set `HC-PRR-001` through `HC-PRR-018` is hereby **FROZEN**. Preserve the original text above verbatim during reconciliation; later notes must be appended separately. Prior audit and remediation ledgers have not been consulted at the time of this freeze. Important caveat: the restart was performed within the same conversation that contained the first four provisional findings; perfect cognitive isolation from the earlier provisional review is therefore impossible, despite independent re-verification from source.

## Frozen count / severity

| Severity | Count | IDs |
|---|---:|---|
| CRITICAL | 0 | — |
| HIGH | 10 | 001, 002, 003, 005, 007, 008, 009, 013, 014, 017 |
| MEDIUM | 8 | 004, 006, 010, 011, 012, 015, 016, 018 |
| LOW | 0 | — |
| INFO | 0 | — |

**Independent verdict before prior-ledger reconciliation: NOT READY FOR UPSTREAM SUBMISSION.** In particular, `HC-PRR-017` exposes a default normal-rollout slot hole that can make ordinary updates permanently invisible to Swarm clients, and `HC-PRR-005` exposes crash/recovery state inconsistency. Green compilation or unit tests would not prove either path safe.

## Complete architectural reconstruction — seventeen required questions

1. **Input:** Docker container create JSON `Healthcheck` and Swarm `TaskTemplate.ContainerSpec.Healthcheck`, decoded in corresponding API handlers and adapter.
2. **Persistence:** Deployment metadata `d2k.portainer.io/healthcheck` annotation; lifecycle ConfigMap for service state.
3. **Monitor creation:** periodic one-second reconciler lists managed Deployments and their Running Pods, finds workload container state and starts monitor goroutines.
4. **Monitor ownership:** in-process `healthMonitors`, `healthCurrent`, registration tokens under mutex; cross-process namespace Lease.
5. **Pod/container fences:** Pod UID + container ID in monitor key, token checks around health-state writes; Pod UID checked before readiness helper call but not inside downstream name-only status updater.
6. **Publication:** in-memory Docker Health object with status, streak, last-five logs, exposed via list/inspect paths. No persisted probe results across d2k process restart.
7. **Standalone versus Swarm:** standalone retains Docker-style reachability and no automatic health-triggered restart; Swarm gates Pod readiness and initiates Pod deletion and task replacement.
8. **Stable slots:** ConfigMap `Slots` and Pod slot label; preferred remembered Pod UID, existing label, committed-pending replacement, next vacant slot; deployment deletion-cost annotation biases scale down.
9. **Failed history:** per-slot last-five `TaskHistory` records in ConfigMap, synthesized into Swarm list/inspect.
10. **Restart attempts/window:** per-slot timestamp list, pruned for positive Window, counted when delete commit recorded, no history for unlimited attempts.
11. **Unhealthy replacement trigger:** monitor sets health `unhealthy`, shuts down probes for Swarm task; lifecycle reconciler marks Pod unready and creates Pending intent after policy checks.
12. **Durable intent:** persisted `Pending` contains failed Pod UID/task/container, replacement task ID, policy, failure timestamp, delete-commit and NotBefore.
13. **Restart delay:** `ActivationNotBefore` suppresses readiness; underlying replacement process can start early (finding 014).
14. **Convergence:** controller creates successor Pod; slot assigner claims new unassigned Pod for committed pending slot, replaces task ID, clears Pending.
15. **Single-owner election:** Kubernetes coordination Lease `d2k-health-manager` per namespace acquired before HTTP server startup; renew interval 5 seconds, duration 15 seconds.
16. **Handoff/loss:** lease GET/update with optimistic conflicts; renewal failure eventually cancels manager; no self-healing reacquisition; release name-only GET then DELETE (findings 003, 004, 007).
17. **Cleanup:** stale in-memory health states removed after monitor desired-set reconciliation; lifecycle orphan ConfigMaps removed on service absence; per-slot history remains for scaled-down slots (finding 011).

## Adversarial test gaps not exercised by candidate

- Multiple concurrent replacement slots; Kubernetes Pod arrival order and equal timestamps.
- Crash after successful Pod DELETE but before persisted delete commit; success-with-client-timeout ambiguity.
- Default RollingUpdate `MaxSurge=1` with one replica and two replicas; old/new ReplicaSet coexistence.
- Lease handoff race between holder GET and DELETE; stale owner updates Pod readiness after successor acquisition.
- d2k outage >=15s followed by Kubernetes recovery; automatic manager re-election and probe reactivation.
- Same-named service recreated while old ReplicaSet/Pods still present.
- Health annotation corruption after gate became healthy; corrupted restart-policy annotation.
- Negative Swarm restart-policy fields; atomic create/update bad-request rejection.
- Running application vs configured Swarm RestartPolicy.Delay; not just Pod ready condition.
- Container restarts inside same Pod and process crashes with sidecar still Running.
- Long-running exec that starts immediately with short Docker health timeout.
- Task list/inspect ServiceID, NodeID, terminating task state and history consistency.
- Service scale churn and aggregate ConfigMap growth.
- Cluster-level integration with real Kubernetes controllers and EndpointSlice data plane.
- End-to-end Docker API 1.44 client compatibility beyond two version/ping shape unit tests.

## Validation handoff

Local Git checkout cannot resolve `github.com` from the container (DNS failure), and the GitHub connector is primarily a repository source/write interface. Repository-wide `gofmt -l .`, clean `go mod tidy`, `go build ./...`, `go test ./...`, `go vet ./...`, `go test -race ./...`, and amd64/arm64 OCI validation have **not** been run on this pinned candidate as part of the restarted review. Attempt a non-product, isolated validation workflow if permitted; otherwise retain **BLOCKED / NOT VERIFIED** for each gate, not PASS.

**Embargo released only after this commit is durable.** Prior ledgers may be consulted only for reconciliation after validation status has been recorded. Neither source code nor tests were modified in the feature candidate.

## Executable validation checkpoint — isolated GitHub Actions

- Local container DNS cannot resolve `github.com`; no local clone.
- Created validation-only branch `review/pinned-health-validation-96d3f92` directly from pinned candidate `96d3f9209ac48dacacb5dd8330e6d86129eb674c`. It adds only a GitHub Actions YAML; **no Go source/tests modified**.
- First attempt [run 37976378393](https://github.com/Proponent-8247/portainer-d2k/actions/runs/37976378393) failed before checks because checkout depth 1 made `HEAD^` unavailable. Harness-only issue, **not a product defect**.
- Fixed the validation harness without rewriting its branch history. New [run 37976451131](https://github.com/Proponent-8247/portainer-d2k/actions/runs/37976451131) at validation-only SHA `25f5fce00b4f7cb7f4ea1d81e586d22b3db1b4f3`.
- GitHub Action provenance step passed: full history, pinned candidate exists, `git diff --name-only <pinned> HEAD` contains **only** `.github/workflows/d2k-pinned-post-remediation-validation.yml`.
- Validated completed gates as of this checkpoint: repository-wide `gofmt -l .` **PASS**, `go mod tidy` clean diff **PASS**.
- Go build, unit test, vet, race test, amd64+arm64 OCI build were **IN PROGRESS** at this checkpoint; do not claim PASS until run finalizes.
- Prior ledgers remain unread while validation is still running.

## Final independent validation results — before prior-ledger reconciliation

The isolated pinned-source workflow [run 37976451131](https://github.com/Proponent-8247/portainer-d2k/actions/runs/37976451131) completed successfully: both `go-gates` job 113975862701 and `multiarch-oci` job 113975862211 ended `success`. The full-history provenance gate proved the only diff from candidate `96d3f9209ac48dacacb5dd8330e6d86129eb674c` was a temporary CI workflow YAML on the separate validation branch. There were **no product-code or test changes** and no registry push.

| Gate | Result |
|---|---|
| `gofmt -l .` across repository | **PASS**, no unformatted files |
| `go mod tidy` and clean `git diff --exit-code -- go.mod go.sum` | **PASS** |
| `go build ./...` | **PASS** |
| `go test ./...` | **PASS** |
| `go vet ./...` | **PASS** |
| `go test -race ./...` | **PASS** |
| Docker Buildx multi-platform `linux/amd64,linux/arm64` OCI build/export | **PASS**, nonempty OCI output confirmed |

The initial test workflow [run 37976378393](https://github.com/Proponent-8247/portainer-d2k/actions/runs/37976378393) failed its self-check due shallow Git checkout. This was corrected within the disposable validation-only workflow, after which all gates passed. It does **not** indicate a product defect.

**Independent upstream verdict before reading older reviews: NOT READY.** Successful Go validation and multiarch builds do not exercise the deterministic one-replica rolling-update slot orphan (HC-PRR-017), crash between Pod DELETE and ConfigMap commit (HC-PRR-005), or cross-owner lease mutation (HC-PRR-003). All frozen findings remain open pending reconciliation.

**Independent phase ended here.** Prior-ledger reconciliation may now begin. The original 18 findings will remain unchanged; any corrections, classification and final conclusion must be separately appended.

---

# Post-freeze reconciliation against earlier audits and remediation

**Reconciliation opened only after:** independent findings `HC-PRR-001..018` were frozen in commit `3e1d514c92c52d3a07f8f50cd39541b7c5999ecb`, and final independent validation + verdict were committed in `04b8bc8f7b3c4789521cb577bbcac349a2b5c962`.

Reviewed the two prior ledgers `AUDIT-HEALTHCHECK.md` (11 original audit findings) and `BLIND-REVIEW-HEALTHCHECK.md` (19 blind-review findings marked fixed) **only now**. Compared remediation ledger claims to pinned post-remediation source, Docker/Moby 27.3.1 and SwarmKit. Also checked select original upstream-base source paths to distinguish pre-existing quirks from remediation-introduced regressions. The original independent finding texts remain unmodified.

## Finding-by-finding reconciliation (exact IDs preserved)

| Independent finding | Classification | Earlier relationship and reconciliation |
|---|---|---|
| **HC-PRR-001 (H)** | **Prior issue incompletely remediated** | HC-BR-014 promised Pod UID/container fencing and HC-BR-001 durable safe replacement. New name-only `setPodHealthCondition` and `Pods.Delete` allow a stale generation to mutate a replacement of the same Pod name. Requires UID preconditions; this is a narrowly timed race, not proven in live Kubernetes. |
| **HC-PRR-002 (H)** | **Prior issue incompletely remediated** | HC-BR-009 promised stable slots and replacement inheritance. Persisted slots fixed the one-replacement case, but concurrent pending slots still greedily bind arbitrary newly created Pods without verified lineage. |
| **HC-PRR-003 (H)** | **Regression introduced by remediation** | HC-BR-013 introduced the Lease to stop split-brain. `releaseHealthManagerLease` GET-then-name-only-DELETE now creates a takeover race and can delete the successor Lease; this code did not exist on upstream base. |
| **HC-PRR-004 (M)** | **Prior issue incompletely remediated** | HC-BR-013 promised exclusive management and fenced mutations. Definitive ownership mismatch does not immediately cancel the old manager; readiness paths do not all recheck Lease ownership. |
| **HC-PRR-005 (H)** | **Prior issue incompletely remediated** | HC-BR-001/003/009's durable replacement model does not close the crash-after-delete-before-persist hole; pending intent and the new replacement's actual slot ownership can diverge permanently. |
| **HC-PRR-006 (M)** | **Independently rediscovered — explicitly accepted compatibility limitation** | Exactly the bounded remote-exec setup-vs-command timeout approximation from HC-BR-005. README already documents lack of process-start signal and total-budget compromise. **Not an undisclosed regression or independently a new upstream blocker**, provided claims stay accurate. Frozen text retained. |
| **HC-PRR-007 (H)** | **Regression introduced by remediation** | New HC-BR-013 Lease loop cancels health management after renewal expiration but never re-elects/restarts, while HTTP serving continues. Lease-based health lifecycle needs a recovery state machine or process failure. |
| **HC-PRR-008 (H)** | **Prior issue incompletely remediated** | HC-BR-012 fixed fail-closed handling for corrupt lifecycle/restart-policy JSON. Corrupt **Healthcheck** annotation is treated like disabled health instead, leaving an existing true readiness condition untouched. |
| **HC-PRR-009 (H)** | **Genuinely new finding — pre-existing selector flaw amplified** | Original upstream list paths also match Pods by `app`/d2k labels without owner chain verification; post-remediation per-service lifecycle state now treats an old Pod as owning a new service slot after same-name recreate. Root selector weakness predates remediation; severity increases under persisted identity. |
| **HC-PRR-010 (M)** | **Genuinely new finding — pre-existing API inconsistency** | Original upstream `SwarmInspectTask` already calls the converter with empty ServiceID/NodeID, unlike task list. HC-BR-010 expanded historical inspect but did not correct live inspect. **Do not label as remediation regression.** |
| **HC-PRR-011 (M)** | **Prior issue incompletely remediated; scope qualified** | HC-BR-010 bounds *per-slot* task history at five, but empty retired high-numbered slots are retained when history remains. Growth is bounded by the historical maximum slot number and the per-slot limits if peak replica count stays fixed. The original text's suggestion that identical repeated scale cycles alone grow slot count indefinitely is overstated. Large/high-water scaling still risks ConfigMap limits; reassess as a scalability/retention issue, not an immediate unbounded leak at a fixed high-water mark. |
| **HC-PRR-012 (M)** | **Genuinely new lifecycle-fidelity gap** | HC-BR-014 handled same-Pod monitor *fencing*, not how Kubelet-initiated target restarts should produce Swarm task generations / restart budgets. The underlying Deployment `RestartPolicyAlways` abstraction also predates health remediation. |
| **HC-PRR-013 (H)** | **Prior issue incompletely remediated** | HC-BR-012 strictly rejects corrupt persisted restart policy but `normalizeSwarmHealthRestartPolicy` accepts negative user-supplied fields, then persists an annotation the strict decoder rejects after successful API response. Invalid user input path missing pre-mutation validation. |
| **HC-PRR-014 (H)** | **Prior issue incompletely remediated; declared approximation** | HC-BR-011 fixed delaying *old-Pod deletion*, but new strategy applies `Delay` to readiness/admission, not process startup. The README says “replacement admission,” so that approximation is somewhat disclosed; it still diverges materially from real SwarmKit start-delay semantics and must be consciously accepted/limited or fixed before claiming restart-policy parity. |
| **HC-PRR-015 (M)** | **Genuinely new finding — pre-existing task converter gap** | Upstream-base Pod-to-task conversion ignored `DeletionTimestamp`; its task reporting could stay RUNNING during termination. Not evidence of regression from the health change. |
| **HC-PRR-016 (M)** | **Regression introduced by remediation** | HC-BR-013 added `StartHealthManager`; unlock-acquire-relock allows two concurrent callers of the same adapter to start duplicate manager loops sharing the same Lease identity. Normal executable calls Start once, so this is currently a lower-likelihood concurrency hazard. |
| **HC-PRR-017 (H)** | **Regression introduced by remediation — key upstream blocker** | HC-BR-009 replaced age-reconstructed task slots with persistent “never renumber survivors” slots but did not integrate the unchanged default Kubernetes `MaxSurge=1` rollout. A one-replica 1→1 rollout can strand the new Pod permanently in slot 2 while task list displays only synthetic slot 1. This did **not** happen with original recompute-by-age numbering, though that original strategy broke other slot invariants. |
| **HC-PRR-018 (M)** | **Prior issue incompletely remediated** | HC-AUD-004/HC-BR-014 improved target-container state and stale-monitor fencing. Sidecar-alive/target-crashloop handling still reports indefinite STARTING without Swarm task-level restart transition. Kubelet `Always` restarts bypass Swarm budgets. |

**Classification totals (frozen 18):**
- Prior issue incompletely remediated: **9** (`001,002,004,005,008,011,013,014,018`).
- Regression introduced by remediation: **4** (`003,007,016,017`).
- Genuinely new, including confirmed pre-existing issues not recorded by earlier audits: **4** (`009,010,012,015`).
- Independently rediscovered/documented and accepted compatibility limit: **1** (`006`).
- False positive after reconciliation: **0 definitively**. Qualification: `011`'s claimed unbounded same-size churn is not established; live reproduction is still required for several races.

**Frozen severity counts remain** 0 CRITICAL / **10 HIGH** / **8 MEDIUM** / 0 LOW / 0 INFO, because source-derived frozen findings are immutable. In the **post-reconciliation triage**, HC-PRR-006 is a disclosed accepted approximation rather than a remediation blocker; HC-PRR-011's likelihood/growth description is narrowed. Treating all 18 as newly introduced defects would be wrong: several are upstream baseline gaps.

## Coherence verdict

The remediation substantially improved important invariants: monitor tokens, state and Pod UID/container-ID keys, durable per-slot history, consistent no-health standalone behavior, docker-compatible result logs, per-slot MaxAttempts and corruption checks. However, **the full cross-component lifecycle is not coherent under common rolling updates, process crashes, leadership changes and selected invalid-input paths**. Critical invariants are enforced locally, but the boundary between Controller-owned Pod creation and d2k-owned task slots is not causally fenced. The same is true of the Lease ownership handoff and the Pod DELETE/ConfigMap-commit boundary.

The most decisive **upstream blockers** are HC-PRR-017 (ordinary rollout slots), HC-PRR-005 (crash convergence), HC-PRR-003/007 (lease ownership and health-manager liveness), HC-PRR-002 (concurrent replacements), HC-PRR-008/013 (fail-closed/validation gaps). HC-PRR-001 and HC-PRR-009 warrant UID/lineage fencing. HC-PRR-006 can remain a precisely described interoperability limitation rather than requiring strict Moby parity.

## Tests and validation final state

All prescribed static Go gates and dual-architecture OCI build **PASSED** on unchanged pinned candidate source through [validation run 37976451131](https://github.com/Proponent-8247/portainer-d2k/actions/runs/37976451131). These are repository build/unit/race gates, not live-controller or fault-injection proof. No test failures were observed and thus no validation-generated HC-PRR finding was needed. No integration suite in the pinned source demonstrates the failure-injection interleavings or the standard one-replica MaxSurge rollout; that remains the decisive missing evidence.

**Final recommendation: NOT READY for submission to `portainer/d2k`.** Do not apply fixes as part of this review. Implement separately with one conceptual fix per commit and deterministic Pod/Lease/controller fault-injection tests, then commission another independent review of the corrected SHA. Never rewrite published branch history.
