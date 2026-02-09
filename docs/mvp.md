# MVP Build Plan

This MVP is optimized for:
- employee UX first (fast reconnection, SSH for VS Code Remote)
- agents are first-class tenants (same image lineage, stricter policy)
- strong agent isolation + least-privilege + auditing

See also: `docs/storage.md` (Longhorn + VolumeSnapshots).

## 0. Cluster Prereqs (On-Prem / Longhorn)

1. Enable nested virtualization on your worker nodes/VMs (KVM passthrough if virtualized).
2. Install k3s + Cilium (with Hubble).
3. Install Kata Containers (create a `RuntimeClass` named `kata`).
4. Install Longhorn (CSI storage).
5. Install CSI snapshot support (`snapshot.storage.k8s.io` CRDs + snapshot-controller).
6. Run MinIO (artifacts + caches + audit bundles).

## 1. Node Pools (Day 1)

Label + taint nodes so policy matches dev/prod:
- desktops:
  - label: `workspaces.platform.dev/pool=desktops`
  - taint: `workspaces.platform.dev/pool=desktops:NoSchedule`
- agents:
  - label: `workspaces.platform.dev/pool=agents`
  - taint: `workspaces.platform.dev/pool=agents:NoSchedule`

Desktops and agents should *not* run on the general-purpose node pool.

## 2. Deploy The Operator

CRDs + namespaces + operator + broker + baseline policies:
```bash
kubectl apply -k k8s
```

## 3. Default-Deny Agents

Apply baseline policies in the `agents` namespace:
```bash
kubectl apply -f k8s/policies/agents-default-deny.yaml
```

Then add allowlists for:
- DNS (already included)
- `capability-broker` service
- MinIO
- package proxies / registries
- GitHub (or internal git mirror), ideally via proxies and/or scoped `NetworkGrant`s

## 4. Create A Desktop

Example `Desktop`:
```bash
kubectl apply -f k8s/examples/desktop.yaml
```

Or with `wsctl`:
```bash
wsctl desktop create --name jonathan --user jonathan --ssh-key-file ~/.ssh/id_ed25519.pub
```

The operator creates:
- home PVC (`desktop-<name>-home`)
- authorized keys secret (`desktop-<name>-authkeys`)
- a `Deployment` (`desktop-<name>`) with `strategy: Recreate`
- a `Service` (`desktop-<name>-ssh`)

Access is via a Tailscale SSH gateway. See `docs/access.md` for SSH/VS Code Remote wiring.

### Warm Start (HomeTemplate)

To seed new desktops from a warmed home snapshot (Longhorn VolumeSnapshot restore), create a `HomeTemplate`:
```bash
kubectl apply -f k8s/examples/hometemplate.yaml
```

Then reference it from `Desktop.spec.home.seed.templateRef`.

## 5. Create An Agent Job (Kata)

Example `AgentJob`:
```bash
kubectl apply -f k8s/examples/agentjob.yaml
```

Or with `wsctl`:
```bash
wsctl agent create --name example --script $'set -euo pipefail\necho hello from agent\nsleep 5'
```

The operator creates a `Job` (`agentjob-<name>`) with:
- `runtimeClassName: kata` by default
- `automountServiceAccountToken: false`
- no Linux caps, `seccomp: RuntimeDefault`
- an ephemeral `/workspace`

For PR-scoped agent runs (via `/agent run`), add a repo-local `.workspaces/agent.sh` and keep it deterministic (no implicit curl|bash).

## 6. Network Unlocks (Approval)

`NetworkGrant` is the primitive for "unlock this destination for this job for N minutes".
The operator translates it to a `CiliumNetworkPolicy` named `netgrant-<grant>`.

MVP schema constraints (enforced by controller):
- exact FQDN only (no wildcards)
- TCP only
- 443-only by default (set `allowNon443: true` to permit non-443)
- `purpose` is required on every `NetworkGrant`

MVP approval flow:
- broker (or admin) creates `NetworkGrant` with `approved=false`
- approver flips it to `approved=true` (+ `ttlSeconds`, `approvedBy`, `reason`)
- operator enforces it until expiry

Optional (GitHub workflow):
- agent includes GitHub context (`repo`, `pullNumber`) when requesting a `NetworkGrant`
- broker posts an approval request comment on the PR
- `cmd/github-webhook` approves grants from allowlisted approvers via `/netgrant approve ...` comments (prefer webhook token over admin token)

See: `docs/github.md`.

To deploy the optional webhook bridge (after creating `github-webhook-secrets`):
```bash
kubectl apply -k k8s/github-webhook
```

## 7. GitHub Writes (Broker Only)

MVP policy: agents never get GitHub write tokens.

Broker does:
- accept agent-produced patches
- clone repo with GitHub App installation token
- create `agent/<job>` branch
- apply patch + commit
- push + open PR
- write an audit event for every write

`cmd/capability-broker` exists in this repo and exposes:
- `POST /v1/network-grants` (create unapproved grant request)
- `POST /v1/network-grants/{namespace}/{name}/approve` (admin approve)

MVP auth:
- agent-facing endpoints require `X-Broker-Agent-Token` (or admin token)
- approval endpoints require `X-Broker-Admin-Token`

`POST /v1/github/open-pr` is implemented using a GitHub App installation token. To enable it, configure:
- `capability-broker-github-app` Secret (keys: `app_id`, `installation_id`, `private-key.pem`)
- `capability-broker-config` ConfigMap (set `github_repo_allowlist`)
