# Reproducibility + Supply Chain

This platform is designed to be:
- reproducible (pinned inputs)
- proxy-first (few or no direct internet dependencies at build time)
- enforceable (admission/policy guardrails)

## 1) Pin Runtime Images By Digest

For production, prefer `image@sha256:<digest>` everywhere (desktops, agents, control plane).

Optional guardrails (Kubernetes ValidatingAdmissionPolicy):
- `k8s/optional/agentjob-validatingadmissionpolicy.yaml` enforces `AgentJob.spec.image` is digest-pinned
- `k8s/optional/desktop-validatingadmissionpolicy.yaml` enforces `Desktop.spec.image` is digest-pinned when explicitly set

Operationally:
- set broker default agent image (`k8s/broker/configmap.yaml` key `agent_default_image`) to a digest-pinned value
- set operator desktop default (`DEFAULT_DESKTOP_IMAGE` / `--default-desktop-image`) to a digest-pinned value

## 2) Use Nix For Toolchains (Preferred)

The recommended developer experience is:
- a digest-pinned base image with just the essentials
- project toolchains provided by Nix (flake-locked)

Suggested repo convention:
- `.workspaces/agent.sh` runs `nix develop` (or `nix build`) with a pinned flake
- desktop dotfiles can install Nix and set standard substituters once per user

## 3) Add A Binary Cache (On-Prem)

To avoid “cold internet builds” and make agents fast:
- run an internal Nix binary cache (Attic is a common choice)
- back it with MinIO/S3 for storage
- configure desktops/agents to use it as a substituter

Example Nix config snippet (conceptual):
```text
substituters = http://attic.workspaces-system.svc.cluster.local:8080/cache/default https://cache.nixos.org
trusted-public-keys = default:<your-attic-public-key>
```

## 4) Proxy-First Dependencies (Strongly Recommended)

Agents should not talk to the public internet by default. Instead:
- run internal package proxies/mirrors
- allow agent egress to those proxies (see `k8s/policies/agents-allow-internal-proxies.yaml`)
- treat direct internet `NetworkGrant` as an exception path with explicit approval + TTL

This repo intentionally labels proxies using:
- `workspaces.platform.dev/app=proxy`
- `workspaces.platform.dev/component=package-proxy`

So baseline agent policy can allow them without widening everything else.

Optional example manifests:
- `k8s/optional/package-proxy-verdaccio.yaml` (npm proxy/cache)
- `k8s/optional/package-proxy-athens.yaml` (Go module proxy)

Example client config:
- npm:
  - `npm config set registry http://verdaccio.workspaces-system.svc.cluster.local:4873`
- Go:
  - `export GOPROXY=http://athens.workspaces-system.svc.cluster.local:3000,direct`

## 5) Tighten DNS (Prevent “Free Exfil”)

Agents can exfiltrate data through DNS even when TCP egress is denied.

Baseline policy in this repo restricts DNS queries to in-cluster names by default (`k8s/policies/agents-default-deny.yaml`).
Approved `NetworkGrant`s add per-host DNS allow rules for their destinations (and optional `spec.dnsAllow` for extra DNS names like CNAME targets).
