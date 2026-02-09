package redact

import (
	"regexp"
	"strings"
)

// Redactor implements best-effort redaction for common secret/token patterns.
//
// This is intentionally conservative and incomplete in the MVP. The goal is to
// prevent obvious secrets from ending up in logs by default.
type Redactor struct {
	patterns []*regexp.Regexp
}

func NewDefault() *Redactor {
	// Keep patterns narrowly targeted to reduce false positives.
	pats := []*regexp.Regexp{
		// GitHub classic + fine-grained tokens.
		regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9_]{20,}\b`),
		regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`),
		// Slack tokens.
		regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`),
		// AWS access key IDs.
		regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
		// PEM blocks (redact the header; downstream logic should avoid printing entire blocks).
		regexp.MustCompile(`-----BEGIN [A-Z0-9 ]+-----`),
		regexp.MustCompile(`-----END [A-Z0-9 ]+-----`),
	}
	return &Redactor{patterns: pats}
}

func (r *Redactor) RedactString(s string) string {
	out := s
	for _, re := range r.patterns {
		out = re.ReplaceAllString(out, "[REDACTED]")
	}
	return out
}

// RedactBytes applies string-based redaction on UTF-8-ish data. This is best-effort;
// binary output is not supported and should be suppressed before logging.
func (r *Redactor) RedactBytes(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	// Avoid allocating when nothing looks interesting.
	s := string(b)
	if !strings.Contains(s, "gh") && !strings.Contains(s, "xox") && !strings.Contains(s, "AKIA") && !strings.Contains(s, "BEGIN ") {
		return b
	}
	return []byte(r.RedactString(s))
}

