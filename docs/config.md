# Configuration

This doc lists key flags and environment variables for binaries in this repo. The intent is to make it obvious what you can tune without reading code.

## workspaces-operator

Binary: `cmd/workspaces-operator`

Flags:
- `--default-desktop-image` (env: `DEFAULT_DESKTOP_IMAGE`): image used when `Desktop.spec.image` is empty.
- `--default-agent-runtimeclass` (env: `DEFAULT_AGENT_RUNTIMECLASS`): RuntimeClass used when `AgentJob.spec.runtimeClassName` is empty.
- `--networkgrant-max-ttl-seconds` (env: `NETWORKGRANT_MAX_TTL_SECONDS`): max `NetworkGrant.spec.ttlSeconds` allowed (0 disables cap). Default `7200`.
- `--networkgrant-max-egress-rules` (env: `NETWORKGRANT_MAX_EGRESS_RULES`): max `NetworkGrant.spec.egress` rules allowed (0 disables cap). Default `20`.
- `--networkgrant-max-dns-names` (env: `NETWORKGRANT_MAX_DNS_NAMES`): max unique DNS names allowed for DNS L7 allow rules (union of `spec.egress.host` and `spec.dnsAllow`). Default `50`.

## capability-broker

Binary: `cmd/capability-broker`

Core:
- `LISTEN_ADDR` (default `:8080`)
- `BROKER_JOB_JWT_SECRET` (required): HS256 signing key used to mint/verify per-job tokens for agent-facing endpoints.
- `BROKER_ADMIN_TOKEN` (optional): admin token for approval endpoints.
- `BROKER_WEBHOOK_TOKEN` (optional, preferred): least-privilege token for `github-webhook` and GitHub-triggered actions.

NetworkGrant policy guardrails (proxy-first by default):
- `BROKER_NETWORK_PUBLIC_EGRESS_MODE` (default `deny`): `deny|allow`. When `deny`, non-admin callers (job tokens + GitHub comment approvals) cannot request/approve public internet egress unless it is explicitly allowlisted.
- `BROKER_NETWORK_PUBLIC_EGRESS_ALLOWLIST` (default empty): CSV of exact public hostnames allowed for non-admin egress requests/approvals (e.g. `github.com,api.github.com`).
- `BROKER_NETWORK_PUBLIC_DNS_ALLOWLIST` (default empty): CSV of exact public DNS names allowed in `NetworkGrant.spec.dnsAllow` for non-admin approvals.
- `BROKER_NETWORK_INTERNAL_SUFFIX_ALLOWLIST` (default `svc.cluster.local,cluster.local`): CSV of suffixes treated as internal destinations (exempt from public egress restrictions).

GitHub App (broker-only PR writes and PR-scoped agent runs):
- `GITHUB_APP_ID` (required to enable GitHub features)
- `GITHUB_APP_INSTALLATION_ID` (required to enable GitHub features)
- `GITHUB_APP_PRIVATE_KEY_PEM` or `GITHUB_APP_PRIVATE_KEY_FILE` (required to enable GitHub features)
- `GITHUB_REPO_ALLOWLIST` (required for PR writes): CSV of `owner/repo` entries (secure default: deny all).

GitHub policy (PR writes):
- `GITHUB_API_URL` (default `https://api.github.com`)
- `GITHUB_GIT_URL` (default `https://github.com`)
- `GITHUB_DEFAULT_BASE_BRANCH` (default `main`)
- `GITHUB_BASE_BRANCH_ALLOWLIST` (default: the default base branch)
- `GITHUB_BRANCH_PREFIX` (default `agent/`)
- `GITHUB_GIT_AUTHOR_NAME` (default `workspaces-broker`)
- `GITHUB_GIT_AUTHOR_EMAIL` (default `workspaces-broker@localhost`)
- `GITHUB_MAX_FILES_CHANGED` (default `100`)
- `GITHUB_ALLOW_BINARY_PATCHES` (default `false`)
- `GITHUB_SENSITIVE_PATH_PREFIX_DENYLIST` (default `.github/workflows/,infra/,terraform/,deploy/`)
- `GITHUB_SENSITIVE_PATH_ALLOWLIST_REPOS` (optional): repos allowed to touch sensitive paths.
- `GITHUB_ENABLE_CHECK_RUNS` (default `false`)
- `GITHUB_CHECK_RUN_NAME` (default `workspaces-agent`)

Agent job defaults (GitHub-triggered jobs created via broker):
- `AGENT_DEFAULT_IMAGE` (default `ghcr.io/workspaces-platform/agent-runner:latest`)
- `AGENT_DEFAULT_POLICY_PROFILE` (default `restricted`)
- `AGENT_DEFAULT_TTL_SECONDS_AFTER_FINISHED` (default `3600`)
- `AGENT_DEFAULT_RUNTIME_CLASS` (default `kata`)
- `AGENT_DEFAULT_SCRIPT` (optional): default script body for jobs.

Concurrency + auto-grants:
- `AGENT_MAX_CONCURRENT` (default `0` = disabled)
- `AGENT_MAX_CONCURRENT_PER_REPO` (default `0` = disabled)
- `AGENT_AUTO_GRANT_GITHUB` (default `true`): create a short-lived GitHub `NetworkGrant` per job.
- `AGENT_AUTO_GRANT_TTL_SECONDS` (default `7200`)
- `AGENT_JOB_TOKEN_TTL_SECONDS` (default `7200`): per-job broker token TTL.

Artifacts (MinIO/S3; used for check-run logs/results, optional):
- `ARTIFACT_S3_ENDPOINT`
- `ARTIFACT_S3_BUCKET`
- `ARTIFACT_S3_REGION` (default `us-east-1`)
- `ARTIFACT_S3_ACCESS_KEY_ID`
- `ARTIFACT_S3_SECRET_ACCESS_KEY`
- `ARTIFACT_S3_PREFIX` (default `workspaces`)
- `ARTIFACT_S3_FORCE_PATH_STYLE` (default `true`)
- `ARTIFACT_PUBLISH_LINKS` (default `false`)
- `ARTIFACT_PRESIGN_TTL_SECONDS` (default `86400`)

Audit (optional; see also `docs/audit.md`):
- `AUDIT_SINK` (`stdout|file`, default `stdout`)
- `AUDIT_STREAM` (default: component name)
- `AUDIT_DIR` (required for `AUDIT_SINK=file`)
- `AUDIT_CHECKPOINT_EVERY_N` (default `500`)
- `AUDIT_CHECKPOINT_EVERY_SECONDS` (default `60`)
- `AUDIT_FSYNC_ON_CHECKPOINT` (default `false`)
- `AUDIT_HMAC_KEY` (optional; hex-encoded)

## github-webhook

Binary: `cmd/github-webhook`

Core:
- `LISTEN_ADDR` (default `:8080`)
- `GITHUB_WEBHOOK_SECRET` (required): webhook HMAC secret.

Policy:
- `GITHUB_REPO_ALLOWLIST` (required): repos this bridge will act on.
- `GITHUB_APPROVER_ALLOWLIST` (required): users allowed to approve `/netgrant approve ...` comments.
- `GITHUB_LAUNCHER_ALLOWLIST` (optional): users allowed to trigger `/agent run ...` comments (defaults to approvers).

Broker integration:
- `BROKER_BASE_URL` (required)
- `BROKER_WEBHOOK_TOKEN` (preferred) or `BROKER_ADMIN_TOKEN` (fallback)

## ws-proxy

Binary: `cmd/ws-proxy`

Flags:
- `--timeout` (default `45s`): setup timeout for establishing port-forward (does not limit session duration).

Env:
- `WORKSPACES_HEARTBEAT_SECONDS` (default `300`): how often to update desktop `last-active-at` for autosuspend.

## agent-runner

Binary: `cmd/agent-runner` (built into `images/agent-runner`)

Required:
- `WORKSPACES_TASK_SCRIPT`: shell script to execute via `bash -lc`.

Optional:
- `WORKSPACES_WORKDIR` (or `WORKSPACES_REPO_DIR`): working directory for script execution.
- `WORKSPACES_LOG_CAP_BYTES` (default `65536`): max bytes of stdout/stderr captured (process output is drained after cap to avoid blocking).

Audit + redaction:
- inherits `AUDIT_*` config when used
- uses best-effort redaction (`internal/redact`) for stdout/stderr (token patterns, PEM blocks, and env-derived literal secrets)

## auditship

Binary: `cmd/auditship`

Flags:
- `--dir` (or `AUDIT_DIR`): directory containing `events-*.jsonl` and `checkpoints-*.jsonl`.
- `--env-prefix` (default `AUDIT_`): prefix for S3 env vars (e.g. `AUDIT_` => `AUDIT_S3_ENDPOINT`, `AUDIT_S3_BUCKET`, ...).

S3 env vars:
- `${PREFIX}S3_ENDPOINT`
- `${PREFIX}S3_BUCKET`
- `${PREFIX}S3_REGION` (default `us-east-1`)
- `${PREFIX}S3_ACCESS_KEY_ID`
- `${PREFIX}S3_SECRET_ACCESS_KEY`
- `${PREFIX}S3_PREFIX` (default `workspaces`)
- `${PREFIX}S3_FORCE_PATH_STYLE` (default `true`)
