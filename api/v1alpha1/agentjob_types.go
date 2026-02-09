package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AgentJobSpec defines the desired state of an AgentJob.
type AgentJobSpec struct {
	// Image is the OCI image used for the agent job.
	Image string `json:"image"`

	// Command and Args are passed to the container.
	Command []string `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`

	// Script is an optional shell script to run under the workspaces agent runner
	// (for capped + redacted logs and consistent metadata). If set, the operator
	// will run the agent runner and ignore Command/Args for the main container.
	//
	// This is the recommended execution mode for agent sandboxes.
	Script string `json:"script,omitempty"`

	// Workdir is the working directory for Script execution. If empty, defaults
	// to /workspace or /workspace/repo when GitHub checkout is enabled.
	Workdir string `json:"workdir,omitempty"`

	// GitHub optionally enables a repo checkout initContainer (read-only) before
	// running Script. This is typically populated by the broker when creating
	// PR-scoped AgentJobs.
	GitHub *AgentJobGitHubSpec `json:"github,omitempty"`

	Env []corev1.EnvVar `json:"env,omitempty"`

	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// NodeSelector and Tolerations let you target an agent node pool.
	NodeSelector map[string]string   `json:"nodeSelector,omitempty"`
	Tolerations  []corev1.Toleration `json:"tolerations,omitempty"`

	// RuntimeClassName should point at the Kata RuntimeClass for strong isolation.
	// If omitted, the operator default is used.
	RuntimeClassName *string `json:"runtimeClassName,omitempty"`

	// TTLSecondsAfterFinished is applied to the underlying Job.
	TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished,omitempty"`

	// PolicyProfile selects a policy bundle (e.g. "restricted", "browser-automation").
	PolicyProfile string `json:"policyProfile,omitempty"`
}

type AgentJobGitHubSpec struct {
	// Repo is the GitHub repo in owner/repo format.
	Repo string `json:"repo"`

	// PullNumber is the PR number.
	PullNumber int32 `json:"pullNumber"`

	// HeadSHA is the expected PR head SHA (optional but recommended).
	HeadSHA string `json:"headSha,omitempty"`
}

type AgentJobStatus struct {
	Phase   string `json:"phase,omitempty"`
	JobName string `json:"jobName,omitempty"`

	// PodName is the selected pod backing the Job (best-effort; typically a single pod).
	PodName string `json:"podName,omitempty"`

	StartedAt   *metav1.Time `json:"startedAt,omitempty"`
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// ExitCode/ExitReason are best-effort values for the main agent container.
	ExitCode   *int32 `json:"exitCode,omitempty"`
	ExitReason string `json:"exitReason,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=ajob

type AgentJob struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentJobSpec   `json:"spec,omitempty"`
	Status AgentJobStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type AgentJobList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentJob `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentJob{}, &AgentJobList{})
}
