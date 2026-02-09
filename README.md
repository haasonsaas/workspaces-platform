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

Docs:
- `ARCHITECTURE.md`
- `docs/mvp.md`
- `docs/access.md`
- `docs/security.md`
- `docs/audit.md`
- `docs/cost.md`
- `docs/github.md`
- `docs/storage.md`
- `docs/config.md`
- `docs/reproducibility.md`
- `docs/wsctl.md`

This repo is an MVP that compiles and deploys manifests, but you still need to wire:
- image build/publish for operator/broker/desktop/agent images
- GitHub App Secret + allowlists for broker PR creation
- Vault integration + tamper-evident audit shipping to MinIO
