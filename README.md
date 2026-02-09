# workspaces-platform

Ephemeral employee dev desktops + isolated agent sandboxes on Kubernetes.

MVP locks:
- Employee desktops: container-based "workspace pods" (no systemd), accessed over SSH for VS Code Remote.
- Agents: stronger isolation first (Kata `RuntimeClass` microVMs), default-deny egress.
- GitHub writes: broker-only (GitHub App), PR-only by default.
- Access: private via Tailscale; "gateway" is the only ingress.

Repo layout:
- `cmd/workspaces-operator`: reconciles CRDs into Kubernetes workloads.
- `api/v1alpha1`: CRD Go types (Desktop, AgentJob, NetworkGrant).
- `k8s/`: CRDs + example manifests (Cilium policies, operator deploy).

This repo is an MVP skeleton. It compiles, but you still need to wire:
- real desktop/agent images (see TODOs in manifests)
- GitHub App credentials in the capability broker
- Vault + audit ingestion (planned, not implemented here yet)

