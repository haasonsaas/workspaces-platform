# PortShare (Shared Desktop Ports)

`PortShare` is a CRD that models "share this port from a Desktop" (inspired by Coder's port sharing).

MVP behavior in this repo:
- The operator creates a **ClusterIP Service** that targets the Desktop pods on `spec.port`.
- It records the Service name in `status.serviceName`.
- Optional TTL (`spec.ttlSeconds`) is enforced by deleting the Service after expiry.

What it is *not* (yet):
- A public URL feature by itself. To get "preview URLs", you still need a gateway/proxy that maps an authenticated request to `status.serviceName` and enforces `spec.shareLevel`.

## Example

```yaml
apiVersion: workspaces.platform.dev/v1alpha1
kind: PortShare
metadata:
  name: ps-jonathan-3000
  namespace: desktops
spec:
  desktopRef:
    name: jonathan
  port: 3000
  protocol: http
  shareLevel: owner
  ttlSeconds: 3600
```

Expected resources:
- `Service desktops/desktop-jonathan-port-3000` (name may be truncated with a hash suffix for very long desktop names)

## Share Levels (Future Enforcement)

`spec.shareLevel` is stored today, and intended to be enforced by the gateway/proxy layer:
- `owner`: only the desktop owner
- `authenticated`: any authenticated user (tailnet / SSO)
- `organization`: any user in the org (requires org modeling)
- `public`: unauthenticated (should be disabled by default)

## Security Notes

- Sharing a port increases exposure. Prefer `owner` and short TTLs for most use cases.
- The safe default is to keep preview traffic mediated by a gateway that:
  - authenticates users
  - authorizes based on share level
  - audits access
  - routes to desktops via `ws-proxy` port-forward (not ClusterIP routing)

Optional hardening:
- `k8s/optional/hardened/portshare-validatingadmissionpolicy.yaml` denies `shareLevel=public` by default and requires a TTL for non-owner shares.
