package netutil

import (
	"fmt"
	"net"
	"strings"
)

// NormalizeHostname canonicalizes a hostname for matching:
// - trims surrounding whitespace
// - lowercases
// - removes a single trailing dot (absolute FQDN form)
func NormalizeHostname(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	h = strings.TrimSuffix(h, ".")
	return h
}

var reservedHostDenylist = map[string]struct{}{
	// Never allow granting access to the Kubernetes API service via NetworkGrant.
	// If an agent needs cluster access, it should be via an explicit, audited
	// capability (and with auth), not via raw network egress.
	"kubernetes.default.svc":               {},
	"kubernetes.default.svc.cluster.local": {},
}

func isReservedHost(host string) bool {
	_, ok := reservedHostDenylist[host]
	return ok
}

func isValidDNSLabel(label string) bool {
	if label == "" || len(label) > 63 {
		return false
	}
	// RFC 1035-ish hostname label. Kubernetes service names are also DNS labels.
	for i := 0; i < len(label); i++ {
		c := label[i]
		isAZ := c >= 'a' && c <= 'z'
		is09 := c >= '0' && c <= '9'
		if !(isAZ || is09 || c == '-') {
			return false
		}
	}
	if label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	return true
}

// ValidateExactHostname validates an exact hostname suitable for use in
// NetworkGrant rules (no wildcards, no scheme/path/port, no IP literals).
//
// NOTE: This is intentionally strict and ASCII-only for MVP safety.
func ValidateExactHostname(host string) error {
	raw := strings.TrimSpace(host)
	if raw == "" {
		return fmt.Errorf("empty")
	}
	if strings.ContainsAny(raw, " \t\r\n") {
		return fmt.Errorf("contains whitespace")
	}
	if strings.Contains(raw, "*") {
		return fmt.Errorf("must be an exact hostname (no wildcards)")
	}
	if strings.ContainsAny(raw, "/:") {
		return fmt.Errorf("must be a hostname only (no scheme/path/port)")
	}
	if strings.HasPrefix(raw, ".") || strings.HasSuffix(raw, ".") {
		return fmt.Errorf("must not start or end with '.'")
	}
	if strings.Contains(raw, "..") {
		return fmt.Errorf("must not contain '..'")
	}
	if len(raw) > 253 {
		return fmt.Errorf("too long")
	}

	h := strings.ToLower(raw)
	if net.ParseIP(h) != nil {
		return fmt.Errorf("ip literals are not allowed")
	}
	if isReservedHost(h) {
		return fmt.Errorf("reserved hostname is not allowed")
	}

	parts := strings.Split(h, ".")
	for _, p := range parts {
		if !isValidDNSLabel(p) {
			return fmt.Errorf("invalid dns label %q", p)
		}
	}
	return nil
}

