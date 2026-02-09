package controller

import (
	"context"
	"fmt"
	"strings"

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

		workspaceEnv := append([]corev1.EnvVar{{Name: "WORKSPACE", Value: "/workspace"}}, aj.Spec.Env...)

		pod := corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			RuntimeClassName:             &desiredRuntimeClass,
			AutomountServiceAccountToken: ptrTo(false),
			EnableServiceLinks:           ptrTo(false),
			NodeSelector:                 aj.Spec.NodeSelector,
			Tolerations:                  aj.Spec.Tolerations,
			Volumes: []corev1.Volume{
				{
					Name:         "workspace",
					VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
				},
				{
					Name:         "tmp",
					VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
				},
			},
		}

		sec := &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptrTo(false),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			ReadOnlyRootFilesystem:   ptrTo(true),
			RunAsNonRoot:             ptrTo(true),
			RunAsUser:                ptrTo(int64(1000)),
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		}

		// Per-job broker auth (token Secret is created by the broker when launching
		// GitHub-triggered jobs). Optional for direct/admin-created AgentJobs.
		brokerSecretName := fmt.Sprintf("agentjob-%s-broker", aj.Name)
		brokerEnv := []corev1.EnvVar{
			{Name: "WORKSPACES_BROKER_URL", Value: "http://capability-broker.workspaces-system.svc.cluster.local:8080"},
			{
				Name: "WORKSPACES_BROKER_JOB_TOKEN",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: brokerSecretName},
						Key:                  "token",
						Optional:             ptrTo(true),
					},
				},
			},
		}

		if strings.TrimSpace(aj.Spec.Script) != "" {
			// Script mode: run via the workspaces agent runner to ensure output is
			// capped + redacted in logs and exec metadata is emitted.
			workdir := strings.TrimSpace(aj.Spec.Workdir)
			repoDir := "/workspace/repo"
			if workdir == "" {
				if aj.Spec.GitHub != nil {
					workdir = repoDir
				} else {
					workdir = "/workspace"
				}
			}

			// Optional: checkout a PR head before running.
			if aj.Spec.GitHub != nil {
				secretName := fmt.Sprintf("agentjob-%s-github", aj.Name)
				checkout := corev1.Container{
					Name:    "checkout",
					Image:   aj.Spec.Image,
					Command: []string{"/bin/bash", "-lc"},
					Args: []string{`set -euo pipefail
: "${WORKSPACES_GITHUB_REPO:?missing}"
: "${WORKSPACES_GITHUB_PR_NUMBER:?missing}"
dest="${WORKSPACE}/repo"
rm -rf "$dest"
mkdir -p "$dest"
cat > /tmp/askpass.sh <<'EOF'
#!/bin/sh
echo "$WORKSPACES_GITHUB_TOKEN"
EOF
chmod 0700 /tmp/askpass.sh
export GIT_ASKPASS=/tmp/askpass.sh
export GIT_TERMINAL_PROMPT=0
git -C "$dest" init
git -C "$dest" remote add origin "https://x-access-token@github.com/${WORKSPACES_GITHUB_REPO}.git"
git -C "$dest" fetch --depth 1 origin "refs/pull/${WORKSPACES_GITHUB_PR_NUMBER}/head:pr"
git -C "$dest" checkout --force pr
if [ -n "${WORKSPACES_GITHUB_HEAD_SHA:-}" ]; then
  got="$(git -C "$dest" rev-parse HEAD)"
  if [ "$got" != "$WORKSPACES_GITHUB_HEAD_SHA" ]; then
    echo "head sha mismatch: $got" >&2
    exit 1
  fi
fi
`},
					Env: append(workspaceEnv, []corev1.EnvVar{
						{Name: "WORKSPACES_GITHUB_REPO", Value: strings.TrimSpace(aj.Spec.GitHub.Repo)},
						{Name: "WORKSPACES_GITHUB_PR_NUMBER", Value: fmt.Sprintf("%d", aj.Spec.GitHub.PullNumber)},
						{Name: "WORKSPACES_GITHUB_HEAD_SHA", Value: strings.TrimSpace(aj.Spec.GitHub.HeadSHA)},
						{
							Name: "WORKSPACES_GITHUB_TOKEN",
							ValueFrom: &corev1.EnvVarSource{
								SecretKeyRef: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
									Key:                  "token",
								},
							},
						},
					}...),
					VolumeMounts: []corev1.VolumeMount{
						{Name: "workspace", MountPath: "/workspace"},
						{Name: "tmp", MountPath: "/tmp"},
					},
					SecurityContext: sec,
				}
				pod.InitContainers = append(pod.InitContainers, checkout)
			}

			mainEnv := append(workspaceEnv,
				corev1.EnvVar{Name: "WORKSPACES_TASK_SCRIPT", Value: strings.TrimSpace(aj.Spec.Script)},
				corev1.EnvVar{Name: "WORKSPACES_WORKDIR", Value: workdir},
				corev1.EnvVar{Name: "WORKSPACES_REPO_DIR", Value: repoDir},
			)
			mainEnv = append(mainEnv, brokerEnv...)
			if aj.Spec.GitHub != nil {
				mainEnv = append(mainEnv,
					corev1.EnvVar{Name: "WORKSPACES_GITHUB_REPO", Value: strings.TrimSpace(aj.Spec.GitHub.Repo)},
					corev1.EnvVar{Name: "WORKSPACES_GITHUB_PR_NUMBER", Value: fmt.Sprintf("%d", aj.Spec.GitHub.PullNumber)},
					corev1.EnvVar{Name: "WORKSPACES_GITHUB_HEAD_SHA", Value: strings.TrimSpace(aj.Spec.GitHub.HeadSHA)},
				)
			}

			main := corev1.Container{
				Name:      "agent",
				Image:     aj.Spec.Image,
				Command:   []string{"/usr/local/bin/workspaces-agent-runner"},
				Env:       mainEnv,
				Resources: aj.Spec.Resources,
				VolumeMounts: []corev1.VolumeMount{
					{Name: "workspace", MountPath: "/workspace"},
					{Name: "tmp", MountPath: "/tmp"},
				},
				WorkingDir:      workdir,
				SecurityContext: sec,
			}
			pod.Containers = []corev1.Container{main}
		} else {
			// Direct mode: run the container command as-is.
			pod.Containers = []corev1.Container{
				{
					Name:      "agent",
					Image:     aj.Spec.Image,
					Command:   aj.Spec.Command,
					Args:      aj.Spec.Args,
					Env:       append(workspaceEnv, brokerEnv...),
					Resources: aj.Spec.Resources,
					VolumeMounts: []corev1.VolumeMount{
						{Name: "workspace", MountPath: "/workspace"},
						{Name: "tmp", MountPath: "/tmp"},
					},
					SecurityContext: sec,
				},
			}
		}

		job.Spec.Template.Spec = pod

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

	// Cleanup secrets + auto-grants after completion. These resources are owned
	// by AgentJob for GC when AgentJob is deleted, but we want them gone ASAP on
	// success/failure.
	if phase == "Succeeded" || phase == "Failed" {
		_ = r.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("agentjob-%s-github", aj.Name), Namespace: aj.Namespace}})
		_ = r.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("agentjob-%s-broker", aj.Name), Namespace: aj.Namespace}})
		_ = r.Delete(ctx, &workspacesv1alpha1.NetworkGrant{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("autogrant-%s-github", aj.Name), Namespace: aj.Namespace}})
	}

	return ctrl.Result{}, nil
}

func (r *AgentJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&workspacesv1alpha1.AgentJob{}).
		Owns(&batchv1.Job{}).
		Complete(r)
}
