package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type NetworkGrantEgressRule struct {
	// Host is a DNS name allowed for egress (Cilium FQDN policy).
	Host string `json:"host"`

	// Ports are allowed TCP ports. If empty, defaults to [443].
	Ports []int32 `json:"ports,omitempty"`
}

type NetworkGrantSpec struct {
	// PodSelector selects the pods this grant applies to.
	PodSelector metav1.LabelSelector `json:"podSelector"`

	// Egress destinations to allow.
	Egress []NetworkGrantEgressRule `json:"egress,omitempty"`

	// TTLSeconds is how long this grant remains active after approval.
	TTLSeconds int32 `json:"ttlSeconds,omitempty"`

	// Approved gates whether the grant should be enforced.
	Approved bool `json:"approved,omitempty"`

	ApprovedBy string `json:"approvedBy,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

type NetworkGrantStatus struct {
	Active    bool        `json:"active,omitempty"`
	ExpiresAt metav1.Time `json:"expiresAt,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=ngrant

type NetworkGrant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NetworkGrantSpec   `json:"spec,omitempty"`
	Status NetworkGrantStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type NetworkGrantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NetworkGrant `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NetworkGrant{}, &NetworkGrantList{})
}
