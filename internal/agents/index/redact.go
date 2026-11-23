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

// RedactorVersion changes whenever the rules below change, so index
// entries written under older rules are redacted again.
const RedactorVersion = "2"

// Secret patterns. Order matters: whole blocks first, then credentials in
// URLs and command lines, prefixed tokens, assignments, and last anything
// that merely looks random.
var (
	privateKey = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY[A-Z ]*-----(?s:.*?)(?:-----END [A-Z0-9 ]*PRIVATE KEY[A-Z ]*-----|\z)`)
	// The userinfo runs to the last '@' before the host, so an '@' inside
	// the password is covered.
	urlUser    = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^/\s?#]*@`)
	authScheme = regexp.MustCompile(`(?i)\b(bearer|basic|token)[ \t]+([A-Za-z0-9._~+/=-]{3,})`)
	curlUser   = regexp.MustCompile(`(^|\s)(-u|--user)([ \t]+|=)([^\s:/]+):([^\s@]+)`)
	shortPass  = regexp.MustCompile(`(^|\s)-p([^\s-]\S{5,})`)
	prefixed   = regexp.MustCompile(`\b(?:` +
		`(?:sk|pk|rk)-[A-Za-z0-9_-]{10,}` +
		`|(?:sk|pk|rk)_(?:live|test)_[A-Za-z0-9]{10,}` +
		`|gh[pousr]_[A-Za-z0-9]{16,}` +
		`|github_pat_[A-Za-z0-9_]{20,}` +
		`|glpat-[A-Za-z0-9_-]{20,}` +
		`|xox[abposr]-[A-Za-z0-9-]{10,}` +
		`|xapp-[A-Za-z0-9-]{10,}` +
		`|hf_[A-Za-z0-9]{20,}` +
		`|npm_[A-Za-z0-9]{20,}` +
		`|shp(?:at|ca|pa|ss)_[A-Fa-f0-9]{20,}` +
		`|dop_v1_[a-f0-9]{20,}` +
		`|ya29\.[A-Za-z0-9_-]{20,}` +
		`|(?:AKIA|ASIA)[0-9A-Z]{16}` +
		`|AIza[0-9A-Za-z_-]{35}` +
		`|SK[0-9a-fA-F]{32}` +
		`)`)
	// An assignment, a flag or a "key value" pair whose key mentions a
	// secret. Whether the value is masked is decided by maskKV; the key
	// always stays visible.
	secretKV = regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_.\\-])(-{0,2}[A-Za-z0-9_.-]*?(?:secret|passw|pwd|pass|key|token|auth|credential|cookie|session|private)[A-Za-z0-9_.-]*)` + kvTail)
	// A YAML key naming a secret whose value is on the next, deeper line.
	yamlNext = regexp.MustCompile(`(?im)^([ \t]*)(-?[ \t]*["']?[A-Za-z0-9_.-]*(?:secret|passw|pwd|token|api_?key|credential|private_?key)[A-Za-z0-9_.-]*["']?[ \t]*:[ \t]*\n)([ \t]+)([^\s][^\n]*)`)
	opaque   = regexp.MustCompile(`[A-Za-z0-9+/_-]{32,}={0,2}`)
	hexOrID  = regexp.MustCompile(`^(?:[0-9a-fA-F]{32,}|[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})$`)
)

// kvTail is the closing quote of a (possibly escaped) JSON key, the
// separator ('=', ':' or blanks) and the value: already masked, quoted,
// escaped-quoted or bare.
const kvTail = `(\\?["']?)([ \t]*[:=][ \t]*|[ \t]+)(\[redacted\]|\\"(?:[^"\\\n]|\\[^"\n])*\\"|"[^"\n]*"|'[^'\n]*'|[^\s"',;}\]\\]+)`

// Redact masks secrets in s and keeps the names around them:
//   - private key blocks, URL credentials (to the last '@'), bearer, basic
//     and token credentials, curl -u and mysql -p passwords;
//   - well-known token formats (sk-, sk_live_, ghp_, AKIA, ASIA, xoxb-,
//     hf_, npm_, ya29. ...);
//   - the values of assignments, flags and JSON or YAML members whose key
//     names a secret (password, secret, token, auth, session, cookie,
//     api key ...), the key itself kept; a key that only contains such a
//     word (monkey, keyboard) needs a credential-like value, and a short
//     number is kept (max_tokens=4096);
//   - long strings that look random (at least 32 characters, letters and
//     digits, entropy over 4 bits), or the random segments of a path.
//
// Hex digests (git SHAs), UUIDs, paths, identifiers and prose are kept.
// Redact is idempotent.
func Redact(s string) string { return RedactFields(s, nil) }

// RedactFields is Redact that also masks the values of the given field
// names (an adapter's history.secret_fields, validated as [A-Za-z0-9_.-]).
func RedactFields(s string, fields []string) string {
	s = privateKey.ReplaceAllString(s, Mask)
	s = urlUser.ReplaceAllString(s, "${1}"+Mask+"@")
	s = authScheme.ReplaceAllStringFunc(s, func(m string) string {
		sub := authScheme.FindStringSubmatch(m)
		scheme, cred := sub[1], sub[2]
		if !credentialLike(cred) && (!strings.EqualFold(scheme, "bearer") || len(cred) < 6) {
			return m // "basic stuff", "token budget": prose
		}
		return scheme + " " + Mask
	})
	s = curlUser.ReplaceAllString(s, "${1}${2}${3}${4}:"+Mask)
	s = shortPass.ReplaceAllStringFunc(s, func(m string) string {
		sub := shortPass.FindStringSubmatch(m)
		if v := sub[2]; v != Mask && hasLetter(v) && (hasDigit(v) || hasSymbol(v)) {
			return sub[1] + "-p" + Mask
		}
		return m
	})
	s = prefixed.ReplaceAllString(s, Mask)
	s = yamlNext.ReplaceAllStringFunc(s, func(m string) string {
		sub := yamlNext.FindStringSubmatch(m)
		indent, next, val := sub[1], sub[3], sub[4]
		if len(next) <= len(indent) || strings.Contains(val, ": ") || val == Mask {
			return m
		}
		return indent + sub[2] + next + Mask
	})
	s = replaceKV(secretKV, s, maskKV)
	if re := fieldsPattern(fields); re != nil {
		s = replaceKV(re, s, func(_, _, v string) bool { return unquote(v) != Mask })
	}
	return opaque.ReplaceAllStringFunc(s, maskRandom)
}

// replaceKV replaces the value of each match of re (groups: prefix, key,
// quote, separator, value) for which mask(key, separator, value) holds.
func replaceKV(re *regexp.Regexp, s string, mask func(key, sep, val string) bool) string {
	idx := re.FindAllStringSubmatchIndex(s, -1)
	if idx == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, m := range idx {
		g := func(i int) string { return s[m[2*i]:m[2*i+1]] }
		if !mask(g(2), g(4), g(5)) {
			continue
		}
		b.WriteString(s[last:m[10]])
		b.WriteString(Mask)
		last = m[11]
	}
	b.WriteString(s[last:])
	return b.String()
}

type keyClass int

const (
	keyWeak   keyClass = iota // contains a secret word: monkey, keyboard, passage
	keyMedium                 // is one: key, pass, db_pass, key_id
	keyStrong                 // names a secret: password, secret, token, api_key, auth, session
)

var strongWords = []string{"secret", "passw", "pwd", "credential", "private", "cookie", "session",
	"apikey", "api_key", "api-key", "accesskey", "access_key", "access-key", "token"}

func classify(key string) keyClass {
	k := strings.ToLower(strings.TrimLeft(key, "-"))
	for _, w := range strongWords {
		if strings.Contains(k, w) {
			return keyStrong
		}
	}
	// auth, oauth, x-auth, authorization; not author.
	for rest := k; ; {
		i := strings.Index(rest, "auth")
		if i < 0 {
			break
		}
		rest = rest[i+4:]
		if !strings.HasPrefix(rest, "or") || strings.HasPrefix(rest, "oriz") {
			return keyStrong
		}
	}
	for _, w := range []string{"pass", "key"} {
		if k == w || strings.HasPrefix(k, w+"_") || strings.HasPrefix(k, w+"-") || strings.HasPrefix(k, w+".") ||
			strings.HasSuffix(k, "_"+w) || strings.HasSuffix(k, "-"+w) || strings.HasSuffix(k, "."+w) {
			return keyMedium
		}
	}
	return keyWeak
}

// maskKV decides whether the value of a secret-sounding key is masked.
func maskKV(key, sep, val string) bool {
	v := unquote(val)
	if v == "" || v == Mask {
		return false
	}
	switch strings.ToLower(v) {
	case "bearer", "basic", "token": // the credential after it is masked on its own
		return false
	}
	class := classify(key)
	k := strings.ToLower(key)
	alwaysSecret := strings.Contains(k, "passw") || strings.Contains(k, "pwd") || strings.Contains(k, "secret")
	if allDigits(v) && len(v) <= 6 && !alwaysSecret {
		return false // max_tokens=4096, session 3
	}
	switch {
	case strings.Trim(sep, " \t") == "" && strings.HasPrefix(key, "-"): // --password v
		return class != keyWeak
	case strings.Trim(sep, " \t") == "": // "password hunter2", not "password is wrong"
		return class == keyStrong && credentialLike(v) && !hexOrID.MatchString(v)
	case strings.Contains(sep, "="):
		return class != keyWeak || credentialLike(v)
	default: // ':'
		return class == keyStrong || credentialLike(v)
	}
}

func unquote(v string) string {
	if len(v) >= 4 && strings.HasPrefix(v, `\"`) && strings.HasSuffix(v, `\"`) {
		return v[2 : len(v)-2]
	}
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}

// credentialLike reports whether a word looks like a password or token
// rather than prose: letters with digits or symbols, or random-looking.
func credentialLike(v string) bool {
	if strings.ContainsAny(v, " \t") {
		return false
	}
	if len(v) >= 6 && hasLetter(v) && (hasDigit(v) || hasSymbol(v)) {
		return true
	}
	return len(v) >= 16 && entropy(v) > 3.5
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

// maskRandom masks a long run of token characters that looks random. A
// run with slashes is a path unless it is random as a whole; then only its
// random segments are masked.
func maskRandom(m string) string {
	if !strings.Contains(m, "/") {
		if len(m) >= 32 && randomRun(m, 4.0) {
			return Mask
		}
		return m
	}
	segs := strings.Split(m, "/")
	wordy := false
	for _, seg := range segs {
		if len(seg) >= 3 && allLower(seg) {
			wordy = true
		}
	}
	if whole := strings.ReplaceAll(m, "/", ""); !wordy && len(m) >= 32 && mixedCase(whole) && entropy(m) > 4.0 && !identifierLike(whole) {
		return Mask
	}
	changed := false
	for i, seg := range segs {
		if len(seg) >= 16 && randomRun(seg, 3.5) {
			segs[i], changed = Mask, true
		}
	}
	if !changed {
		return m
	}
	return strings.Join(segs, "/")
}

// randomRun reports whether s (no slashes) looks like a key: not a digest
// or UUID, letters and digits, not an identifier, entropy over min bits.
func randomRun(s string, min float64) bool {
	core := strings.TrimRight(s, "=")
	if hexOrID.MatchString(core) || !hasDigit(core) || !hasLetter(core) || identifierLike(core) {
		return false
	}
	return entropy(core) > min
}

// identifierLike reports whether s reads as words: CamelCase (most letters
// in lowercase runs of three or more, with capitals between) or words
// joined by '_' or '-'.
func identifierLike(s string) bool {
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == '_' || r == '-' })
	if len(parts) >= 3 {
		alpha := 0
		for _, p := range parts {
			if isAlpha(p) {
				alpha++
			}
		}
		if alpha*4 >= len(parts)*3 {
			return true
		}
	}
	if !hasUpper(s) {
		return false
	}
	inRuns, run := 0, 0
	for i := 0; i <= len(s); i++ {
		if i < len(s) && s[i] >= 'a' && s[i] <= 'z' {
			run++
			continue
		}
		if run >= 3 {
			inRuns += run
		}
		run = 0
	}
	return inRuns*2 >= len(s)
}

// mixedCase reports whether at least 15% of s is upper and 15% lower case.
func mixedCase(s string) bool {
	var up, low int
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 'A' && c <= 'Z':
			up++
		case c >= 'a' && c <= 'z':
			low++
		}
	}
	return up*100 >= len(s)*15 && low*100 >= len(s)*15
}

func hasDigit(s string) bool { return strings.ContainsAny(s, "0123456789") }

func hasUpper(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r >= 'A' && r <= 'Z' }) >= 0
}

func hasLetter(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' }) >= 0
}

func hasSymbol(s string) bool { return strings.ContainsAny(s, "!#$%&*+-./:;<=>?@^_~|") }

func allDigits(s string) bool {
	return s != "" && strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }) < 0
}

func allLower(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r < 'a' || r > 'z' }) < 0
}

func isAlpha(s string) bool {
	return s != "" && strings.IndexFunc(s, func(r rune) bool { return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') }) < 0
}

// entropy is the Shannon entropy of s in bits per byte.
func entropy(s string) float64 {
	if s == "" {
		return 0
	}
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
