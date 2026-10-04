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
- [ ] Review Docker container-create health-check translation.
- [x] Review Docker inspect/list health-state reporting.
- [x] Review Swarm/Compose create/update/readback behavior.
- [ ] Review Docker timing/default semantics.
- [ ] Review Kubernetes probe semantic mismatches.
- [ ] Review API validation/error behavior.
- [ ] Review image-inherited HEALTHCHECK behavior.
- [x] Review concurrency/restart/readiness edge cases.
- [ ] Review tests for blind spots and missing failure cases.
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

This file is updated during the audit so progress and findings survive chat interruption.
