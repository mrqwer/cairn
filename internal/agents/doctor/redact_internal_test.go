package doctor

import "testing"

func TestRedactKeepsOrdinaryText(t *testing.T) {
	for in, want := range map[string]string{
		"2.1.330 (Claude Code)": "2.1.330 (Claude Code)",
		"error: no such file /usr/lib/node_modules/codex/bin/run.js": "error: no such file /usr/lib/node_modules/codex/bin/run.js",
		"api_key=hunter2hunter2 next":                                "api_key=[redacted] next",
		"Authorization: Bearer abc.def":                              "Authorization: [redacted] [redacted]",
		"https://user:pw@example.com/x":                              "https://[redacted]@example.com/x",
		"token sk-ant-abcdefghijk":                                   "token [redacted]",
	} {
		if got := redact(in); got != want {
			t.Errorf("redact(%q) = %q, want %q", in, got, want)
		}
	}
}
