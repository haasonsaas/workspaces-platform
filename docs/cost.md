# Cost Controls

Cost control is intentionally a *third-order* concern behind (1) agent isolation and (2) reproducibility, but you still want guardrails from day 1 to prevent surprise resource exhaustion on an on-prem cluster.

This repo supports three layers of cost control:

1. **Concurrency caps (broker)** for GitHub-triggered agent jobs.
2. **Autosuspend (operator + gateway)** for employee desktops.
3. **Kubernetes quotas/limits (optional)** for hard ceilings.

## 1) Agent Concurrency Caps (Broker)

The capability broker enforces caps for GitHub-triggered jobs:
- `AGENT_MAX_CONCURRENT` (total active AgentJobs)
- `AGENT_MAX_CONCURRENT_PER_REPO` (per repo active AgentJobs)

These are applied only to broker-created jobs; admins creating `AgentJob` CRs directly bypass them by design.

## 2) Desktop Autosuspend

`Desktop.spec.idleTimeoutSeconds` suspends a desktop (scales replicas to 0) after inactivity.

Inactivity is measured via a heartbeat written by `ws-proxy`:
- annotation `workspaces.platform.dev/last-active-at` on the `Desktop`
- updated on connect and periodically while the session is open
- heartbeat interval: `WORKSPACES_HEARTBEAT_SECONDS` (default 300s)

To force state:
- `Desktop.spec.suspended=true` (stop compute)
- `Desktop.spec.suspended=false` (resume compute)

## 3) Kubernetes Quotas + Default Limits (Optional)

Apply namespace-level quotas/limits when you want hard ceilings:
- Agents: `k8s/optional/hardened/agents-quotas.yaml`
- Desktops: `k8s/optional/hardened/desktops-quotas.yaml`

These are intentionally **not** installed by default because every cluster size differs.

Example:
```bash
kubectl apply -f k8s/optional/hardened/agents-quotas.yaml
kubectl apply -f k8s/optional/hardened/desktops-quotas.yaml
```

Notes:
- `LimitRange` sets default requests/limits when a workload omits them.
- `ResourceQuota` sets hard ceilings across the namespace (pods, CPU/memory totals, storage totals).
