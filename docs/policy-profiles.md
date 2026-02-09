# Policy Profiles

Policy profiles are **security and UX bundles** applied to agent sandboxes (and, later, desktops).

They are enforced in multiple layers:
- capability-broker (what NetworkGrants a job may request/approve without admin override)
- workspaces-operator (pod shape tweaks that matter for the workload)
- optional admission guardrails (`ValidatingAdmissionPolicy`) in `k8s/optional/hardened/`

## Agent Profiles (Today)

### `restricted` (default)

Intent: safest default for "agents can safely run code".

Enforcement:
- strict default-deny egress at the Cilium layer (baseline agent policies)
- broker guardrails for non-admin callers:
  - public egress requires explicit allowlist (`BROKER_NETWORK_PUBLIC_EGRESS_ALLOWLIST`)
  - public egress must use `policyMode=PROXY_CONNECT` (proxy-first)
  - `allowNon443` is denied for non-admin

### `browser-automation`

Intent: run headless browser tasks (Playwright/Selenium) while keeping isolation and audit.

Enforcement:
- operator mounts a tmpfs-backed `/dev/shm` (256Mi) into the pod
- broker policy can be made more permissive for this profile via overrides (see below)

Default: this repo does **not** widen public egress for this profile unless you opt in via `BROKER_NETWORK_PROFILE_OVERRIDES`.

Note: the operator injects `WORKSPACES_POLICY_PROFILE` into agent pods for scripts/tools that want to branch on profile.

### `internal`

Intent: agent work that targets internal services.

Enforcement:
- use `BROKER_NETWORK_INTERNAL_SUFFIX_ALLOWLIST` (and profile overrides) to treat internal domains as "internal"
- non-admin internal grants default to `policyMode=STRICT_FQDN`

## Broker: Per-Profile NetworkGrant Overrides

Env: `BROKER_NETWORK_PROFILE_OVERRIDES`

Format: JSON map of `policyProfile` -> override object.

Supported fields:
- `publicEgressMode`: `"deny"` or `"allow"`
- `internalSuffixAllowlist`: list of suffixes treated as internal
- `publicEgressAllowlist`: list of exact public hostnames allowed when mode is `"deny"`
- `publicDNSAllowlist`: list of exact DNS names allowed in `spec.dnsAllow` when mode is `"deny"`
- `allowNon443`: boolean (only applies when `NetworkGrant.spec.allowNon443=true`)
- `allowedNon443Ports`: list of non-443 ports allowed (defaults to `[80]` if empty and `allowNon443=true`)

Example (make `browser-automation` able to request/approve any exact public hostname via proxy, and allow port 80):
```json
{
  "browser-automation": {
    "publicEgressMode": "allow",
    "allowNon443": true,
    "allowedNon443Ports": [80]
  }
}
```

Where to configure:
- `k8s/broker/configmap.yaml` key `broker_network_profile_overrides`

## wsctl UX

`wsctl netgrant request` defaults to `--policy-mode AUTO`:
- public hosts -> `PROXY_CONNECT`
- internal suffixes -> `STRICT_FQDN`

You can still force it:
```bash
wsctl netgrant request ... --policy-mode PROXY_CONNECT
```
