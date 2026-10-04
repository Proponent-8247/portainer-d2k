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


## Validation status

Not started.

## Review log

### Checkpoint 001 — target verification

The feature branch was verified to point exactly at the pinned candidate before any review work. The pinned candidate is a descendant of the stated upstream base with no commits behind the base. Blind-review ledger initialized before implementation analysis.

### Checkpoint 002 — initial architecture / semantics / concurrency pass

Reviewed the pinned candidate's health monitor, container and Swarm translation paths, readiness gating, restart-history persistence, API readback, and Kubernetes exec path. Independently compared core timing, output, timeout, and restart-policy behavior against Moby 27.3.1 and SwarmKit source. Recorded HC-BR-001 through HC-BR-006. No prior audit material has been read.
