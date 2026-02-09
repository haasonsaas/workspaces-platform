package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// HomeTemplate defines a reusable “golden home” PVC and a rolling set of
// VolumeSnapshots used to seed and reset Desktop home volumes.
//
// Storage is intentionally CSI-generic; Longhorn is the recommended default.
type HomeTemplateSpec struct {
	// StorageClassName for the template PVC. If omitted, the cluster default is used.
	StorageClassName *string `json:"storageClassName,omitempty"`

	// Size is the requested storage size (e.g. "50Gi").
	Size string `json:"size,omitempty"`

	// AccessModes for the template PVC. Defaults to [ReadWriteOnce].
	AccessModes []corev1.PersistentVolumeAccessMode `json:"accessModes,omitempty"`

	// SnapshotClassName is the VolumeSnapshotClass to use. If omitted, the CSI
	// default (if any) is used.
	SnapshotClassName *string `json:"snapshotClassName,omitempty"`

	// IntervalSeconds controls how often a new snapshot is created. Defaults to 86400 (daily).
	IntervalSeconds int32 `json:"intervalSeconds,omitempty"`

	// Retention controls how many snapshots to retain. Defaults to 7.
	Retention int32 `json:"retention,omitempty"`
}

type HomeTemplateStatus struct {
	TemplatePVC string `json:"templatePVC,omitempty"`

	LatestSnapshot string `json:"latestSnapshot,omitempty"`

	LastSnapshotTime *metav1.Time `json:"lastSnapshotTime,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=htpl

type HomeTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HomeTemplateSpec   `json:"spec,omitempty"`
	Status HomeTemplateStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type HomeTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HomeTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HomeTemplate{}, &HomeTemplateList{})
}

