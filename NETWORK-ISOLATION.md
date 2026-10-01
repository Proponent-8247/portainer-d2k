# Kit d2k network-isolation overlay

Applied to upstream Portainer d2k 1.2.3 by the kit build workflow.

The overlay persists Docker network definitions in Kubernetes ConfigMaps, stamps translated workloads with deterministic multi-network membership labels, and enforces those memberships with standard Kubernetes NetworkPolicy (enforced by Cilium).

It also adds Docker network connect/disconnect handling, fail-closed baseline policy, world egress excluding Pod/Service CIDRs, internal-network egress restrictions, published-port ingress policy, and host-network rejection by default.
