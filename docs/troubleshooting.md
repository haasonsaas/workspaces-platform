# Troubleshooting

## NetworkGrant rejected (SpecValid=false)

Common causes (and fixes):

- **Wildcard hostnames**
  - Error: `must be an exact hostname (no wildcards)`
  - Fix: request an exact hostname in `spec.egress[*].host` and (if needed) add exact CNAME targets in `spec.dnsAllow`.

- **Host contains scheme/path/port**
  - Error: `must be a hostname only (no scheme/path/port)`
  - Fix: use `github.com`, not `https://github.com` and not `github.com:443`.

- **Non-443 ports**
  - Error: `requests non-443 port ... but allowNon443 is false`
  - Fix: set `spec.allowNon443: true` (and keep ports as narrow as possible).

- **IP literals**
  - Error: `ip literals are not allowed`
  - Fix: use the hostname, not the IP.

- **Reserved destinations**
  - Error: `reserved hostname is not allowed`
  - Fix: `kubernetes.default.svc` is intentionally blocked. If an agent needs cluster access, add an explicit capability in the broker instead of opening network egress to the API.

## Egress proxy denies CONNECT

Events (from `egress-proxy`) to look for:
- `egressproxy.connect_denied` with `error=not_allowed`: no active approved `NetworkGrant` allows that `host:port` for the job.
- `egressproxy.connect_denied` with `error=dest_not_public`: the hostname resolved to a private/link-local/CGNAT IP (DNS rebinding defense).

Fixes:
- Prefer internal mirrors (Verdaccio/devpi/Athens/Cargo proxy/registry cache).
- If you need **public** egress, request a `NetworkGrant` with `policyMode=PROXY_CONNECT` and an exact hostname.
- If you need **internal** corp egress (private IP destinations), use `policyMode=STRICT_FQDN` (admin-only escape hatch by default).

## Overlay apply fails (ValidatingAdmissionPolicy not found)

If you see:
- `no matches for kind "ValidatingAdmissionPolicy" in version "admissionregistration.k8s.io/v1"`

Your cluster doesn't support the CEL admission APIs.

Fix:
- Use `kubectl apply -k k8s` (base)
- Apply quotas only: `kubectl apply -f k8s/optional/hardened/agents-quotas.yaml` and `kubectl apply -f k8s/optional/hardened/desktops-quotas.yaml`
