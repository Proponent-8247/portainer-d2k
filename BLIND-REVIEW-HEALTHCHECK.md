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

- [x] Docker container health semantics
- [x] Docker API readback/list/inspect compatibility
- [x] Command execution semantics: CMD / CMD-SHELL / NONE / invalid discriminators
- [x] Timing semantics: interval / timeout / retries / start period / start interval / defaults / minimums
- [x] Health state machine: failing streak / logs / truncation / recovery / stopped and restarted containers
- [x] Swarm service/task semantics
- [x] Restart policy semantics and persistence
- [x] Kubernetes readiness/lifecycle/controller behavior
- [x] Monitor creation/cancellation/reconciliation/stale state
- [x] Concurrency and multi-process behavior
- [x] Networking/exposure and endpoint gating
- [x] Persistence/metadata limits and corruption handling
- [x] Atomicity / partial-mutation behavior
- [x] RBAC / security / malformed or oversized input
- [x] Upgrade/migration behavior
- [x] Interactions with existing non-health functionality
- [x] Test-suite adequacy and missing negative/lifecycle/concurrency cases
- [x] Static validation
- [x] `go mod tidy` diff
- [x] `go build ./...`
- [x] `go test ./...`
- [x] `go vet ./...`
- [x] `go test -race ./...`
- [x] Portainer normal PR multi-architecture OCI build (executed; normal job fails due HC-BR-019)
- [x] Freeze blind findings
- [x] Reconcile against prior audit only after freeze

## Remediation ledger

This committed file is the authoritative remediation state for the blind-review findings.

**Totals:** Open: **0** · In progress: **0** · Fixed: **19**

| Finding | Status | Work |
|---|---|---|
| HC-BR-001 | **FIXED** | Durable replacement intent is persisted and reconciled until convergence; transient readiness/delete/API failures retry instead of stranding the task. |
| HC-BR-002 | **FIXED** | Monitor registrations carry unique tokens; stale teardown can remove only the exact registration it owns. |
| HC-BR-003 | **FIXED** | Per-slot restart history is committed only after failed-task deletion is accepted; failed delete attempts do not consume MaxAttempts. |
| HC-BR-004 | **FIXED** | Start-period scheduling now mirrors Moby: the full start interval selected after the prior probe is not clamped to the boundary. Regression: `TestDockerHealthStartPeriodUsesFullStartIntervalAcrossBoundary`. |
| HC-BR-005 | **FIXED — bounded platform approximation** | client-go exposes no remote-process-start signal. d2k gives exec establishment a separate bounded allowance, documents the unavoidable timing approximation, and no longer claims byte-for-byte timeout timing parity. |
| HC-BR-006 | **FIXED** | Health output preserves Docker’s 4096-byte buffer behavior and appends `...` when truncated. Boundary regressions cover 4095/4096/4097 bytes. |
| HC-BR-007 | **FIXED** | Unlimited policies persist no restart history; bounded policies prune per window and store only required per-slot attempts. |
| HC-BR-008 | **FIXED** | Restart accounting moved to persistent stable slot state, independently enforcing MaxAttempts/window for each replica slot. |
| HC-BR-009 | **FIXED** | Stable slots persist in lifecycle state and Pod labels; initial assignment is deterministic and replacement inherits the failed slot without renumbering survivors. |
| HC-BR-010 | **FIXED** | Failed tasks persist as bounded per-slot task history and remain available to list/inspect after replacement. |
| HC-BR-011 | **FIXED** | Failed Pod is withdrawn/deleted first; restart delay is persisted as replacement activation-not-before instead of delaying failed-task shutdown. |
| HC-BR-012 | **FIXED** | Corrupt lifecycle/restart-policy metadata surfaces as an error and marks health readiness false rather than falling back to permissive restart behavior. |
| HC-BR-013 | **FIXED** | Namespace-scoped Kubernetes Lease enforces one active health manager; startup/renewal/replacement mutations verify ownership. |
| HC-BR-014 | **FIXED** | Pod UID + container ID + monitor token fence state publication/readiness; stale cancelled monitor results cannot overwrite or replace a newer container generation. |
| HC-BR-015 | **FIXED — explicit compatibility boundary** | d2k cannot obtain OCI image config from the Kubernetes API. Standalone/Swarm create now explicitly warn when image HEALTHCHECK inheritance may be lost; empty Test warns too; README narrows claims and documents zero-field/image-SHELL limits. |
| HC-BR-016 | **FIXED** | d2k now advertises Docker API 1.44 so negotiated clients can send StartInterval; `/version` and `/_ping` regressions assert 1.44. |
| HC-BR-017 | **FIXED** | Timeout results synthesize Docker-compatible `Health check exceeded timeout (...)` output while retaining partial probe output. Regression covers the timeout path. |
| HC-BR-018 | **FIXED** | Repository-wide gofmt is clean; exact-head feature validation enforces gofmt before build/test/vet. |
| HC-BR-019 | **FIXED** | PR Docker build uses the checked-out workspace (`context: .`) instead of the synthetic remote pull ref. Exact-tree validation run `37523311270` successfully built `linux/amd64,linux/arm64` from `context: .`. |

### Remediation checkpoint — lifecycle redesign recovered

Recovered implementation head before stale-test repair: `54e70d0f78bc34262e6c192dde1c462cbd9e474e`.

Implemented lifecycle work already present:

- persistent per-service lifecycle ConfigMap state;
- stable replica-slot identity and Pod slot labels;
- per-slot restart history and bounded failed-task history;
- durable pending replacement intents retried by reconciliation;
- failed-task shutdown before replacement activation delay;
- replacement task ID preservation with stable slot inheritance;
- generation/token fencing for health monitor teardown and state publication;
- Pod UID + container-ID fencing for same-Pod restarts;
- namespace Lease ownership to prevent split-brain health managers;
- fail-closed handling for corrupt lifecycle/restart-policy metadata;
- cleanup of orphan lifecycle state;
- PR OCI build switched to checked-out local context.

Current validation exposed only stale test-call signatures after the refactor. Those were repaired in `f4d133905a8707eb440d7842d3cce4c3f6ba7bd1`; exact-head push validation `37513625309` is in progress.

Remaining work before ledger closure:

- resolve any behavioral test failures exposed by the redesigned lifecycle;
- verify timeout approximation/docs for HC-BR-005;
- verify explicit image-health inheritance limitation handling for HC-BR-015;
- validate API 1.44 accessibility for HC-BR-016;
- confirm Docker-compatible timeout/output behavior (HC-BR-006/017);
- complete exact-head test/vet/race + normal PR multi-arch validation;
- update each HC-BR status only after its regression/validation evidence is green.

### Lifecycle redesign validation checkpoint

Functional lifecycle head `38cf8551548654b2cd53140101de0ceab073cedc` passed the feature-branch validation through:

- repository-wide gofmt;
- strict go mod tidy diff;
- go build ./...;
- go test ./... including persistent lifecycle, transient delete retry, restart-delay ordering, stable slots, per-slot accounting, bounded task history, lease exclusivity, monitor-generation fencing, same-Pod container generation fencing, and replacement task-ID uniqueness;
- go vet ./....

Race and multi-architecture PR validation are intentionally deferred until the remaining Docker-semantic/CI findings are closed on one final head.

### Docker-semantic remediation checkpoint

Exact feature head `6e91cbb3155b6091f8a728eda53a48b8f26f8962` passed repository-wide gofmt, strict tidy, build, full tests, and vet in push validation run `37514983650`.

This checkpoint validates:

- Moby start-period/start-interval boundary behavior;
- Docker truncation marker behavior;
- Docker-compatible timeout diagnostic formatting;
- direct-CMD literal argv preservation;
- API 1.44 negotiation for StartInterval;
- create-scoped warnings for unavailable image HEALTHCHECK inheritance in standalone and Swarm flows;
- lifecycle redesign regression suite.

HC-BR-005 and HC-BR-015 are closed using the alternatives explicitly allowed by the blind-review remediation text: client-go has no process-start event for exact Docker timeout accounting, and Kubernetes does not expose OCI image config for Docker-equivalent inheritance. Both limitations are now surfaced and narrowly documented instead of silently claiming parity.

### Final remediation closure

All 19 blind-review findings are remediated.

Validated functional/remediation head: `f68ad3da2cca527d2f4e0472759eb1fe3be62161`

Final validation run: `37523311270`

Passed on that exact tree:

- repository-wide `gofmt` check;
- strict `go mod tidy` diff check;
- `go build ./...`;
- `go test ./...`;
- `go vet ./...`;
- `go test -race ./...`;
- local-context Buildx OCI build for `linux/amd64,linux/arm64` using the same `context: .` remediation now present in normal PR CI.

The temporary `.github/workflows/healthcheck-final-validation.yml` workflow is removed in the ledger-closure commit. Product code and normal `.github/workflows/ci.yml` are unchanged by that cleanup.

The temporary non-default PR harness could not itself emit a fresh `pull_request` run because GitHub does not register that temporary-base workflow like a default-branch PR workflow. HC-BR-019's failing synthetic remote context has nevertheless been directly eliminated, and the repaired local-context multi-architecture build passed on the exact candidate tree.

### Remediation strategy

The lifecycle findings are being solved as one coherent model rather than local conditionals:

1. stable replicated-service slot identity;
2. one current Pod/container generation per slot;
3. monitor registrations with explicit ownership tokens and stale-result fencing;
4. durable per-slot restart intent/history/task history;
5. single active health-manager ownership across d2k processes;
6. reconciliation retries until replacement converges or policy definitively forbids it.

After the lifecycle model is green, Docker semantic mismatches and CI/formatting findings will be remediated, then the exact final head will receive tidy/build/test/vet/race and multi-architecture PR validation.

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


### HC-BR-014 — HIGH — old-container monitor can overwrite new-container health state and act on the restarted Pod

**Evidence**

- Monitor identity includes container ID, but API health state is keyed only by Pod UID.
- When Kubernetes restarts the target container in the same Pod, reconciliation creates a new monitor for the new container ID and immediately stores a fresh `starting` state under the Pod UID.
- The old monitor is then cancelled. If it was inside `runDockerHealthCommand`, cancellation causes that call to return, but `monitorDockerHealth` does not re-check `ctx.Done()` before applying and storing the result.
- The cancelled old monitor can therefore overwrite the new container's state with a stale result.
- For Swarm, that stale result is then processed by the old monitor's health-state switch. If it reaches `unhealthy`, the old monitor can write the readiness gate false and invoke health replacement against the Pod name even though the target container has already restarted with a different container ID.

**Affected behavior**

Container restart within the same Pod, stale state, Swarm readiness, task replacement, container identity.

**Proposed remediation**

Fence state publication and replacement actions by the exact monitor/container generation. After every probe returns, verify the monitor still owns the current Pod UID/container ID before mutating shared state, readiness, restart history, or Pod lifecycle. Cancellation must prevent post-cancel probe results from being committed.

**Tests that should be added**

- block an old-container probe, switch ContainerStatus to a new container ID, reconcile, then release/cancel the old probe;
- assert old result cannot overwrite new `starting` state;
- assert old unhealthy result cannot alter readiness or delete the Pod;
- repeat under race detector.

**Validation status:** static concurrency review confirmed.

### HC-BR-015 — MEDIUM — image-inherited health checks and image health defaults are silently lost in normal create flows

**Evidence**

- Docker 27.3.1 merges image `Config.Healthcheck` into an incomplete user container config: if user health config is nil, the image healthcheck is inherited; if present but fields are zero/empty, individual Test/timing/retry fields inherit from the image.
- d2k does not inspect image metadata. A nil healthcheck simply passes validation and creates no monitor, with no warning.
- A non-nil healthcheck with an empty Test emits a warning, but cannot reconstruct the image command. Explicit commands with zero timing/retry fields use daemon defaults rather than image-specific values.
- This is partially documented, but it remains a Docker API/Compose compatibility gap in a feature whose review scope explicitly includes inherited image health checks and defaults.

**Affected behavior**

Images with Dockerfile `HEALTHCHECK`, Compose services relying on image health configuration, partial health overrides.

**Proposed remediation**

Resolve image config metadata before finalizing container/service health configuration and perform Docker-equivalent field-by-field merge. If that is intentionally out of scope, return a clear warning for the nil/inheritance case too and narrow compatibility claims.

**Tests that should be added**

- nil request healthcheck + image healthcheck;
- empty Test + image Test;
- explicit Test + image interval/timeout/retries/start values;
- `NONE` explicitly disables image healthcheck.

**Validation status:** confirmed against `moby/moby v27.3.1 daemon/commit.go`.

### HC-BR-016 — LOW — normal API 1.41 clients cannot configure StartInterval even though raw requests are accepted

**Evidence**

- d2k advertises Docker API `1.41`.
- Docker's health start-interval option was introduced in a later API and the normal Docker CLI gates the option by negotiated API version.
- The implementation accepts `HealthConfig.StartInterval` in raw JSON, but a normal negotiated 1.41 CLI session cannot exercise it.
- The README documents this limitation, so this is a compatibility limitation rather than an undisclosed correctness bug.

**Affected behavior**

CLI/Compose accessibility of `StartInterval`, API-version fidelity.

**Proposed remediation**

Either raise the advertised API version only when the broader API surface is compatible enough to do so, or explicitly keep this as a documented limitation. Do not imply ordinary CLI support for start interval while advertising 1.41.

**Tests that should be added**

Client negotiation/CLI integration test showing whether `--health-start-interval` is accepted and transmitted.

**Validation status:** static/API compatibility review confirmed.

### HC-BR-017 — LOW — timeout result text does not match Docker's health-log diagnostics

**Evidence**

- Docker 27.3.1 returns an exit code of -1 with output beginning `Health check exceeded timeout (...)`, including any captured probe output.
- d2k relies on Kubernetes exec returning an error after context deadline. It preserves captured output as-is, or uses the raw exec/context error string when output is empty.
- Thus even aside from HC-BR-005's timeout-start semantic difference, inspect health logs do not reproduce Docker's timeout diagnostic.

**Affected behavior**

Health-log readback and operator diagnostics.

**Proposed remediation**

Detect the configured timeout path explicitly and synthesize Docker-compatible timeout output while retaining captured output within the same truncation rules.

**Tests that should be added**

Timeout with no output and timeout after partial stdout/stderr.

**Validation status:** confirmed against `moby/moby v27.3.1 daemon/health.go`.


### HC-BR-018 — LOW — exact pinned candidate is not gofmt-clean

**Evidence**

An exact-SHA GitHub Actions validation explicitly checked out `6e7d77eaca67e5684b35012714553fde81bdae39`. `gofmt -l .` reported 13 files:

- `internal/adapter/adapter.go`
- `internal/adapter/container.go`
- `internal/adapter/events.go`
- `internal/adapter/helpers.go`
- `internal/adapter/metrics.go`
- `internal/adapter/swarm.go`
- `internal/adapter/volume.go`
- `internal/api/containers/containers.go`
- `internal/api/exec/exec.go`
- `internal/api/images/images.go`
- `internal/api/networks/networks.go`
- `internal/api/system/system.go`
- `internal/middleware/middleware.go`

This is not a health semantic defect, but it fails the requested formatting gate and is upstream-PR cleanup.

**Affected behavior**

Formatting/maintainer acceptance only.

**Proposed remediation**

Run `gofmt` on the reported files in a dedicated formatting-only commit or limit the formatting patch to files genuinely modified by the candidate if upstream prefers avoiding unrelated churn.

**Tests that should be added**

CI should fail with the file list when `gofmt -l` is non-empty.

**Validation status:** reproduced on exact pinned SHA in workflow run `37235040312`.

### HC-BR-019 — MEDIUM — repository's normal PR multi-architecture build job is broken before Dockerfile execution

**Evidence**

The existing candidate-associated PR workflow run `37223631986` passed its Go remediation-validation job but its normal multi-architecture image job failed before reading the Dockerfile. The build action used a remote Git context:

`https://github.com/Proponent-8247/portainer-d2k.git#refs/pull/6/merge`

BuildKit reported that the repository does not contain `refs/pull/6/merge`. GitHub's Actions checkout can fetch that synthetic PR ref, but an unauthenticated/ordinary remote Git context cannot assume the synthetic ref is advertised.

A separate exact-SHA validation using a checked-out local context successfully built the pinned candidate for `linux/amd64,linux/arm64`, showing the failure is CI plumbing rather than the candidate Dockerfile/buildability.

**Affected behavior**

Normal upstream-style PR CI, multi-architecture OCI validation, PR submission readiness.

**Proposed remediation**

Use the already checked-out workspace (`context: .`) or a remotely advertised commit SHA/branch as the BuildKit context instead of a synthetic `refs/pull/*/merge` remote ref.

**Tests that should be added**

Keep the PR multi-arch job required and ensure it builds from the same checked-out revision that the validation job verifies.

**Validation status:** reproduced in normal PR run `37223631986`; local-context exact-SHA multi-arch build passes in run `37235040312`.

## Validation status

Validation completed after static findings were committed.

Exact pinned validation was arranged through temporary draft PR #7 / workflow `blind-healthcheck-pinned-validation`. The workflow explicitly checked out `6e7d77eaca67e5684b35012714553fde81bdae39` and verified that exact HEAD before running gates.

- [x] exact candidate SHA verified
- [ ] formatting — FAIL: `gofmt -l .` reports 13 files (HC-BR-018)
- [x] `go mod tidy` diff — PASS
- [x] `go build ./...` — PASS
- [x] `go test ./...` — PASS
- [x] `go vet ./...` — PASS
- [x] `go test -race ./...` — PASS
- [x] exact-SHA multi-architecture OCI build (`linux/amd64,linux/arm64`) — PASS using checked-out local context
- [ ] repository normal PR multi-architecture job — FAIL before Dockerfile execution due remote synthetic PR ref context (HC-BR-019)

Exact-SHA validation workflow run: `37235040312`.

Normal PR workflow run inspected after static review: `37223631986`.

## Review log

### Checkpoint 001 — target verification

The feature branch was verified to point exactly at the pinned candidate before any review work. The pinned candidate is a descendant of the stated upstream base with no commits behind the base. Blind-review ledger initialized before implementation analysis.

### Checkpoint 002 — initial architecture / semantics / concurrency pass

Reviewed the pinned candidate's health monitor, container and Swarm translation paths, readiness gating, restart-history persistence, API readback, and Kubernetes exec path. Independently compared core timing, output, timeout, and restart-policy behavior against Moby 27.3.1 and SwarmKit source. Recorded HC-BR-001 through HC-BR-006. No prior audit material has been read.

### Checkpoint 003 — multi-replica / persistence / task identity pass

Reviewed restart history scope and growth, task slot synthesis, failed-task retention, delayed replacement semantics, corrupted persisted metadata, and multi-instance operation. Additional findings follow.

### Checkpoint 004 — static blind review complete

Completed the independent static review of the pinned candidate, including changed implementation and tests, indirectly affected container/Swarm API paths, Docker 27.3.1 health semantics, SwarmKit restart semantics, Kubernetes lifecycle/readiness, concurrency, persistence, networking, atomicity, RBAC, malformed metadata, and multi-replica behavior.

The current blind finding set is HC-BR-001 through HC-BR-017. Validation has not yet been used as a source of findings. `AUDIT-HEALTHCHECK.md` has not been read or searched.

### Checkpoint 005 — validation complete; blind finding list frozen

Exact-SHA validation completed after the static review. HC-BR-018 and HC-BR-019 were added from validation evidence.

**Frozen blind finding set:** HC-BR-001 through HC-BR-019.

No finding above will be deleted or rewritten during reconciliation. Any comparison to the prior audit will be added separately. At this checkpoint, `AUDIT-HEALTHCHECK.md` still has not been read, searched, or inspected.

## Reconciliation against prior audit

Reconciliation was performed only after Checkpoint 005 froze HC-BR-001 through HC-BR-019. The prior audit file was then read at the pinned candidate revision. Original blind findings above remain unchanged.

| Blind finding | Reconciliation classification | Prior-audit relationship |
|---|---|---|
| HC-BR-001 | **Prior issue appears incompletely fixed** | HC-AUD-005 introduced d2k-owned Swarm replacement/restart handling, but the remediation is one-shot after unhealthy and does not reconcile transient readiness/reservation/delete failures. |
| HC-BR-002 | **Genuinely new** | Prior audit did not identify monitor-registration ownership/teardown races that can erase a newer registration and create duplicate monitors. |
| HC-BR-003 | **Prior issue appears incompletely fixed** | HC-AUD-005 claimed MaxAttempts/restart-policy handling; attempt accounting is persisted before replacement succeeds, so failed deletion can consume attempts. |
| HC-BR-004 | **Prior issue appears incompletely fixed** | HC-AUD-006 claimed Docker interval/start-period scheduling was fixed. Exact Moby comparison shows the remediation clamps StartInterval to the start-period boundary, which Docker 27.3.1 does not do. |
| HC-BR-005 | **Genuinely new** | HC-AUD-008 addressed exec/infrastructure failures, but did not identify that d2k starts the configured health Timeout before Kubernetes exec establishment while Docker gives exec startup a separate timeout. |
| HC-BR-006 | **Genuinely new** | Prior audit did not identify Docker's `...` truncation marker mismatch. |
| HC-BR-007 | **Genuinely new** | Prior audit checked health-config annotation size, but not unbounded growth of the restart-history annotation under the default unlimited policy. |
| HC-BR-008 | **Prior issue appears incompletely fixed** | HC-AUD-005 claimed MaxAttempts/window were honored. SwarmKit enforces those per slot; d2k's remediation uses one service-global history budget. |
| HC-BR-009 | **Prior issue appears incompletely fixed** | HC-AUD-005 required replacement to create a new task identity. The remediation creates a new Pod identity but reconstructs slot numbers from current Pod age, so unaffected replicas change slots after replacement. |
| HC-BR-010 | **Genuinely new** | Prior audit discussed replacement identity but did not identify loss of failed task history / inability to inspect the replaced failed task. |
| HC-BR-011 | **Prior issue appears incompletely fixed** | HC-AUD-005 claimed restart delay was honored. SwarmKit shuts the old task down and delays replacement start; d2k delays deletion of the unhealthy old task itself. |
| HC-BR-012 | **Genuinely new** | Prior audit did not test or discuss corrupted restart-policy/history annotations or fail-open fallback behavior. |
| HC-BR-013 | **Genuinely new** | Prior audit did not examine multiple d2k processes/replicas and duplicate monitor ownership. |
| HC-BR-014 | **Genuinely new** | Prior audit did not identify the same-Pod container-ID restart race where a cancelled old monitor can overwrite new state or act on the restarted Pod. |
| HC-BR-015 | **Independently rediscovered** | Prior audit explicitly listed image-defined/inherited HEALTHCHECK and image-specific zero-field defaults as accepted/documented compatibility limitations. Blind review independently reached the same limitation and additionally noted the nil-inheritance path is silent. |
| HC-BR-016 | **Independently rediscovered** | Prior audit explicitly documented API 1.41 preventing ordinary clients from using health-start-interval. |
| HC-BR-017 | **Genuinely new** | Prior audit covered infrastructure error semantics but not Docker-compatible timeout diagnostic output. |
| HC-BR-018 | **Genuinely new** | Prior audit's earlier validated remediation head was reported format-clean, but the pinned Stage 3 candidate fails repository-wide `gofmt -l .` on 13 files. |
| HC-BR-019 | **Genuinely new** | Prior audit reported an earlier normal PR multi-arch build passed. The candidate-associated current PR workflow fails because BuildKit is given a synthetic `refs/pull/*/merge` remote Git context; exact-SHA local-context multi-arch build succeeds. |

### Reconciliation totals

- Independently rediscovered: **2**
- Genuinely new: **10**
- Prior issue appears incompletely fixed: **7**
- False positives after reconciliation: **0**

The prior audit contained 11 remediated HC-AUD findings plus documented compatibility limits. The blind review does not dispute the fixes that were directly validated there (for example conflict reapplication, target-container readiness, invalid-healthcheck prevalidation, literal CMD argv preservation, and health metadata sizing). It identifies additional failure modes and several incomplete semantic claims in the newer d2k-owned health/restart architecture.

## Final blind-review disposition

**Not ready for upstream submission. Another remediation pass is required.**

Frozen blind severity totals:

- **HIGH:** 5
- **MEDIUM:** 10
- **LOW:** 4
- **CRITICAL:** 0
- **INFO:** 0

Highest-priority remediation areas:

1. Make unhealthy Swarm replacement a durable reconciled operation rather than a one-shot action, including transient API failure recovery (HC-BR-001 / HC-BR-003).
2. Fence monitor ownership by exact container generation and eliminate duplicate/stale monitor publication (HC-BR-002 / HC-BR-014).
3. Implement stable per-slot identity and restart accounting for replicated services (HC-BR-008 / HC-BR-009), with bounded retained failed-task history (HC-BR-010).
4. Correct restart-delay semantics and persistence robustness (HC-BR-007 / HC-BR-011 / HC-BR-012).
5. Correct exact Docker timing/timeout/readback mismatches where upstream fidelity is claimed (HC-BR-004 / HC-BR-005 / HC-BR-006 / HC-BR-017).
6. Resolve the formatting gate and normal PR multi-arch CI context before submission (HC-BR-018 / HC-BR-019).

### Checkpoint 006 — reconciliation complete

Prior audit read only after the blind list was frozen. All 19 blind findings remain preserved. No feature fixes were implemented during this stage. The candidate requires remediation before upstream submission.
