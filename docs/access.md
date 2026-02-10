# Access Model (Day 1)

Lock: private access via Tailscale; no public SSH.

## Goal

VS Code Remote connects to the desktop's `sshd`, but the desktop is only reachable from inside the cluster.

## Default Path (Recommended): `ws-proxy` (K8s Port-Forward)

Do **not** make the gateway routable to `ClusterIP` services by default. It’s convenient, but it couples your edge box into the cluster network plane and expands lateral movement surface if the gateway is ever compromised.

Instead, default to a gateway that only talks to the Kubernetes API and uses port-forwarding to reach the target pod.

This repo implements the TCP shuttling helper as `ws-proxy` (`cmd/ws-proxy`).

Example SSH config:
```sshconfig
Host ws-gateway
  HostName <tailscale-ip-or-hostname>
  User <tailscale-ssh-user>

Host desk-*
  User <linux-user-in-desktop>
  ProxyCommand wsctl proxy --gateway ws-gateway --namespace desktops --host-prefix desk- %h %p
  StrictHostKeyChecking accept-new
```

You can generate and maintain this block automatically with:
```bash
wsctl ssh-config \
  --gateway-hostname <tailscale-ip-or-hostname> \
  --gateway-user <tailscale-ssh-user> \
  --desktop-user <linux-user-in-desktop>
```

`ws-proxy` parses the host as `<service>.<namespace>...`, finds the Service selector, picks a ready pod, then port-forwards to the pod and proxies bytes.

On each connection (and periodically while the session is open), `ws-proxy` updates the Desktop annotation `workspaces.platform.dev/last-active-at` (used for autosuspend).
The heartbeat interval is configurable via `WORKSPACES_HEARTBEAT_SECONDS` (default: 300s).

Gateway kubeconfig RBAC needs (minimum):
- `get` Services in the desktop namespaces
- `list` Pods in the desktop namespaces
- `create` on `pods/portforward` in the desktop namespaces
- `patch` on `workspaces.platform.dev/desktops` in the desktop namespaces (to write last-active annotation)

Example RBAC: `k8s/examples/gateway-rbac.yaml`.

## Privileged Mode (Optional): Gateway Can Route To `ClusterIP`

If you still want the “simple” flow where the gateway can resolve `*.svc.cluster.local` and route to `ClusterIP`, treat it as a more privileged gateway mode with separate hardening and monitoring.

Example (not recommended as the default):
```sshconfig
Host desk-jonathan
  HostName desktop-jonathan-ssh.desktops.svc.cluster.local
  User jonathan
  ProxyCommand ssh ws-gateway -- nc %h %p
```

If you enable this mode, assume that compromising the gateway meaningfully increases the blast radius.

## Experimental: Reverse Tunnel Mode (No Gateway Kubeconfig)

This repo includes an experimental "desktop agent reverse tunnel" design to remove Kubernetes API credentials from the gateway.

See: `docs/connectivity.md`.
