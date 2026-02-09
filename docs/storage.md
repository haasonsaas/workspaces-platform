# Storage (Longhorn-First)

This project assumes CSI storage that supports:
- `PersistentVolumeClaim` provisioning
- `VolumeSnapshot` create/restore (`snapshot.storage.k8s.io/v1`)

Longhorn is the recommended default for on-prem.

## Required Cluster Addons

1. Longhorn (CSI driver)
2. CSI snapshot-controller + CRDs (`VolumeSnapshot`, `VolumeSnapshotClass`)

You will typically also define a `VolumeSnapshotClass` for Longhorn, for example:
```yaml
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshotClass
metadata:
  name: longhorn
driver: driver.longhorn.io
deletionPolicy: Delete
```

This repo includes an example manifest at `k8s/optional/storage/longhorn-volumesnapshotclass.yaml`.

## HomeTemplate (Warm Start + Reset)

`HomeTemplate` creates:
- a template PVC (`hometemplate-<name>`)
- periodic `VolumeSnapshot`s of that PVC

`Desktop` can then:
- seed its home PVC from the latest ready snapshot (`spec.home.seed.templateRef`)
- reset home by switching to a new PVC restored from snapshot (`spec.home.reset.*`)

This keeps Desktop compute ephemeral while preserving home state, and enables “reset to clean” without rebuilding the base image.

### Reset Trigger

Reset is triggered by changing `Desktop.spec.home.reset.requestedAt` (RFC3339 timestamp) along with a valid snapshot source:
- `spec.home.reset.templateRef` (preferred), or
- `spec.home.reset.snapshotName`
