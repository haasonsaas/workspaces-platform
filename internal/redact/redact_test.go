package redact

import (
	"strings"
	"testing"
)

func TestRedactor_RedactString(t *testing.T) {
	r := NewDefault()
	in := "token=ghp_abcdefghijklmnopqrstuvwxyz0123456789 and slack=xoxb-1234567890-abcdef and aws=AKIAABCDEFGHIJKLMNOP"
	out := r.RedactString(in)
	if out == in {
		t.Fatalf("expected redaction; got unchanged output")
	}
	if contains(out, "ghp_") || contains(out, "xoxb-") || contains(out, "AKIA") {
		t.Fatalf("expected tokens redacted; got %q", out)
	}
}

func TestRedactor_LiteralSecretsFromEnv(t *testing.T) {
	r := NewDefault()
	n := r.AddPotentialSecretsFromEnviron([]string{
		"WORKSPACES_GITHUB_TOKEN=github_pat_abcdefghijklmnopqrstuvwxyz0123456789",
		"NOT_A_SECRET=hello",
		"MY_TOKEN=supersecretvalue-supersecretvalue",
	})
	if n == 0 {
		t.Fatalf("expected at least one env-derived literal secret added")
	}

	out := r.RedactString("leak: supersecretvalue-supersecretvalue")
	if strings.Contains(out, "supersecretvalue") {
		t.Fatalf("expected literal secret redacted; got %q", out)
	}
}

func TestRedactor_RedactPEMBlocks(t *testing.T) {
	r := NewDefault()
	in := "header\n-----BEGIN PRIVATE KEY-----\nABCDEF1234567890\n-----END PRIVATE KEY-----\nfooter\n"
	out := r.RedactString(in)
	if out == in {
		t.Fatalf("expected redaction; got unchanged output")
	}
	if strings.Contains(out, "ABCDEF1234567890") || strings.Contains(out, "BEGIN PRIVATE KEY") {
		t.Fatalf("expected PEM block redacted; got %q", out)
	}
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && (func() bool { return stringIndex(s, sub) >= 0 })()
}

func stringIndex(s, sub string) int {
	// avoid importing strings just for this tiny test helper
outer:
	for i := 0; i+len(sub) <= len(s); i++ {
		for j := 0; j < len(sub); j++ {
			if s[i+j] != sub[j] {
				continue outer
			}
		}
		return i
	}
	return -1
}
