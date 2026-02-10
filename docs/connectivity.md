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

Safety defaults in `ws-relayd`:
- requires an explicit desktop token allowlist (`WS_RELAYD_TOKENS_JSON` or `WS_RELAYD_TOKENS_FILE`)
- restricts target ports by default to `2222` only (`WS_RELAYD_ALLOWED_PORTS=2222`)

What this mode does *not* solve yet:
- automatic token provisioning + rotation (should be done by the operator)
- tying dial permissions to user identity (today it relies on unix socket permissions + Tailscale SSH host auth)
- "preview URLs" and `PortShare` enforcement (future integration)

## How This Connects To `PortShare`

Long-term, the reverse-tunnel relay should only allow non-SSH ports when a corresponding `PortShare` exists (and share level permits it).
Today, `ws-relayd` uses a static allowed port list as a stopgap.

## Recommendation

- ship Mode A first (it already works and is safe with minimal RBAC)
- prototype Mode B in a separate node pool/profile for higher security needs
- once Mode B is stable, consider making it the default for the most sensitive workflows

