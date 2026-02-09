package controller

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	workspacesv1alpha1 "workspaces-platform/api/v1alpha1"
)

type AgentJobReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	DefaultAgentRuntimeClass string
}

func (r *AgentJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var aj workspacesv1alpha1.AgentJob
	if err := r.Get(ctx, req.NamespacedName, &aj); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	labels := map[string]string{
		labelApp:      "agent",
		labelAgentJob: aj.Name,
	}

	jobName := fmt.Sprintf("agentjob-%s", aj.Name)
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: aj.Namespace}}

	desiredRuntimeClass := r.DefaultAgentRuntimeClass
	if aj.Spec.RuntimeClassName != nil && *aj.Spec.RuntimeClassName != "" {
		desiredRuntimeClass = *aj.Spec.RuntimeClassName
	}

	ttl := int32(3600)
	if aj.Spec.TTLSecondsAfterFinished != nil {
		ttl = *aj.Spec.TTLSecondsAfterFinished
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, job, func() error {
		if job.Labels == nil {
			job.Labels = map[string]string{}
		}
		for k, v := range labels {
			job.Labels[k] = v
		}

		job.Spec.BackoffLimit = ptrTo(int32(0))
		job.Spec.TTLSecondsAfterFinished = &ttl
		job.Spec.Template.ObjectMeta.Labels = labels
		job.Spec.Template.Spec = corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			RuntimeClassName:             &desiredRuntimeClass,
			AutomountServiceAccountToken: ptrTo(false),
			NodeSelector:                 aj.Spec.NodeSelector,
			Tolerations:                  aj.Spec.Tolerations,
			Volumes: []corev1.Volume{
				{
					Name:         "workspace",
					VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
				},
			},
			Containers: []corev1.Container{
				{
					Name:      "agent",
					Image:     aj.Spec.Image,
					Command:   aj.Spec.Command,
					Args:      aj.Spec.Args,
					Env:       append([]corev1.EnvVar{{Name: "WORKSPACE", Value: "/workspace"}}, aj.Spec.Env...),
					Resources: aj.Spec.Resources,
					VolumeMounts: []corev1.VolumeMount{
						{Name: "workspace", MountPath: "/workspace"},
					},
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: ptrTo(false),
						Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						RunAsNonRoot:             ptrTo(true),
						RunAsUser:                ptrTo(int64(1000)),
						SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
				},
			},
		}

		return controllerutil.SetControllerReference(&aj, job, r.Scheme)
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	// Status best-effort.
	phase := "Running"
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			phase = "Succeeded"
			break
		}
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			phase = "Failed"
			break
		}
	}
	if aj.Status.Phase != phase || aj.Status.JobName != jobName {
		patch := client.MergeFrom(aj.DeepCopy())
		aj.Status.Phase = phase
		aj.Status.JobName = jobName
		_ = r.Status().Patch(ctx, &aj, patch)
	}

	return ctrl.Result{}, nil
}

func (r *AgentJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&workspacesv1alpha1.AgentJob{}).
		Owns(&batchv1.Job{}).
		Complete(r)
}
