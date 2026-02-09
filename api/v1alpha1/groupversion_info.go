package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

const GroupName = "workspaces.platform.dev"

var (
	// GroupVersion is used to register these objects with the Scheme.
	GroupVersion = schema.GroupVersion{Group: GroupName, Version: "v1alpha1"}

	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}
	AddToScheme   = SchemeBuilder.AddToScheme
)
