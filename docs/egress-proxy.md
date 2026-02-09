# Egress Proxy (HTTP CONNECT)

This repo includes an optional-but-recommended **egress proxy** component (`cmd/egress-proxy`) that enables a stricter agent network model:
- agent pods have **no direct internet egress**
- agent pods can only reach in-cluster services (broker, package proxies, egress-proxy)
- approved `NetworkGrant`s allow a job to CONNECT to specific `host:port` destinations **through the egress-proxy**

## How It Works

1. Agent pods connect to `egress-proxy.workspaces-system.svc.cluster.local:8080` using standard `HTTP_PROXY` / `HTTPS_PROXY`.
2. The proxy identifies the caller by **Pod IP** (must be a pod in `agents` with `workspaces.platform.dev/app=agent`).
3. The proxy reads active, approved `NetworkGrant`s in the `agents` namespace and computes an allowlist by:
   - `spec.agentJobRef.name` (preferred), or
   - `spec.podSelector.matchLabels` (best-effort)
4. For `CONNECT host:port`, the proxy allows the connection only if `host:port` is in the allowlist.

## NetworkGrant Modes

`NetworkGrant.spec.policyMode` controls enforcement:
- `STRICT_FQDN`: controller creates a `CiliumNetworkPolicy` allowing direct pod-to-destination egress.
- `PROXY_CONNECT`: controller does **not** create direct egress policy; the egress-proxy enforces the allowlist.

For non-admin workflows, the broker defaults to **proxy-first**:
- internal destinations: `STRICT_FQDN`
- public destinations: `PROXY_CONNECT`

## Enabling For Agents

The operator can inject proxy environment variables into AgentJob pods:
- `AGENT_EGRESS_PROXY_URL` -> `HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY`
- `AGENT_NO_PROXY` -> `NO_PROXY` (and lowercase variants)

See `docs/config.md` and `k8s/operator/configmap.yaml`.

## Notes / Limitations

- MVP proxy supports **CONNECT only** (no plaintext HTTP proxying).
- Destination matching is **exact hostnames** (no wildcards), consistent with `NetworkGrant` validation.
- The proxy is not a security boundary by itself. It is one layer of the overall model:
  - agent pods are default-deny egress
  - the broker is the capability choke point
  - NetworkGrants are TTL-bound + audited

