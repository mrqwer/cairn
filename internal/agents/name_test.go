package agents_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mrqwer/cairn/internal/agents"
)

func TestSuggestName(t *testing.T) {
	cases := []struct{ prompt, name, purpose string }{
		{"Please fix the login redirect loop on Safari. It started yesterday.", "fix login redirect loop safari", "fix the login redirect loop on Safari"},
		{"can you add a --json flag to ls?\nmore text", "add json flag ls", "add a --json flag to ls?"},
		{"   ", "", ""},
		{"Рефакторинг модуля оплаты", "рефакторинг модуля оплаты", "Рефакторинг модуля оплаты"},
	}
	for _, c := range cases {
		n, p := agents.SuggestName(c.prompt)
		if n != c.name || p != c.purpose {
			t.Errorf("SuggestName(%q) = %q, %q; want %q, %q", c.prompt, n, p, c.name, c.purpose)
		}
	}
}

func TestFirstPromptTolerant(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	data := "garbage\n" +
		`{"type":"user","message":{"role":"user","content":"<command-name>/clear</command-name>"}}` + "\n" +
		`{"role":"user","content":"first real prompt"}` + "\n" +
		`{"role":"user","content":"second"}` + "\n"
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := agents.FirstPrompt(p)
	if err != nil || got != "first real prompt" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := agents.FirstPrompt(filepath.Join(t.TempDir(), "none")); err == nil {
		t.Fatal("missing file accepted")
	}
}
