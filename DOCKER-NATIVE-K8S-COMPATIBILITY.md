# Docker/Compose application support through native Kubernetes primitives

**Status:** Product design and compatibility-gap register, **not** a claim that these features are all implemented.
**Scope:** `Proponent-8247/portainer-d2k` healthcheck feature branch and its eventual integration with the separate network-isolation patch.
**User intent:** A Docker/Compose/Swarm-oriented deployment experience, but Kubernetes remains the authoritative workload controller, service/network dataplane and volume/secret platform wherever practical.

## Architectural rule

Translate the *intent* of Docker Engine/Compose/Swarm resources to a Kubernetes-native resource or a minimal compatibility controller:

- **Native reconciliation first:** Kubernetes Deployment/ReplicaSet, StatefulSet, DaemonSet, Job/CronJob, Service/EndpointSlice, PVC/CSI, Secret, ConfigMap, Kubernetes admission/status, NetworkPolicy/Cilium.
- **Compatibility layer only where semantic gaps are unavoidable:** Docker healthcheck timers/failing streak/execution, Swarm task API IDs and slot history, restart attempt window and task replacement intent.
- **Avoid competing engines/controllers:** no hidden dockerd, no second overlay dataplane, no custom scheduler duplicating an upstream Kubernetes controller.
- **No silent semantic changes:** explicit warning or refusal if a mapping can cause data loss, break reachability, skip health isolation, or alter restart/update guarantees. Unsupported native choice should be opt-in, not guessed.

## Capability matrix: present vs proposed

| Docker/Compose intent | Native Kubernetes mapping | This healthcheck branch today | Direction and release gate |
|---|---|---|---|
| Replicated stateless service (`deploy.replicas`) | Deployment + ReplicaSet, native RollingUpdate | **Implemented**, single Deployment per Swarm service | Retain default; preserve start-first/maxSurge and use truthful task readback |
| Global service (`deploy.mode: global`) | DaemonSet | **Not implemented:** warns then deploys one replica | Add explicit DaemonSet adapter with create/update/scale/list/inspect parity; do not call one replica equivalent to global |
| Stable individual replica identity | StatefulSet ordinals or per-slot Deployment | **Not implemented:** ConfigMap-based logical slot assignment over a shared ReplicaSet | Optional, capability-selected native controller after migration/rollback analysis; no silent controller replacement |
| Explicit healthcheck | d2k health command monitor + native readiness gate / EndpointSlice gating | **Implemented with known limits** | Verify failures remove workload from native Service endpoints; preserve Docker monitor log/timing separately |
| Image-inherited HEALTHCHECK | OCI image metadata or registry/cache resolution | **Not fully supported** (warning rather than silent inheritance) | Consider OCI metadata resolution opt-in and pinning; never fabricate that Kubernetes exposes Dockerfile HEALTHCHECK directly |
| Service discovery and published ports | Core Service (ClusterIP/LoadBalancer/NodePort), EndpointSlice, DNS | **Implemented with mode limits** | Verify native readiness/ejection; hostPort/dnsrr with healthchecks remain fail-closed where un-gatable |
| Docker networks (`internal`, attached networks, per-network isolation) | Kubernetes NetworkPolicy via Cilium, labels and Service DNS | **Not enforced on this branch:** synthetic flat networking | Integrate the *separate* hardened network-isolation patch later, as its own upstream PR; verify Docker network membership/isolation with health-gated Service endpoints |
| Named volumes | PVCs + CSI | **Implemented**, default RWO, NFS CSI matching for RWX | Make storage-class/access-mode constraints explicit; never silently share RWO writable volume across replicas |
| `tmpfs` | `emptyDir.medium: Memory` | **Implemented** | Validate capacity, lifecycle semantics |
| Host bind mount | `hostPath` | **Implemented with host coupling** | Warn about node-affinity, security, portability and storage persistence; no misleading portability guarantee |
| Secrets and configs | Kubernetes Secret and ConfigMap volumes | **Implemented** | Version/update/remount behavior and least privilege must be tested |
| One-shot Compose job/cron-like task | Job / CronJob | **Not implemented as a general Docker API mapping** | Add only with explicit task/workload intent and API semantic design |
| Docker restart/task lifecycle | Kubernetes container restart plus d2k persisted per-slot intent | **Partial emulation; HC-PRR lifecycle findings open** | No claimed Swarm parity without kill-tests for Lease handoff, crash-after-delete, MaxAttempts and delay |
| Network policy and WAF / ingress | Cilium policy, Ingress/Gateway API/Traefik as separate ingress infrastructure | **Not provided by this healthcheck patch** | Keep external proxy, CNI dataplane and application orchestration independently managed |

## Selecting controllers safely

1. **Default replicated service -> Deployment** to preserve existing manifests, rollout/scale semantics and compatibility.
2. **Stable ordinal workload -> optionally StatefulSet**, only if user/app requires ordinal network identity, per-slot PVCs or identical restart identity. StatefulSet has reverse-ordinal update and different start-first guarantees; migration must be explicit and reversible.
3. **Per-node global service -> DaemonSet** after corresponding Docker Swarm global API translation and tests exist.
4. **Strong independent task-slot identity with start-first overlap -> consider one Deployment per slot**. This offers native per-slot controllers but multiplies resources; must define exact ownership, Service selectors, scale/rollout APIs, and migration first.
5. **Batch/one-shot -> Job/CronJob** when Docker intent actually maps to completion semantics rather than a continuous service.

Do not choose StatefulSet just because a Compose service mounts a shared persistent volume. The storage/access-mode semantics, especially RWX vs RWO, and update policy must decide suitability.

## Current HC-PRR-017 / 002 strategy

- Keep native Deployment surge rollouts and readiness gates.
- Maintain logical slot state in ConfigMap as a *compatibility index*, not as a replacement for ReplicaSet.
- Allow provisional surge slots for monitor readiness, but do not advertise them as an extra desired Swarm replica.
- Promote surviving surge Pods into vacant desired slots after old Pods are retired.
- Pending health failure state reserves a slot across Kubernetes delete/ConfigMap crash gap.
- Admit replacement candidates only after excluding pre-failure Pod UID snapshot, bounding creation-time interpretation, checking ReplicaSet revision and using deterministic tie-breaks.
- The surviving ambiguity of simultaneous same-ReplicaSet replacements is **explicitly unresolved causal lineage**, even when deterministic unique mapping passes a test.

## Cross-patch integration acceptance

Before combining health-check and Cilium network-isolation patches in the Talos kit:

1. Bring up a multi-service Compose stack with internal network, shared/external networks, service discovery, health checks, volumes and secrets.
2. Verify d2k-created native Services/EndpointSlices remove unhealthy Pods from routable backends; test `ClusterIP`, `LoadBalancer` and service DNS.
3. Verify Cilium policies enforce Docker-like network membership and `internal` egress deny **without** blocking legitimate liveness/health Pod exec or DNS resolution.
4. Verify each accepted endpoint mode is really health-gated; reject hostPort/dnsrr exposure if safety cannot be enforced.
5. Run real Kubernetes controller, resize, rolling update, node loss, ReplicaSet overlap, Lease takeover and crash recovery tests.
6. Verify Kubernetes RBAC least privilege, controller ownerReferences, UID precondition deletion, namespace confinement and cleanup.
7. Compare Docker CLI/Portainer service/task readback and inspect against the native runtime state through restarts.
8. Do not mark feature complete from Go unit/race/build green alone.

**PR hygiene:** This file and the HC-PRR journal are engineering working records. Extract separate, reviewable upstream changes for health and network isolation; do not conflate them into a single cosmetic rewrite.
