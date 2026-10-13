# Post-remediation lifecycle remediation — HC-PRR

## Authority and baseline
- Repository: `Proponent-8247/portainer-d2k`
- Working branch: `feature/docker-healthchecks-upstream`
- Immutable reviewed source: `96d3f9209ac48dacacb5dd8330e6d86129eb674c`
- Final independent review: `POST-REMEDIATION-REVIEW-HEALTHCHECK.md` at `4609b1aa5fed1e119f770a38f2eeb30c850d35e7`
- Upstream base: `portainer/d2k@2508dde0d06476029be6aaee999b01c8262b6cc0`
- Preserve frozen source/review history. No rebases or history rewrites.

## Status summary
Open: **18** · In progress: **2** · Fixed: **0** · Accepted with documented limitation: **0**

The 18 frozen findings remain open pending behavioral evidence. Counts above represent
18 total OPEN-or-IN-PROGRESS findings, not 20 independent findings.

| ID | Severity | Status | Scope |
|---|---|---|---|
| HC-PRR-001 | HIGH | OPEN | UID-fenced destructive Pod operations |
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
| HC-PRR-017 | HIGH | IN PROGRESS | Ordinary Deployment rolling-update surge slot drift |
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
  new surge Pod appears while old lives; no durable slot 2; old terminates;
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
- **PENDING** write red tests and implement focused allocator guard.
- **PENDING** validation and lifecycle regression/fault injection.

## Mutation policy
- One conceptual fix/commit where practical, plus independent ledger checkpoints.
- Never rewrite published Git history.
- Preserve default behavior outside Swarm health lifecycle.
- New findings must be appended with evidence; do not erase frozen review text.
