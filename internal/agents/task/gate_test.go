package task

import (
	"os"
	"strings"
	"testing"
)

func words(n int) string { return strings.TrimSpace(strings.Repeat("word ", n)) }

func allOK(cs []Check) bool {
	for _, c := range cs {
		if !c.OK {
			return false
		}
	}
	return true
}

func TestGate(t *testing.T) {
	refs := " internal/db/pool.go go test"
	cases := []struct {
		name  string
		text  string
		extra []string
		ok    bool
		fail  int // index of the failing check, -1 for none
	}{
		{"120 words", words(117) + refs, nil, true, -1},
		{"121 words", words(118) + refs, nil, false, 0},
		{"one ref", "fixed internal/db/pool.go", nil, false, 1},
		{"two refs", "fixed internal/db/pool.go with go test", nil, true, -1},
		{"same ref with sentence period", "see internal/db/pool.go and internal/db/pool.go. ./internal/db/pool.go", nil, false, 1},
		{"Links line alone is no content", "Goal: x\nLinks: branch task/fix-login, commits 3f2a9c1..b81d0e4", nil, false, 1},
		{"Links line apart from content", "Changed: internal/db/pool.go\nLinks: branch task/fix-login, commit 3f2a9c1", nil, false, 1},
		{"TODOs plural", "TODOs left internal/db/pool.go go test", nil, false, 2},
		{"TODO_x", "TODO_x internal/db/pool.go go test", nil, false, 2},
		{"same ref twice", "internal/db/pool.go and internal/db/pool.go", nil, false, 1},
		{"robust", "a robust fix" + refs, nil, false, 2},
		{"Best Practices", "Follows Best Practices" + refs, nil, false, 2},
		{"TODO", "Decision: TODO" + refs, nil, false, 2},
		{"todo lower case is prose", "the todo list" + refs, nil, true, -1},
		{"extra banned", "A holistic fix" + refs, []string{"Holistic"}, false, 2},
		{"extra banned non-Latin", "Надёжный фикс" + refs, []string{"надёжный"}, false, 2},
		{"empty extra bans nothing", "plain fix" + refs, []string{"", "  "}, true, -1},
		{"non-Latin path counts", "правка docs/заметка.md и go test", nil, true, -1},
		{"non-Latin words counted", strings.Repeat("слово ", 119) + "go test x/y", nil, false, 0},
		{"unicode space separates words", strings.Repeat("a ", 121) + refs, nil, false, 0},
		{"7-hex word is not a commit", "defaced fix", nil, false, 1},
		{"commit and make", "3f2a9c1 make test", nil, true, -1},
	}
	for _, c := range cases {
		got := Gate(c.text, c.extra...)
		if len(got) != 3 {
			t.Fatalf("%s: %d checks", c.name, len(got))
		}
		if allOK(got) != c.ok {
			t.Errorf("%s: %+v", c.name, got)
		}
		for i, ch := range got {
			if want := i != c.fail; ch.OK != want {
				t.Errorf("%s: check %d OK=%v (%s)", c.name, i, ch.OK, ch.Text)
			}
		}
	}
}

func TestGatePrototypeSample(t *testing.T) {
	b, err := os.ReadFile("testdata/finish_sample.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got := Gate(string(b)); !allOK(got) {
		t.Fatalf("%+v", got)
	}
}

func TestGateTodoMessageIsSeparate(t *testing.T) {
	got := Gate("TODO and a robust fix internal/db/pool.go go test")[2].Text
	if !strings.Contains(got, "TODO markers left: 1") || !strings.Contains(got, "Filler phrases: robust") {
		t.Fatal(got)
	}
	if got := Gate("TODO internal/db/pool.go go test")[2].Text; strings.Contains(got, "Filler") {
		t.Fatal(got)
	}
}

func TestGateTexts(t *testing.T) {
	got := Gate("a robust fix, internal/db/pool.go")
	if !strings.Contains(got[0].Text, "of 120 words") || !strings.Contains(got[1].Text, "need 2") || !strings.Contains(got[2].Text, "robust") {
		t.Fatalf("%+v", got)
	}
}
