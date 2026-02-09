# Kustomize Overlays

This repo keeps the MVP base (`k8s/`) small and then layers optional features via `k8s-overlays/`.

## Images

The manifests default to GHCR images under `ghcr.io/haasonsaas/`:
- `workspaces-operator`
- `workspaces-capability-broker`
- `workspaces-egress-proxy`
- `workspaces-github-webhook`
- `workspaces-agent-runner`
- `workspaces-desktop`
- `workspaces-auditship`

For local dev, you can build these images and push them to whatever registry your cluster can pull from:
```bash
make images REGISTRY=ghcr.io/<you> TAG=dev
```

Then override images via kustomize (`kustomize edit set image ...`) or by editing the manifests.

## Base

Applies:
- CRDs + namespaces
- operator + capability-broker + egress-proxy
- baseline Cilium policies for agents (default-deny + allow in-cluster proxies)

```bash
kubectl apply -k k8s
```

## Hardened

Adds:
- quotas for `agents` and `desktops`
- optional CEL admission guardrails (`ValidatingAdmissionPolicy`)
- namespace-level Pod Security Standards labels (`agents=restricted`, `desktops=baseline`, `workspaces-system=baseline`)
- broker-as-chokepoint enforcement (admission): `AgentJob` + `NetworkGrant` create/update restricted to the broker (or cluster admins)

```bash
kubectl apply -k k8s-overlays/hardened
```

Notes:
- `ValidatingAdmissionPolicy` requires a cluster version that supports `admissionregistration.k8s.io/v1`.
- If your cluster doesn't support it, use `k8s/` and apply quotas only.

## Proxies

Adds common internal mirrors/caches:
- Attic (Nix)
- Verdaccio (npm)
- devpi (PyPI)
- Cargo proxy
- Athens (Go)
- Docker Registry pull-through cache
- Maven cache
- MinIO (S3-compatible object store; required for some artifact/audit flows)

```bash
kubectl apply -k k8s-overlays/proxies
```

## Complete ("All Of The Things")

Bundles:
- base
- audit overlay
- proxies
- quotas
- admission guardrails

```bash
kubectl apply -k k8s-overlays/complete
```

This is the fastest path to a "secure-by-default" dev cluster, but it does assume:
- Cilium is installed (network policy enforcement)
- snapshot CRDs/controller exist if you apply the Longhorn `VolumeSnapshotClass` example
- your cluster supports `ValidatingAdmissionPolicy` for the admission layer
