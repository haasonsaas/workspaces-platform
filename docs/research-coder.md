# Coder Research Notes (Patterns Worth Stealing)

This repo is building a secure-by-default “dev desktops + agent sandboxes” platform on Kubernetes.
Coder is a mature reference implementation for remote workspaces, and it’s useful to study because it solves:
- inbound connectivity without exposing per-workspace IPs
- durable UX via SSH/VS Code workflows
- workspace lifecycle (start/stop, rebuilds, metadata)
- auditability and admin controls

These notes are based on a local read-through of `coder/coder` (Go code + docs), with emphasis on agent connectivity and SSH ergonomics.

## What Coder Does (Relevant Bits)

### 1) “Workspace Agent” Is The Real Boundary

Coder runs an agent inside the workspace (“workspace agent”). The agent:
- connects **outbound** to the control plane (no inbound needed)
- speaks multiple “protocols” (SSH, dial, reconnecting PTY, etc) over a multiplexed connection
- reports lifecycle/health/metadata and connection stats
- can proxy audit-ish streams (“boundary logs”) back to the server

Useful code landmarks in Coder:
- `agent/` (agent main loops, subsystems)
- `agent/agentssh/` (SSH server surface)

### 2) Tailnet: A Purpose-Built Overlay Network

Coder implements an embedded tailnet layer to route traffic between:
- CLI (client)
- coderd (server)
- workspace agent(s)

It uses tailscale components (WireGuard engine, DERP relays) but tunes behavior for workspace-style sessions:
- point-to-point-ish connections
- long-lived SSH/port-forward sessions that shouldn’t stall due to peer trimming

Useful code landmarks:
- `tailnet/` (Conn + coordinator)
- `coderd/tailnet.go` (server-side dialing to agents)
- `docs/reference/api/agents.md` (`GET /tailnet`, `GET /derp-map`)

### 3) SSH UX: Managed `~/.ssh/config` + ProxyCommand

Coder provides a `config-ssh` command that:
- writes a **managed section** into the user’s SSH config
- makes `ssh workspace.coder` work via `ProxyCommand`
- uses a CLI “stdio mode” so OpenSSH can treat it like a TCP transport

Landmarks:
- `cli/configssh.go`
- `cli/ssh.go` (`--stdio` path)

### 4) “Apps” / Port Sharing: Safe Preview URLs

Coder has a concept of workspace “apps” and port sharing levels (public/private/authenticated).
coderd can reverse-proxy to agent ports without exposing the workspace network directly.

Landmarks:
- `coderd/workspaceapps/` (reverse proxy to agent)
- `coderd/portsharing/`
- `codersdk/workspaceagentportshare.go` (share levels + protocol hints)

### 5) Auditing Is First-Class In The Control Plane

Coder records audit logs centrally and also collects runtime metadata from agents.
This is useful both for security and for debugging “what happened inside the workspace.”

Landmarks:
- `coderd/audit/`
- agent reporting loops in `agent/agent.go` (metadata/connection reporting)

## What We Should Adopt In workspaces-platform

### A) `wsctl ssh-config` (Coder-inspired)

We want the same “one command makes SSH work” loop.
This repo includes `wsctl ssh-config`, which writes a managed SSH config block:
- default (`--mode pattern`): `Host desk-*` + `ProxyCommand wsctl proxy ...` (Coder-style, no kubeconfig needed on the laptop)
- optional (`--mode list`): explicit `Host desk-<name>` entries (requires kubeconfig to enumerate Desktops)

Why it matters:
- VS Code Remote adoption depends on SSH ergonomics
- managed sections prevent config drift and make updates repeatable

### B) Consider “Desktop Agent Reverse Tunnel” (Future, Optional)

Our current access path is:
- user → gateway (Tailscale SSH)
- gateway → Kubernetes API `port-forward` (ws-proxy) → desktop pod `sshd`

That’s safe, but it forces the gateway to hold Kubernetes credentials and reach the apiserver.

Coder’s pattern suggests an alternative:
- run a small per-desktop “ws-agent” sidecar that dials outbound to a gateway service
- gateway multiplexes streams (SSH + port forwards) back to the desktop
- gateway no longer needs `pods/portforward` RBAC at all

This would reduce “gateway compromise” blast radius by removing direct apiserver capabilities.
It’s more moving parts, so it should be an optional profile, not a day-1 requirement.

This repo now includes an **experimental** proof-of-concept skeleton:
- `cmd/ws-relayd`, `cmd/ws-relay`, `cmd/ws-desktop-agent`
- design notes: `docs/connectivity.md`

### C) Port Sharing As A First-Class CRD (Future)

Coder’s “apps/port sharing” is a useful model for preview URLs:
- explicit user intent (“share port 3000”)
- scoped TTL and share levels (`owner`, `authenticated`, `organization`, `public`)
- audit trail for exposures

We should implement this as a CRD instead of ad-hoc port-forwarding:
- `PortShare` (or `WorkspaceApp`) referencing a Desktop and a port
- gateway (or in-cluster proxy) enforces auth + routes to the desktop

Coder also enforces a template-level "max port share level". The analogous control for us is:
- enforce allowed share levels per Desktop security profile (or per repo/org policy)
- default-deny `public` unless explicitly enabled

## What We Probably Should Not Copy

- Building a full custom tailnet/DERP stack on day 1.
  - We already decided to use Tailscale for the edge and Kubernetes primitives internally.
  - If we need multi-region relay and “no gateway kubeconfig,” we can iterate toward a smaller reverse-tunnel first.

- Mixing “human convenience” and “agent execution” trust models.
  - Coder’s model is workspace-centric; our model must keep **agents** as strictly isolated tenants with proxy-first egress and brokered capabilities.

## Concrete Next Steps (If You Want To Go Further)

1. Add a `ws-agent` proof-of-concept (reverse tunnel) as an **optional** desktop class.
2. Add `PortShare` CRD and a minimal gateway reverse proxy to implement “preview URLs” with auth + TTL.
3. Extend auditing:
   - log gateway → desktop connection metadata (start/stop, desktop id, duration)
   - keep “no keystrokes” for humans by default
