# Security Model

This platform is designed around the assumption that **agent-executed code is hostile**.

## Isolation Profiles

Day 1 profiles:
- `desktop` (humans): container "workspace pods" for iteration speed.
- `agent` (automation): Kata microVMs (`RuntimeClass: kata`) with strict policy.
- `priv-compat` (opt-in): for rare workflows that need privileged features; isolated node pool + heavier audit.

## Kubernetes Hardening (Baseline)

For agents (and ideally desktops over time):
- `allowPrivilegeEscalation: false`
- drop all Linux capabilities
- `seccompProfile: RuntimeDefault`
- `runAsNonRoot: true` (agents)
- `automountServiceAccountToken: false` (agents)

## Network Security

Agents are **default-deny egress**.

Allowed by default:
- DNS
- capability broker
- required package registries/proxies (prefer internal mirrors; agents should use proxies by default)

Everything else:
- requires a `NetworkGrant` approval
- is scoped to a specific job’s pod labels
- is time-bounded (TTL)

Implementation detail:
- `NetworkGrant` → `CiliumNetworkPolicy` using `toFQDNs` + `toPorts`

MVP constraints (enforced by controller; no admission webhook required):
- exact FQDN only (no wildcards)
- TCP only
- 443-only by default (set `allowNon443: true` to permit non-443)
- `purpose` required on every `NetworkGrant`

Hard defaults:
- agents are default-deny egress
- deny cloud metadata endpoints (`169.254.169.254`, etc.)
- do not allow direct public internet egress by default; prefer internal proxies/mirrors

## Capability Broker (Least Privilege)

The broker is the choke point for:
- network unlocks (temporary egress allow)
- GitHub writes (PR-only)
- Vault credentials (later)

Design rules:
- no long-lived secrets in pods
- mint short-lived tokens per action
- enforce allowlists (repos, branches, destinations)
- write audit events for each capability use

## GitHub Writes (Broker-Only)

MVP policy:
- agents never get GitHub write tokens
- broker uses a GitHub App installation token to:
  - clone
  - create `agent/*` branch
  - push
  - open PR

Broker enforces:
- `GITHUB_REPO_ALLOWLIST`
- base branch allowlist
- branch prefix policy

## Secrets + Output Redaction

Agents often print secrets accidentally (dependency scripts, debug output).

Target behavior:
- prevent secrets from being present in the environment whenever possible
- redact known token/key patterns in captured stdout/stderr
- avoid storing unredacted logs; if you add break-glass, make it explicit, rare, and heavily gated

## Supply Chain Controls (Planned)

Recommended next steps:
- pin all runtime images by digest in production
- sign images (cosign) and enforce verification at admission
- route dependency downloads through internal proxies/mirrors
- use Nix flake locking for toolchain pinning
