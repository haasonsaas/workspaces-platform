package redact

import "testing"

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

