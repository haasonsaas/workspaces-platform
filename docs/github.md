# GitHub Integration (MVP)

This repo uses a GitHub App for **broker-only writes** (open PRs from patches) and supports an optional GitHub webhook bridge for **NetworkGrant approvals via PR comments**.

## Broker GitHub App (Write Path)

Broker endpoint:
- `POST /v1/github/open-pr`

The broker clones, applies a unified diff, pushes `agent/*`, and opens a PR using a GitHub App installation token.

## NetworkGrant Approval Via PR Comments (Optional)

Flow:
1. Agent requests a `NetworkGrant` via broker `POST /v1/network-grants` and includes GitHub context:
   - `github.repo` = `owner/repo`
   - `github.pullNumber` = PR number
2. Broker creates the `NetworkGrant` (unapproved) and posts a comment on the PR with an approval command:
   - `/netgrant approve <namespace>/<name> ttl=1800`
3. `github-webhook` listens for `issue_comment` events, verifies signature, checks allowlists, and calls broker approval:
   - `POST /v1/network-grants/{namespace}/{name}/approve-github` (preferred; webhook token)
   - falls back to `/approve` if configured with admin token

Approval command syntax (PR comment):
- `/netgrant approve agents/netgrant-abc123`
- `/netgrant approve agents/netgrant-abc123 ttl=1800`

## Agent Runs Via PR Comments (Optional)

Flow:
1. A repo owner triggers an agent run by commenting:
   - `/agent run`
2. `github-webhook` listens for `issue_comment` events and calls broker:
   - `POST /v1/agent-jobs`
3. Broker creates an `AgentJob` in the `agents` namespace and posts a status comment (and optionally a check-run).
   - If check-runs are enabled, broker creates a GitHub Check Run in-progress and a reporter loop marks it completed when the AgentJob finishes.
   - Broker also mints a short-lived, repo-scoped **read** token and stores it in a per-job Secret (`agentjob-<name>-github`) used only by a checkout initContainer.

Command syntax (PR comment):
- `/agent run`
- `/agent run profile=restricted ttl=3600`

## Repo Convention For Agent Runs

By default, the broker launches a PR-scoped AgentJob that checks out the PR head into `/workspace/repo` and runs:
- `.workspaces/agent.sh` if present (executable preferred)

Example `.workspaces/agent.sh`:
```bash
#!/usr/bin/env bash
set -euo pipefail

echo "Running repo checks..."
go test ./...
```

## github-webhook Service

Binary:
- `cmd/github-webhook`

Endpoints:
- `POST /github/webhook` (GitHub webhooks)
- `GET /healthz`, `GET /readyz`

Required env:
- `GITHUB_WEBHOOK_SECRET` (HMAC secret configured in the GitHub App webhook settings)
- `GITHUB_REPO_ALLOWLIST` (comma-separated `owner/repo`)
- `GITHUB_APPROVER_ALLOWLIST` (comma-separated GitHub usernames)
- `GITHUB_LAUNCHER_ALLOWLIST` (optional; defaults to approver allowlist)
- `BROKER_BASE_URL` (e.g. `http://capability-broker.workspaces-system.svc.cluster.local:8080`)
- `BROKER_WEBHOOK_TOKEN` (preferred; passed as `X-Broker-Webhook-Token`)
- `BROKER_ADMIN_TOKEN` (fallback; not recommended)

Notes:
- Secure default: deny all if allowlists are empty.
- Approvals are audited by the broker (`networkgrant.approve` events).

Kubernetes manifests (optional; not included in the root `k8s/` kustomization):
- `k8s/github-webhook/`
