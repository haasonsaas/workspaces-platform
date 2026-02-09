package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	workspacesv1alpha1 "workspaces-platform/api/v1alpha1"
)

var (
	errHomeSeedNotReady  = errors.New("home_seed_not_ready")
	errNoSnapshotSource  = errors.New("no_snapshot_source_configured")
)

type DesktopReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	DefaultDesktopImage string
}

func (r *DesktopReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var desk workspacesv1alpha1.Desktop
	if err := r.Get(ctx, req.NamespacedName, &desk); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if desk.Spec.User == "" {
		// Don't create anything if user isn't set; surface in status.
		return ctrl.Result{}, nil
	}

	labels := map[string]string{
		labelApp:     "desktop",
		labelDesktop: desk.Name,
	}

	// Track the active home PVC in status so we can support snapshot-based reset
	// without deleting/recreating the Desktop object.
	baseHomePVCName := fmt.Sprintf("desktop-%s-home", desk.Name)
	activeHomePVCName := strings.TrimSpace(desk.Status.HomeClaimName)
	if activeHomePVCName == "" {
		activeHomePVCName = baseHomePVCName
	}
	homeRev := desk.Status.HomeRevision
	if homeRev == 0 {
		homeRev = 1
	}
	if desk.Status.HomeClaimName != activeHomePVCName || desk.Status.HomeRevision != homeRev {
		patchBase := desk.DeepCopy()
		desk.Status.HomeClaimName = activeHomePVCName
		desk.Status.HomeRevision = homeRev
		_ = r.Status().Patch(ctx, &desk, client.MergeFrom(patchBase))
	}

	// Handle reset request (if any). Reset is triggered by a change in requestedAt.
	if desk.Spec.Home.Reset != nil && desk.Spec.Home.Reset.RequestedAt != nil {
		needReset := desk.Status.LastResetAt == nil || !desk.Status.LastResetAt.Time.Equal(desk.Spec.Home.Reset.RequestedAt.Time)
		if needReset {
			snapName, snapErr := r.resolveDesktopHomeSnapshot(ctx, &desk, desk.Spec.Home.Reset.TemplateRef, desk.Spec.Home.Reset.SnapshotName)
			if snapErr != nil {
				if errors.Is(snapErr, errNoSnapshotSource) {
					// Misconfigured reset request; skip rather than requeue forever.
					return ctrl.Result{}, nil
				}
				return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
			}
			ready, rerr := r.volumeSnapshotReady(ctx, desk.Namespace, snapName)
			if rerr != nil {
				return ctrl.Result{}, rerr
			}
			if !ready {
				return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
			}

			newRev := desk.Status.HomeRevision + 1
			newPVCName := fmt.Sprintf("desktop-%s-home-r%d", desk.Name, newRev)
			if len(newPVCName) > 63 {
				newPVCName = newPVCName[:63]
			}

			if err := r.ensureHomePVC(ctx, &desk, labels, newPVCName, newRev, snapName); err != nil {
				return ctrl.Result{}, err
			}

			patch := client.MergeFrom(desk.DeepCopy())
			desk.Status.HomeClaimName = newPVCName
			desk.Status.HomeRevision = newRev
			t := metav1.NewTime(desk.Spec.Home.Reset.RequestedAt.Time)
			desk.Status.LastResetAt = &t
			_ = r.Status().Patch(ctx, &desk, patch)

			activeHomePVCName = newPVCName
		}
	}

	// Authorized keys secret (mounted into the desktop container).
	secretName := fmt.Sprintf("desktop-%s-authkeys", desk.Name)
	authorizedKeys := strings.TrimSpace(strings.Join(desk.Spec.SSH.AuthorizedKeys, "\n"))
	if authorizedKeys != "" {
		authorizedKeys += "\n"
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: desk.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if secret.Labels == nil {
			secret.Labels = map[string]string{}
		}
		for k, v := range labels {
			secret.Labels[k] = v
		}
		secret.Type = corev1.SecretTypeOpaque
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data["authorized_keys"] = []byte(authorizedKeys)
		return controllerutil.SetControllerReference(&desk, secret, r.Scheme)
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	// Home PVC (explicit PVC so we can evolve snapshot/clone workflows with CSI
	// VolumeSnapshots). If the PVC doesn't exist and a seed snapshot is
	// configured, it will be created from the snapshot.
	if err := r.ensureHomePVC(ctx, &desk, labels, activeHomePVCName, desk.Status.HomeRevision, ""); err != nil {
		if errors.Is(err, errHomeSeedNotReady) {
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}

	deployName := fmt.Sprintf("desktop-%s", desk.Name)
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: deployName, Namespace: desk.Namespace}}
	desiredImage := desk.Spec.Image
	if desiredImage == "" {
		desiredImage = r.DefaultDesktopImage
	}

	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		if deploy.Labels == nil {
			deploy.Labels = map[string]string{}
		}
		for k, v := range labels {
			deploy.Labels[k] = v
		}

		deploy.Spec.Replicas = ptrTo(int32(1))
		deploy.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
		deploy.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		deploy.Spec.Template.ObjectMeta.Labels = labels

		podSpec := &deploy.Spec.Template.Spec
		podSpec.AutomountServiceAccountToken = ptrTo(false)
		podSpec.NodeSelector = desk.Spec.NodeSelector
		podSpec.Tolerations = desk.Spec.Tolerations

		podSpec.Volumes = []corev1.Volume{
			{
				Name: "home",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: activeHomePVCName},
				},
			},
			{
				Name: "authkeys",
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{SecretName: secretName, DefaultMode: ptrTo(int32(0444))},
				},
			},
		}

		podSpec.Containers = []corev1.Container{
			{
				Name:  "desktop",
				Image: desiredImage,
				Ports: []corev1.ContainerPort{{Name: "ssh", ContainerPort: 2222}},
				Env: []corev1.EnvVar{
					{Name: "WORKSPACES_USERNAME", Value: desk.Spec.User},
					{Name: "WORKSPACES_AUTHORIZED_KEYS_FILE", Value: "/etc/workspaces/authorized_keys"},
				},
				Resources: desk.Spec.Resources,
				VolumeMounts: []corev1.VolumeMount{
					{Name: "home", MountPath: "/home"},
					{Name: "authkeys", MountPath: "/etc/workspaces", ReadOnly: true},
				},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptrTo(false),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				},
			},
		}

		return controllerutil.SetControllerReference(&desk, deploy, r.Scheme)
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	// SSH Service.
	svcName := fmt.Sprintf("desktop-%s-ssh", desk.Name)
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: svcName, Namespace: desk.Namespace}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		if svc.Labels == nil {
			svc.Labels = map[string]string{}
		}
		for k, v := range labels {
			svc.Labels[k] = v
		}
		svc.Spec.Selector = labels
		svc.Spec.Ports = []corev1.ServicePort{
			{Name: "ssh", Port: 22, TargetPort: intstrFromInt(2222), Protocol: corev1.ProtocolTCP},
		}
		return controllerutil.SetControllerReference(&desk, svc, r.Scheme)
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	// Update status best-effort.
	var latest appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Name: deployName, Namespace: desk.Namespace}, &latest); err == nil {
		phase := "Pending"
		if latest.Status.AvailableReplicas > 0 {
			phase = "Running"
		}
		if desk.Status.Phase != phase || desk.Status.ServiceName != svcName {
			patch := client.MergeFrom(desk.DeepCopy())
			desk.Status.Phase = phase
			desk.Status.ServiceName = svcName
			_ = r.Status().Patch(ctx, &desk, patch)
		}
	}

	// Best-effort: enforce home PVC retention after resets.
	if desk.Spec.Home.Reset != nil {
		retain := desk.Spec.Home.Reset.RetainOldClaims
		if retain <= 0 {
			retain = 1
		}
		_ = r.enforceHomeRetention(ctx, &desk, labels, activeHomePVCName, int(retain))
	}

	return ctrl.Result{}, nil
}

func (r *DesktopReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&workspacesv1alpha1.Desktop{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Complete(r)
}

func ptrTo[T any](v T) *T { return &v }

func intstrFromInt(v int) intstr.IntOrString {
	return intstr.IntOrString{Type: intstr.Int, IntVal: int32(v)}
}

func (r *DesktopReconciler) resolveDesktopHomeSnapshot(ctx context.Context, desk *workspacesv1alpha1.Desktop, templateRef, snapshotName *string) (string, error) {
	if snapshotName != nil && strings.TrimSpace(*snapshotName) != "" {
		return strings.TrimSpace(*snapshotName), nil
	}
	if templateRef != nil && strings.TrimSpace(*templateRef) != "" {
		var ht workspacesv1alpha1.HomeTemplate
		if err := r.Get(ctx, client.ObjectKey{Namespace: desk.Namespace, Name: strings.TrimSpace(*templateRef)}, &ht); err != nil {
			return "", err
		}
		if strings.TrimSpace(ht.Status.LatestSnapshot) == "" {
			return "", fmt.Errorf("hometemplate %s has no latestSnapshot yet", ht.Name)
		}
		return strings.TrimSpace(ht.Status.LatestSnapshot), nil
	}
	return "", errNoSnapshotSource
}

func (r *DesktopReconciler) ensureHomePVC(ctx context.Context, desk *workspacesv1alpha1.Desktop, labels map[string]string, pvcName string, revision int64, snapshotName string) error {
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: pvcName, Namespace: desk.Namespace}}

	homeSize := desk.Spec.Home.Size
	if homeSize == "" {
		homeSize = "50Gi"
	}
	qty, qerr := resource.ParseQuantity(homeSize)
	if qerr != nil {
		return qerr
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, pvc, func() error {
		creating := pvc.CreationTimestamp.IsZero()
		seedSnap := strings.TrimSpace(snapshotName)
		if creating && seedSnap == "" && desk.Spec.Home.Seed != nil {
			sn, err := r.resolveDesktopHomeSnapshot(ctx, desk, desk.Spec.Home.Seed.TemplateRef, desk.Spec.Home.Seed.SnapshotName)
			if err != nil {
				if errors.Is(err, errNoSnapshotSource) {
					// Seed is optional; if configured without a source, treat as no-op.
					err = nil
				} else {
					return errHomeSeedNotReady
				}
			}
			if err == nil && strings.TrimSpace(sn) != "" {
				ready, rerr := r.volumeSnapshotReady(ctx, desk.Namespace, sn)
				if rerr != nil {
					return rerr
				}
				if !ready {
					return errHomeSeedNotReady
				}
				seedSnap = sn
			}
		}

		if pvc.Labels == nil {
			pvc.Labels = map[string]string{}
		}
		for k, v := range labels {
			pvc.Labels[k] = v
		}
		pvc.Labels["workspaces.platform.dev/home-revision"] = fmt.Sprintf("%d", revision)

		pvc.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
		pvc.Spec.Resources.Requests = corev1.ResourceList{corev1.ResourceStorage: qty}
		pvc.Spec.StorageClassName = desk.Spec.Home.StorageClassName

		// DataSource is immutable; only set it on create.
		if creating && seedSnap != "" {
			pvc.Spec.DataSource = &corev1.TypedLocalObjectReference{
				APIGroup: ptrTo("snapshot.storage.k8s.io"),
				Kind:     "VolumeSnapshot",
				Name:     seedSnap,
			}
		}
		return controllerutil.SetControllerReference(desk, pvc, r.Scheme)
	})
	return err
}

func (r *DesktopReconciler) volumeSnapshotReady(ctx context.Context, namespace, name string) (bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return false, fmt.Errorf("snapshot name is empty")
	}
	vs := &unstructured.Unstructured{}
	vs.SetAPIVersion("snapshot.storage.k8s.io/v1")
	vs.SetKind("VolumeSnapshot")
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, vs); err != nil {
		return false, err
	}
	ready, found, err := unstructured.NestedBool(vs.Object, "status", "readyToUse")
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	return ready, nil
}

func (r *DesktopReconciler) enforceHomeRetention(ctx context.Context, desk *workspacesv1alpha1.Desktop, labels map[string]string, activePVC string, retainOld int) error {
	if retainOld < 0 {
		retainOld = 0
	}

	var pvcs corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &pvcs, client.InNamespace(desk.Namespace), client.MatchingLabels(labels)); err != nil {
		return err
	}

	type pvcInfo struct {
		name      string
		createdAt time.Time
	}
	all := make([]pvcInfo, 0, len(pvcs.Items))
	prefix := fmt.Sprintf("desktop-%s-home", desk.Name)
	for _, p := range pvcs.Items {
		if !strings.HasPrefix(p.Name, prefix) {
			continue
		}
		all = append(all, pvcInfo{name: p.Name, createdAt: p.CreationTimestamp.Time})
	}
	if len(all) <= retainOld+1 {
		return nil
	}

	// Newest-first.
	sort.Slice(all, func(i, j int) bool { return all[i].createdAt.After(all[j].createdAt) })

	keep := map[string]struct{}{activePVC: {}}
	for _, p := range all {
		if p.name == activePVC {
			continue
		}
		if len(keep)-1 >= retainOld {
			break
		}
		keep[p.name] = struct{}{}
	}

	for _, p := range all {
		if _, ok := keep[p.name]; ok {
			continue
		}
		// Avoid racing rollouts.
		if time.Since(p.createdAt) < time.Hour {
			continue
		}
		_ = r.Delete(ctx, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: p.name, Namespace: desk.Namespace}})
	}
	return nil
}
