# Healthcheck implementation - durable recovery journal

> Authority: GitHub code, CI and `POST-REMEDIATION-REMEDIATION-HEALTHCHECK.md`. This journal is the chronological recovery index, **not** a replacement for the 18-item HC-PRR finding ledger. Preserve historical entries. Update it at each substantial code/validation checkpoint.

## Resume protocol
1. Get `feature/docker-healthchecks-upstream` HEAD SHA and compare to the most recent entry here.
2. Read this journal **from newest checkpoint backward**, followed by `POST-REMEDIATION-REMEDIATION-HEALTHCHECK.md` and `HEALTHCHECK-SLOT-ARCHITECTURE.md`.
3. Inspect latest GitHub Actions for the **exact HEAD** and distinguish Go test failures, formatting failures, Kubernetes-controller failures, and untested behavior. Do not infer green status from previous SHAs.
4. Preserve the pinned original reviewed product tree `96d3f9209ac48dacacb5dd8330e6d86129eb674c` and frozen independent review `POST-REMEDIATION-REVIEW-HEALTHCHECK.md`.
5. Implement one conceptual fix per commit where practical, add regression first, record red/green Actions IDs, update remediation ledger only after evidence, then append a dated checkpoint to this journal.
6. Do not rebase/rewrite public commits. Do not push an upstream PR while HIGH findings or release gates remain open.
7. External Kubernetes/Kind tests are authoritative for controller behavior; fake-client tests are necessary but insufficient.
8. Never execute a branch action assuming HEAD has not changed; re-fetch head and file SHA before updates.

## Product requirements / invariants
- Primary goal: **strong Docker/Compose/Swarm-style application support implemented on Kubernetes with native features wherever possible**. Preserve Docker-facing APIs, health/restart behavior, service discovery, network isolation and deployed application compatibility.
- Default for ordinary replicated services: **Kubernetes Deployment + ReplicaSet**, native `Service` and EndpointSlice readiness behavior, Pod readiness gates, and Kubernetes controller convergence. Do not silently migrate existing workloads to another controller kind.
- Retain a native K8s reconciliation/data-plane boundary instead of manufacturing a second Docker engine, competing overlay network, or independent Pod scheduler.
- Docker health command timing/state are a d2k compatibility layer because Kubernetes probes cannot fully represent Docker `HEALTHCHECK`; d2k should use native readiness/endpoints admission.
- Logical Swarm replica slots **cannot be proven causally stable** using a single ReplicaSet for multiple indistinguishable replacement Pods; deterministic assignment is a compatibility compromise. For strict physical per-slot identity, evaluate explicit opt-in per-slot Deployment or StatefulSet with migration, not guessed ancestry.
- Fail closed when ownership is ambiguous, never delete a Pod by name without UID preconditions, never let a stale Lease holder mutate/delete a successor's resources, and persist replacement intent before destructive mutations.
- Keep networking/Cilium and Docker stack semantics stable; upgrade/migration must be explicit and backward compatible. Avoid cosmetic refactoring.

## Frozen sources
- Source pinned by second blind review: `96d3f9209ac48dacacb5dd8330e6d86129eb674c`.
- Review freeze and finding ledger HEAD: `4609b1aa5fed1e119f770a38f2eeb30c850d35e7`.
- Upstream comparison base: `portainer/d2k develop 2508dde0d06476029be6aaee999b01c8262b6cc0`.
- Branch: `feature/docker-healthchecks-upstream`.
- Working remediation ledger: `POST-REMEDIATION-REMEDIATION-HEALTHCHECK.md`: **16 OPEN / 2 IN PROGRESS / 0 FIXED** (HC-PRR-002 and HC-PRR-017 in progress).
- Architecture decision record: `HEALTHCHECK-SLOT-ARCHITECTURE.md`.

## 2026-10-13 - Recovery checkpoint R1: both real-controller acceptance tests passed; formatting remains

Recovery HEAD before journal commit: `2bec586f6964d36e51284c6ec5c452a9cb9c4e36` (last visible commit "Test Swarm task list hides provisional Deployment surge slots"). This is **past** the preceding conversation's HEAD; do not duplicate its fixes.

### Validated evidence (exact SHA)
- Disposable Kind workflow **38454340305**, source `2bec586f...`: **PASS**, both `TestLiveDeploymentSurgeSlotConvergesAfterRollingUpdate` (8.53 s) and `TestLiveConcurrentReplacementSlotsRemainUnique` (4.84 s).
- Focused slot workflow **38454340018**, same source: all eight new `Test*Slot*`/related regression tests **PASS**, but the **overall workflow FAILED only at gofmt** in `internal/adapter/swarm_slot_reconciliation_test.go`, around line 504. Fix gofmt and rerun; do not call that run green.
- Previous Kind run **38454044823** and focused regression run **38454044803** both **PASS** on `842043a1509fcc71301ebc801cf03cc0be49a8fc`; that is not current HEAD.
- Focused tests exercise surge promotion, uncommitted delete-intent slot reservation, preexisting-surge exclusion, cross-ReplicaSet provenance, deterministic same-ReplicaSet ties, Kubernetes CreationTimestamp precision, failure-time known-Pod snapshot, and terminating-Pod task readback.
- Initial real Kind test caught terminating predecessor overriding active slot in `SwarmListTasks`. Fix committed as `7d8c659fce8faba9e8b6eb3434b5b4809c24b55a`; later Kind runs pass.
- Lifecycle metadata and snapshot code in `internal/adapter/swarm_health_lifecycle.go`; unit/live regression tests in `internal/adapter/swarm_slot_reconciliation_test.go` and `internal/adapter/swarm_slot_live_test.go`.

### Remaining tasks at R1
1. Repair `gofmt` reported in focused run **38454340018**; re-run exact-head focused suite.
2. Run full `gofmt -l .`, strict tidy diff, `go build ./...`, `go test ./...`, `go vet ./...`, `go test -race ./...`, and amd64+arm64 OCI build on an exact SHA. The old earlier validation does **not** validate R1.
3. Add controller-realistic failure injection: Pod-delete timeout after success, d2k crash between deletion and lifecycle CAS, scale/rollout overlap, wrong Deployment UID, Lease handoff/renewal failure. Require test evidence before fixing HC-PRR-005/009/etc.
4. Keep `HC-PRR-002` **IN PROGRESS** due to causal identity limitation; assess whether to document conditional support boundary or implement opt-in native per-slot controller.
5. `HC-PRR-017` has real Kind proof on R1 but full gates/reconciliation review needed before changing ledger to FIXED.
6. CI harnesses `.github/workflows/healthcheck-prr-slot-validation.yml` and `.github/workflows/healthcheck-prr-kind-validation.yml` are temporary; remove before upstream PR, but keep validation evidence in ledger/journal.
7. Other 16 HC-PRR findings remain open and must be remediated separately; do not declare whole feature done.

### Decisions taken
- Prioritize native K8s Deployment/ReplicaSet for replaceable stateless Swarm workloads; use Pod readiness gate / native Services for health-gated routing.
- Maintain compatibility allocator for existing Deployments, with provisional surge identities then promotion and deterministic restart-slot adoption.
- Explicitly separate application intent (Docker/Compose/Swarm API) from Kubernetes workload/controller choice. StatefulSet may suit ordinal identity but has different start-first semantics and storage behavior; no implicit migration.
- No new user-private site settings or hypervisor mutation.

### Next checkpoint
After a formatter commit plus exact-head gate evidence, append R2 here and cross-link the remediation ledger; keep previous entries unchanged.
