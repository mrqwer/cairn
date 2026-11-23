// Package index is the redacted, incremental full-text index of agent
// transcripts, the versioned transcript parsers that feed it, and
// `agents import`. Text is redacted before anything is written.
package index

import (
	"math"
	"regexp"
	"strings"
)

// Mask replaces every secret Redact finds.
const Mask = "[redacted]"

// Secret patterns. Order matters: whole blocks first, then prefixed
// tokens, then assignments, then anything that merely looks random.
var (
	privateKey = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY[A-Z ]*-----(?s:.*?)(?:-----END [A-Z0-9 ]*PRIVATE KEY[A-Z ]*-----|\z)`)
	urlUser    = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^/@\s]+@`)
	authScheme = regexp.MustCompile(`(?i)\b(bearer|basic|token)[ \t]+[A-Za-z0-9._~+/=-]{3,}`)
	prefixed   = regexp.MustCompile(`\b(?:` +
		`(?:sk|pk|rk)-[A-Za-z0-9_-]{10,}` +
		`|gh[pousr]_[A-Za-z0-9]{20,}` +
		`|github_pat_[A-Za-z0-9_]{20,}` +
		`|glpat-[A-Za-z0-9_-]{20,}` +
		`|xox[abposr]-[A-Za-z0-9-]{10,}` +
		`|AKIA[0-9A-Z]{16}` +
		`|AIza[0-9A-Za-z_-]{35}` +
		`)`)
	// An assignment whose key names a secret: KEY=value, key: value,
	// "key": "value". The key is kept, the value masked.
	secretKV = regexp.MustCompile(`(?i)\b([A-Za-z0-9_.-]*(?:secret|token|passw(?:or)?d|pass|key|credential|cookie)[A-Za-z0-9_.-]*)` + kvTail)
	opaque   = regexp.MustCompile(`[A-Za-z0-9+/_=-]{32,}`)
	hexOrID  = regexp.MustCompile(`^(?:[0-9a-fA-F]{32,}|[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})$`)
)

// kvTail is the separator and value of an assignment: an optional closing
// quote of a JSON key, '=' or ':', then an already masked, quoted or bare
// value.
const kvTail = `(["']?[ \t]*[:=][ \t]*)(\[redacted\]|"[^"\n]*"|'[^'\n]*'|[^\s"',;}\]]+)`

// Redact masks secrets in s: private key blocks, URL credentials, bearer,
// basic and token credentials, well-known token formats (sk-, ghp_, AKIA,
// xoxb- ...), the values of assignments whose key names a secret (secret,
// token, password, key, pass ...), and long strings that look random (at
// least 32 characters with letters and digits and an entropy over 4 bits).
// Hex digests (git SHAs), UUIDs, paths and identifiers are kept. Redact is
// idempotent.
func Redact(s string) string { return RedactFields(s, nil) }

// RedactFields is Redact that also masks the values of the given field
// names (an adapter's history.secret_fields, validated as [A-Za-z0-9_.-]).
func RedactFields(s string, fields []string) string {
	s = privateKey.ReplaceAllString(s, Mask)
	s = urlUser.ReplaceAllString(s, "${1}"+Mask+"@")
	s = authScheme.ReplaceAllStringFunc(s, func(m string) string {
		i := strings.IndexAny(m, " \t")
		scheme, cred := m[:i], m[i:]
		if strings.EqualFold(scheme, "token") && !strings.ContainsAny(cred, "0123456789") {
			return m // "token limit", "token budget": prose, not a credential
		}
		return scheme + " " + Mask
	})
	s = prefixed.ReplaceAllString(s, Mask)
	s = secretKV.ReplaceAllString(s, "${1}${2}"+Mask)
	if re := fieldsPattern(fields); re != nil {
		s = re.ReplaceAllString(s, "${1}${2}${3}"+Mask)
	}
	return opaque.ReplaceAllStringFunc(s, func(m string) string {
		if looksRandom(m) {
			return Mask
		}
		return m
	})
}

// fieldsPattern matches an assignment to one of fields; nil when none.
func fieldsPattern(fields []string) *regexp.Regexp {
	quoted := make([]string, 0, len(fields))
	for _, f := range fields {
		if f != "" {
			quoted = append(quoted, regexp.QuoteMeta(f))
		}
	}
	if len(quoted) == 0 {
		return nil
	}
	return regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_.-])(` + strings.Join(quoted, "|") + `)` + kvTail)
}

// looksRandom reports whether a long run of token characters is a secret
// rather than a digest, id, path or identifier.
func looksRandom(m string) bool {
	if hexOrID.MatchString(m) || !hasDigitAndLetter(m) {
		return false
	}
	if strings.Contains(m, "/") {
		// A path is words between slashes; a base64 secret has a long
		// random run between them.
		for _, seg := range strings.Split(m, "/") {
			if len(seg) >= 16 && hasDigitAndLetter(seg) && !hexOrID.MatchString(seg) && entropy(seg) > 3.5 {
				return true
			}
		}
		return false
	}
	return entropy(m) > 4.0
}

func hasDigitAndLetter(s string) bool {
	var d, l bool
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			d = true
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			l = true
		}
	}
	return d && l
}

// entropy is the Shannon entropy of s in bits per byte.
func entropy(s string) float64 {
	var n [256]int
	for i := 0; i < len(s); i++ {
		n[s[i]]++
	}
	var h float64
	for _, c := range n {
		if c > 0 {
			p := float64(c) / float64(len(s))
			h -= p * math.Log2(p)
		}
	}
	return h
}
