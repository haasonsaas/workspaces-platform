# wsctl (CLI)

`wsctl` is a small CLI for creating `Desktop` / `AgentJob` resources and interacting with the capability-broker (network grants, broker-only PR writes).

Build:
```bash
go build ./cmd/wsctl
```

## Desktop

Create:
```bash
wsctl desktop create \
  --name jonathan \
  --user jonathan \
  --ssh-key-file ~/.ssh/id_ed25519.pub
```

List:
```bash
wsctl desktop list
```

## AgentJob

Create a simple agent job (Kata by default in the operator; this just creates the CR):
```bash
wsctl agent create \
  --name example \
  --image ghcr.io/workspaces-platform/agent-runner:latest \
  --shell 'echo hello from agent'
```

Trigger a PR-scoped agent run through the broker (requires webhook/admin token):
```bash
export WORKSPACES_BROKER_URL=http://capability-broker.workspaces-system.svc.cluster.local:8080
export BROKER_WEBHOOK_TOKEN=...

wsctl agent run-pr --repo owner/repo --pr 123
```

## Network Grants (Broker)

Request a grant (requires agent token):
```bash
export WORKSPACES_BROKER_URL=http://capability-broker.workspaces-system.svc.cluster.local:8080
export BROKER_AGENT_TOKEN=...

wsctl netgrant request \
  --selector 'workspaces.platform.dev/app=agent,workspaces.platform.dev/agentjob=example' \
  --purpose 'fetch GitHub metadata' \
  --egress github.com:443 \
  --egress api.github.com:443
```

Approve a grant (requires admin token):
```bash
export BROKER_ADMIN_TOKEN=...

wsctl netgrant approve --name netgrant-abc123 --approved-by admin --ttl 1800
```

## Broker-Only PR Writes

Open a PR from a unified diff (requires agent token):
```bash
wsctl github open-pr \
  --repo owner/repo \
  --title 'Fix thing' \
  --patch-file /tmp/patch.diff
```

