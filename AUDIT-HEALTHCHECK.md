# Docker health-check patch audit

Audit branch: `feature/docker-healthchecks-upstream`

Upstream base: `portainer/d2k@2508dde0d06476029be6aaee999b01c8262b6cc0`

Recovered patch head at audit start: `8e63ba3b7a01fb884846c2b00b79215fb1839044`

## Stage

Stage 2 — audit and edge-case review.

Functional fixes are intentionally deferred until the audit is complete.

## Review progress

- [x] Recover exact upstream base and current patch head.
- [x] Confirm patch is ahead-only of upstream at audit start.
- [x] Review Docker container-create health-check translation.
- [x] Review Docker inspect/list health-state reporting.
- [x] Review Swarm/Compose create/update/readback behavior.
- [x] Review Docker timing/default semantics.
- [x] Review Kubernetes probe semantic mismatches.
- [x] Review API validation/error behavior.
- [x] Review image-inherited HEALTHCHECK behavior.
- [x] Review concurrency/restart/readiness edge cases.
- [x] Review tests for blind spots and missing failure cases.
- [ ] Run complete build/test/vet/race validation after audit findings are recorded.

## Findings

### HC-AUD-001 — LOW / DOCUMENTED LIMITATION — intermediate Docker failing streak is not observable

Kubernetes readiness applies `failureThreshold` internally before `ContainerStatus.Ready` flips false. Re-review therefore confirms that d2k's healthy -> unhealthy *status* transition is broadly aligned with the configured threshold.

The remaining mismatch is Docker's intermediate `FailingStreak`: after a formerly healthy container has one or more failures below the threshold, Docker exposes streak values 1..N-1 while remaining `healthy`. Kubernetes does not expose that probe worker counter through Pod status, so d2k reports streak 0 until readiness flips and then reports the threshold value.

This is already disclosed in the README as a threshold-level approximation. It is retained as a compatibility limitation, not treated as a blocker.

Recommended action after audit: no mandatory code change unless exact `FailingStreak` compatibility becomes a requirement.

### HC-AUD-002 — HIGH — Swarm update conflict retry can silently discard the health-check update

`SwarmUpdateService` mutates the fetched Deployment before entering its update retry loop. On a Kubernetes `Conflict`, it replaces `target` with a freshly fetched Deployment and retries `Update` without reapplying the requested image/env/replica/health-check mutations.

For this patch, a concurrent update can therefore cause a requested health-check add/change/remove to be lost while the API returns success on the next iteration.

This retry defect predates the health-check patch for other service fields, but the new health-check update path inherits it and needs protection before the health-check PR is upstream-ready.

Recommended fix after audit: express the service mutation as a reusable function and reapply it after every conflict re-fetch before retrying.

### HC-AUD-003 — MEDIUM — DNSRR/host-port Swarm endpoints ignore health readiness

`swarmServiceEndpointDNSRR` advertises a node whenever the Pod phase is `Running`; it does not require the target container to be running and ready. With translated health checks, an unhealthy task or a container currently restarting from the liveness probe can therefore remain in the returned node-IP endpoint list.

VIP-mode Kubernetes Services are readiness-gated, so the discrepancy is specific to DNSRR/host-port mode.

Recommended fix after audit: when health checks are translated, filter DNSRR endpoint reporting on the target container's running + ready status (while preserving current behavior for services without translated health checks).

### HC-AUD-004 — HIGH — Swarm task can be reported running before the health-checked container is ready

`kubePodToSwarmTask` marks a Running Pod as Swarm task `running` when either:

1. `ContainerStatuses` is temporarily empty, or
2. *any* container in the Pod is ready.

That conflicts with Swarm health semantics. Moby's service health tests require a task with an initial failing health check to remain in `STARTING` until the health check succeeds. A transient empty status can therefore let Docker CLI progress converge before the first health result. More importantly, a mutating webhook / service-mesh sidecar that becomes ready can make the task appear running indefinitely even while the d2k service container's health probe is failing.

The standalone runtime-state code already avoids this class by selecting the named d2k container, but the Swarm task converter does not.

Recommended fix after audit: determine the d2k target container by service/container name and require *that container* to be running + ready. If health probes are configured and its status has not appeared yet, report `starting`, not `running`.

### HC-AUD-005 — MEDIUM / ARCHITECTURAL — liveness-based Swarm health restarts ignore Swarm RestartPolicy

Translated Swarm health checks add a Kubernetes liveness probe. A liveness failure restarts the container according to the Pod restart policy, which d2k hard-codes to `Always`.

Real Swarm marks an unhealthy task failed and then applies the service RestartPolicy. SwarmKit explicitly supports `RestartOnNone`, `RestartOnFailure`, delays, windows, and `MaxAttempts`; `RestartOnNone` means the task is never restarted.

Consequences include:

- a service configured with restart condition `none` is still restarted by d2k after health failure;
- `MaxAttempts` cannot stop repeated unhealthy restarts;
- restart delay/window semantics are bypassed;
- the same Pod/task identity is reused rather than creating a replacement task.

Part of this is a pre-existing d2k Swarm restart-policy limitation, but the health-check patch actively depends on liveness to approximate Swarm health failure, so the mismatch is directly exposed by this feature.

Recommended action after audit: either implement health-failure handling through a Swarm-aware controller/lifecycle path, or clearly scope the upstream PR as an approximation and document that RestartPolicy is not honored for health-triggered failures.

### HC-AUD-006 — MEDIUM — Kubernetes probe cadence can diverge materially from Docker health interval semantics

Docker's health monitor waits the configured interval from the end of one probe before starting the next. Kubernetes probe workers are periodic/ticker-driven, and Kubernetes also documents that readiness probes may be executed at times other than `periodSeconds` while a container is not Ready.

Consequences for translated health checks:

- a slow probe can be followed by another Kubernetes probe sooner than Docker would schedule it;
- readiness may be retried more aggressively while not-ready;
- failure/success threshold timing can therefore differ even when `periodSeconds`, timeout, and thresholds numerically match Docker;
- d2k's synthetic `docker inspect` timing estimate assumes the nominal Kubernetes period and cannot observe these extra/shifted probe executions.

This matters most for long-running checks, checks with interval close to execution time, and startup/unhealthy transitions.

Recommended action after audit: document continuous-cadence differences explicitly. If tighter Docker fidelity is required, native Kubernetes probes are insufficient as the sole health state machine.

### HC-AUD-007 — MEDIUM — invalid Swarm healthcheck can leave storage side effects before returning 400

`SwarmCreateService` does not validate/translate the health check immediately after decoding the service spec. Health validation happens only after earlier service preparation, including mount handling that can create fallback PVCs.

An invalid health configuration can therefore:

1. cause d2k to create one or more PVCs;
2. later fail `buildHealthProbes`;
3. return the expected HTTP 400;
4. leave the created storage resources behind even though the service was never created.

Standalone container creation validates the healthcheck during deployment construction before Kubernetes resources are created, so this issue is specific to the Swarm create ordering.

Recommended fix after audit: validate the Docker HealthConfig at the start of `SwarmCreateService`, before any mutating mount/PVC/resource preparation.

### HC-AUD-008 — HIGH — Kubernetes exec-probe infrastructure errors do not match Docker health failure semantics

Docker converts an error running the health-check command into a health result with exit code `-1`; that result increments the failing streak and can transition the container to unhealthy.

Kubernetes' exec prober distinguishes a normal non-zero process exit (probe failure) from an execution/infrastructure error. Non-`ExitError` failures are returned as `Unknown` with an error, and the kubelet probe worker discards that result without advancing the success/failure run.

This is especially important for commands that cannot be started at all, such as a missing direct `CMD` executable or a missing `/bin/sh` for a translated `CMD-SHELL` probe. In Swarm mode:

- readiness can remain false;
- liveness can retain its initial success state;
- the failure threshold may never be reached;
- the task can remain `starting` rather than becoming unhealthy/failed and being restarted/replaced as Docker would.

Timeouts and ordinary non-zero exit codes are treated as probe failures by Kubernetes; the mismatch is specifically the exec/infrastructure-error class.

Recommended action after audit: add an explicit live/runtime test for an unstartable health command. If Docker-compatible failure semantics are required, native Kubernetes exec probes alone cannot guarantee them and a wrapper/controller approach is needed.

## Reviewed compatibility limits that are already documented

The following were re-checked and are not being opened as new blockers because the README already scopes/discloses them:

- image-defined/inherited HEALTHCHECK cannot be discovered without image metadata;
- zero-valued explicit timing fields cannot inherit unknown image-specific health defaults and instead use Docker daemon defaults;
- `CMD-SHELL` cannot honor image-specific Docker `SHELL` metadata;
- sub-second Docker durations are rounded to Kubernetes whole seconds;
- `start_period` and `start_interval` do not have exact Kubernetes equivalents;
- Swarm uses two independent Kubernetes probes, so the health command is executed twice;
- liveness restarts the same Kubernetes container/Pod identity instead of creating a new Swarm task identity;
- Docker health log/output history is not reconstructed;
- stopped workloads do not retain historical Docker health state;
- health-status event synthesis and `docker ps --filter health=...` are not implemented;
- API 1.41 negotiation means normal clients cannot use the newer `health-start-interval` flag.

## Missing regression/live tests identified by this audit

The current automated tests do not cover:

- Kubernetes optimistic-concurrency conflict during a health-check service update (HC-AUD-002);
- unhealthy/restarting DNSRR or host-port task endpoint filtering (HC-AUD-003);
- empty ContainerStatuses and injected-ready-sidecar behavior in Swarm task state (HC-AUD-004);
- health failure with Swarm RestartPolicy `none` / `MaxAttempts` (HC-AUD-005);
- slow-probe / non-Docker cadence behavior (HC-AUD-006);
- invalid healthcheck plus volume mount proving no PVC side effects (HC-AUD-007);
- an exec health command that cannot be started, proving Docker-vs-kubelet error semantics (HC-AUD-008).

These should become regression tests alongside fixes, except where a finding is accepted as a documented architectural limitation.

This file is updated during the audit so progress and findings survive chat interruption.
