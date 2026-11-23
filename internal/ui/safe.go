package ui

import (
	"strings"
	"unicode/utf8"
)

// SafeText makes untrusted text (transcripts, file names, tool output)
// safe to print to a terminal: escape sequences (CSI, OSC such as the
// clipboard-writing OSC 52, and their 8-bit C1 forms) are removed whole,
// every other C0 and C1 control character, DEL and the bidi overrides are
// dropped, and a run of invalid UTF-8 becomes one U+FFFD. Newlines and tabs are kept.
func SafeText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	bad := false // a run of invalid bytes becomes one U+FFFD
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size <= 1 {
			if !bad {
				b.WriteRune(utf8.RuneError)
			}
			bad = true
			i++
			continue
		}
		bad = false
		switch {
		case r == 0x1b:
			i += escLen(s[i+1:]) + 1
			continue
		case r == 0x9b: // C1 CSI
			i += size + csiLen(s[i+size:])
			continue
		case r == 0x9d || r == 0x90 || r == 0x9e || r == 0x9f: // C1 OSC, DCS, PM, APC
			i += size + stringLen(s[i+size:])
			continue
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
		case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069, r == 0x200e, r == 0x200f:
		default:
			b.WriteRune(r)
		}
		i += size
	}
	return b.String()
}

// SafeLine is SafeText on one line: newlines and tabs become spaces and
// runs of blanks collapse.
func SafeLine(s string) string { return strings.Join(strings.Fields(SafeText(s)), " ") }

// escLen is the length of the escape sequence after an ESC byte.
func escLen(s string) int {
	if s == "" {
		return 0
	}
	switch s[0] {
	case '[':
		return 1 + csiLen(s[1:])
	case ']', 'P', '^', '_', 'X':
		return 1 + stringLen(s[1:])
	}
	if s[0] >= 0x20 && s[0] <= 0x2f { // intermediate bytes, then a final byte
		n := 0
		for n < len(s) && s[n] >= 0x20 && s[n] <= 0x2f {
			n++
		}
		if n < len(s) {
			n++
		}
		return n
	}
	return 1
}

// csiLen is the length of CSI parameters and the final byte.
func csiLen(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x40 && s[i] <= 0x7e {
			return i + 1
		}
		if s[i] < 0x20 || s[i] > 0x3f && s[i] < 0x40 {
			return i
		}
	}
	return len(s)
}

// stringLen is the length of an OSC/DCS string up to and including its
// terminator: BEL, ESC \ or the C1 ST.
func stringLen(s string) int {
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == 0x07:
			return i + 1
		case s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\':
			return i + 2
		case strings.HasPrefix(s[i:], "\u009c"):
			return i + len("\u009c")
		}
	}
	return len(s)
}
