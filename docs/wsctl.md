# wsctl (CLI)

`wsctl` is a small CLI for creating `Desktop` / `AgentJob` resources and interacting with the capability-broker (network grants, broker-only PR writes).

Build:
```bash
go build ./cmd/wsctl
```

## Doctor

Preflight and installation checks (exits non-zero on required failures):
```bash
wsctl doctor

# Only cluster capabilities (Cilium, snapshot CRDs, Kata RuntimeClass, Longhorn CSIDriver).
wsctl doctor --mode preflight

# Only checks that `kubectl apply -k k8s` has been applied successfully.
wsctl doctor --mode installed
```

## Desktop

Create:
```bash
wsctl desktop create \
  --name jonathan \
  --user jonathan \
  --ssh-key-file ~/.ssh/id_ed25519.pub \
  --idle-timeout 3600
```

List:
```bash
wsctl desktop list
```

Suspend:
```bash
wsctl desktop suspend --name jonathan
```

Resume:
```bash
wsctl desktop resume --name jonathan
```

## SSH Config

Generate a managed SSH config section (inserts/updates a `BEGIN/END workspaces-platform` block in `~/.ssh/config`):
```bash
wsctl ssh-config \
  --gateway-hostname <tailscale-ip-or-hostname> \
  --gateway-user <tailscale-ssh-user>
```

Dry-run (print unified diff only):
```bash
wsctl ssh-config --dry-run \
  --gateway-hostname <tailscale-ip-or-hostname> \
  --gateway-user <tailscale-ssh-user>
```

Notes:
- The generated desktop hosts default to `HostName desktop-<name>-ssh.<namespace>` which does **not** need to resolve locally; it is passed to `ws-proxy` for parsing.
- If you need a resolvable `HostName` (privileged gateway mode), set `--cluster-domain cluster.local` so entries use `<svc>.<ns>.svc.cluster.local`.

## AgentJob

Create a simple agent job (Kata by default in the operator; this just creates the CR).

Recommended (script mode, runs via `workspaces-agent-runner`):
```bash
wsctl agent create \
  --name example \
  --image ghcr.io/haasonsaas/workspaces-agent-runner:latest \
  --script $'set -euo pipefail\necho hello from agent\nsleep 5'
```

Direct mode (runs container `command/args` as-is):
```bash
wsctl agent create \
  --name example-direct \
  --image ghcr.io/haasonsaas/workspaces-agent-runner:latest \
  --shell 'echo hello from agent'
```

Trigger a PR-scoped agent run through the broker (requires webhook/admin token):
```bash
export WORKSPACES_BROKER_URL=http://capability-broker.workspaces-system.svc.cluster.local:8080
export BROKER_WEBHOOK_TOKEN=...

wsctl agent run-pr --repo owner/repo --pr 123
```

## Network Grants (Broker)

Request a grant (requires admin token for `wsctl`; in-cluster jobs use per-job auth):
```bash
export WORKSPACES_BROKER_URL=http://capability-broker.workspaces-system.svc.cluster.local:8080
export BROKER_ADMIN_TOKEN=...

wsctl netgrant request \
  --agentjob example \
  --purpose 'fetch GitHub metadata' \
  --egress github.com:443 \
  --egress api.github.com:443

# Proxy-first mode (recommended for public egress when using egress-proxy):
# wsctl netgrant request ... --policy-mode PROXY_CONNECT
#
# Default: --policy-mode AUTO (the broker selects PROXY_CONNECT for public hosts
# and STRICT_FQDN for internal suffixes).

# Optional: allow DNS resolution for additional names (e.g. CNAME targets) without granting direct egress.
# wsctl netgrant request ... --dns-allow github.map.fastly.net
```

Approve a grant (requires admin token):
```bash
export BROKER_ADMIN_TOKEN=...

wsctl netgrant approve --name netgrant-abc123 --approved-by admin --ttl 1800
```

## Broker-Only PR Writes

Open a PR from a unified diff (requires admin token for `wsctl`; in-cluster jobs use per-job auth):
```bash
wsctl github open-pr \
  --repo owner/repo \
  --title 'Fix thing' \
  --patch-file /tmp/patch.diff
```
