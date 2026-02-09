# Access Model (Day 1)

Lock: private access via Tailscale; no public SSH.

## Goal

VS Code Remote connects to the desktop's `sshd`, but the desktop is only reachable from inside the cluster.

## MVP Option A (Fastest): Gateway Can Reach Cluster DNS + Services

If your gateway host can resolve `*.svc.cluster.local` and reach the cluster Service network (or you run the gateway *inside* the cluster), you can use a plain SSH `ProxyCommand` that pipes bytes through the gateway:

```sshconfig
Host ws-gateway
  HostName <tailscale-ip-or-hostname>
  User <tailscale-ssh-user>

Host desk-jonathan
  HostName desktop-jonathan-ssh.desktops.svc.cluster.local
  User jonathan
  ProxyCommand ssh ws-gateway -- nc %h %p
  StrictHostKeyChecking accept-new
```

This works even when the gateway is using Tailscale SSH, because it doesn't require SSH port forwarding; it just executes `nc` on the gateway.

## MVP Option B: Gateway Outside Cluster (No Service Routing)

If the gateway cannot reach cluster Services directly, the clean approach is:
- gateway has access to the Kubernetes API
- gateway runs a small "TCP proxy" command that uses the K8s API (port-forward) and then shuttles stdin/stdout

That command can be used in `ProxyCommand` the same way as `nc`.

This repo implements it as `ws-proxy` (`cmd/ws-proxy`).

Example SSH config:
```sshconfig
Host ws-gateway
  HostName <tailscale-ip-or-hostname>
  User <tailscale-ssh-user>

Host desk-jonathan
  HostName desktop-jonathan-ssh.desktops.svc.cluster.local
  User jonathan
  ProxyCommand ssh ws-gateway -- ws-proxy %h %p
  StrictHostKeyChecking accept-new
```

`ws-proxy` parses the host as `<service>.<namespace>...`, finds the Service selector, picks a ready pod, then port-forwards to the pod and proxies bytes.

Gateway kubeconfig RBAC needs (minimum):
- `get` Services in the desktop namespaces
- `list` Pods in the desktop namespaces
- `create` on `pods/portforward` in the desktop namespaces
