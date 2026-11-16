package history_test

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/history"
)

const cfgPath = "/home/u/.config/cairn/projects/demo.toml"

func newStore(t *testing.T) (history.Store, fsx.Root) {
	t.Helper()
	root := fsx.Root{Dir: t.TempDir()}
	return history.Store{
		Root:         root,
		Path:         "/home/u/.local/state/cairn/history.jsonl",
		AllowedRoots: []string{"/home/u/.config/cairn"},
	}, root
}

func write(t *testing.T, r fsx.Root, p, s string) {
	t.Helper()
	if err := r.WriteAtomic(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, r fsx.Root, p string) string {
	t.Helper()
	b, err := r.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func change(before, after string) history.FileChange {
	return history.FileChange{Path: cfgPath, Before: []byte(before), After: []byte(after), BeforeExists: true, AfterExists: true}
}

func TestAppendList(t *testing.T) {
	s, root := newStore(t)
	e1, err := s.Append("edit", "first", []history.FileChange{change("a\n", "b\n")})
	if err != nil {
		t.Fatal(err)
	}
	e2, err := s.Append("edit", "second", nil)
	if err != nil {
		t.Fatal(err)
	}
	if e1.ID != "000001" || e2.ID != "000002" {
		t.Fatalf("ids = %s %s", e1.ID, e2.ID)
	}
	if e1.TS.IsZero() || e1.TS.Location() != nil && e1.TS.Location().String() != "UTC" {
		t.Fatalf("ts not UTC: %v", e1.TS)
	}
	evs, err := s.List(0)
	if err != nil || len(evs) != 2 {
		t.Fatalf("list = %v %v", evs, err)
	}
	if evs[0].ID != "000002" || evs[1].ID != "000001" {
		t.Fatalf("not newest first: %v", evs)
	}
	if !bytes.Equal(evs[1].Files[0].After, []byte("b\n")) || evs[1].Files[0].Path != cfgPath {
		t.Fatalf("snapshot lost: %+v", evs[1].Files)
	}
	if got, _ := s.List(1); len(got) != 1 || got[0].ID != "000002" {
		t.Fatalf("limit: %v", got)
	}
	g, err := s.Get("000001")
	if err != nil || g.Summary != "first" {
		t.Fatalf("get = %+v %v", g, err)
	}
	if _, err := s.Get("999999"); err == nil {
		t.Fatal("want error for unknown id")
	}
	st, err := os.Stat(root.Path(s.Path))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", st.Mode().Perm())
	}
	raw := read(t, root, s.Path)
	for _, k := range []string{`"id":"000001"`, `"ts":"`, `"before_exists":true`, `"after_exists":true`, `"before":"YQo="`} {
		if !strings.Contains(raw, k) {
			t.Errorf("missing %s in %s", k, raw)
		}
	}
}

func TestListMissingFile(t *testing.T) {
	s, _ := newStore(t)
	evs, err := s.List(0)
	if err != nil || len(evs) != 0 {
		t.Fatalf("%v %v", evs, err)
	}
}

func TestTornLineThenAppend(t *testing.T) {
	s, root := newStore(t)
	if _, err := s.Append("edit", "one", nil); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(root.Path(s.Path), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"id":"000002","ts":"2026-`)
	_ = f.Close()
	e, err := s.Append("edit", "two", nil)
	if err != nil {
		t.Fatal(err)
	}
	if e.ID != "000002" {
		t.Fatalf("torn line must not count: id %s", e.ID)
	}
	evs, _ := s.List(0)
	if len(evs) != 2 || evs[0].Summary != "two" || evs[1].Summary != "one" {
		t.Fatalf("list = %+v", evs)
	}
}

func TestConcurrentAppendUniqueIDs(t *testing.T) {
	s, _ := newStore(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Append("edit", "x", nil); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	evs, _ := s.List(0)
	seen := map[string]bool{}
	for _, e := range evs {
		if seen[e.ID] {
			t.Fatalf("duplicate id %s", e.ID)
		}
		seen[e.ID] = true
	}
	if len(evs) != 20 {
		t.Fatalf("len = %d", len(evs))
	}
}

func TestUndoRestoresBeforeAndRecords(t *testing.T) {
	s, root := newStore(t)
	write(t, root, cfgPath, "new\n")
	e, _ := s.Append("edit", "change", []history.FileChange{change("old\n", "new\n")})
	u, err := s.Undo(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, root, cfgPath); got != "old\n" {
		t.Fatalf("content = %q", got)
	}
	if u.Kind != "undo" || len(u.Files) != 1 {
		t.Fatalf("undo event = %+v", u)
	}
	f := u.Files[0]
	if string(f.Before) != "new\n" || string(f.After) != "old\n" {
		t.Fatalf("reversed change = %+v", f)
	}
	// the undo itself is undoable
	if _, err := s.Undo(u.ID); err != nil {
		t.Fatal(err)
	}
	if got := read(t, root, cfgPath); got != "new\n" {
		t.Fatalf("redo content = %q", got)
	}
}

func TestUndoRemovesCreatedFile(t *testing.T) {
	s, root := newStore(t)
	write(t, root, cfgPath, "x\n")
	e, _ := s.Append("create", "c", []history.FileChange{{Path: cfgPath, After: []byte("x\n"), AfterExists: true}})
	if _, err := s.Undo(e.ID); err != nil {
		t.Fatal(err)
	}
	if root.Exists(cfgPath) {
		t.Fatal("file should be removed")
	}
}

func TestUndoRecreatesRemovedFile(t *testing.T) {
	s, root := newStore(t)
	e, _ := s.Append("rm", "r", []history.FileChange{{Path: cfgPath, Before: []byte("x\n"), BeforeExists: true}})
	if _, err := s.Undo(e.ID); err != nil {
		t.Fatal(err)
	}
	if read(t, root, cfgPath) != "x\n" {
		t.Fatal("not recreated")
	}
}

func TestUndoConflictWhenFileChangedSince(t *testing.T) {
	s, root := newStore(t)
	other := "/home/u/.config/cairn/projects/web.toml"
	write(t, root, cfgPath, "edited later\n")
	write(t, root, other, "b\n")
	e, _ := s.Append("edit", "c", []history.FileChange{
		change("old\n", "new\n"),
		{Path: other, Before: []byte("a\n"), After: []byte("b\n"), BeforeExists: true, AfterExists: true},
	})
	_, err := s.Undo(e.ID)
	if !errors.Is(err, history.ErrConflict) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), cfgPath) || strings.Contains(err.Error(), other) {
		t.Fatalf("error should name only the conflicting path: %v", err)
	}
	if read(t, root, cfgPath) != "edited later\n" || read(t, root, other) != "b\n" {
		t.Fatal("nothing may be written on conflict")
	}
	if evs, _ := s.List(0); len(evs) != 1 {
		t.Fatal("no undo event on conflict")
	}
	// absence conflict
	root.Remove(cfgPath) //nolint:errcheck
	if _, err := s.Undo(e.ID); !errors.Is(err, history.ErrConflict) {
		t.Fatalf("absent file vs AfterExists: %v", err)
	}
}

func TestRestore(t *testing.T) {
	s, root := newStore(t)
	write(t, root, cfgPath, "later\n")
	e, _ := s.Append("edit", "c", []history.FileChange{change("old\n", "new\n")})
	r, err := s.Restore(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if read(t, root, cfgPath) != "new\n" {
		t.Fatal("not restored")
	}
	if r.Kind != "restore" || string(r.Files[0].Before) != "later\n" || string(r.Files[0].After) != "new\n" {
		t.Fatalf("restore event = %+v", r)
	}
	// restore of an event whose file did not exist after: removes
	e2, _ := s.Append("rm", "r", []history.FileChange{{Path: cfgPath, Before: []byte("new\n"), BeforeExists: true}})
	if _, err := s.Restore(e2.ID); err != nil {
		t.Fatal(err)
	}
	if root.Exists(cfgPath) {
		t.Fatal("should be removed")
	}
}

func TestPathsOutsideAllowedRootsRefused(t *testing.T) {
	for _, bad := range []string{
		"/etc/passwd",
		"/home/u/.config/cairn/../x",
		"/home/u/.config/cairn-evil/x",
		"relative/path",
	} {
		s, root := newStore(t)
		write(t, root, "/etc/passwd", "root\n")
		write(t, root, "/home/u/.config/x", "x\n")
		fc := history.FileChange{Path: bad, Before: []byte("pwn\n"), After: []byte("pwn\n"), BeforeExists: true, AfterExists: true}
		// craft the event file directly, bypassing Append
		line := `{"id":"000001","ts":"2026-01-01T00:00:00Z","kind":"edit","summary":"x","files":[{"path":"` + fc.Path +
			`","before":"cHduCg==","after":"cHduCg==","before_exists":true,"after_exists":true}]}` + "\n"
		if err := root.WriteAtomic(s.Path, []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}
		for name, fn := range map[string]func(string) (history.Event, error){"undo": s.Undo, "restore": s.Restore} {
			_, err := fn("000001")
			if err == nil || !strings.Contains(err.Error(), "outside") {
				t.Errorf("%s %q: err = %v", name, bad, err)
			}
		}
		if got := read(t, root, "/etc/passwd"); got != "root\n" {
			t.Errorf("%q: /etc/passwd modified: %q", bad, got)
		}
		if _, err := os.Stat(root.Path("/home/u/.config/x")); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
		if read(t, root, "/home/u/.config/x") != "x\n" {
			t.Errorf("%q: sibling modified", bad)
		}
	}
}

func TestOutsideRootRefusesWholeEventBeforeWriting(t *testing.T) {
	s, root := newStore(t)
	write(t, root, cfgPath, "new\n")
	line := `{"id":"000001","ts":"2026-01-01T00:00:00Z","kind":"edit","summary":"x","files":[` +
		`{"path":"` + cfgPath + `","before":"b2xkCg==","after":"bmV3Cg==","before_exists":true,"after_exists":true},` +
		`{"path":"/etc/passwd","before":"eAo=","after":"eAo=","before_exists":true,"after_exists":true}]}` + "\n"
	if err := root.WriteAtomic(s.Path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Restore("000001"); err == nil {
		t.Fatal("want error")
	}
	if read(t, root, cfgPath) != "new\n" {
		t.Fatal("first file written despite refusal")
	}
}

func TestNoAllowedRootsRefusesAll(t *testing.T) {
	s, root := newStore(t)
	s.AllowedRoots = nil
	write(t, root, cfgPath, "new\n")
	e, _ := s.Append("edit", "c", []history.FileChange{change("old\n", "new\n")})
	if _, err := s.Undo(e.ID); err == nil {
		t.Fatal("want refusal")
	}
}

func TestExistingHistoryModeNotChanged(t *testing.T) {
	s, root := newStore(t)
	if err := root.WriteAtomic(s.Path, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append("edit", "x", nil); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(root.Path(s.Path))
	if st.Mode().Perm() != 0o640 {
		t.Fatalf("mode changed: %v", st.Mode().Perm())
	}
}

func TestDiff(t *testing.T) {
	got := history.Diff(change("a\nb\nc\n", "a\nB\nc\n"))
	want := "--- " + cfgPath + "\n+++ " + cfgPath + "\n- b\n+ B\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	n := history.Diff(history.FileChange{Path: "/p", After: []byte("x\ny\n"), AfterExists: true})
	if n != "--- /p\n+++ /p\n(new file)\n+ x\n+ y\n" {
		t.Fatalf("new:\n%s", n)
	}
	r := history.Diff(history.FileChange{Path: "/p", Before: []byte("x\n"), BeforeExists: true})
	if r != "--- /p\n+++ /p\n(removed)\n- x\n" {
		t.Fatalf("removed:\n%s", r)
	}
	if same := history.Diff(change("a\n", "a\n")); same != "--- "+cfgPath+"\n+++ "+cfgPath+"\n" {
		t.Fatalf("same:\n%s", same)
	}
}

func twoFiles(t *testing.T, s history.Store, root fsx.Root) (history.Event, string, string) {
	t.Helper()
	p1 := "/home/u/.config/cairn/projects/a.toml"
	p2 := "/home/u/.config/cairn/projects/b.toml"
	write(t, root, p1, "a-new\n")
	write(t, root, p2, "b-new\n")
	e, err := s.Append("edit", "two", []history.FileChange{
		{Path: p1, Before: []byte("a-old\n"), After: []byte("a-new\n"), BeforeExists: true, AfterExists: true},
		{Path: p2, Before: []byte("b-old\n"), After: []byte("b-new\n"), BeforeExists: true, AfterExists: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	return e, p1, p2
}

func TestUndoResumableAfterPartialRevert(t *testing.T) {
	s, root := newStore(t)
	e, p1, p2 := twoFiles(t, s, root)
	write(t, root, p1, "a-old\n") // simulated crash after first file
	u, err := s.Undo(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if read(t, root, p1) != "a-old\n" || read(t, root, p2) != "b-old\n" {
		t.Fatal("not fully reverted")
	}
	if u.Kind != "undo" || len(u.Files) != 2 {
		t.Fatalf("event = %+v", u)
	}
	if string(u.Files[0].Before) != "a-new\n" {
		t.Fatalf("skipped file should record the true pre-undo state: %q", u.Files[0].Before)
	}
	// neither At nor Before -> conflict
	s2, root2 := newStore(t)
	e2, q1, _ := twoFiles(t, s2, root2)
	write(t, root2, q1, "something else\n")
	if _, err := s2.Undo(e2.ID); !errors.Is(err, history.ErrConflict) {
		t.Fatalf("err = %v", err)
	}
}

func TestUndoAlreadyApplied(t *testing.T) {
	s, root := newStore(t)
	e, _, _ := twoFiles(t, s, root)
	if _, err := s.Undo(e.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Undo(e.ID); !errors.Is(err, history.ErrAlreadyApplied) {
		t.Fatalf("err = %v", err)
	}
}

func TestConcurrentUndoRecordsOneEvent(t *testing.T) {
	s, root := newStore(t)
	e, _, _ := twoFiles(t, s, root)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() { defer wg.Done(); _, errs[i] = s.Undo(e.ID) }()
	}
	wg.Wait()
	evs, _ := s.List(0)
	n := 0
	for _, x := range evs {
		if x.Kind == "undo" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("undo events = %d (errs %v)", n, errs)
	}
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		} else if !errors.Is(err, history.ErrAlreadyApplied) {
			t.Fatalf("unexpected %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("errs = %v", errs)
	}
}

func TestWriteFailureRollsBack(t *testing.T) {
	s, root := newStore(t)
	p1 := "/home/u/.config/cairn/projects/a.toml"
	blocked := "/home/u/.config/cairn/blocked"
	p2 := blocked + "/b.toml"
	write(t, root, p1, "a-new\n")
	write(t, root, blocked, "i am a file\n")
	e, _ := s.Append("edit", "x", []history.FileChange{
		{Path: p1, Before: []byte("a-old\n"), After: []byte("a-new\n"), BeforeExists: true, AfterExists: true},
		{Path: p2, Before: []byte("b-old\n"), BeforeExists: true},
	})
	if _, err := s.Undo(e.ID); err == nil || errors.Is(err, history.ErrConflict) {
		t.Fatalf("want write error, got %v", err)
	}
	if read(t, root, p1) != "a-new\n" {
		t.Fatal("first file not rolled back")
	}
	if evs, _ := s.List(0); len(evs) != 1 {
		t.Fatal("no event on failure")
	}
}

func TestRestoreSkipsFilesAlreadyAtAfter(t *testing.T) {
	s, root := newStore(t)
	e, p1, p2 := twoFiles(t, s, root)
	write(t, root, p2, "drift\n")
	if _, err := s.Restore(e.ID); err != nil {
		t.Fatal(err)
	}
	if read(t, root, p1) != "a-new\n" || read(t, root, p2) != "b-new\n" {
		t.Fatal("restore incomplete")
	}
}

func TestNextIDIsMaxPlusOne(t *testing.T) {
	s, root := newStore(t)
	for i := 0; i < 3; i++ {
		if _, err := s.Append("edit", "x", nil); err != nil {
			t.Fatal(err)
		}
	}
	raw := strings.SplitAfter(read(t, root, s.Path), "\n")
	if err := os.WriteFile(root.Path(s.Path), []byte(raw[0]+raw[2]), 0o600); err != nil { // drop event 2
		t.Fatal(err)
	}
	e, err := s.Append("edit", "y", nil)
	if err != nil || e.ID != "000004" {
		t.Fatalf("id = %s %v", e.ID, err)
	}
	// 000003 was max; deleting the last would reuse, which is acceptable; check no dup:
	evs, _ := s.List(0)
	seen := map[string]bool{}
	for _, x := range evs {
		if seen[x.ID] {
			t.Fatal("dup")
		}
		seen[x.ID] = true
	}
}

func TestHugeEventLine(t *testing.T) {
	s, _ := newStore(t)
	big := bytes.Repeat([]byte("x"), 2<<20)
	if _, err := s.Append("edit", "big", []history.FileChange{{Path: cfgPath, After: big, AfterExists: true}}); err != nil {
		t.Fatal(err)
	}
	e, err := s.Append("edit", "next", nil)
	if err != nil || e.ID != "000002" {
		t.Fatalf("%v %v", e.ID, err)
	}
	g, err := s.Get("000001")
	if err != nil || len(g.Files[0].After) != len(big) {
		t.Fatalf("get: %v", err)
	}
	if evs, _ := s.List(0); len(evs) != 2 {
		t.Fatalf("list = %d", len(evs))
	}
}

func TestAllowedRootEdgeCases(t *testing.T) {
	craft := func(s history.Store, root fsx.Root, path string) {
		line := `{"id":"000001","ts":"2026-01-01T00:00:00Z","kind":"edit","summary":"x","files":[{"path":"` + path +
			`","before":"cHduCg==","after":"cHduCg==","before_exists":true,"after_exists":true}]}` + "\n"
		if err := root.WriteAtomic(s.Path, []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, bad := range []string{"/home/u/.config/cairn", "/home/u/.config/cairn/", "/home/u/.config/cairn/.", "//etc/passwd"} {
		s, root := newStore(t)
		craft(s, root, bad)
		if _, err := s.Restore("000001"); err == nil || !strings.Contains(err.Error(), "outside") {
			t.Errorf("%q: %v", bad, err)
		}
	}
	// duplicate slashes inside the root are cleaned and written at the clean path
	s, root := newStore(t)
	craft(s, root, "/home/u/.config/cairn//projects//d.toml")
	e, err := s.Restore("000001")
	if err != nil {
		t.Fatal(err)
	}
	if e.Files[0].Path != "/home/u/.config/cairn/projects/d.toml" || read(t, root, e.Files[0].Path) != "pwn\n" {
		t.Fatalf("path not cleaned: %q", e.Files[0].Path)
	}
	// root "/" allows everything (documented), but still not "/" itself
	s, root = newStore(t)
	s.AllowedRoots = []string{"/"}
	craft(s, root, "/etc/x")
	if _, err := s.Restore("000001"); err != nil {
		t.Fatal(err)
	}
	craft(s, root, "/")
	if _, err := s.Restore("000001"); err == nil {
		t.Fatal("root itself must be refused")
	}
}
