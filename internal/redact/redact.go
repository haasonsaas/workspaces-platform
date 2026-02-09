package redact

import (
	"regexp"
	"sort"
	"strings"
)

// Redactor implements best-effort redaction for common secret/token patterns.
//
// This is intentionally conservative and incomplete in the MVP. The goal is to
// prevent obvious secrets from ending up in logs by default.
type Redactor struct {
	patterns []*regexp.Regexp
	literals []string

	literalSet map[string]struct{}
}

var secretEnvKeyRE = regexp.MustCompile(`(?i)(token|secret|password|passwd|key|api[_-]?key)`)

func NewDefault() *Redactor {
	// Keep patterns narrowly targeted to reduce false positives.
	pats := []*regexp.Regexp{
		// GitHub classic + fine-grained tokens.
		regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9_]{20,}\b`),
		regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`),
		// Slack tokens.
		regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`),
		// Vault tokens.
		regexp.MustCompile(`\bhvs\.[A-Za-z0-9]{20,}\b`),
		regexp.MustCompile(`\bs\.[A-Za-z0-9]{20,}\b`),
		// AWS access key IDs.
		regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
		// PEM blocks (redact the entire block, not just the header/footer).
		// DOTALL is safe here because output is capped upstream.
		regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]+-----.*?-----END [A-Z0-9 ]+-----`),
	}
	return &Redactor{patterns: pats}
}

func (r *Redactor) AddLiteralSecret(value string) {
	v := strings.TrimSpace(value)
	if v == "" {
		return
	}
	// Avoid redacting trivial values.
	if len(v) < 12 {
		return
	}
	// Avoid multiline/whitespace-y values; log redaction is byte-oriented.
	if strings.ContainsAny(v, " \t\r\n") {
		return
	}

	if r.literalSet == nil {
		r.literalSet = map[string]struct{}{}
	}
	if _, ok := r.literalSet[v]; ok {
		return
	}
	r.literalSet[v] = struct{}{}
	r.literals = append(r.literals, v)
	sort.Slice(r.literals, func(i, j int) bool { return len(r.literals[i]) > len(r.literals[j]) })
}

// AddPotentialSecretsFromEnviron adds best-effort literal redactions for env var
// values that look like secrets, keyed off the variable name.
func (r *Redactor) AddPotentialSecretsFromEnviron(environ []string) int {
	added := 0
	for _, kv := range environ {
		i := strings.IndexByte(kv, '=')
		if i <= 0 || i >= len(kv)-1 {
			continue
		}
		k := kv[:i]
		v := kv[i+1:]
		if !secretEnvKeyRE.MatchString(k) {
			continue
		}
		before := len(r.literals)
		r.AddLiteralSecret(v)
		if len(r.literals) != before {
			added++
		}
	}
	return added
}

func (r *Redactor) RedactString(s string) string {
	out := s
	for _, lit := range r.literals {
		if lit == "" {
			continue
		}
		out = strings.ReplaceAll(out, lit, "[REDACTED]")
	}
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
	if len(r.literals) == 0 &&
		!strings.Contains(s, "gh") &&
		!strings.Contains(s, "xox") &&
		!strings.Contains(s, "AKIA") &&
		!strings.Contains(s, "BEGIN ") &&
		!strings.Contains(s, "hvs.") &&
		!strings.Contains(s, "s.") {
		return b
	}
	return []byte(r.RedactString(s))
}
