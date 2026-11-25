package task

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestSlug(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Fix login bug", "fix-login-bug"},
		{"  --Fix   the LOGIN!! bug--  ", "fix-the-login-bug"},
		{"Fix Ünïcode — login", "fix-n-code-login"},
		{"", "task"},
		{"!!!", "task"},
		{"../../etc/passwd", "etc-passwd"},
		{"-rf", "rf"},
		{strings.Repeat("abcdefghij ", 10), "abcdefghij-abcdefghij-abcdefghij-abcdefg"},
	}
	for _, c := range cases {
		got := Slug(c.in)
		if got != c.want {
			t.Errorf("Slug(%q) = %q, want %q", c.in, got, c.want)
		}
		if n := utf8.RuneCountInString(got); n > 40 || n == 0 {
			t.Errorf("Slug(%q) has %d runes", c.in, n)
		}
		if !ValidSlug(got) {
			t.Errorf("Slug(%q) = %q is not a valid slug", c.in, got)
		}
	}
	// A title with no Latin letters or digits gets a stable short hash.
	ru := Slug("Починить логин")
	if !strings.HasPrefix(ru, "task-") || len(ru) != len("task-")+8 || ru != Slug("Починить логин") || ru == Slug("Другое") || !ValidSlug(ru) {
		t.Errorf("non-Latin slug %q", ru)
	}
	// A cut that ends on a separator does not leave a trailing "-".
	if got := Slug(strings.Repeat("a", 39) + " b"); got != strings.Repeat("a", 39) {
		t.Errorf("trailing separator kept: %q", got)
	}
}

func TestBranchCollisionSuffix(t *testing.T) {
	taken := map[string]bool{"task/fix-login": true, "task/fix-login-2": true}
	slug, branch := UniqueBranch("task", "fix-login", func(b, s string) bool { return taken[b] })
	if slug != "fix-login-3" || branch != "task/fix-login-3" {
		t.Fatalf("got %q %q", slug, branch)
	}
	slug, branch = UniqueBranch("task", "new", func(string, string) bool { return false })
	if slug != "new" || branch != "task/new" {
		t.Fatalf("got %q %q", slug, branch)
	}
	// A taken slug (worktree dir, other task) counts like a taken branch.
	slug, _ = UniqueBranch("t", "x", func(_, s string) bool { return s == "x" })
	if slug != "x-2" {
		t.Fatalf("slug %q", slug)
	}
	// The suffix keeps the slug within 40 runes.
	long := strings.Repeat("a", 40)
	slug, _ = UniqueBranch("task", long, func(b, _ string) bool { return b == "task/"+long })
	if slug != strings.Repeat("a", 38)+"-2" {
		t.Fatalf("long slug %q", slug)
	}
}

func TestValidBranchPrefixAndRef(t *testing.T) {
	for _, p := range []string{"task", "agents/task", "t_1"} {
		if !ValidBranchPrefix(p) {
			t.Errorf("prefix %q rejected", p)
		}
	}
	for _, p := range []string{"", "-x", "a//b", "a/../b", "a..b", "/a", "a/", "a b", "a.lock", "a~1", "a:b"} {
		if ValidBranchPrefix(p) {
			t.Errorf("prefix %q accepted", p)
		}
	}
	for _, r := range []string{"main", "origin/main", "v1.2", "HEAD~2", "abc123"} {
		if err := CheckRef(r); err != nil {
			t.Errorf("ref %q: %v", r, err)
		}
	}
	for _, r := range []string{"", "-x", "--upload-pack=x", "a b", "a\nb", "a..b"} {
		if CheckRef(r) == nil {
			t.Errorf("ref %q accepted", r)
		}
	}
}

func TestTaskEncodeDecode(t *testing.T) {
	tk := Task{ID: "01JTEST0000000000000000000", Title: "Fix \"login\"", Slug: "fix-login", Project: "demo",
		Repos:     []Repo{{Repo: "/home/u/src/demo", Base: "main", BaseCommit: strings.Repeat("ab", 20), Branch: "task/fix-login", Worktree: "/home/u/wt/demo/fix-login"}},
		Isolation: IsolationWorktree, Status: StatusOpen, CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	b, err := tk.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(b)
	if err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	if got.Title != tk.Title || got.Repos[0] != tk.Repos[0] || !got.CreatedAt.Equal(tk.CreatedAt) {
		t.Fatalf("round trip %+v", got)
	}
	bad := []string{
		strings.Replace(string(b), `status = 'open'`, `status = 'lost'`, 1),
		strings.Replace(string(b), `slug = 'fix-login'`, `slug = '../x'`, 1),
		strings.Replace(string(b), `branch = 'task/fix-login'`, `branch = '-x'`, 1),
		strings.Replace(string(b), `worktree = '/home/u/wt/demo/fix-login'`, `worktree = 'rel/x'`, 1),
		string(b) + "\nunknown = 1\n",
	}
	for _, s := range bad[:4] {
		if s == string(b) {
			t.Fatal("replacement did not apply")
		}
	}
	for _, s := range bad {
		if _, err := Decode([]byte(s)); err == nil {
			t.Errorf("accepted:\n%s", s)
		}
	}
}

func TestTaskProfileValidated(t *testing.T) {
	tk := Task{ID: "01JTEST0000000000000000000", Title: "x", Slug: "x", Project: "demo", Profile: "careful",
		Repos:     []Repo{{Repo: "/home/u/src/demo", Branch: "task/x"}},
		Isolation: IsolationHere, Status: StatusOpen}
	if err := tk.Validate(); err != nil {
		t.Fatal(err)
	}
	tk.Profile = "-x"
	if err := tk.Validate(); err == nil || !strings.Contains(err.Error(), "profile") {
		t.Fatalf("a bad profile name must be refused: %v", err)
	}
}

func TestTaskTitleRejectsC1AndBidi(t *testing.T) {
	base := Task{ID: "01JTEST0000000000000000000", Slug: "x", Project: "demo",
		Repos:     []Repo{{Repo: "/home/u/src/demo", Branch: "task/x"}},
		Isolation: IsolationHere, Status: StatusOpen}
	for _, title := range []string{"a\u009b31mb", "a\u202eb", "a\u2066b", "a\u200fb", "a\u061cb"} {
		tk := base
		tk.Title = title
		if err := tk.Validate(); err == nil {
			t.Errorf("title %q accepted", title)
		}
		if ValidTitle(title) {
			t.Errorf("ValidTitle(%q)", title)
		}
	}
	if !ValidTitle("Fix «login» — ü") {
		t.Fatal("ordinary text is a valid title")
	}
}
