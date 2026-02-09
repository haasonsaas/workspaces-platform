package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DesktopSpec defines the desired state of Desktop.
type DesktopSpec struct {
	// User is the Linux username inside the desktop.
	User string `json:"user"`

	// Image is the OCI image used for the desktop container.
	// If omitted, the operator default is used.
	Image string `json:"image,omitempty"`

	SSH DesktopSSHSpec `json:"ssh,omitempty"`

	Home DesktopHomeSpec `json:"home,omitempty"`

	// Resources applied to the desktop container.
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// NodeSelector and Tolerations let you target a desktop node pool.
	NodeSelector map[string]string   `json:"nodeSelector,omitempty"`
	Tolerations  []corev1.Toleration `json:"tolerations,omitempty"`

	// SecurityProfile selects a policy bundle (e.g. "standard", "priv-compat").
	SecurityProfile string `json:"securityProfile,omitempty"`
}

type DesktopSSHSpec struct {
	// AuthorizedKeys are SSH public keys allowed to access this desktop.
	AuthorizedKeys []string `json:"authorizedKeys,omitempty"`
}

type DesktopHomeSpec struct {
	// StorageClassName for the home PVC created for this Desktop.
	StorageClassName *string `json:"storageClassName,omitempty"`

	// Size is the requested storage size (e.g. "50Gi").
	Size string `json:"size,omitempty"`

	// Seed controls initial creation of the home PVC when it does not exist.
	Seed *DesktopHomeSeedSpec `json:"seed,omitempty"`

	// Reset controls home reset behavior. A reset is triggered when requestedAt
	// changes and a valid snapshot source is configured.
	Reset *DesktopHomeResetSpec `json:"reset,omitempty"`
}

type DesktopHomeSeedSpec struct {
	// TemplateRef names a HomeTemplate in the same namespace whose latest
	// ready snapshot should be used as the PVC dataSource.
	TemplateRef *string `json:"templateRef,omitempty"`

	// SnapshotName is a specific VolumeSnapshot name to seed from.
	SnapshotName *string `json:"snapshotName,omitempty"`
}

type DesktopHomeResetSpec struct {
	// RequestedAt triggers a reset when changed.
	RequestedAt *metav1.Time `json:"requestedAt,omitempty"`

	// TemplateRef names a HomeTemplate to reset from (preferred).
	TemplateRef *string `json:"templateRef,omitempty"`

	// SnapshotName is a specific VolumeSnapshot name to reset from.
	SnapshotName *string `json:"snapshotName,omitempty"`

	// RetainOldClaims controls how many previous home PVC revisions to keep.
	// Defaults to 1.
	RetainOldClaims int32 `json:"retainOldClaims,omitempty"`
}

// DesktopStatus defines the observed state of Desktop.
type DesktopStatus struct {
	Phase string `json:"phase,omitempty"`

	// ServiceName is the in-cluster Service exposing SSH.
	ServiceName string `json:"serviceName,omitempty"`

	// HomeClaimName is the currently mounted home PVC name.
	HomeClaimName string `json:"homeClaimName,omitempty"`

	// HomeRevision increments when home is reset.
	HomeRevision int64 `json:"homeRevision,omitempty"`

	// LastResetAt records the last observed successful reset request timestamp.
	LastResetAt *metav1.Time `json:"lastResetAt,omitempty"`

	// Conditions follows Kubernetes conventions.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=desk

// Desktop is the Schema for the desktops API.
type Desktop struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DesktopSpec   `json:"spec,omitempty"`
	Status DesktopStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// DesktopList contains a list of Desktop.
type DesktopList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Desktop `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Desktop{}, &DesktopList{})
}
