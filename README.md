# workspaces-platform

Ephemeral employee dev desktops + isolated agent sandboxes on Kubernetes.

MVP locks:
- Employee desktops: container-based "workspace pods" (no systemd), accessed over SSH for VS Code Remote.
- Agents: stronger isolation first (Kata `RuntimeClass` microVMs), default-deny egress.
- GitHub writes: broker-only (GitHub App), PR-only by default.
- Access: private via Tailscale; "gateway" is the only ingress.

Repo layout:
- `cmd/workspaces-operator`: reconciles CRDs into Kubernetes workloads.
- `cmd/egress-proxy`: HTTP CONNECT proxy that enforces `NetworkGrant` allowlists (proxy-first egress).
- `cmd/wsctl`: minimal CLI (create desktops/jobs, request netgrants, `wsctl doctor` preflight checks).
- `api/v1alpha1`: CRD Go types (Desktop, AgentJob, NetworkGrant).
- `k8s/`: CRDs + example manifests (Cilium policies, operator deploy).
- `k8s-overlays/`: Kustomize overlays that bundle optional features (audit, proxies, admission guardrails).

Docs:
- `ARCHITECTURE.md`
- `docs/mvp.md`
- `docs/overlays.md`
- `docs/access.md`
- `docs/security.md`
- `docs/audit.md`
- `docs/ci.md`
- `docs/cost.md`
- `docs/threat-model.md`
- `docs/github.md`
- `docs/storage.md`
- `docs/config.md`
- `docs/reproducibility.md`
- `docs/egress-proxy.md`
- `docs/wsctl.md`
- `docs/troubleshooting.md`

This repo is an MVP that compiles and deploys manifests, but you still need to wire:
- image build/publish for operator/broker/desktop/agent images
- GitHub App Secret + allowlists for broker PR creation
- Vault integration + tamper-evident audit shipping to MinIO
