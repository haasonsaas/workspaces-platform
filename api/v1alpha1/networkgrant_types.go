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

type NetworkGrantAgentJobRef struct {
	// Name is the AgentJob name this grant applies to.
	// The grant must be created in the same namespace as the AgentJob.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

type NetworkGrantSpec struct {
	// AgentJobRef selects the AgentJob this grant applies to.
	// When set, the controller derives a stable pod selector based on the
	// AgentJob name (labels: workspaces.platform.dev/app=agent and
	// workspaces.platform.dev/agentjob=<name>).
	AgentJobRef *NetworkGrantAgentJobRef `json:"agentJobRef,omitempty"`

	// PodSelector selects the pods this grant applies to.
	// Deprecated: prefer AgentJobRef for stable least-privilege binding.
	PodSelector *metav1.LabelSelector `json:"podSelector,omitempty"`

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

	// DNSAllow adds additional DNS names the pod may resolve (DNS queries),
	// without granting direct egress to those names. This is useful to allow
	// CNAME chains while keeping egress constrained by `spec.egress`.
	//
	// MVP: exact match only (no wildcards).
	DNSAllow []string `json:"dnsAllow,omitempty"`

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
	Active     bool        `json:"active,omitempty"`
	ApprovedAt metav1.Time `json:"approvedAt,omitempty"`
	ExpiresAt  metav1.Time `json:"expiresAt,omitempty"`

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
