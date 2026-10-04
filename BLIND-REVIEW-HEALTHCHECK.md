# Blind Review — Docker Health Checks

## Pinned review target

- Repository: `Proponent-8247/portainer-d2k`
- Feature branch at review start: `feature/docker-healthchecks-upstream`
- Pinned candidate SHA: `6e7d77eaca67e5684b35012714553fde81bdae39`
- Upstream comparison base: `portainer/d2k develop @ 2508dde0d06476029be6aaee999b01c8262b6cc0`
- Initial branch verification: branch was identical to the pinned candidate SHA.
- Base relationship verification: candidate was 88 commits ahead, 0 behind, with merge base exactly `2508dde0d06476029be6aaee999b01c8262b6cc0`.
- Review rule: all implementation reads are pinned to the candidate SHA above. Ledger commits made after verification are not part of the reviewed implementation.

## Blind-review guardrails

- Do not read, search, inspect, or use `AUDIT-HEALTHCHECK.md` until the blind finding list is frozen.
- Do not use prior review conclusions, prior issue lists, chat history, or remediation commit messages as expected findings.
- Do not implement fixes during blind review.
- GitHub is the durable source of truth.

## Review areas completed

- [x] Target SHA / branch verification
- [x] Upstream-base ancestry / divergence verification
- [x] Feature diff and health-monitor architecture reconstruction
- [x] Docker 27.3.1 health monitor source comparison (core timing/result semantics)
- [x] Initial Swarm restart-policy source comparison
- [x] Initial monitor lifecycle/concurrency review

## Review areas remaining

- [ ] Docker container health semantics
- [ ] Docker API readback/list/inspect compatibility
- [ ] Command execution semantics: CMD / CMD-SHELL / NONE / invalid discriminators
- [ ] Timing semantics: interval / timeout / retries / start period / start interval / defaults / minimums
- [ ] Health state machine: failing streak / logs / truncation / recovery / stopped and restarted containers
- [ ] Swarm service/task semantics
- [ ] Restart policy semantics and persistence
- [ ] Kubernetes readiness/lifecycle/controller behavior
- [ ] Monitor creation/cancellation/reconciliation/stale state
- [ ] Concurrency and multi-process behavior
- [ ] Networking/exposure and endpoint gating
- [ ] Persistence/metadata limits and corruption handling
- [ ] Atomicity / partial-mutation behavior
- [ ] RBAC / security / malformed or oversized input
- [ ] Upgrade/migration behavior
- [ ] Interactions with existing non-health functionality
- [ ] Test-suite adequacy and missing negative/lifecycle/concurrency cases
- [ ] Static validation
- [ ] `go mod tidy` diff
- [ ] `go build ./...`
- [ ] `go test ./...`
- [ ] `go vet ./...`
- [ ] `go test -race ./...`
- [ ] Portainer normal PR multi-architecture OCI build
- [ ] Freeze blind findings
- [ ] Reconcile against prior audit only after freeze

## Findings

### HC-BR-001 — HIGH — unhealthy Swarm task can become permanently stranded after a transient replacement-path failure

**Evidence**

- `monitorDockerHealth` treats an unhealthy Swarm task as terminal: after one call to `handleSwarmHealthFailure`, it blocks on `<-ctx.Done()` and never retries replacement.
- `handleSwarmHealthFailure` returns immediately if restart reservation fails, and logs-but-returns if Pod deletion fails.
- The readiness-condition update immediately before replacement is also best-effort: `setPodHealthCondition(...Unhealthy...)` errors are discarded.
- Therefore a transient Kubernetes API failure can leave the same unhealthy Pod present indefinitely. If the readiness-condition write failed while the Pod had previously been marked healthy, and Pod deletion also fails, the workload can remain admitted to Service endpoints while d2k has already classified it unhealthy.

**Affected behavior**

Swarm health-triggered replacement, readiness gating, transient API failure recovery, convergence.

**Proposed remediation**

Model unhealthy replacement as a reconciled state, not a one-shot side effect. Persist/reconstruct enough state to retry readiness withdrawal and replacement until either the task is replaced, the service is deleted/updated, or restart policy definitively forbids another attempt. Do not enter a terminal wait solely because one Kubernetes mutation failed.

**Tests that should be added**

- status update conflict/API failure followed by recovery;
- Pod delete failure followed by recovery;
- both failures while the prior readiness condition is `True`;
- verify endpoint withdrawal and eventual single replacement;
- verify service deletion/update cancels pending retry.

**Validation status:** static review confirmed; live-cluster fault-injection validation pending.

### HC-BR-002 — HIGH — monitor teardown can delete a newer monitor registration and permit duplicate health probes

**Evidence**

- Monitor key is `podUID/containerID`.
- Reconciliation cancels and immediately deletes registrations that temporarily disappear from the desired set.
- A subsequent reconciliation can create a new monitor with the same key before the old goroutine has fully exited.
- The old goroutine's deferred cleanup only tests that an entry with the key exists; it does not verify that the map entry belongs to that goroutine. It therefore deletes the newer monitor's registration.
- The next reconciliation then sees no registration and can launch another monitor for the same container.
- Kubernetes remote exec cancellation is asynchronous enough that this interleaving is realistic during transient status/list gaps or slow exec teardown.

**Affected behavior**

Concurrency, duplicate probes, health-state races, duplicate Swarm restart reservation/deletion, unnecessary exec load.

**Proposed remediation**

Associate each monitor entry with a unique generation/token (or comparable identity) and only remove the registration if the deferred cleanup still owns that exact entry. Prefer cancellation plus ownership-aware teardown; add tests that force delayed goroutine exit across reconciliation gaps.

**Tests that should be added**

- desired monitor disappears for one reconcile then reappears with the same Pod UID/container ID while old exec teardown is blocked;
- assert exactly one active monitor/exec loop after old goroutine exits;
- unhealthy duplicate-monitor scenario must reserve/delete at most once.

**Validation status:** static concurrency review confirmed; deterministic race test pending.

### HC-BR-003 — MEDIUM — restart-attempt history is consumed before Pod replacement succeeds

**Evidence**

- `reserveHealthRestart` appends and persists the restart timestamp before the restart delay and before Pod deletion.
- If the later Pod delete fails, the history entry is not rolled back.
- With `MaxAttempts`, a transient Kubernetes deletion failure can consume an attempt even though no replacement occurred.
- Combined with HC-BR-001, the current monitor then stops retrying entirely.

**Affected behavior**

Swarm `MaxAttempts`, restart window accounting, atomicity.

**Proposed remediation**

Count a restart at the same semantic point SwarmKit records a replacement task, or make reservation transactional/recoverable so a failed replacement does not permanently consume an attempt. Ensure concurrent instances still cannot double-count one replacement.

**Tests that should be added**

- `MaxAttempts=1` plus injected Pod delete failure;
- verify no attempt is permanently consumed until a replacement is actually committed;
- retry succeeds after transient failure without exceeding the configured limit.

**Validation status:** static review confirmed.

### HC-BR-004 — MEDIUM — start-period boundary scheduling is earlier than Docker 27.3.1

**Evidence**

- Docker 27.3.1 computes the next interval only after each probe. While the container is still `starting` and current time is before `StartPeriod`, it schedules the full `StartInterval`, even if that timer fires after the start-period boundary.
- d2k instead clamps `StartInterval` to the exact remaining start-period duration.
- Example: `StartPeriod=10s`, `StartInterval=9s`, first failing probe near 9s. Docker schedules the next probe about 9s later; d2k schedules it about 1s later at the boundary.

**Affected behavior**

Docker timing fidelity, failure cadence, load, time-to-unhealthy.

**Proposed remediation**

Mirror Moby's interval selection: choose start vs normal interval at the end of the preceding probe without clamping to the start-period boundary.

**Tests that should be added**

Boundary-crossing start-interval cases with deterministic clock/timer control, including long-running probes.

**Validation status:** confirmed against `moby/moby v27.3.1 daemon/health.go`.

### HC-BR-005 — MEDIUM — d2k counts Kubernetes exec setup time against Docker health timeout

**Evidence**

- Docker 27.3.1 gives health exec startup a separate 30-second startup timeout and starts the configured health `Timeout` after the exec process reports started.
- d2k wraps the entire Kubernetes SPDY exec operation in `context.WithTimeout(healthTimeout)`; connection negotiation / API-server / kubelet exec setup is therefore charged against the probe timeout.
- Slow control-plane or kubelet exec setup can cause d2k to report a failed health check even though Docker would still be waiting for the health command to start.

**Affected behavior**

Timeout semantics, infrastructure latency sensitivity, false unhealthy transitions.

**Proposed remediation**

Separate exec-start establishment from command-runtime timeout as far as Kubernetes exec APIs permit, with an independent bounded setup timeout. If exact process-start signaling is unavailable, document the unavoidable approximation and avoid claiming exact timeout compatibility.

**Tests that should be added**

Delayed exec establishment with a fast successful command; distinguish setup timeout from command timeout.

**Validation status:** confirmed against `moby/moby v27.3.1 daemon/health.go`.

### HC-BR-006 — LOW — truncated health output does not match Docker readback

**Evidence**

- Docker's 4096-byte health output buffer records truncation and appends `...` on readback.
- d2k caps stored bytes at 4096 but silently drops the remainder and returns no truncation marker.

**Affected behavior**

`docker inspect` health log output compatibility and diagnostics.

**Proposed remediation**

Track truncation and append the same marker Docker uses.

**Tests that should be added**

4095/4096/4097+ byte stdout/stderr cases.

**Validation status:** confirmed against `moby/moby v27.3.1 daemon/health.go`.



### HC-BR-007 — MEDIUM — default unlimited restart policy grows restart-history annotation without bound

**Evidence**

- `reserveHealthRestart` appends a timestamp to `AnnotationHealthRestartHistory` on every permitted health-triggered restart.
- History is pruned only when `policy.Window > 0`.
- The default Swarm restart policy has `MaxAttempts=0` and `Window=0`, meaning unlimited retries and no pruning.
- Consequently the JSON timestamp array grows for the lifetime of the service. Kubernetes annotations have a finite aggregate size; eventually Deployment updates used to reserve another restart will be rejected.
- Once reservation updates begin failing, HC-BR-001 turns the unhealthy task into a stranded terminal monitor state.

**Affected behavior**

Long-running flapping services, persistence, metadata limits, default Swarm restart policy.

**Proposed remediation**

Do not persist history when it is not needed to enforce a limit. If `MaxAttempts==0`, no restart history is required. When history is required, bound/prune it according to the restart policy and validate aggregate annotation size before mutation.

**Tests that should be added**

- unlimited/default restart policy performs many restarts without growing history;
- bounded policy prunes exactly what is needed;
- near-limit annotations do not become unrecoverable.

**Validation status:** static review confirmed.

### HC-BR-008 — HIGH — Swarm MaxAttempts/window accounting is service-global instead of per replica slot

**Evidence**

- d2k stores one restart-history array on the Deployment and `reserveHealthRestart` takes only the Deployment name and policy; Pod/task slot identity is not part of the history key.
- SwarmKit restart history is keyed by `SlotTuple`: replicated-service restart limits are enforced independently per service slot.
- With a replicated service and `MaxAttempts=1`, one unhealthy replica consumes the sole d2k history entry. A different replica that later becomes unhealthy is denied replacement even though its own slot has never restarted.
- Simultaneous unhealthy replicas likewise compete for one shared attempt budget.

**Affected behavior**

Scaled services, multiple replicas, simultaneous failures, `MaxAttempts`, restart window.

**Proposed remediation**

Persist restart accounting per stable replicated-service slot (and per node for global semantics if/when supported), reset by service spec version as SwarmKit does, and make reservation concurrency-safe per slot.

**Tests that should be added**

- 2+ replicas with `MaxAttempts=1`: each slot independently receives one restart;
- simultaneous unhealthy replicas;
- window expiry independently per slot;
- service update resets each slot's old-spec history.

**Validation status:** confirmed by static comparison with SwarmKit restart supervisor.

### HC-BR-009 — HIGH — health-triggered replacement can renumber surviving task slots

**Evidence**

- `SwarmListTasks` reconstructs slot numbers by sorting the *currently existing* Pods by creation timestamp, then assigning slots 1..N.
- A health-triggered replacement deletes an unhealthy Pod and lets the Deployment create a new Pod.
- For three replicas A/B/C mapped to slots 1/2/3, replacing A leaves B/C/new-D; sorting current Pods remaps B→1, C→2, D→3. Surviving task slot identities change even though those tasks were never replaced.
- Real Swarm replicated-service slots are stable identities across task replacement and are the basis for restart accounting.

**Affected behavior**

Task identity, `docker service ps`, scaled services, replacement semantics, correlation with restart policy/history.

**Proposed remediation**

Introduce a stable slot identity model rather than deriving slots from the current Pod age ordering. Replacement tasks must inherit the failed task's slot while unaffected tasks retain theirs.

**Tests that should be added**

- replace oldest, middle, and newest replica in a 3-replica service and assert unaffected slot numbers remain stable;
- repeated replacement in one slot;
- scale down/up behavior with stable slot identity.

**Validation status:** static review confirmed.

### HC-BR-010 — MEDIUM — failed health tasks disappear instead of remaining inspectable task history

**Evidence**

- Health replacement is implemented by deleting the failed Kubernetes Pod.
- `SwarmListTasks` synthesizes tasks only from current Deployment state/current Pods, and `SwarmInspectTask` can inspect only extant Pods.
- After health-triggered deletion, the failed task identity and its `failed / container unhealthy` state disappear.
- Swarm retains historical tasks (subject to task-history retention), and those failed tasks are visible through service/task APIs. Health-triggered failures are operationally important history.

**Affected behavior**

Task state history, task inspect, `docker service ps`, postmortem diagnostics.

**Proposed remediation**

Persist bounded task-history metadata/resources sufficient to preserve replaced task identity, slot, terminal health state, timestamps and error until the configured retention policy expires.

**Tests that should be added**

- unhealthy task remains visible as failed after replacement;
- replacement has a distinct task ID but same slot;
- inspect old failed task works until retention cleanup;
- retention bound is enforced.

**Validation status:** static review confirmed.

### HC-BR-011 — MEDIUM — restart delay is applied before shutting down the unhealthy task

**Evidence**

- d2k marks the readiness gate false, reserves history, sleeps for `RestartPolicy.Delay`, and only then deletes the unhealthy Pod.
- SwarmKit marks the old task desired shutdown and creates the replacement task before applying the delayed start to the replacement.
- Thus d2k leaves the failed process alive for the whole restart delay. VIP readiness normally removes it from Service endpoints, but the process/resources continue running and any direct Pod reachability remains.
- If readiness withdrawal fails, HC-BR-001 makes this mismatch externally reachable for the entire delay.

**Affected behavior**

Restart delay semantics, resource usage, failure isolation, direct Pod access.

**Proposed remediation**

Separate old-task shutdown from replacement start delay: withdraw/terminate the failed task promptly, create/reserve the replacement identity, then delay the replacement becoming active.

**Tests that should be added**

- assert unhealthy old task is shut down without waiting for restart delay;
- assert replacement start is delayed;
- test readiness-update failure and cancellation during delay.

**Validation status:** confirmed by static comparison with SwarmKit restart supervisor.

### HC-BR-012 — MEDIUM — corrupted persisted restart-policy/history metadata fails open or changes policy

**Evidence**

- Invalid JSON in `AnnotationHealthRestartHistory` is silently ignored, treating the prior history as empty. A corrupted history therefore bypasses configured `MaxAttempts`.
- Invalid JSON in `AnnotationSwarmRestartPolicy` silently falls back to the default policy (`Condition=any`, default delay). A service originally configured with `Condition=none` or a bounded policy can therefore acquire materially different restart behavior after metadata corruption/manual mutation.
- The review scope explicitly includes corrupted annotations and manual Kubernetes modifications.

**Affected behavior**

Persistence integrity, restart limits, manual Kubernetes changes, upgrade/migration safety.

**Proposed remediation**

Treat malformed internal restart metadata as a surfaced/reconcilable error rather than silently selecting a more permissive policy. Preserve safety by failing closed for replacement until metadata is repaired or deterministically reconstructed from authoritative service metadata.

**Tests that should be added**

- malformed policy JSON for each original condition;
- malformed history with `MaxAttempts` already consumed;
- API/readiness behavior while metadata is invalid;
- recovery after metadata repair.

**Validation status:** static review confirmed.

### HC-BR-013 — MEDIUM — multiple d2k replicas create split-brain health monitors

**Evidence**

- Health-manager ownership is process-local; there is no leader election, lease, per-Pod monitor ownership, or fencing token.
- Every d2k process reconciling the namespace will independently execute the same health commands and write the same readiness condition.
- On unhealthy results, instances can concurrently reserve restart history and delete the same Pod. Optimistic Deployment updates serialize the writes but do not make the health decision single-owner; with bounded retries this can consume multiple attempts for one task, and with unlimited retries it still duplicates probes/mutations.
- The shipped manifest uses one replica, but the implementation does not reject or safely coordinate additional replicas.

**Affected behavior**

High availability/scaling of d2k, duplicate exec load, restart accounting, health state consistency.

**Proposed remediation**

Either explicitly enforce/document single-active-instance operation or add Kubernetes Lease-based leader/monitor ownership with fencing so only one process owns health monitoring/replacement for a workload at a time.

**Tests that should be added**

- two managers against one fake/live namespace execute one logical probe stream;
- simultaneous unhealthy result consumes one restart and one deletion;
- leadership handoff reconstructs state safely.

**Validation status:** static design review confirmed.

## Validation status

Static review remains in progress. Exact-pinned CI metadata exists but its logs/results have intentionally not yet been inspected, to keep validation from biasing the independent static review.

## Review log

### Checkpoint 001 — target verification

The feature branch was verified to point exactly at the pinned candidate before any review work. The pinned candidate is a descendant of the stated upstream base with no commits behind the base. Blind-review ledger initialized before implementation analysis.

### Checkpoint 002 — initial architecture / semantics / concurrency pass

Reviewed the pinned candidate's health monitor, container and Swarm translation paths, readiness gating, restart-history persistence, API readback, and Kubernetes exec path. Independently compared core timing, output, timeout, and restart-policy behavior against Moby 27.3.1 and SwarmKit source. Recorded HC-BR-001 through HC-BR-006. No prior audit material has been read.

### Checkpoint 003 — multi-replica / persistence / task identity pass

Reviewed restart history scope and growth, task slot synthesis, failed-task retention, delayed replacement semantics, corrupted persisted metadata, and multi-instance operation. Additional findings follow.
