# Longhorn Ops Runbook

This repo assumes **Longhorn-first** for on-prem:
- `StorageClass` provisioning for Desktop homes and control-plane PVCs
- `VolumeSnapshot` / restore (required for `HomeTemplate` warm starts and `Desktop` home reset)

This doc focuses on the parts that directly impact workspaces-platform.

## Prereqs (What Must Exist)

1. Longhorn installed (CSI driver + `StorageClass`).
2. CSI snapshot CRDs + snapshot-controller installed:
   - `volumesnapshots.snapshot.storage.k8s.io`
   - `volumesnapshotclasses.snapshot.storage.k8s.io`
   - `volumesnapshotcontents.snapshot.storage.k8s.io`
3. A `VolumeSnapshotClass` compatible with Longhorn.

This repo includes an example `VolumeSnapshotClass`:
- `k8s/optional/storage/longhorn-volumesnapshotclass.yaml`

Quick check:
```bash
wsctl doctor --mode preflight
wsctl doctor --mode installed
```

## Runbook: Warm Start Desktops (HomeTemplate -> Seed)

Goal: new desktops start "warm" (dotfiles, caches, editor state) without baking per-user images.

1. Create a `HomeTemplate` (template PVC + recurring snapshots):
```bash
kubectl apply -f k8s/examples/hometemplate.yaml
kubectl -n desktops get hometemplates default-home -o yaml
```

2. Populate the template PVC once (one-time action).

The template PVC name is:
- `hometemplate-<name>` by default (e.g. `hometemplate-default-home`)

How you populate it is up to you:
- attach it to a one-off pod
- rsync dotfiles/caches in
- run `nix develop` once to populate caches

3. Seed a Desktop home from the latest ready snapshot:

In `k8s/examples/desktop.yaml`, uncomment:
```yaml
home:
  seed:
    templateRef: default-home
```

Apply:
```bash
kubectl apply -f k8s/examples/desktop.yaml
```

Notes:
- Seeding happens only when the Desktop home PVC does not exist.
- If you want to "re-seed" an existing Desktop, use the reset workflow below.

## Runbook: Reset Desktop Home (Snapshot Restore)

Goal: rollback to a clean baseline without deleting the Desktop object.

1. Ensure the HomeTemplate has a ready snapshot:
```bash
kubectl -n desktops get hometemplates default-home -o yaml
kubectl -n desktops get volumesnapshots | head
```

2. Trigger a reset by bumping `spec.home.reset.requestedAt`.

Example (merge-patch):
```bash
NOW="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
kubectl -n desktops patch desktop jonathan --type merge -p "{
  \"spec\": {
    \"home\": {
      \"reset\": {
        \"requestedAt\": \"${NOW}\",
        \"templateRef\": \"default-home\",
        \"retainOldClaims\": 1
      }
    }
  }
}"
```

What happens:
- operator creates a **new** home PVC from the snapshot (`desktop-<name>-home-r<rev>`)
- Desktop pod restarts (Recreate strategy) and mounts the new PVC
- old PVC revisions are retained up to `retainOldClaims`

## Runbook: Snapshot Hygiene

HomeTemplate snapshot settings:
- `spec.intervalSeconds` default: daily
- `spec.retention` default: 7

Keep retention small in early clusters to avoid unbounded snapshot growth.

## Runbook: Backups (Longhorn -> MinIO)

workspaces-platform uses MinIO optionally (artifacts/audit/caches). You can also use MinIO as a backup target for Longhorn.

This repo includes an optional single-node MinIO deployment:
- `k8s/optional/storage/minio.yaml`
- `k8s/examples/minio-root.yaml` (required secret)

High-level steps:
1. Deploy MinIO (`kubectl apply -f k8s/examples/minio-root.yaml && kubectl apply -k k8s/optional/storage`).
2. Configure Longhorn's backup target to point at MinIO (Longhorn UI / settings).
3. Test a backup + restore on a non-critical volume first.

Keep in mind:
- backup/restore is Longhorn-specific plumbing; treat it as separate from `VolumeSnapshot` (CSI) semantics.
- validate your restore path before you rely on it.

## Common Failure Modes (Practical)

- `VolumeSnapshot` never becomes ready:
  - snapshot-controller missing or CRDs not installed
  - Longhorn snapshot support misconfigured
  - check events in `kube-system` and `longhorn-system`

- Desktop reset requests "do nothing":
  - `spec.home.reset.requestedAt` did not change
  - no snapshot source configured (`templateRef` or `snapshotName`)
  - HomeTemplate has no ready snapshot yet

- "Warm start" is slow:
  - template PVC isn't actually warmed (no caches)
  - snapshot restore takes time due to storage performance limits
  - consider smaller initial caches + more proxy usage (npm/pypi/cargo) to reduce per-user PVC bloat

