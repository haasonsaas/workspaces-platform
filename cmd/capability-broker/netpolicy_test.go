package main

import (
	"testing"

	workspacesv1alpha1 "workspaces-platform/api/v1alpha1"
)

func TestNetworkGrantPolicy_HostIsInternal(t *testing.T) {
	p := networkGrantPolicy{
		internalSuffixAllowlist: []string{"svc.cluster.local", "cluster.local"},
		publicEgressMode:        "deny",
		publicEgressAllowlist:   map[string]struct{}{},
		publicDNSAllowlist:      map[string]struct{}{},
	}

	cases := []struct {
		host string
		want bool
	}{
		{"svc.cluster.local", true},
		{"foo.svc.cluster.local", true},
		{"foo.cluster.local", true},
		{"foo.svc.cluster.local.evil.com", false},
		{"github.com", false},
	}

	for _, tc := range cases {
		if got := p.hostIsInternal(tc.host); got != tc.want {
			t.Fatalf("hostIsInternal(%q)=%v want %v", tc.host, got, tc.want)
		}
	}
}

func TestNetworkGrantPolicy_ValidateNonAdminNetworkGrant(t *testing.T) {
	p := networkGrantPolicy{
		publicEgressMode:        "deny",
		internalSuffixAllowlist: []string{"svc.cluster.local", "cluster.local"},
		publicEgressAllowlist:   map[string]struct{}{"github.com": {}},
		publicDNSAllowlist:      map[string]struct{}{"github.map.fastly.net": {}},
		nonAdminAllowNon443:     false,
		nonAdminAllowedNon443Ports: map[int32]struct{}{
			80: {},
		},
		profileOverrides: map[string]networkGrantProfileOverride{
			"browser-automation": {
				PublicEgressMode:      "allow",
				AllowNon443:           ptrTo(true),
				AllowedNon443Ports:    []int32{80},
				PublicDNSAllowlist:    []string{"example.com"},
				PublicEgressAllowlist: []string{"example.com"},
			},
		},
	}

	t.Run("allows internal 443", func(t *testing.T) {
		spec := workspacesv1alpha1.NetworkGrantSpec{
			PolicyMode: workspacesv1alpha1.NetworkGrantPolicyModeStrictFQDN,
			Protocol:   workspacesv1alpha1.NetworkGrantProtocolTCP,
			Egress: []workspacesv1alpha1.NetworkGrantEgressRule{
				{Host: "capability-broker.workspaces-system.svc.cluster.local", Ports: []int32{443}},
			},
		}
		if err := p.validateNonAdminNetworkGrant("restricted", spec); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("denies allowNon443", func(t *testing.T) {
		spec := workspacesv1alpha1.NetworkGrantSpec{
			PolicyMode:  workspacesv1alpha1.NetworkGrantPolicyModeStrictFQDN,
			Protocol:    workspacesv1alpha1.NetworkGrantProtocolTCP,
			AllowNon443: true,
			Egress: []workspacesv1alpha1.NetworkGrantEgressRule{
				{Host: "capability-broker.workspaces-system.svc.cluster.local", Ports: []int32{80}},
			},
		}
		if err := p.validateNonAdminNetworkGrant("restricted", spec); err == nil {
			t.Fatalf("expected error")
		}
	})

	t.Run("denies public host not allowlisted", func(t *testing.T) {
		spec := workspacesv1alpha1.NetworkGrantSpec{
			PolicyMode: workspacesv1alpha1.NetworkGrantPolicyModeProxyConnect,
			Protocol:   workspacesv1alpha1.NetworkGrantProtocolTCP,
			Egress: []workspacesv1alpha1.NetworkGrantEgressRule{
				{Host: "crates.io", Ports: []int32{443}},
			},
		}
		if err := p.validateNonAdminNetworkGrant("restricted", spec); err == nil {
			t.Fatalf("expected error")
		}
	})

	t.Run("allows public host allowlisted", func(t *testing.T) {
		spec := workspacesv1alpha1.NetworkGrantSpec{
			PolicyMode: workspacesv1alpha1.NetworkGrantPolicyModeProxyConnect,
			Protocol:   workspacesv1alpha1.NetworkGrantProtocolTCP,
			Egress: []workspacesv1alpha1.NetworkGrantEgressRule{
				{Host: "github.com", Ports: []int32{443}},
			},
		}
		if err := p.validateNonAdminNetworkGrant("restricted", spec); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("denies public dnsAllow not allowlisted", func(t *testing.T) {
		spec := workspacesv1alpha1.NetworkGrantSpec{
			PolicyMode: workspacesv1alpha1.NetworkGrantPolicyModeProxyConnect,
			Protocol:   workspacesv1alpha1.NetworkGrantProtocolTCP,
			Egress: []workspacesv1alpha1.NetworkGrantEgressRule{
				{Host: "github.com", Ports: []int32{443}},
			},
			DNSAllow: []string{"not.allowed.example"},
		}
		if err := p.validateNonAdminNetworkGrant("restricted", spec); err == nil {
			t.Fatalf("expected error")
		}
	})

	t.Run("allows public dnsAllow allowlisted", func(t *testing.T) {
		spec := workspacesv1alpha1.NetworkGrantSpec{
			PolicyMode: workspacesv1alpha1.NetworkGrantPolicyModeProxyConnect,
			Protocol:   workspacesv1alpha1.NetworkGrantProtocolTCP,
			Egress: []workspacesv1alpha1.NetworkGrantEgressRule{
				{Host: "github.com", Ports: []int32{443}},
			},
			DNSAllow: []string{"github.map.fastly.net"},
		}
		if err := p.validateNonAdminNetworkGrant("restricted", spec); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("denies public host in STRICT_FQDN", func(t *testing.T) {
		spec := workspacesv1alpha1.NetworkGrantSpec{
			PolicyMode: workspacesv1alpha1.NetworkGrantPolicyModeStrictFQDN,
			Protocol:   workspacesv1alpha1.NetworkGrantProtocolTCP,
			Egress: []workspacesv1alpha1.NetworkGrantEgressRule{
				{Host: "github.com", Ports: []int32{443}},
			},
		}
		if err := p.validateNonAdminNetworkGrant("restricted", spec); err == nil {
			t.Fatalf("expected error")
		}
	})

	t.Run("denies internal host in PROXY_CONNECT", func(t *testing.T) {
		spec := workspacesv1alpha1.NetworkGrantSpec{
			PolicyMode: workspacesv1alpha1.NetworkGrantPolicyModeProxyConnect,
			Protocol:   workspacesv1alpha1.NetworkGrantProtocolTCP,
			Egress: []workspacesv1alpha1.NetworkGrantEgressRule{
				{Host: "capability-broker.workspaces-system.svc.cluster.local", Ports: []int32{443}},
			},
		}
		if err := p.validateNonAdminNetworkGrant("restricted", spec); err == nil {
			t.Fatalf("expected error")
		}
	})

	t.Run("browser-automation allows allowNon443 port 80 to public hosts", func(t *testing.T) {
		spec := workspacesv1alpha1.NetworkGrantSpec{
			PolicyMode:  workspacesv1alpha1.NetworkGrantPolicyModeProxyConnect,
			Protocol:    workspacesv1alpha1.NetworkGrantProtocolTCP,
			AllowNon443: true,
			Egress: []workspacesv1alpha1.NetworkGrantEgressRule{
				{Host: "example.com", Ports: []int32{80}},
			},
		}
		if err := p.validateNonAdminNetworkGrant("browser-automation", spec); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func ptrTo[T any](v T) *T { return &v }
