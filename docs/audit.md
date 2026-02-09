# Auditing

Auditing is a first-order requirement for agent execution. The goal is to be able to reconstruct and explain agent behavior (and constrain blast radius) without turning employee desktops into a surveillance product.

## What To Audit

Agents (default: heavy):
- job identity: who launched it, what repo, what image digest, what policy profile, start/stop, exit code
- command exec metadata: argv, cwd, duration, exit code
- stdout/stderr capture with mandatory redaction
- network egress: destination + port + DNS/SNI when available
- capability uses: NetworkGrant requests/approvals, GitHub PR writes, Vault credentials minted

Humans (default: light):
- session metadata only: who/when/image/class
- avoid keystroke logging by default

## Retention (Default)

- 30 days hot
- 90 days cold

## Storage Design (Target)

Properties:
- append-only
- tamper-evident (hash chaining)
- signed (Vault transit or offline signing key)
- stored in an object store (MinIO), with Object Lock/WORM in production

## Current State In This Repo

By default, the broker logs audit events as JSON records (prefixed with `AUDIT`) to its normal logs (useful for dev, not tamper-evident).

Optional (recommended): enable a tamper-evident local file sink that writes JSONL with a hash chain.

Config (broker):
- `AUDIT_SINK=stdout|file` (default: `stdout`)
- `AUDIT_STREAM` (default: component name)
- `AUDIT_DIR` (required if `AUDIT_SINK=file`)
- `AUDIT_CHECKPOINT_EVERY_N` (default: `500`)
- `AUDIT_CHECKPOINT_EVERY_SECONDS` (default: `60`)
- `AUDIT_FSYNC_ON_CHECKPOINT=true|false` (default: `false`)

Implementation: `internal/audit/` and wired into `cmd/capability-broker/`.

Agent jobs (script mode) run via `cmd/agent-runner`, which also emits `AUDIT` records to stdout by default. This keeps exec metadata close to the sandbox boundary even before a full log shipping pipeline exists.

The file sink provides hash chaining (tamper-evident) but still needs append-only/WORM storage in production to prevent history rewrites.

## Next Implementation Step

1. Introduce an `audit-sink` that:
   - assigns monotonically increasing sequence numbers per stream (per day or per hour)
   - computes `hash_i = sha256(hash_{i-1} || event_i)`
   - signs periodic checkpoints (every N events or every M seconds)
2. Write bundles + checkpoints to MinIO:
   - `audit/YYYY-MM-DD/stream=<name>/bundle=<uuid>.jsonl`
   - `audit/YYYY-MM-DD/stream=<name>/checkpoint=<ts>.json`
3. Lock buckets in production (MinIO Object Lock).
