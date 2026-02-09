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
   - `POST /v1/network-grants/{namespace}/{name}/approve`

Approval command syntax (PR comment):
- `/netgrant approve agents/netgrant-abc123`
- `/netgrant approve agents/netgrant-abc123 ttl=1800`

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
- `BROKER_BASE_URL` (e.g. `http://capability-broker.workspaces-system.svc.cluster.local:8080`)
- `BROKER_ADMIN_TOKEN` (passed as `X-Broker-Admin-Token` to approve grants)

Notes:
- Secure default: deny all if allowlists are empty.
- Approvals are audited by the broker (`networkgrant.approve` events).

Kubernetes manifests (optional; not included in the root `k8s/` kustomization):
- `k8s/github-webhook/`
