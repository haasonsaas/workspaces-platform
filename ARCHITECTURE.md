# Architecture (workspaces-platform)

This document describes the target architecture for ephemeral employee dev desktops and strongly isolated agent sandboxes on Kubernetes.

## Goals

- **Agents can safely run code**
  - hardware-virtualization-backed isolation (Kata microVMs first)
  - least privilege: no standing credentials, scoped/time-bounded capabilities
  - default-deny networking, with explicit unlocks
  - auditing sufficient to answer: who/what/when/where for agent actions
- **Employee UX first**
  - low latency interactive dev via SSH + VS Code Remote
  - fast reconnects and “warm start” via persistent home volumes + caches
- **Reproducible builds**
  - pinned base images; toolchains driven by Nix (preferred)
  - shared caches/proxies to avoid “cold internet builds”
- **Cost control**
  - quotas + concurrency caps + TTL/autosuspend

## Non-Goals (MVP)

- Full VDI (RDP/WebRTC) desktop by default.
- Multi-region out of the gate.
- Complex identity propagation inside guests (OIDC in-guest users, etc).
- Perfect audit immutability on day 1 (we design for it and add it once pipelines are stable).

## High-Level System Diagram

```
                    (private)
   Laptop  ──Tailscale SSH──►  Gateway
                               │
                               │ Option A: nc → ClusterIP Service
                               │ Option B: ws-proxy → K8s port-forward
                               ▼
                         Desktop Pod (sshd)
                         + home PVC


            AgentJob CRD
                │
                ▼
         Agent Job Pod (Kata RuntimeClass)
         (default-deny egress)
                │
                ▼
         Capability Broker
        ┌────────┼───────────────────────────┐
        │        │                           │
        ▼        ▼                           ▼
   NetworkGrant  GitHub PR write path     Vault (later)
      CRD          (GitHub App)        (short-lived creds)
        │
        ▼
 CiliumNetworkPolicy (FQDN egress, TTL)


 Audit events (agents heavy; humans metadata-only)
   → append-only store (MinIO + signing; later WORM/ObjectLock)
```

## Kubernetes Substrate

### Node Pools

Separate pools (labels + taints) so dev matches prod.

- **desktops**
  - label: `workspaces.platform.dev/pool=desktops`
  - taint: `workspaces.platform.dev/pool=desktops:NoSchedule`
- **agents**
  - label: `workspaces.platform.dev/pool=agents`
  - taint: `workspaces.platform.dev/pool=agents:NoSchedule`
- **priv-compat** (optional)
  - dedicated hardware / pool with heavier auditing + stricter quotas

### Runtime Isolation

- **Agents** run using Kata Containers via a `RuntimeClass` (typically `kata`).
  - This gives “VM-like” isolation (microVMs) even when the control plane is Kubernetes-native.
  - Nested virt is acceptable in dev; production should prefer bare metal or guaranteed nested virt hosts.
- **Employee desktops (day 1)** run as regular containers in the `desktops` pool for iteration speed.
  - Later: add a **VM desktop class** (KubeVirt) for users that truly need systemd or stronger isolation.

### Networking

- Cilium provides:
  - namespace and pod-based policy enforcement
  - DNS/FQDN-aware egress allowlists
  - flow visibility via Hubble

Policy shape:
- `agents`: default-deny egress; allow only DNS + broker + required registries/proxies. Everything else is unlocked per-job via `NetworkGrant`.
- `desktops`: can be less strict initially, but should still prefer “known-good” outbound (package proxies, artifact store).

## Core Control Plane Components

### 1) Workspaces Operator

Binary: `cmd/workspaces-operator`

CRDs:
- `Desktop` → Deployment + Service + Secret (authorized_keys) + PVC (home)
- `AgentJob` → Kubernetes Job (Kata runtime class by default)
- `NetworkGrant` → CiliumNetworkPolicy (FQDN egress, TTL)

Key conventions:
- Labels:
  - `workspaces.platform.dev/app` = `desktop` or `agent`
  - `workspaces.platform.dev/desktop` = Desktop name
  - `workspaces.platform.dev/agentjob` = AgentJob name

### 2) Capability Broker (Choke Point)

Binary: `cmd/capability-broker`

Responsibilities:
- **Network unlocks**: create/approve `NetworkGrant` resources (scoped, TTL-bound).
- **GitHub writes (broker-only)**: apply an agent-produced patch and open a PR via GitHub App.
- Later:
  - mint short-lived Vault credentials per action
  - mint scoped GitHub read tokens (if needed)
  - implement stricter “capability approval” policy and audit trails

Security properties:
- Agents never get GitHub write tokens.
- Broker enforces:
  - repo allowlist (`GITHUB_REPO_ALLOWLIST`)
  - base branch allowlist
  - branch prefix (default `agent/`)
- Broker logs audit events without storing patch bodies in logs.

### 3) Gateway (Tailscale Edge)

Day 1: Tailscale SSH terminates user access on a single gateway.

Desktop access patterns:
- **Option A**: gateway can resolve and route to `*.svc.cluster.local` and ClusterIP Services
  - `ProxyCommand ssh gateway -- nc %h %p`
- **Option B**: gateway cannot route to ClusterIP Services
  - `ProxyCommand ssh gateway -- ws-proxy %h %p`
  - `ws-proxy` uses Kubernetes API port-forward to the backing pod and shuttles bytes.

See `docs/access.md`.

## Data Flows

### Desktop Provisioning (Employee)

1. User (or automation) creates `Desktop` in namespace `desktops`.
2. Operator creates:
  - a home PVC (persistent)
  - an `authorized_keys` Secret (mounted read-only)
  - a single-replica Deployment (Recreate strategy)
  - a ClusterIP Service exposing SSH (Service port 22 → container port 2222)
3. User connects via the gateway; VS Code Remote uses SSH.

Persistence model:
- compute is ephemeral (pod can be rescheduled/replaced)
- the home PVC persists and should be snapshotted/cloned for “reset to clean” and “warm start”

### Agent Execution

1. System creates an `AgentJob` (often from a CLI/API or GitHub trigger later).
2. Operator creates a Kubernetes Job:
  - `runtimeClassName: kata` by default
  - no service account token mounted
  - non-root, no capabilities, seccomp default
3. Agent runs in default-deny egress mode.
4. If agent needs extra network access, it requests a `NetworkGrant` via broker.

### Network Unlock (In-Progress)

1. Agent calls broker to request access to `dest.example.com:443` for job X.
2. Broker creates `NetworkGrant` with `approved=false`.
3. Approver flips to `approved=true` with `ttlSeconds`, `approvedBy`, `reason`.
4. Operator translates it into a `CiliumNetworkPolicy` scoped to that job’s labels.
5. On expiry, operator deletes/invalidates the policy.

### GitHub PR (Broker-Only Writes)

1. Agent produces a patch (unified diff) and calls broker `POST /v1/github/open-pr`.
2. Broker:
  - validates repo/base/branch policy
  - clones repo using GitHub App installation token
  - checks out base, creates `agent/<slug>` branch
  - applies patch, commits, pushes
  - opens PR via GitHub API
3. Broker emits an audit event for the write.

## Storage + Caching

Persistent:
- Desktop home PVC (dotfiles, editor state, bounded caches)

Shared:
- MinIO for artifacts, caches (sccache, package proxy storage), and audit bundles
- package proxies (npm/pypi/cargo, docker registry proxy)
- optional: Nix binary cache (backed by MinIO)

Snapshots/clones:
- required for fast warm provisioning (“clone warmed home/cache”)
- required for “reset to clean” rollback

## Auditing (Design)

Minimum required (agents):
- job metadata (who/when/image/runtime/policy)
- command exec metadata (argv, exit, cwd, duration)
- stdout/stderr capture with mandatory redaction
- network egress tuples (dest + port + DNS/SNI when available)
- GitHub actions (branches/PRs created)

Humans:
- metadata only by default (no keystroke logging)

Target properties:
- append-only, tamper-evident storage
- signatures (Vault transit or similar)
- 30 days hot / 90 days cold (baseline)

MVP implementation logs audit events as JSON to broker logs; the next step is shipping to MinIO with hash chaining + signing.

## Roadmap (Concrete Next Steps)

1. Vault integration in broker:
   - broker issues short-lived, scoped credentials
   - agents never receive long-lived secrets
2. Audit pipeline:
   - hash chain + signature
   - MinIO Object Lock/WORM in prod
3. Desktop VM class (KubeVirt) for systemd + stronger isolation users
4. Autosuspend + quotas:
   - desktop idle detection, suspend/stop
   - org/user concurrency caps for agent jobs
5. GitHub-native triggers:
   - PR comment `/agent run`, check runs, status reporting
