package controller

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	workspacesv1alpha1 "workspaces-platform/api/v1alpha1"
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

	// Home PVC (explicit PVC so we can evolve towards snapshot/clone workflows).
	pvcName := fmt.Sprintf("desktop-%s-home", desk.Name)
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: pvcName, Namespace: desk.Namespace}}
	homeSize := desk.Spec.Home.Size
	if homeSize == "" {
		homeSize = "50Gi"
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, pvc, func() error {
		if pvc.Labels == nil {
			pvc.Labels = map[string]string{}
		}
		for k, v := range labels {
			pvc.Labels[k] = v
		}
		qty, qerr := resource.ParseQuantity(homeSize)
		if qerr != nil {
			return qerr
		}
		pvc.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
		pvc.Spec.Resources.Requests = corev1.ResourceList{corev1.ResourceStorage: qty}
		pvc.Spec.StorageClassName = desk.Spec.Home.StorageClassName
		return controllerutil.SetControllerReference(&desk, pvc, r.Scheme)
	})
	if err != nil {
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
		podSpec.NodeSelector = desk.Spec.NodeSelector
		podSpec.Tolerations = desk.Spec.Tolerations

		podSpec.Volumes = []corev1.Volume{
			{
				Name: "home",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcName},
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
