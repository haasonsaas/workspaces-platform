# Threat Model (MVP)

This platform assumes **agent-executed code is hostile**, even when sourced from internal repos (supply chain, dependency scripts, prompt injection, compromised contributors).

## Assets To Protect

- company internal network reachability
- secrets (Vault tokens, GitHub write tokens, cloud creds)
- GitHub org integrity (protected branches, workflows)
- cluster integrity (Kubernetes API, node escape)
- audit integrity (tamper evidence)

## Trust Boundaries

- **Agent sandboxes** (untrusted): run code, produce outputs (logs/patches), request capabilities.
- **Capability broker** (trusted choke point): mints short-lived capabilities (GitHub writes, network grants).
- **Operator/controllers** (trusted): enforce spec validation + policy reconciliation.
- **Employee desktops** (semi-trusted): human convenience; do not treat as a security boundary.

## Key Threats And MVP Mitigations

- **Arbitrary internet egress from agents**
  - Default deny egress in `agents` (Cilium).
  - Approved destinations via `NetworkGrant` (exact hostnames, TTL).
  - Public egress is proxy-first (`PROXY_CONNECT` via `egress-proxy`).

- **DNS exfiltration**
  - Baseline agent policy allows DNS only for in-cluster names.
  - `NetworkGrant` adds DNS allow rules for specific approved hostnames only.

- **Cluster API access**
  - Agent pods do not mount service account tokens.
  - `NetworkGrant` explicitly blocks `kubernetes.default.svc*` hostnames (no “unlock K8s API via egress”).

- **Policy bypass (self-approve egress / inject Cilium policies)**
  - Operator RBAC is reduced (can’t mutate `NetworkGrant.spec`).
  - Optional CEL admission policies restrict:
    - `NetworkGrant` create/update to the broker
    - `CiliumNetworkPolicy` changes in `agents` to the operator

- **GitHub write escalation (agent pushes to protected branches / workflow edits)**
  - Agents never get GitHub write tokens (MVP).
  - Broker is the only writer (GitHub App) and enforces:
    - repo allowlist + base branch allowlist + branch prefix
    - sensitive path denylist (workflows/infra/terraform/deploy by default)
    - size caps (patch body cap, max files changed)

- **Proxy as an internal bridge (DNS rebinding)**
  - `egress-proxy` refuses to dial private/link-local/CGNAT-resolved IPs by default.
  - Proxy ingress restricted to agent pods only.

## Residual Risks (MVP)

- Desktop pods are not strong isolation (container desktops by design for UX).
- Redaction is best-effort; assume some secrets can leak via logs in edge cases.
- Kata/VM isolation reduces kernel exposure but does not eliminate supply-chain risk.

