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
		// Cases the doctor's own patterns masked before (review I1).
		{"doctor auth", "error: auth=hunter2x failed", "error: auth=" + r + " failed"},
		{"doctor session", "session=s3ssion99 expired", "session=" + r + " expired"},
		{"doctor authorization", "AUTHORIZATION=abc123def", "AUTHORIZATION=" + r},
		{"doctor x-auth", "x-auth: abc123", "x-auth: " + r},
		{"doctor oauth", "oauth_verifier: v3rifier99", "oauth_verifier: " + r},
		{"doctor password space", "password hunter2", "password " + r},
		{"doctor cookie", "Cookie: sessionid=abc123def456", "Cookie: " + r},
		{"doctor api_key", "api_key=hunter2hunter2 next", "api_key=" + r + " next"},
		{"doctor credential", "credentials: c0rrect-h0rse", "credentials: " + r},
		// Shapes the review probe found leaking (M1).
		{"escaped json", `{\"api_key\":\"hunter2hunter\",\"password\":\"p4ss\"}`, `{\"api_key\":` + r + `,\"password\":` + r + `}`},
		{"flag password", "run --password hunter2secret now", "run --password " + r + " now"},
		{"flag password letters", "--password hunter", "--password " + r},
		{"flag api key eq", "--api-key=abc", "--api-key=" + r},
		{"mysql -p", "mysql -u root -pS3cretPw! db", "mysql -u root -p" + r + " db"},
		{"curl -u", "curl -u admin:S3cretPw https://x", "curl -u admin:" + r + " https://x"},
		{"yaml next line", "db:\n  password:\n    hunter2secret\n  host: x", "db:\n  password:\n    " + r + "\n  host: x"},
		{"at in url password", "postgres://user:p@ss:w0rd@db.example.com/app", "postgres://" + r + "@db.example.com/app"},
		{"aws secret kv", "aws_secret_access_key = wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "aws_secret_access_key = " + r},
		{"aws secret bare", "it is wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY ok", "it is " + r + " ok"},
		{"aws sts", "ASIAIOSFODNN7EXAMPLE", r},
		{"stripe", "STRIPE_SK=sk_" + "test_" + "4eC39HqLyjWDarjtT1zdp7dc", "STRIPE_SK=" + r},
		{"stripe bare", "use sk_live_51HxYzAbCdEf now", "use " + r + " now"},
		{"huggingface", "hf_AbCdEfGhIjKlMnOpQrStUv", r},
		{"npm", "npm_AbCdEfGhIjKlMnOpQrStUv01", r},
		{"slack app", "xapp-1-A012345-abcdef", r},
		{"google oauth", "ya29.a0AfH6SMBabcdefghijklmnop", r},
		{"webhook path", "discord.com/api/webhooks/123456789012345678/AbCdEfGh1234IjKlMnOp5678QrStUvWx", "discord.com/api/webhooks/123456789012345678/" + r},
		// Prose and identifiers stay searchable (M2).
		{"basic prose", "This is basic stuff", "This is basic stuff"},
		{"key prose", "The key: use the cache", "The key: use the cache"},
		{"key markdown", "**Key:** important", "**Key:** important"},
		{"monkey", "monkey=3", "monkey=3"},
		{"max tokens", "max_tokens=4096", "max_tokens=4096"},
		{"keyboard", "keyboard: qwerty", "keyboard: qwerty"},
		{"author", "author: Jane Doe", "author: Jane Doe"},
		{"password prose", "the password is wrong", "the password is wrong"},
		{"token prose", "token budget exceeded", "token budget exceeded"},
		{"camel identifier", "MyVeryLongGeneratedIdentifierName2024Version3", "MyVeryLongGeneratedIdentifierName2024Version3"},
		{"snake identifier", "test_index_never_contains_secrets_2024_v3", "test_index_never_contains_secrets_2024_v3"},
		{"mkdir -p", "mkdir -p build/out", "mkdir -p build/out"},
		{"ssh port", "ssh -p2222 host", "ssh -p2222 host"},
		{"mailto in url", "https://example.com/x?to=a@b.c", "https://example.com/x?to=a@b.c"},
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
