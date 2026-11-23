package task

import (
	"fmt"
	"regexp"
	"strings"
)

// MaxRecordWords is the longest finish record the gate accepts.
const MaxRecordWords = 120

// MinRecordRefs is the fewest distinct concrete references it needs.
const MinRecordRefs = 2

// DefaultBanned is the filler the gate always rejects (case-insensitive,
// as a substring). Config tasks.gate_banned adds to it.
var DefaultBanned = []string{"robust", "seamless", "leverage", "cutting-edge", "state-of-the-art", "best practices"}

// Check is one line of the quality gate.
type Check struct {
	OK   bool
	Text string
}

var (
	// refRe ports the references of the prototype gate: a path-like name
	// (letters and digits of any script), a 7-hex commit, go test, pnpm,
	// make <target>.
	refRe  = regexp.MustCompile(`[\p{L}\p{N}_.-]+/[\p{L}\p{N}_./-]+|\b[0-9a-f]{7}\b|go test|pnpm|make \w+`)
	todoRe = regexp.MustCompile(`\bTODO\b`)
	digit  = regexp.MustCompile(`[0-9]`)
)

// refs returns the distinct concrete references of text, in order. A
// 7-hex word needs a digit, so that "defaced" is not a commit.
func refs(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range refRe.FindAllString(text, -1) {
		if len(m) == 7 && !strings.ContainsAny(m, "/ ") && !digit.MatchString(m) {
			continue
		}
		if k := strings.ToLower(m); !seen[k] {
			seen[k] = true
			out = append(out, m)
		}
	}
	return out
}

// Gate runs the three checks of a finish record: at most 120 words, at
// least 2 distinct concrete references, no filler phrase (DefaultBanned
// and extra) and no TODO marker. Words are runs of non-space characters of
// any script. Blank entries of extra are ignored: they would match all.
func Gate(text string, extra ...string) []Check {
	n := len(strings.Fields(text))
	nref := len(refs(text))
	low := strings.ToLower(text)
	var bad []string
	for _, b := range append(append([]string(nil), DefaultBanned...), extra...) {
		if b = strings.ToLower(strings.TrimSpace(b)); b != "" && strings.Contains(low, b) {
			bad = append(bad, b)
		}
	}
	if k := len(todoRe.FindAllString(text, -1)); k > 0 {
		bad = append(bad, fmt.Sprintf("%d TODO to fill in", k))
	}
	third := Check{OK: true, Text: "No filler phrases"}
	if len(bad) > 0 {
		third = Check{Text: "Filler phrases: " + strings.Join(bad, ", ")}
	}
	return []Check{
		{n <= MaxRecordWords, fmt.Sprintf("Length %d of %d words", n, MaxRecordWords)},
		{nref >= MinRecordRefs, fmt.Sprintf("Concrete references (files, commits, commands): %d, need %d", nref, MinRecordRefs)},
		third,
	}
}
