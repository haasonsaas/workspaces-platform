package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	workspacesv1alpha1 "workspaces-platform/api/v1alpha1"
	"workspaces-platform/internal/netutil"
)

type networkGrantProfileOverride struct {
	// PublicEgressMode controls whether non-admin callers can request/approve
	// public internet egress via NetworkGrants for jobs in this policy profile.
	//
	// Supported values: "deny", "allow".
	PublicEgressMode string `json:"publicEgressMode,omitempty"`

	// InternalSuffixAllowlist extends the default internal suffix list for this
	// policy profile (exact or suffix match).
	InternalSuffixAllowlist []string `json:"internalSuffixAllowlist,omitempty"`

	// PublicEgressAllowlist extends the default public egress allowlist when the
	// effective mode is "deny".
	PublicEgressAllowlist []string `json:"publicEgressAllowlist,omitempty"`

	// PublicDNSAllowlist extends the default public dnsAllow allowlist when the
	// effective mode is "deny".
	PublicDNSAllowlist []string `json:"publicDNSAllowlist,omitempty"`

	// AllowNon443, when true, permits non-admin callers to request non-443 ports
	// when spec.allowNon443=true (still limited by AllowedNon443Ports).
	AllowNon443 *bool `json:"allowNon443,omitempty"`

	// AllowedNon443Ports is the set of TCP ports allowed for non-admin, non-443
	// access when AllowNon443=true. If empty, a safe default of [80] is used.
	AllowedNon443Ports []int32 `json:"allowedNon443Ports,omitempty"`
}

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

	// maxGrantsPerJob caps how many NetworkGrant objects may exist for a single
	// AgentJob (defense-in-depth against policy sprawl and object spam).
	// 0 disables the cap.
	maxGrantsPerJob int

	// nonAdminAllowNon443 controls whether non-admin callers may request non-443
	// ports when spec.allowNon443=true.
	nonAdminAllowNon443 bool

	// nonAdminAllowedNon443Ports is an allowlist of non-443 ports permitted for
	// non-admin callers when nonAdminAllowNon443=true.
	nonAdminAllowedNon443Ports map[int32]struct{}

	// profileOverrides optionally adjusts the policy based on AgentJob.spec.policyProfile.
	profileOverrides map[string]networkGrantProfileOverride
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

	maxGrantsPerJob := 20
	if raw := strings.TrimSpace(getenv("BROKER_NETWORK_MAX_GRANTS_PER_JOB", "20")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return networkGrantPolicy{}, fmt.Errorf("BROKER_NETWORK_MAX_GRANTS_PER_JOB=%q is invalid (want non-negative int)", raw)
		}
		maxGrantsPerJob = n
	}

	overrides, err := parseNetworkGrantProfileOverrides(getenv("BROKER_NETWORK_PROFILE_OVERRIDES", ""))
	if err != nil {
		return networkGrantPolicy{}, err
	}

	return networkGrantPolicy{
		publicEgressMode:        mode,
		internalSuffixAllowlist: internalSuffixes,
		publicEgressAllowlist:   normalizeSet(parseCSVSet(getenv("BROKER_NETWORK_PUBLIC_EGRESS_ALLOWLIST", ""))),
		publicDNSAllowlist:      normalizeSet(parseCSVSet(getenv("BROKER_NETWORK_PUBLIC_DNS_ALLOWLIST", ""))),
		maxGrantsPerJob:         maxGrantsPerJob,

		nonAdminAllowNon443:        false,
		nonAdminAllowedNon443Ports: map[int32]struct{}{},
		profileOverrides:           overrides,
	}, nil
}

func parseNetworkGrantProfileOverrides(raw string) (map[string]networkGrantProfileOverride, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return map[string]networkGrantProfileOverride{}, nil
	}

	var in map[string]networkGrantProfileOverride
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		return nil, fmt.Errorf("BROKER_NETWORK_PROFILE_OVERRIDES is invalid JSON: %w", err)
	}

	out := map[string]networkGrantProfileOverride{}
	for k, v := range in {
		prof := normalizeToken(k)
		if prof == "" {
			continue
		}
		if v.PublicEgressMode != "" {
			m := strings.ToLower(strings.TrimSpace(v.PublicEgressMode))
			if m != "deny" && m != "allow" {
				return nil, fmt.Errorf("BROKER_NETWORK_PROFILE_OVERRIDES[%q].publicEgressMode=%q is invalid (want deny|allow)", k, v.PublicEgressMode)
			}
			v.PublicEgressMode = m
		}

		// Validate hostnames in public allowlists (exact hostnames only).
		for _, h := range v.PublicEgressAllowlist {
			if err := netutil.ValidateExactHostname(strings.TrimSpace(h)); err != nil {
				return nil, fmt.Errorf("BROKER_NETWORK_PROFILE_OVERRIDES[%q].publicEgressAllowlist contains invalid hostname %q: %w", k, strings.TrimSpace(h), err)
			}
		}
		for _, h := range v.PublicDNSAllowlist {
			if err := netutil.ValidateExactHostname(strings.TrimSpace(h)); err != nil {
				return nil, fmt.Errorf("BROKER_NETWORK_PROFILE_OVERRIDES[%q].publicDNSAllowlist contains invalid hostname %q: %w", k, strings.TrimSpace(h), err)
			}
		}

		if v.AllowNon443 != nil && *v.AllowNon443 {
			for _, p := range v.AllowedNon443Ports {
				if p <= 0 || p > 65535 {
					return nil, fmt.Errorf("BROKER_NETWORK_PROFILE_OVERRIDES[%q].allowedNon443Ports contains invalid port %d", k, p)
				}
				if p == 443 {
					return nil, fmt.Errorf("BROKER_NETWORK_PROFILE_OVERRIDES[%q].allowedNon443Ports must not include 443", k)
				}
			}
		}

		out[prof] = v
	}

	return out, nil
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

func (p networkGrantPolicy) forProfile(profile string) networkGrantPolicy {
	prof := normalizeToken(profile)
	if prof == "" || p.profileOverrides == nil {
		return p
	}
	ov, ok := p.profileOverrides[prof]
	if !ok {
		return p
	}

	out := p

	if ov.PublicEgressMode != "" {
		out.publicEgressMode = strings.ToLower(strings.TrimSpace(ov.PublicEgressMode))
	}

	if len(ov.InternalSuffixAllowlist) != 0 {
		out.internalSuffixAllowlist = normalizeUniqueList(append(out.internalSuffixAllowlist, ov.InternalSuffixAllowlist...))
	}

	if len(ov.PublicEgressAllowlist) != 0 {
		if out.publicEgressAllowlist == nil {
			out.publicEgressAllowlist = map[string]struct{}{}
		}
		for _, h := range ov.PublicEgressAllowlist {
			n := normalizeToken(h)
			if n == "" {
				continue
			}
			out.publicEgressAllowlist[n] = struct{}{}
		}
	}

	if len(ov.PublicDNSAllowlist) != 0 {
		if out.publicDNSAllowlist == nil {
			out.publicDNSAllowlist = map[string]struct{}{}
		}
		for _, h := range ov.PublicDNSAllowlist {
			n := normalizeToken(h)
			if n == "" {
				continue
			}
			out.publicDNSAllowlist[n] = struct{}{}
		}
	}

	if ov.AllowNon443 != nil {
		out.nonAdminAllowNon443 = *ov.AllowNon443
		if !out.nonAdminAllowNon443 {
			out.nonAdminAllowedNon443Ports = map[int32]struct{}{}
		}
	}

	if out.nonAdminAllowNon443 {
		if out.nonAdminAllowedNon443Ports == nil {
			out.nonAdminAllowedNon443Ports = map[int32]struct{}{}
		}
		if len(ov.AllowedNon443Ports) != 0 {
			for _, port := range ov.AllowedNon443Ports {
				if port <= 0 || port > 65535 || port == 443 {
					continue
				}
				out.nonAdminAllowedNon443Ports[port] = struct{}{}
			}
		}
		// Safe default: allow port 80 only (in addition to always-allowed 443).
		if len(out.nonAdminAllowedNon443Ports) == 0 {
			out.nonAdminAllowedNon443Ports[80] = struct{}{}
		}
	}

	return out
}

// validateNonAdminNetworkGrant enforces a strict, proxy-first policy for callers
// that are not using the broker admin token.
func (p networkGrantPolicy) validateNonAdminNetworkGrant(policyProfile string, spec workspacesv1alpha1.NetworkGrantSpec) error {
	p = p.forProfile(policyProfile)

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

	if spec.AllowNon443 && !p.nonAdminAllowNon443 {
		return fmt.Errorf("allowNon443 not allowed for non-admin (policyProfile=%q)", strings.TrimSpace(policyProfile))
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
			if pnum == 443 {
				continue
			}
			if !spec.AllowNon443 {
				return fmt.Errorf("egress[%d] requests non-443 port %d but allowNon443 is false", i, pnum)
			}
			if !p.nonAdminAllowNon443 {
				return fmt.Errorf("egress[%d] non-443 port %d not allowed for non-admin (policyProfile=%q)", i, pnum, strings.TrimSpace(policyProfile))
			}
			if len(p.nonAdminAllowedNon443Ports) != 0 {
				if _, ok := p.nonAdminAllowedNon443Ports[pnum]; !ok {
					return fmt.Errorf("egress[%d] non-443 port %d not allowed for non-admin (policyProfile=%q)", i, pnum, strings.TrimSpace(policyProfile))
				}
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
