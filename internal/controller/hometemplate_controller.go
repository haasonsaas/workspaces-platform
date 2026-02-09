package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	workspacesv1alpha1 "workspaces-platform/api/v1alpha1"
)

// HomeTemplateReconciler maintains a template PVC and a rolling set of CSI
// VolumeSnapshots for warm Desktop home provisioning/reset.
type HomeTemplateReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func (r *HomeTemplateReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ht workspacesv1alpha1.HomeTemplate
	if err := r.Get(ctx, req.NamespacedName, &ht); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	labels := map[string]string{
		labelApp: "hometemplate",
		"workspaces.platform.dev/hometemplate": ht.Name,
	}

	pvcName := ht.Status.TemplatePVC
	if pvcName == "" {
		pvcName = fmt.Sprintf("hometemplate-%s", ht.Name)
	}

	size := ht.Spec.Size
	if size == "" {
		size = "50Gi"
	}
	qty, err := resource.ParseQuantity(size)
	if err != nil {
		return ctrl.Result{}, err
	}

	accessModes := ht.Spec.AccessModes
	if len(accessModes) == 0 {
		accessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
	}

	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: pvcName, Namespace: ht.Namespace}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, pvc, func() error {
		if pvc.Labels == nil {
			pvc.Labels = map[string]string{}
		}
		for k, v := range labels {
			pvc.Labels[k] = v
		}
		pvc.Spec.AccessModes = accessModes
		pvc.Spec.Resources.Requests = corev1.ResourceList{corev1.ResourceStorage: qty}
		pvc.Spec.StorageClassName = ht.Spec.StorageClassName
		return controllerutil.SetControllerReference(&ht, pvc, r.Scheme)
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	if ht.Status.TemplatePVC != pvcName {
		patch := client.MergeFrom(ht.DeepCopy())
		ht.Status.TemplatePVC = pvcName
		_ = r.Status().Patch(ctx, &ht, patch)
	}

	interval := ht.Spec.IntervalSeconds
	if interval <= 0 {
		interval = 24 * 60 * 60
	}
	retention := ht.Spec.Retention
	if retention <= 0 {
		retention = 7
	}

	snaps, err := r.listVolumeSnapshots(ctx, ht.Namespace, labels)
	if err != nil {
		// If the VolumeSnapshot CRD isn't installed yet, fail soft and requeue.
		// This keeps the operator usable in dev while storage plumbing catches up.
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	latest := latestSnapshot(snaps)
	needNew := false
	if latest == nil {
		needNew = true
	} else if !latest.createdAt.IsZero() && time.Since(latest.createdAt) >= time.Duration(interval)*time.Second {
		needNew = true
	}

	// Avoid stampeding: if there's a very recent snapshot (even if not ready),
	// don't create another one yet.
	if needNew && latest != nil && !latest.createdAt.IsZero() && time.Since(latest.createdAt) < 5*time.Minute {
		needNew = false
	}

	if needNew {
		if err := r.createVolumeSnapshot(ctx, &ht, pvcName, labels); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Update status with latest ready snapshot name (if any).
	latestReady := latestReadySnapshot(snaps)
	latestReadyName := ""
	var latestReadyTime *metav1.Time
	if latestReady != nil {
		latestReadyName = latestReady.name
		if !latestReady.createdAt.IsZero() {
			t := metav1.NewTime(latestReady.createdAt)
			latestReadyTime = &t
		}
	}

	changed := false
	before := ht.DeepCopy()
	if ht.Status.LatestSnapshot != latestReadyName {
		ht.Status.LatestSnapshot = latestReadyName
		changed = true
	}
	if (ht.Status.LastSnapshotTime == nil) != (latestReadyTime == nil) {
		ht.Status.LastSnapshotTime = latestReadyTime
		changed = true
	} else if ht.Status.LastSnapshotTime != nil && latestReadyTime != nil && !ht.Status.LastSnapshotTime.Time.Equal(latestReadyTime.Time) {
		ht.Status.LastSnapshotTime = latestReadyTime
		changed = true
	}
	if changed {
		patch := client.MergeFrom(before)
		_ = r.Status().Patch(ctx, &ht, patch)
	}

	// Best-effort retention.
	_ = r.enforceSnapshotRetention(ctx, ht.Namespace, retention, snaps)

	// Requeue on cadence boundary.
	if latest != nil && !latest.createdAt.IsZero() {
		next := latest.createdAt.Add(time.Duration(interval) * time.Second)
		return ctrl.Result{RequeueAfter: time.Until(next)}, nil
	}
	return ctrl.Result{RequeueAfter: time.Duration(interval) * time.Second}, nil
}

func (r *HomeTemplateReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&workspacesv1alpha1.HomeTemplate{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Complete(r)
}

type volumeSnapshotInfo struct {
	name      string
	createdAt time.Time
	ready     bool
}

func (r *HomeTemplateReconciler) listVolumeSnapshots(ctx context.Context, namespace string, matchLabels map[string]string) ([]volumeSnapshotInfo, error) {
	list := &unstructured.UnstructuredList{}
	list.SetAPIVersion("snapshot.storage.k8s.io/v1")
	list.SetKind("VolumeSnapshotList")
	if err := r.List(ctx, list, client.InNamespace(namespace), client.MatchingLabels(matchLabels)); err != nil {
		return nil, err
	}
	out := make([]volumeSnapshotInfo, 0, len(list.Items))
	for _, it := range list.Items {
		created := it.GetCreationTimestamp()
		ready, _, _ := unstructured.NestedBool(it.Object, "status", "readyToUse")
		out = append(out, volumeSnapshotInfo{
			name:      it.GetName(),
			createdAt: created.Time,
			ready:     ready,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].createdAt.Before(out[j].createdAt) })
	return out, nil
}

func latestSnapshot(snaps []volumeSnapshotInfo) *volumeSnapshotInfo {
	if len(snaps) == 0 {
		return nil
	}
	s := snaps[len(snaps)-1]
	return &s
}

func latestReadySnapshot(snaps []volumeSnapshotInfo) *volumeSnapshotInfo {
	for i := len(snaps) - 1; i >= 0; i-- {
		if snaps[i].ready {
			s := snaps[i]
			return &s
		}
	}
	return nil
}

func (r *HomeTemplateReconciler) createVolumeSnapshot(ctx context.Context, ht *workspacesv1alpha1.HomeTemplate, pvcName string, labels map[string]string) error {
	name := fmt.Sprintf("hometemplate-%s-%d", ht.Name, time.Now().UTC().Unix())
	if len(name) > 63 {
		name = name[:63]
	}

	vs := &unstructured.Unstructured{}
	vs.SetAPIVersion("snapshot.storage.k8s.io/v1")
	vs.SetKind("VolumeSnapshot")
	vs.SetNamespace(ht.Namespace)
	vs.SetName(name)
	vs.SetLabels(labels)

	spec := map[string]any{
		"source": map[string]any{
			"persistentVolumeClaimName": pvcName,
		},
	}
	if ht.Spec.SnapshotClassName != nil && *ht.Spec.SnapshotClassName != "" {
		spec["volumeSnapshotClassName"] = *ht.Spec.SnapshotClassName
	}
	_ = unstructured.SetNestedField(vs.Object, spec, "spec")
	if err := controllerutil.SetControllerReference(ht, vs, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, vs)
}

func (r *HomeTemplateReconciler) enforceSnapshotRetention(ctx context.Context, namespace string, keep int32, snaps []volumeSnapshotInfo) error {
	if keep <= 0 {
		return nil
	}
	if int32(len(snaps)) <= keep {
		return nil
	}
	toDelete := snaps[:len(snaps)-int(keep)]
	for _, s := range toDelete {
		obj := &unstructured.Unstructured{}
		obj.SetAPIVersion("snapshot.storage.k8s.io/v1")
		obj.SetKind("VolumeSnapshot")
		obj.SetNamespace(namespace)
		obj.SetName(s.name)
		_ = r.Delete(ctx, obj)
	}
	return nil
}

