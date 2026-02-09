package controller

import (
	"testing"

	workspacesv1alpha1 "workspaces-platform/api/v1alpha1"
)

func TestResolveNetworkGrantTTLSeconds(t *testing.T) {
	t.Parallel()

	ttl, err := resolveNetworkGrantTTLSeconds(0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ttl != networkGrantDefaultTTLSeconds {
		t.Fatalf("ttl=%d want=%d", ttl, networkGrantDefaultTTLSeconds)
	}

	ttl, err = resolveNetworkGrantTTLSeconds(0, 900)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ttl != 900 {
		t.Fatalf("ttl=%d want=%d", ttl, 900)
	}

	if _, err := resolveNetworkGrantTTLSeconds(901, 900); err == nil {
		t.Fatalf("expected error for ttlSeconds above max")
	}
}

func TestValidateAndResolveNetworkGrantMatchLabels_CapsAndValidation(t *testing.T) {
	t.Parallel()

	base := func() workspacesv1alpha1.NetworkGrant {
		return workspacesv1alpha1.NetworkGrant{
			Spec: workspacesv1alpha1.NetworkGrantSpec{
				AgentJobRef: &workspacesv1alpha1.NetworkGrantAgentJobRef{Name: "job"},
				Purpose:     "fetch dependencies",
				Egress: []workspacesv1alpha1.NetworkGrantEgressRule{
					{Host: "example.com", Ports: []int32{443}},
				},
			},
		}
	}

	t.Run("ok", func(t *testing.T) {
		g := base()
		ml, dns, err := validateAndResolveNetworkGrantMatchLabels(&g, 7200, 20, 50)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ml[labelApp] != "agent" || ml[labelAgentJob] != "job" {
			t.Fatalf("unexpected match labels: %#v", ml)
		}
		if len(dns) == 0 {
			t.Fatalf("expected dns names to be derived from egress hosts")
		}
	})

	t.Run("ttl_exceeds_cap", func(t *testing.T) {
		g := base()
		g.Spec.TTLSeconds = 9001
		if _, _, err := validateAndResolveNetworkGrantMatchLabels(&g, 7200, 20, 50); err == nil {
			t.Fatalf("expected error")
		}
	})

	t.Run("egress_too_many", func(t *testing.T) {
		g := base()
		g.Spec.Egress = append(g.Spec.Egress, workspacesv1alpha1.NetworkGrantEgressRule{Host: "api.example.com", Ports: []int32{443}})
		if _, _, err := validateAndResolveNetworkGrantMatchLabels(&g, 7200, 1, 50); err == nil {
			t.Fatalf("expected error")
		}
	})

	t.Run("non443_requires_allowNon443", func(t *testing.T) {
		g := base()
		g.Spec.Egress = []workspacesv1alpha1.NetworkGrantEgressRule{{Host: "example.com", Ports: []int32{80}}}
		if _, _, err := validateAndResolveNetworkGrantMatchLabels(&g, 7200, 20, 50); err == nil {
			t.Fatalf("expected error")
		}
	})

	t.Run("wildcard_denied", func(t *testing.T) {
		g := base()
		g.Spec.Egress = []workspacesv1alpha1.NetworkGrantEgressRule{{Host: "*.example.com", Ports: []int32{443}}}
		if _, _, err := validateAndResolveNetworkGrantMatchLabels(&g, 7200, 20, 50); err == nil {
			t.Fatalf("expected error")
		}
	})
}
