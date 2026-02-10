# Security Model

This platform is designed around the assumption that **agent-executed code is hostile**.

## Isolation Profiles

Day 1 profiles:
- `desktop` (humans): container "workspace pods" for iteration speed.
- `agent` (automation): Kata microVMs (`RuntimeClass: kata`) with strict policy.
- `priv-compat` (opt-in): for rare workflows that need privileged features; isolated node pool + heavier audit.

Agent policy profiles (sub-profiles) are documented in `docs/policy-profiles.md` and are enforced primarily at the broker and network policy layers.

## Kubernetes Hardening (Baseline)

For agents (and ideally desktops over time):
- `allowPrivilegeEscalation: false`
- drop all Linux capabilities
- `seccompProfile: RuntimeDefault`
- `runAsNonRoot: true` (agents)
- `automountServiceAccountToken: false` (agents)

For control plane components (operator/broker/webhook/proxies where supported):
- `runAsNonRoot: true`
- `readOnlyRootFilesystem: true` (with explicit `/tmp` emptyDir mounts)

## Network Security

Agents are **default-deny egress**.

Allowed by default:
- DNS (restricted to in-cluster names to prevent DNS exfil)
- capability broker
- required package registries/proxies (prefer internal mirrors; agents should use proxies by default)

Implementation:
- `k8s/policies/agents-allow-internal-proxies.yaml` allows egress from agent pods to in-cluster proxy workloads labeled `workspaces.platform.dev/app=proxy` and `workspaces.platform.dev/component=package-proxy`.

Everything else:
- requires a `NetworkGrant` approval
- is scoped to a specific job’s pod labels
- is time-bounded (TTL)

Implementation detail:
- `NetworkGrant.spec.policyMode=STRICT_FQDN` → `CiliumNetworkPolicy` using `toFQDNs` + `toPorts`
- `NetworkGrant.spec.policyMode=PROXY_CONNECT` → enforced by `egress-proxy` (HTTP CONNECT); the controller installs a DNS-only allow rule for the approved hostnames, and the proxy refuses to dial private/link-local/CGNAT IPs by default (DNS rebinding defense)

MVP constraints (enforced by controller; no admission webhook required):
- exact FQDN only (no wildcards)
- TCP only
- 443-only by default (set `allowNon443: true` to permit non-443)
- `purpose` required on every `NetworkGrant`
- `ttlSeconds` is capped by the operator (default `7200`; `NETWORKGRANT_MAX_TTL_SECONDS` / `--networkgrant-max-ttl-seconds`)
- `egress` destinations are capped by the operator (default `20`; `NETWORKGRANT_MAX_EGRESS_RULES` / `--networkgrant-max-egress-rules`)
- approved grants add per-host DNS L7 allow rules (and optional `spec.dnsAllow` for extra DNS names like CNAME targets)
- DNS allow rules are capped (default `50`; `NETWORKGRANT_MAX_DNS_NAMES` / `--networkgrant-max-dns-names`)

Hard defaults:
- agents are default-deny egress
- deny cloud metadata endpoints (`169.254.169.254`, etc.)
- do not allow direct public internet egress by default; prefer internal proxies/mirrors

Broker guardrail:
- GitHub comment approvals (webhook token) cannot approve public internet `NetworkGrant`s unless the hostnames are explicitly allowlisted (`BROKER_NETWORK_*`).
- Admin token approvals can override this (explicit escape hatch).

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
- redact literal secret values from env vars that look sensitive (names matching `*TOKEN*`, `*SECRET*`, `*PASSWORD*`, `*KEY*`, etc)
- redact entire PEM blocks (not just headers)
- avoid storing unredacted logs; if you add break-glass, make it explicit, rare, and heavily gated

Repo checkout tokens:
- PR-scoped AgentJobs need repo contents; broker mints a short-lived, repo-scoped **read** token via GitHub App.
- Token is stored in a per-job Secret (`agentjob-<name>-github`) and mounted only into the checkout initContainer.
- Main agent container never receives the token by default.

## Supply Chain Controls (Planned)

Recommended next steps:
- pin all runtime images by digest in production
- sign images (cosign) and enforce verification at admission
- route dependency downloads through internal proxies/mirrors
- use Nix flake locking for toolchain pinning

## Optional Admission Policies

These manifests are optional so clusters without this feature can still run the MVP.

- `k8s/optional/hardened/agents-validatingadmissionpolicy.yaml`: denies non-conforming **agent Pods** (host mounts, privileged flags, missing `runtimeClassName=kata`, etc).
- `k8s/optional/hardened/desktops-validatingadmissionpolicy.yaml`: denies non-conforming **desktop Pods** (host mounts, privileged flags, service account tokens, etc).
- `k8s/optional/hardened/agentjob-validatingadmissionpolicy.yaml`: enforces **AgentJob CR** guardrails (broker-only create/update, pinned image digests, runtime class constraints, policyProfile allowlist).
- `k8s/optional/hardened/desktop-validatingadmissionpolicy.yaml`: enforces **Desktop CR** guardrails (pinned image digests when explicitly set).
- `k8s/optional/hardened/networkgrant-validatingadmissionpolicy.yaml`: restricts **NetworkGrant** create/update to the capability-broker (prevents bypass via direct kubectl edits).
- `k8s/optional/hardened/portshare-validatingadmissionpolicy.yaml`: enforces **PortShare** guardrails (disables `public` by default; requires TTL for non-owner shares).
- `k8s/optional/hardened/ciliumnetworkpolicy-validatingadmissionpolicy.yaml`: restricts **CiliumNetworkPolicy** changes in `agents` to the operator (prevents bypass via direct Cilium policy injection).

Additional hardened network policy (non-admission):
- `k8s/optional/hardened/broker-ingress-policy.yaml`: restricts **capability-broker** ingress to agent pods and the GitHub webhook bridge (plus host probes).
