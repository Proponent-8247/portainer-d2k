# Docker-network-equivalent isolation

d2k can preserve Docker logical-network isolation while still using Kubernetes/Cilium as the only datapath.

## Enablement

Set all of the following on the d2k server:

```text
D2K_NETWORK_ISOLATION=true
D2K_POD_CIDRS=<comma-separated Kubernetes Pod CIDRs>
D2K_SERVICE_CIDRS=<comma-separated Kubernetes Service CIDRs>
D2K_REJECT_HOST_NETWORK=true
```

CIDRs are parsed and validated at startup. Invalid or missing CIDRs make d2k fail closed instead of creating an unsafe world-egress policy.

The d2k ServiceAccount also needs namespaced CRUD access to `networking.k8s.io/networkpolicies`. The bundled `deploy/kubernetes.yaml` includes that RBAC permission.

## Security model

d2k persists each user-created Docker network as a Kubernetes ConfigMap and assigns a deterministic membership label to every translated Deployment/Pod attached to that network.

For each logical Docker network, d2k reconciles a Kubernetes NetworkPolicy:

- same-network ingress is allowed;
- same-network egress is allowed;
- DNS to CoreDNS is allowed;
- ordinary networks receive world egress, excluding configured Pod and Service CIDRs;
- `internal: true` networks do not receive world egress;
- `none` receives no connectivity;
- published ports receive a separate explicit ingress policy;
- a namespace baseline denies ingress and egress for d2k-managed workloads unless another d2k policy allows it.

Multiple Docker networks are additive: a workload attached to `frontend` and `backend` receives both membership labels and can communicate with peers on either network. Peers that share no logical network remain isolated even though all Pods are in the same Kubernetes namespace.

Standard Kubernetes NetworkPolicy is used intentionally. On the Talos deployment kit, Cilium is the policy engine and remains the sole CNI.

## Migration safety

Enabling isolation over legacy d2k workloads is deliberately fail-closed.

At startup, before installing the baseline default-deny policy, d2k checks every existing d2k-managed Deployment. Each must already contain valid network-membership annotations and matching Pod-template labels. If a legacy or inconsistent workload is found, d2k refuses to enable isolation and explains which workload must be recreated.

This prevents an upgrade from silently blackholing existing workloads.

The baseline default-deny policy should therefore be created by d2k itself after this preflight, not pre-applied by external deployment tooling.

## Persistent reconciliation

Network definitions are stored in ConfigMaps labelled `d2k.portainer.io/network-state=true`. On startup and network-list operations, d2k restores this state and reconciles the corresponding NetworkPolicies.

If an isolation policy is deleted while d2k is stopped, the next restore recreates it.

Corrupt persisted state causes startup/list failure rather than being silently ignored.

## Docker API fidelity

When isolation is enabled:

- container inspect reports the logical networks actually attached to the workload;
- Swarm service inspect reports the network IDs stored on the Deployment;
- Swarm task responses include those network attachments;
- network inspect reports attached d2k workloads;
- the old per-service synthetic network workaround is disabled.

This keeps Portainer's Docker/Swarm view aligned with the network membership Cilium enforces.

## Lifecycle behavior

`docker network rm` refuses to remove a network with active endpoints.

`docker network connect` respects Swarm overlay `Attachable`; a standalone container cannot manually join a non-attachable overlay.

`docker stack rm` routes service and network deletion through canonical cleanup paths so it also removes published-port policies, persisted network ConfigMaps, and network isolation policies. In isolation mode, stack ownership is taken from Docker's explicit `com.docker.stack.namespace` label; external networks without that label remain intact.

Network create failures roll back partially persisted state.

Published-port policies are installed only after the workload and Services have been created successfully, so failed workload creation does not leave orphan allow policies.

## Explicit exceptions

Host networking bypasses Pod-level NetworkPolicy and is rejected by default while isolation is enabled. Set `D2K_REJECT_HOST_NETWORK=false` only if that escape hatch is explicitly desired.

`macvlan` and `ipvlan` remain unsupported because d2k does not create secondary CNI interfaces.

Swarm `dnsrr` / publish-mode `host` still uses Kubernetes `hostPort` for publication. The Pod remains subject to its Docker-network policy for Pod-network traffic, but the externally published path is node-local rather than a normal Kubernetes LoadBalancer Service.

## Validation

The repository contains unit/regression tests for migration safety, policy reconciliation, internal/world egress, in-use deletion, attachability, cleanup, and Docker/Swarm API readback.

For real-cluster validation, run:

```bash
hack/validate-network-isolation.sh
```

against a d2k Docker endpoint on the target cluster. The Talos deployment kit additionally performs its own live Cilium acceptance checks before declaring the feature production-accepted.


## Additional lifecycle guarantees

A workload may be deliberately disconnected from every logical network. d2k records that as an explicit empty membership list (`[]`); restart validation accepts it and the baseline default-deny keeps the workload isolated.

The internal short-name DNS Service and the externally published LoadBalancer/NodePort Service use different Kubernetes object names, so publishing a port cannot collide with the Service used for container-name DNS.

Only ports with an actual Docker/Swarm publication are admitted by the published-port ingress policy. Merely declaring a target/container port does not open ingress.

At runtime, restoring persistent network state also reconciles the baseline default-deny policy after isolation has passed its startup migration check. This repairs accidental policy deletion without weakening the migration guard.


## Stack ownership and disconnected workloads

In isolation mode, stack network ownership is taken from Docker's explicit `com.docker.stack.namespace` label rather than inferred from an underscore in the network name. This prevents `docker stack rm demo` from deleting an unrelated external network merely because it is named `demo_shared`.

An explicit empty network-membership list remains empty in Docker/Swarm readback and during service updates. d2k does not silently reattach a deliberately disconnected workload to its default synthetic network.

Standalone containers may join Swarm-scoped overlay networks only when those networks are attachable. This is enforced both at initial `docker run --network ...` time and on later `docker network connect` operations.

`macvlan` and `ipvlan` network creation is rejected explicitly rather than presenting a compatibility object that cannot provide the requested L2 semantics.


## Update and cleanup consistency

Swarm service updates reapply the requested mutation after Kubernetes optimistic-concurrency conflicts instead of returning success after updating an unmodified refetched Deployment. An explicitly empty network list remains empty across those retries.

Published-port policy is reconciled only after the workload update succeeds. If policy reconciliation fails, d2k removes the allow policy best-effort and returns an error so the failure mode is closed.

Swarm DNS Services are removed using the explicit `d2k.portainer.io/dns-for-deploy` ownership annotation. d2k does not attempt to reconstruct a bare DNS name from a sanitised Deployment name, which is ambiguous when stack or service names contain hyphens.


The live validation harness also verifies initial attachment to non-attachable overlays is rejected, host-network escape is rejected, macvlan/ipvlan creation fails explicitly, a full disconnect remains disconnected in API readback, ordinary networks retain world egress, and internal networks do not.
