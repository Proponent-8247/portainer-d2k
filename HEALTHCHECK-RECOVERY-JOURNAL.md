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

## 2026-10-13 - Recovery checkpoint R2: desired slots and native EndpointSlice acceptance

HEAD before this journal update: `16ddb557eb4775a6c49ef18b09c12e6eac94250d`.

### New product implementation and regressions
- `357d677f4d443b91163d1feb9291fdcb2e30436e`: formatting fix; focused slot regression run **38464467794** PASS.
- Full-Go run **38464468167** found an additional `HC-PRR-017` defect: Swarm task readback used `max(Deployment.Status.Replicas, Deployment.Spec.Replicas)`; a temporary native `MaxSurge` Pod increased the number of **user-visible desired Swarm slots**.
- `13af30ab3b8a60e6f29271a7c23c52719107179d`: fix task list to take the desired replica count from `Deployment.Spec.Replicas`, not temporary controller surge capacity. Focused slot run **38464726804** PASS; prior live Kind run **38464726716** PASS.
- `8b6d536b4511565ea8ccb2cb4fa4e9e57d10ef6d`: add real-controller **2->2 rolling-update** test to cover the case missing from the original HC-PRR-017 acceptance.
- `678d57d49bcc682e35698391b9f79810253b88f4`: add native ClusterIP/EndpointSlice readiness-gate admission/withdrawal test, including health recovery and failure.
- `16ddb557eb4775a6c49ef18b09c12e6eac94250d`: correct Kind workflow selector to include all four live tests. Kind run **38465255625**, focused run **38465255624**, full Go/race/OCI run **38465255658** were queued/in progress at this journal checkpoint; do **not** report them as passed before completion.
- `DOCKER-NATIVE-K8S-COMPATIBILITY.md` committed at `7bc712eecade143c9cec9899ff6d2de0eb3323d4`. Separates implemented Docker-to-native translations from unsupported/approximate ones, including global -> DaemonSet and stable per-slot identity -> optional StatefulSet/per-slot controller.

### Discovered compatibility boundary
- This healthcheck feature branch's `internal/adapter/network.go` still creates **synthetic Docker networks over flat Kubernetes namespace networking**. The hardened Cilium-based Docker-network-equivalent isolation work belongs to a **separate patch/PR**, to be integration-tested with health readiness/EndpointSlice gating later.
- `deploy.mode: global` is still mapped to replicated=1 with a warning; a true DaemonSet adapter requires independent design/implementation. Do not claim native global support exists now.
- Preserve native Deployment/ReplicaSet and K8s Service/EndpointSlice dataplanes for stateless replicated services; avoid inventing a second Docker overlay or scheduler.

### Recovery instructions after R2
1. Verify HEAD and inspect runs **38465255624 / 38465255625 / 38465255658** plus later exact-HEAD jobs.
2. If `gofmt` fails, identify the exact file/lines, commit formatting narrowly, then rerun.
3. If Kind 2->2 or EndpointSlice admission fails, fix test/semantic root cause (never weaken assertion merely for green).
4. Confirm `go test -race ./...`, gofmt, strict tidy, build, vet and multiarch OCI all pass on a *functional* SHA; if documentation-only final commit changes HEAD, identify the validated functional ancestor.
5. Update `POST-REMEDIATION-REMEDIATION-HEALTHCHECK.md` with exact evidence; HC-PRR-017 remains **IN PROGRESS** until live 1->1, 2->2 and full gates pass. HC-PRR-002 remains **IN PROGRESS** with explicit same-ReplicaSet causal-identity limitation.
6. Keep native mapping documentation and do not conflate network-isolation patch into healthcheck upstream PR; do not silently migrate existing Deployments.

