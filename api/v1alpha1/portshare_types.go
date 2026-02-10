package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PortShareLevel determines who can access a shared port.
//
// This intentionally mirrors common remote-workspace semantics (Coder-style),
// but the enforcement mechanism is platform-specific (gateway/proxy).
type PortShareLevel string

const (
	PortShareLevelOwner         PortShareLevel = "owner"
	PortShareLevelAuthenticated PortShareLevel = "authenticated"
	PortShareLevelOrganization  PortShareLevel = "organization"
	PortShareLevelPublic        PortShareLevel = "public"
)

type PortShareProtocol string

const (
	PortShareProtocolHTTP  PortShareProtocol = "http"
	PortShareProtocolHTTPS PortShareProtocol = "https"
	PortShareProtocolTCP   PortShareProtocol = "tcp"
)

type PortShareDesktopRef struct {
	// Name is the Desktop name this share targets.
	// The PortShare must be created in the same namespace as the Desktop.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// PortShareSpec defines the desired state of PortShare.
type PortShareSpec struct {
	// DesktopRef selects the Desktop this share applies to.
	DesktopRef PortShareDesktopRef `json:"desktopRef"`

	// RequestedAt is an optional timestamp used to "renew" a TTL-based share.
	// When ttlSeconds > 0, expiresAt is computed as requestedAt + ttlSeconds.
	// If omitted, the object's creationTimestamp is used as the start time.
	RequestedAt *metav1.Time `json:"requestedAt,omitempty"`

	// Port is the TCP port in the Desktop to expose.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`

	// ShareLevel controls who can access the share (enforcement is gateway-dependent).
	// +kubebuilder:validation:Enum=owner;authenticated;organization;public
	ShareLevel PortShareLevel `json:"shareLevel,omitempty"`

	// Protocol is a hint for the gateway/proxy layer.
	// +kubebuilder:validation:Enum=http;https;tcp
	Protocol PortShareProtocol `json:"protocol,omitempty"`

	// TTLSeconds optionally expires the share after a duration from creation/renewal.
	// 0 means "no TTL".
	// +kubebuilder:validation:Minimum=0
	TTLSeconds int32 `json:"ttlSeconds,omitempty"`
}

// PortShareStatus defines the observed state of PortShare.
type PortShareStatus struct {
	Active bool `json:"active,omitempty"`

	// ServiceName is the in-cluster Service pointing at the Desktop pods on the shared port.
	ServiceName string `json:"serviceName,omitempty"`

	ExpiresAt metav1.Time `json:"expiresAt,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=pshare

// PortShare is the Schema for shared desktop ports.
type PortShare struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PortShareSpec   `json:"spec,omitempty"`
	Status PortShareStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PortShareList contains a list of PortShare.
type PortShareList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PortShare `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PortShare{}, &PortShareList{})
}
