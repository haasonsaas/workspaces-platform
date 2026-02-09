package main

import (
	"fmt"
	"strings"

	workspacesv1alpha1 "workspaces-platform/api/v1alpha1"
	"workspaces-platform/internal/netutil"
)

// networkGrantPolicy enforces "proxy-first" egress defaults at the broker layer.
//
// Important: the operator/controller still validates and enforces NetworkGrant
// safety invariants. This policy is an extra guardrail to avoid policy drift and
// accidental "open internet" unlocks via the GitHub approval path.
type networkGrantPolicy struct {
	// publicEgressMode controls whether non-admin callers can request/approve
	// public internet egress via NetworkGrants.
	//
	// Supported values: "deny", "allow".
	publicEgressMode string

	// internalSuffixAllowlist defines host suffixes treated as "internal" and
	// therefore exempt from public egress restrictions (still requires approval).
	// Example: svc.cluster.local, cluster.local, corp.example.com
	internalSuffixAllowlist []string

	// publicEgressAllowlist defines exact public hostnames allowed when
	// publicEgressMode is "deny".
	publicEgressAllowlist map[string]struct{}

	// publicDNSAllowlist defines additional exact DNS names allowed in dnsAllow
	// when publicEgressMode is "deny". This is intentionally separate from the
	// egress allowlist to avoid DNS-only unlock drift.
	publicDNSAllowlist map[string]struct{}
}

func newNetworkGrantPolicyFromEnv() (networkGrantPolicy, error) {
	mode := strings.ToLower(strings.TrimSpace(getenv("BROKER_NETWORK_PUBLIC_EGRESS_MODE", "deny")))
	switch mode {
	case "deny", "allow":
	default:
		return networkGrantPolicy{}, fmt.Errorf("BROKER_NETWORK_PUBLIC_EGRESS_MODE=%q is invalid (want deny|allow)", mode)
	}

	internalSuffixes := normalizeUniqueList(parseCSVList(getenv("BROKER_NETWORK_INTERNAL_SUFFIX_ALLOWLIST", "svc.cluster.local,cluster.local")))
	if len(internalSuffixes) == 0 {
		internalSuffixes = []string{"svc.cluster.local", "cluster.local"}
	}

	return networkGrantPolicy{
		publicEgressMode:        mode,
		internalSuffixAllowlist: internalSuffixes,
		publicEgressAllowlist:   normalizeSet(parseCSVSet(getenv("BROKER_NETWORK_PUBLIC_EGRESS_ALLOWLIST", ""))),
		publicDNSAllowlist:      normalizeSet(parseCSVSet(getenv("BROKER_NETWORK_PUBLIC_DNS_ALLOWLIST", ""))),
	}, nil
}

func normalizeToken(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, ".")
	return s
}

func normalizeUniqueList(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]struct{}{}
	for _, s := range in {
		s = normalizeToken(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func normalizeSet(in map[string]struct{}) map[string]struct{} {
	out := map[string]struct{}{}
	for k := range in {
		k = normalizeToken(k)
		if k == "" {
			continue
		}
		out[k] = struct{}{}
	}
	return out
}

func (p networkGrantPolicy) hostIsInternal(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return false
	}

	for _, suf := range p.internalSuffixAllowlist {
		suf = normalizeToken(suf)
		if suf == "" {
			continue
		}
		// Exact match or suffix match (".<suffix>").
		if h == suf || strings.HasSuffix(h, "."+suf) {
			return true
		}
	}
	return false
}

func (p networkGrantPolicy) publicHostAllowed(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return false
	}
	if p.publicEgressMode == "allow" {
		return true
	}
	_, ok := p.publicEgressAllowlist[h]
	return ok
}

func (p networkGrantPolicy) publicDNSAllowed(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return false
	}
	if p.publicEgressMode == "allow" {
		return true
	}
	_, ok := p.publicDNSAllowlist[h]
	return ok
}

// validateNonAdminNetworkGrant enforces a strict, proxy-first policy for callers
// that are not using the broker admin token.
func (p networkGrantPolicy) validateNonAdminNetworkGrant(spec workspacesv1alpha1.NetworkGrantSpec) error {
	// Keep the "least privilege" shape stable for non-admin usage:
	// - 443-only
	// - TCP only
	// - internal destinations via STRICT_FQDN
	// - public destinations (if allowlisted) via PROXY_CONNECT (proxy-first)
	mode := spec.PolicyMode

	proto := spec.Protocol
	if proto == "" {
		proto = workspacesv1alpha1.NetworkGrantProtocolTCP
	}
	if proto != workspacesv1alpha1.NetworkGrantProtocolTCP {
		return fmt.Errorf("protocol %q not allowed for non-admin", proto)
	}

	if spec.AllowNon443 {
		return fmt.Errorf("allowNon443 not allowed for non-admin")
	}

	publicRequested := false
	egressHosts := map[string]struct{}{}
	for i, r := range spec.Egress {
		raw := strings.TrimSpace(r.Host)
		if err := netutil.ValidateExactHostname(raw); err != nil {
			return fmt.Errorf("egress[%d].host %q is invalid: %w", i, raw, err)
		}
		h := strings.ToLower(raw)
		if h == "" {
			return fmt.Errorf("egress[%d].host is empty", i)
		}
		egressHosts[h] = struct{}{}

		ports := r.Ports
		if len(ports) == 0 {
			ports = []int32{443}
		}
		for _, pnum := range ports {
			if pnum != 443 {
				return fmt.Errorf("egress[%d] non-443 port %d not allowed for non-admin", i, pnum)
			}
		}

		if p.hostIsInternal(h) {
			continue
		}
		publicRequested = true
		if p.publicHostAllowed(h) {
			continue
		}
		return fmt.Errorf("public egress host %q is not allowed (proxy-first; use internal mirrors or admin override)", h)
	}

	if publicRequested {
		if mode == "" {
			mode = workspacesv1alpha1.NetworkGrantPolicyModeProxyConnect
		}
		if mode != workspacesv1alpha1.NetworkGrantPolicyModeProxyConnect {
			return fmt.Errorf("policyMode %q not allowed for public egress (non-admin requires PROXY_CONNECT)", mode)
		}
	} else {
		if mode == "" {
			mode = workspacesv1alpha1.NetworkGrantPolicyModeStrictFQDN
		}
		if mode != workspacesv1alpha1.NetworkGrantPolicyModeStrictFQDN {
			return fmt.Errorf("policyMode %q not allowed for internal egress (non-admin requires STRICT_FQDN)", mode)
		}
	}

	for i, host := range spec.DNSAllow {
		raw := strings.TrimSpace(host)
		if raw == "" {
			return fmt.Errorf("dnsAllow[%d] is empty", i)
		}
		if err := netutil.ValidateExactHostname(raw); err != nil {
			return fmt.Errorf("dnsAllow[%d] %q is invalid: %w", i, raw, err)
		}
		h := strings.ToLower(raw)
		if h == "" {
			return fmt.Errorf("dnsAllow[%d] is empty", i)
		}
		// Always allow DNS for requested egress hosts (redundant but harmless).
		if _, ok := egressHosts[h]; ok {
			continue
		}
		// Internal DNS is okay (still requires approval for actual egress).
		if p.hostIsInternal(h) {
			continue
		}
		// Public DNS allow needs explicit allowlist; don't implicitly allow it
		// based on the egress allowlist to avoid DNS-only drift.
		if p.publicDNSAllowed(h) {
			continue
		}
		return fmt.Errorf("public dnsAllow host %q is not allowed (proxy-first; admin can override)", h)
	}

	return nil
}
