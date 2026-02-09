package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type NetworkGrantPolicyMode string

const (
	// NetworkGrantPolicyModeStrictFQDN allows exact FQDN egress rules via Cilium toFQDNs.matchName.
	NetworkGrantPolicyModeStrictFQDN NetworkGrantPolicyMode = "STRICT_FQDN"
)

type NetworkGrantProtocol string

const (
	// NetworkGrantProtocolTCP is the only supported protocol in the MVP.
	NetworkGrantProtocolTCP NetworkGrantProtocol = "TCP"
)

type NetworkGrantEgressRule struct {
	// Host is a DNS name allowed for egress (Cilium FQDN policy).
	// MVP: exact match only (no wildcards).
	Host string `json:"host"`

	// Ports are allowed TCP ports. If empty, defaults to [443].
	Ports []int32 `json:"ports,omitempty"`
}

type NetworkGrantSpec struct {
	// PodSelector selects the pods this grant applies to.
	PodSelector metav1.LabelSelector `json:"podSelector"`

	// PolicyMode selects how egress rules are interpreted.
	// MVP: STRICT_FQDN only.
	// +kubebuilder:validation:Enum=STRICT_FQDN
	PolicyMode NetworkGrantPolicyMode `json:"policyMode,omitempty"`

	// Protocol is currently TCP only.
	// +kubebuilder:validation:Enum=TCP
	Protocol NetworkGrantProtocol `json:"protocol,omitempty"`

	// Purpose is a human-readable justification for the access request.
	// +kubebuilder:validation:MinLength=1
	Purpose string `json:"purpose"`

	// Egress destinations to allow.
	// +kubebuilder:validation:MinItems=1
	Egress []NetworkGrantEgressRule `json:"egress"`

	// AllowNon443 allows non-443 ports in egress rules. Defaults to false.
	AllowNon443 bool `json:"allowNon443,omitempty"`

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
