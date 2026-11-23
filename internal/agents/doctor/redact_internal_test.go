package doctor

import (
	"testing"

	"github.com/mrqwer/cairn/internal/agents/index"
)

// Probe details go through the index redactor, the one set of secret
// patterns, and keep ordinary text. The rows are the doctor's earlier
// regression cases (TestRedactKeepsOrdinaryText) plus the keys its own
// patterns used to cover: auth*, session, and "password value".
func TestRedactKeepsOrdinaryText(t *testing.T) {
	for in, want := range map[string]string{
		"2.1.330 (Claude Code)": "2.1.330 (Claude Code)",
		"error: no such file /usr/lib/node_modules/codex/bin/run.js": "error: no such file /usr/lib/node_modules/codex/bin/run.js",
		"api_key=hunter2hunter2 next":                                "api_key=[redacted] next",
		"Authorization: Bearer abc.def":                              "Authorization: Bearer [redacted]",
		"https://user:pw@example.com/x":                              "https://[redacted]@example.com/x",
		"token sk-ant-abcdefghijk":                                   "token [redacted]",
		"denied for AKIAIOSFODNN7EXAMPLE":                            "denied for [redacted]",
		"error: auth=hunter2x failed":                                "error: auth=[redacted] failed",
		"session=s3ssion99 expired":                                  "session=[redacted] expired",
		"AUTHORIZATION=abc123def":                                    "AUTHORIZATION=[redacted]",
		"login failed: password hunter2":                             "login failed: password [redacted]",
	} {
		if got := firstLine(in); got != want {
			t.Errorf("firstLine(%q) = %q, want %q", in, got, want)
		}
		if got := index.Redact(in); got != want {
			t.Errorf("index.Redact(%q) = %q, want %q", in, got, want)
		}
	}
}
