package index_test

import (
	"strings"
	"testing"

	"github.com/mrqwer/cairn/internal/agents/index"
)

func TestRedactTable(t *testing.T) {
	const r = "[redacted]"
	for _, tc := range []struct{ name, in, want string }{
		{"openai key", "use sk-proj-AbCdEf0123456789xyz now", "use " + r + " now"},
		{"anthropic key", "key sk-ant-api03-abcdefghijklmnop", "key " + r},
		{"github token", "push with ghp_0123456789abcdefABCDEF0123456789abcd ok", "push with " + r + " ok"},
		{"github pat", "github_pat_11ABCDEFG0123456789_abcdefghijk", r},
		{"aws access key", "id AKIAIOSFODNN7EXAMPLE end", "id " + r + " end"},
		{"slack bot", "xoxb-1234567890-abcdefghij", r},
		{"slack user", "token is xoxp-1234567890-abcdefghij.", "token is " + r + "."},
		{"bearer", "Authorization: Bearer abc.def-ghi", "Authorization: Bearer " + r},
		{"basic auth", "Authorization: Basic dXNlcjpwYXNzd29yZA==", "Authorization: Basic " + r},
		{"url userinfo", "clone https://user:pw@example.com/x", "clone https://" + r + "@example.com/x"},
		{"env assignment", "API_KEY=abc", "API_KEY=" + r},
		{"export assignment", "export DB_PASSWORD='p a s s'", "export DB_PASSWORD=" + r},
		{"secret colon", "client_secret: s3cr3t-value", "client_secret: " + r},
		{"json field", `{"auth_token": "abc123", "n": 1}`, `{"auth_token": ` + r + `, "n": 1}`},
		{"token lowercase", "token=xyz", "token=" + r},
		{"pass", "PASS=hunter2", "PASS=" + r},
		{"private key", "before\n-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA\nabc\n-----END RSA PRIVATE KEY-----\nafter", "before\n" + r + "\nafter"},
		{"openssh key", "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA\n-----END OPENSSH PRIVATE KEY-----", r},
		{"truncated private key", "x -----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBgkqhkiG9w0BAQEFAASC", "x " + r},
		{"high entropy", "value Zx9Qp2Lm7Vb4Nc8Rt1Ws5Yk3Hd6Jf0Ga end", "value " + r + " end"},
		{"high entropy base64", "q8Fz/2KbW0+Lr7XyN3vTm5Ap9Hd1Cs6Ej4Ug==", r},
		{"normal sentence", "Please refactor the login handler and add tests.", "Please refactor the login handler and add tests."},
		{"git sha", "commit 3f786850e387550fdab836ed7e6dc881de23001b fixes it", "commit 3f786850e387550fdab836ed7e6dc881de23001b fixes it"},
		{"uuid", "session 0b9e3c1a-5d2f-4e8a-9c71-2f3a4b5c6d7e", "session 0b9e3c1a-5d2f-4e8a-9c71-2f3a4b5c6d7e"},
		{"long path", "edit /home/u/projects/demo/internal/agents/index/redact.go", "edit /home/u/projects/demo/internal/agents/index/redact.go"},
		{"long identifier", "TestIndexNeverContainsSecretsAcrossFiles failed", "TestIndexNeverContainsSecretsAcrossFiles failed"},
		{"version", "2.1.330 (Claude Code)", "2.1.330 (Claude Code)"},
		{"skip-like word", "the task-list is long", "the task-list is long"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := index.Redact(tc.in); got != tc.want {
				t.Errorf("Redact(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRedactIdempotent(t *testing.T) {
	in := "API_KEY=sk-ant-abcdefghijklmnop and Bearer xyz and Zx9Qp2Lm7Vb4Nc8Rt1Ws5Yk3Hd6Jf0Ga"
	once := index.Redact(in)
	if twice := index.Redact(once); twice != once {
		t.Fatalf("not idempotent:\n%q\n%q", once, twice)
	}
}

func TestRedactDeclaredFields(t *testing.T) {
	in := `otp: 123456 and "pin":"9876" and other=1`
	got := index.RedactFields(in, []string{"otp", "pin"})
	if strings.Contains(got, "123456") || strings.Contains(got, "9876") {
		t.Fatalf("declared field leaked: %q", got)
	}
	if !strings.Contains(got, "other=1") {
		t.Fatalf("undeclared field masked: %q", got)
	}
	if got := index.Redact(in); !strings.Contains(got, "123456") {
		t.Fatalf("Redact without fields masked otp: %q", got)
	}
}
