# Connectivity Modes (Port-Forward vs Reverse Tunnel)

This repo defaults to a gateway that reaches desktops using the Kubernetes API `port-forward` path (`ws-proxy`).
That is safe and simple, but it requires the gateway to hold Kubernetes credentials.

Coder's architecture strongly favors "agents connect outbound to the control plane", avoiding inbound reachability and reducing blast radius.
We can adopt a similar idea for **desktops** as an optional mode.

## Mode A (Default): Gateway `ws-proxy` (K8s Port-Forward)

Flow:
- laptop SSH (VS Code Remote) -> Tailscale SSH gateway
- gateway runs `ws-proxy <service>.<ns> <port>`
- `ws-proxy` uses Kubernetes API `pods/portforward` and shuttles bytes

Properties:
- gateway needs minimal RBAC:
  - `get` Services
  - `list` Pods
  - `create` `pods/portforward`
  - `patch` Desktop (heartbeat)
- good default for MVP; no new in-pod components

Risk:
- gateway compromise implies some Kubernetes API capabilities (even if limited)

Docs:
- `docs/access.md`

## Mode B (Experimental): Desktop Agent Reverse Tunnel (No Gateway Kubeconfig)

Goal:
- remove Kubernetes API credentials from the gateway entirely
- keep the edge authenticated (Tailscale SSH gateway)
- make inbound access to the desktop strictly mediated by a relay + explicit allowlists

Conceptual flow:
- a small `ws-desktop-agent` inside the Desktop maintains an outbound control connection to a relay on the gateway
- when a user connects, a local `ws-relay` ProxyCommand asks the relay to open a stream to the desktop's `sshd` port
- the desktop agent connects back to the relay for that stream and bridges to localhost

This repo includes **experimental** binaries:
- `cmd/ws-relayd`: relay daemon (gateway host)
- `cmd/ws-relay`: ProxyCommand client (runs on gateway via `ssh ws-gateway -- ws-relay ...`)
- `cmd/ws-desktop-agent`: in-pod agent that bridges a stream to localhost

SSH config helper:
- `wsctl ssh-config --connectivity relay` generates a `ProxyCommand` that uses `ws-relay` on the gateway.

Safety defaults in `ws-relayd`:
- requires auth:
  - preferred: JWT (`WS_RELAYD_JWT_SECRET`, HMAC-SHA256, `sub=<namespace>/<desktop>`)
  - fallback: explicit token allowlist (`WS_RELAYD_TOKENS_JSON` or `WS_RELAYD_TOKENS_FILE`)
- restricts target ports by default to `2222` only (`WS_RELAYD_ALLOWED_PORTS=2222`)

What this mode does *not* solve yet:
- tying dial permissions to user identity (today it relies on unix socket permissions + Tailscale SSH host auth)
- autosuspend heartbeats (today `ws-proxy` updates last-active; relay mode needs a separate signal path)
- "preview URLs" and `PortShare` enforcement (future integration)

## How To Enable Relay Mode (End-to-End)

1. Configure the gateway host:

- run `ws-relayd` (host service) and set:
  - `WS_RELAYD_JWT_SECRET` (required)
  - `WS_RELAYD_CONTROL_ADDR` (default `:7443`)
  - `WS_RELAYD_DATA_ADDR` (default `:7444`)
  - `WS_RELAYD_SOCKET_MODE` / `WS_RELAYD_SOCKET_GID` so the SSH user running `ws-relay` can access the unix socket

The `WS_RELAYD_JWT_SECRET` must match the operator's `DESKTOP_RELAY_JWT_SECRET`.

2. Configure the operator:

- Create the operator Secret (example):
  - `k8s/examples/workspaces-operator-secrets.yaml` (key: `desktop_relay_jwt_secret`)
- Set the operator ConfigMap keys:
  - `desktop_relayd_control_addr` and `desktop_relayd_data_addr` to addresses reachable from Desktop pods (often a LAN/VIP address to the gateway host).
  - `desktop_relay_agent_image` (defaults to `ghcr.io/haasonsaas/workspaces-ws-desktop-agent:latest`).

3. Create a Desktop with relay mode enabled:

```yaml
apiVersion: workspaces.platform.dev/v1alpha1
kind: Desktop
metadata:
  name: jonathan
  namespace: desktops
spec:
  user: jonathan
  connectivity:
    mode: relay
```

Or via CLI:
```bash
wsctl desktop create ... --connectivity relay
```

4. Generate SSH config using relay connectivity:

```bash
wsctl ssh-config --connectivity relay ...
```

This generates a `ProxyCommand` that runs `ws-relay` on the gateway to request a reverse-tunnel stream via `ws-relayd`.

## How This Connects To `PortShare`

Long-term, the reverse-tunnel relay should only allow non-SSH ports when a corresponding `PortShare` exists (and share level permits it).
Today, `ws-relayd` uses a static allowed port list as a stopgap.

## Recommendation

- ship Mode A first (it already works and is safe with minimal RBAC)
- prototype Mode B in a separate node pool/profile for higher security needs
- once Mode B is stable, consider making it the default for the most sensitive workflows
