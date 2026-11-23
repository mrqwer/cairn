package ui

import "testing"

func TestSafeLineStripsControls(t *testing.T) {
	for in, want := range map[string]string{
		"plain text":                         "plain text",
		"osc52 \x1b]52;c;ZXZpbA==\x07 after": "osc52 after",
		"title \x1b]0;pwned\x1b\\ after":     "title after",
		"\x1b[31mred\x1b[0m and \x1b[2J":     "red and",
		"c1 csi \u009b31mx":                  "c1 csi x",
		"c1 osc \u009d0;t\u009c y":           "c1 osc y",
		"bell\x07 nul\x00 del\x7f":           "bell nul del",
		"tab\tnew\nline\r\n":                 "tab new line",
		"bidi \u202eevil\u202c":              "bidi evil",
		"Привет мир":                         "Привет мир",
		"bad utf8 \xff\xfe ok":               "bad utf8 � ok",
	} {
		if got := SafeLine(in); got != want {
			t.Errorf("SafeLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSafeTextKeepsNewlines(t *testing.T) {
	if got := SafeText("a\x1b[1m b\n\tc\x1b]52;c;x\x07\r\n"); got != "a b\n\tc\n" {
		t.Fatalf("SafeText = %q", got)
	}
}
