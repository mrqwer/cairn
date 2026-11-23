package conv_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrqwer/cairn/internal/agents/conv"
	"github.com/mrqwer/cairn/internal/fsx"
)

func newStore(t *testing.T) (conv.Store, string) {
	t.Helper()
	root := t.TempDir()
	return conv.Store{Dir: "/state/agents", Root: fsx.Root{Dir: root}}, root
}

func TestPutGetLastWins(t *testing.T) {
	s, _ := newStore(t)
	if err := s.Put(conv.Conversation{ID: "a", Name: "one", State: conv.StateStarting}); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(conv.Conversation{ID: "a", Name: "two", State: conv.StateWorking}); err != nil {
		t.Fatal(err)
	}
	c, ok, err := s.Get("a")
	if err != nil || !ok {
		t.Fatalf("get: %v %v", ok, err)
	}
	if c.Name != "two" || c.State != conv.StateWorking {
		t.Fatalf("got %+v", c)
	}
	if _, ok, _ := s.Get("missing"); ok {
		t.Fatal("missing found")
	}
}

func TestTruncatedTrailingLineIgnored(t *testing.T) {
	s, root := newStore(t)
	if err := s.Put(conv.Conversation{ID: "a", Name: "one"}); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "state/agents/conversations.jsonl")
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"id":"a","name":"tr`)
	_ = f.Close()
	c, ok, err := s.Get("a")
	if err != nil || !ok || c.Name != "one" {
		t.Fatalf("got %+v %v %v", c, ok, err)
	}
	// a later Put must not be glued to the truncated fragment
	if err := s.Put(conv.Conversation{ID: "b", Name: "bee"}); err != nil {
		t.Fatal(err)
	}
	if c, ok, _ := s.Get("b"); !ok || c.Name != "bee" {
		t.Fatalf("b lost: %+v %v", c, ok)
	}
	if c, _, _ := s.Get("a"); c.Name != "one" {
		t.Fatalf("a changed: %+v", c)
	}
}

func TestFilterTagStateArchived(t *testing.T) {
	s, _ := newStore(t)
	now := time.Now()
	arch := now
	for _, c := range []conv.Conversation{
		{ID: "1", Project: "demo", Task: "t1", Tool: "claude", State: conv.StateWorking, Tags: []string{"x", "y"}, LastActivityAt: now},
		{ID: "2", Project: "api", Tool: "codex", State: conv.StateIdle, Tags: []string{"y"}, LastActivityAt: now.Add(-time.Hour)},
		{ID: "3", Project: "demo", Tool: "claude", State: conv.StateArchived, ArchivedAt: &arch, Tags: []string{"x"}, LastActivityAt: now},
	} {
		if err := s.Put(c); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(f conv.Filter) string {
		l, err := s.List(f)
		if err != nil {
			t.Fatal(err)
		}
		var o []string
		for _, c := range l {
			o = append(o, c.ID)
		}
		return strings.Join(o, ",")
	}
	cases := []struct {
		f    conv.Filter
		want string
	}{
		{conv.Filter{}, "1,2"},
		{conv.Filter{Archived: true}, "1,2,3"},
		{conv.Filter{Tag: "y"}, "1,2"},
		{conv.Filter{Tag: "x"}, "1"},
		{conv.Filter{Tag: "x", Archived: true}, "1,3"},
		{conv.Filter{State: conv.StateIdle}, "2"},
		{conv.Filter{State: conv.StateArchived}, "3"},
		{conv.Filter{Project: "demo"}, "1"},
		{conv.Filter{Task: "t1"}, "1"},
		{conv.Filter{Tool: "codex"}, "2"},
		{conv.Filter{Since: now.Add(-time.Minute)}, "1"},
	}
	for _, tc := range cases {
		if got := ids(tc.f); got != tc.want {
			t.Errorf("%+v: got %q want %q", tc.f, got, tc.want)
		}
	}
}

func TestCompactKeepsLast(t *testing.T) {
	s, root := newStore(t)
	for i := 0; i < 5; i++ {
		_ = s.Put(conv.Conversation{ID: "a", Name: fmt.Sprint("n", i)})
		_ = s.Put(conv.Conversation{ID: "b", Name: fmt.Sprint("m", i)})
	}
	if err := s.Compact(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "state/agents/conversations.jsonl")
	data, _ := os.ReadFile(p)
	if n := bytes.Count(data, []byte("\n")); n != 2 {
		t.Fatalf("lines=%d:\n%s", n, data)
	}
	if c, _, _ := s.Get("a"); c.Name != "n4" {
		t.Fatalf("a=%+v", c)
	}
	if c, _, _ := s.Get("b"); c.Name != "m4" {
		t.Fatalf("b=%+v", c)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("perm %v", fi.Mode().Perm())
	}
}

func TestUpdateConcurrent(t *testing.T) {
	s, _ := newStore(t)
	if err := s.Put(conv.Conversation{ID: "a"}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Update("a", func(c *conv.Conversation) error {
				c.Tags = append(c.Tags, "t"+strconv.Itoa(i))
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	c, _, _ := s.Get("a")
	sort.Strings(c.Tags)
	if len(c.Tags) != 16 {
		t.Fatalf("tags=%v", c.Tags)
	}
}

func TestUpdateErrorsNotPersisted(t *testing.T) {
	s, _ := newStore(t)
	_ = s.Put(conv.Conversation{ID: "a", Name: "x"})
	boom := errors.New("boom")
	if _, err := s.Update("a", func(c *conv.Conversation) error { c.Name = "y"; return boom }); !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
	if c, _, _ := s.Get("a"); c.Name != "x" {
		t.Fatalf("persisted %+v", c)
	}
	if _, err := s.Update("nope", func(*conv.Conversation) error { return nil }); err == nil {
		t.Fatal("expected not found")
	}
}

func TestLockExclusive(t *testing.T) {
	s, _ := newStore(t)
	rel, err := s.AcquireLock("claude", "native-1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.AcquireLock("claude", "native-1")
	var el conv.ErrLocked
	if !errors.As(err, &el) {
		t.Fatalf("err=%v", err)
	}
	if el.Holder != strconv.Itoa(os.Getpid()) {
		t.Fatalf("holder=%q", el.Holder)
	}
	// different conversation is independent
	rel2, err := s.AcquireLock("claude", "native-2")
	if err != nil {
		t.Fatal(err)
	}
	rel2()
	rel()
	rel() // idempotent
	rel3, err := s.AcquireLock("claude", "native-1")
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	rel3()
}

func TestLockPathSafe(t *testing.T) {
	s, root := newStore(t)
	rel, err := s.AcquireLock("claude", "../../evil/x")
	if err != nil {
		t.Fatal(err)
	}
	rel()
	entries, _ := os.ReadDir(filepath.Join(root, "state/agents/locks"))
	if len(entries) != 1 {
		t.Fatalf("entries=%v", entries)
	}
	if _, err := os.Stat(filepath.Join(root, "evil")); err == nil {
		t.Fatal("escaped")
	}
}

func TestLockStalePidTakenOver(t *testing.T) {
	s, root := newStore(t)
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skip("no true")
	}
	dead := cmd.Process.Pid
	dir := filepath.Join(root, "state/agents/locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "claude-n1.lock")
	if err := os.WriteFile(p, []byte(strconv.Itoa(dead)), 0o600); err != nil {
		t.Fatal(err)
	}
	rel, err := s.AcquireLock("claude", "n1")
	if err != nil {
		t.Fatalf("stale not taken over: %v", err)
	}
	defer rel()
	b, _ := os.ReadFile(p)
	if f := strings.Fields(string(b)); len(f) == 0 || f[0] != strconv.Itoa(os.Getpid()) {
		t.Fatalf("pid file=%q", b)
	}
}

func TestLockHeldByOtherProcess(t *testing.T) {
	s, root := newStore(t)
	dir := filepath.Join(root, "state/agents/locks")
	_ = os.MkdirAll(dir, 0o700)
	p := filepath.Join(dir, "claude-n1.lock")
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("no flock binary")
	}
	_ = os.WriteFile(p, []byte("4242"), 0o600)
	cmd := exec.Command("flock", p, "sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	deadline := time.Now().Add(3 * time.Second)
	for {
		rel, err := s.AcquireLock("claude", "n1")
		if err != nil {
			var el conv.ErrLocked
			if !errors.As(err, &el) {
				t.Fatalf("err=%v", err)
			}
			if el.Holder != "4242" {
				t.Fatalf("holder=%q", el.Holder)
			}
			return
		}
		rel()
		_ = os.WriteFile(p, []byte("4242"), 0o600)
		if time.Now().After(deadline) {
			t.Fatal("never saw lock held")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type seqRand struct{ b byte }

func (r *seqRand) Read(p []byte) (int, error) {
	for i := range p {
		r.b++
		p[i] = r.b
	}
	return len(p), nil
}

func TestNewIDSortable(t *testing.T) {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	var prev string
	for i := 0; i < 50; i++ {
		id := conv.NewID(base.Add(time.Duration(i)*time.Millisecond), &seqRand{})
		if len(id) != 26 {
			t.Fatalf("len %d %q", len(id), id)
		}
		if strings.Trim(id, "0123456789ABCDEFGHJKMNPQRSTVWXYZ") != "" {
			t.Fatalf("alphabet %q", id)
		}
		if id <= prev {
			t.Fatalf("not increasing %q <= %q", id, prev)
		}
		prev = id
	}
	id1, id2 := conv.NewID(base, &seqRand{}), conv.NewID(base, &seqRand{})
	if id1 != id2 {
		t.Fatal("not deterministic with injected rand")
	}
	r1, r2 := conv.NewID(base, nil), conv.NewID(base, nil)
	if r1 == r2 {
		t.Fatal("default rand not random")
	}
}

func TestFilePerm0600(t *testing.T) {
	s, root := newStore(t)
	_ = s.Put(conv.Conversation{ID: "a"})
	rel, _ := s.AcquireLock("t", "n")
	rel()
	base := filepath.Join(root, "state/agents")
	for _, f := range []string{"conversations.jsonl", ".store.lock", "locks/t-n.lock"} {
		fi, err := os.Stat(filepath.Join(base, f))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s perm %v", f, fi.Mode().Perm())
		}
	}
	fi, _ := os.Stat(base)
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("dir perm %v", fi.Mode().Perm())
	}
}

func TestUpdateErrorReturnsZero(t *testing.T) {
	s, _ := newStore(t)
	_ = s.Put(conv.Conversation{ID: "a", Name: "x"})
	got, err := s.Update("a", func(c *conv.Conversation) error { c.Name = "y"; return errors.New("boom") })
	if err == nil {
		t.Fatal("expected error")
	}
	if got.ID != "" || got.Name != "" {
		t.Fatalf("want zero value on error, got %+v", got)
	}
}

func TestPutSurfacesDataFileError(t *testing.T) {
	s, root := newStore(t)
	// The data file path is a directory: reading its tail must fail loudly.
	if err := os.MkdirAll(filepath.Join(root, "state/agents/conversations.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(conv.Conversation{ID: "a"}); err == nil {
		t.Fatal("expected error")
	}
	if _, err := s.Update("a", func(*conv.Conversation) error { return nil }); err == nil {
		t.Fatal("expected error")
	}
}

func TestPutHealsTailAfterManyRecords(t *testing.T) {
	s, root := newStore(t)
	for i := 0; i < 50; i++ {
		if err := s.Put(conv.Conversation{ID: fmt.Sprintf("id%02d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	p := filepath.Join(root, "state/agents/conversations.jsonl")
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(`{"id":"half`)
	_ = f.Close()
	if err := s.Put(conv.Conversation{ID: "z", Name: "after"}); err != nil {
		t.Fatal(err)
	}
	c, ok, _ := s.Get("z")
	if !ok || c.Name != "after" {
		t.Fatalf("got %+v %v", c, ok)
	}
	all, _ := s.List(conv.Filter{})
	if len(all) != 51 {
		t.Fatalf("len=%d", len(all))
	}
}

func TestErrLockedEmptyHolderMessage(t *testing.T) {
	msg := conv.ErrLocked{}.Error()
	if strings.Contains(msg, "pid )") || strings.Contains(msg, "pid  ") {
		t.Fatalf("empty pid printed: %q", msg)
	}
	if !strings.Contains(conv.ErrLocked{Holder: "42"}.Error(), "pid 42") {
		t.Fatal("holder missing")
	}
}

func TestCuratedOverlayWins(t *testing.T) {
	s, root := newStore(t)
	_ = s.Put(conv.Conversation{ID: "a", Name: "machine", Tags: []string{"x"}, State: conv.StateWorking})
	now := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	cu := conv.Curated{Name: "curated", Purpose: "p", Tags: []string{"y"}, ArchivedAt: &now}
	p := filepath.Join(root, s.CuratedPath("a"))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, cu.Encode(), 0o600); err != nil {
		t.Fatal(err)
	}
	c, ok, err := s.Get("a")
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if c.Name != "curated" || c.Purpose != "p" || len(c.Tags) != 1 || c.Tags[0] != "y" || c.ArchivedAt == nil || c.State != conv.StateWorking {
		t.Fatalf("got %+v", c)
	}
	if l, _ := s.List(conv.Filter{}); len(l) != 0 {
		t.Fatalf("archived listed: %+v", l)
	}
	// An Update keeps the overlay authoritative.
	if _, err := s.Update("a", func(c *conv.Conversation) error { c.State = conv.StateIdle; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, conv.Curated{Name: "back"}.Encode(), 0o600); err != nil {
		t.Fatal(err)
	}
	c, _, _ = s.Get("a")
	if c.Name != "back" || c.ArchivedAt != nil || len(c.Tags) != 0 || c.State != conv.StateIdle {
		t.Fatalf("got %+v", c)
	}
}

func TestCuratedRemovedHidden(t *testing.T) {
	s, root := newStore(t)
	_ = s.Put(conv.Conversation{ID: "a", Name: "n"})
	_ = s.Put(conv.Conversation{ID: "b", Name: "m"})
	if err := s.SeedCurated("a"); err != nil {
		t.Fatal(err)
	}
	data, ok, err := s.ReadCurated("a")
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	var cu conv.Curated
	if err := cu.Decode(data); err != nil || cu.Name != "n" {
		t.Fatalf("seed %+v %v", cu, err)
	}
	fi, _ := os.Stat(filepath.Join(root, s.CuratedPath("a")))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("perm %v", fi.Mode())
	}
	// seeding again keeps the existing overlay
	cu.Removed = true
	_ = os.WriteFile(filepath.Join(root, s.CuratedPath("a")), cu.Encode(), 0o600)
	if err := s.SeedCurated("a"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Get("a"); ok {
		t.Fatal("removed conversation visible")
	}
	if _, err := s.Update("a", func(*conv.Conversation) error { return nil }); !errors.Is(err, conv.ErrNotFound) {
		t.Fatalf("update removed: %v", err)
	}
	if l, _ := s.List(conv.Filter{Archived: true}); len(l) != 1 || l[0].ID != "b" {
		t.Fatalf("list %+v", l)
	}
	if err := s.Compact(); err != nil {
		t.Fatal(err)
	}
	// Compact keeps removed records so undo can bring them back.
	_ = os.Remove(filepath.Join(root, s.CuratedPath("a")))
	if _, ok, _ := s.Get("a"); !ok {
		t.Fatal("compact dropped a removed record")
	}
}

func TestLockLive(t *testing.T) {
	s, root := newStore(t)
	if s.LockLive("claude", "n1") {
		t.Fatal("no lock file must not be live")
	}
	rel, err := s.AcquireLock("claude", "n1")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "state/agents/locks/claude-n1.lock")
	held, _ := os.ReadFile(p)
	if !s.LockLive("claude", "n1") {
		t.Fatalf("held lock must be live (%q)", held)
	}
	rel()
	if s.LockLive("claude", "n1") {
		t.Fatal("released lock must not be live")
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Skip("no boot_id")
	}
	thisBoot := strings.TrimSpace(string(boot))
	// A pid above pid_max never exists.
	pm, _ := os.ReadFile("/proc/sys/kernel/pid_max")
	maxPid, err := strconv.Atoi(strings.TrimSpace(string(pm)))
	if err != nil {
		t.Skip("no pid_max")
	}
	for _, tc := range []struct {
		content string
		live    bool
	}{
		{fmt.Sprintf("%d %s", os.Getpid(), thisBoot), true},
		{fmt.Sprintf("%d %s", maxPid+1, thisBoot), false},                            // dead holder
		{fmt.Sprintf("%d 00000000-0000-0000-0000-000000000000", os.Getpid()), false}, // written in another boot
		{strconv.Itoa(os.Getpid()), false},                                           // no boot id
		{"", false},
	} {
		if err := os.WriteFile(p, []byte(tc.content), 0o600); err != nil {
			t.Fatal(err)
		}
		// The file time does not matter (a clock stepped after boot).
		old := time.Unix(1000, 0)
		_ = os.Chtimes(p, old, old)
		if got := s.LockLive("claude", "n1"); got != tc.live {
			t.Errorf("%q: live=%v want %v", tc.content, got, tc.live)
		}
	}
}

func TestLockHolderIsPid(t *testing.T) {
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("no flock binary")
	}
	s, root := newStore(t)
	rel, err := s.AcquireLock("claude", "n1")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "state/agents/locks/claude-n1.lock")
	content, _ := os.ReadFile(p)
	rel()
	// Another process holds it with our content: the holder shown is the pid alone.
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command("flock", "-n", p, "sh", "-c", "touch "+ready+"; sleep 30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("flock never held")
		}
	}
	_, err = s.AcquireLock("claude", "n1")
	var el conv.ErrLocked
	if !errors.As(err, &el) || el.Holder != strconv.Itoa(os.Getpid()) {
		t.Fatalf("err %v holder %q", err, el.Holder)
	}
}

func TestTryUpdateGivesUpWhenBusy(t *testing.T) {
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("no flock binary")
	}
	s, root := newStore(t)
	if err := s.Put(conv.Conversation{ID: "a", Name: "one"}); err != nil {
		t.Fatal(err)
	}
	c, err := s.TryUpdate("a", time.Second, func(c *conv.Conversation) error { c.Name = "two"; return nil })
	if err != nil || c.Name != "two" {
		t.Fatalf("free lock: %+v %v", c, err)
	}
	lock := filepath.Join(root, "state/agents/.store.lock")
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command("flock", lock, "sh", "-c", "touch "+ready+"; sleep 30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("flock never held")
		}
	}
	start := time.Now()
	_, err = s.TryUpdate("a", 100*time.Millisecond, func(c *conv.Conversation) error { c.Name = "three"; return nil })
	if !errors.Is(err, conv.ErrBusy) {
		t.Fatalf("err %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("waited %v", d)
	}
	if got, _, _ := s.Get("a"); got.Name != "two" {
		t.Fatalf("busy update written: %+v", got)
	}
}

// TestScanAppliesOverlaysToKeptOnly: Scan filters on the raw record, then
// applies the overlays of what it kept (archived and removed hide it).
func TestScanAppliesOverlaysToKeptOnly(t *testing.T) {
	s, _ := newStore(t)
	arch := time.Now().UTC()
	for _, c := range []conv.Conversation{
		{ID: "a", State: conv.StateWorking, Name: "raw-a"},
		{ID: "b", State: conv.StateWorking},
		{ID: "c", State: conv.StateWaiting},
		{ID: "d", State: conv.StateStopped},
	} {
		if err := s.Put(c); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.WriteCurated("a", conv.Curated{Name: "curated-a"}); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteCurated("b", conv.Curated{ArchivedAt: &arch}); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteCurated("c", conv.Curated{Removed: true}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Scan(func(c conv.Conversation) bool { return c.State != conv.StateStopped })
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "a" || got[0].Name != "curated-a" {
		t.Fatalf("got %+v", got)
	}
}

func dataLines(t testing.TB, root string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "state/agents/conversations.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Count(b, []byte("\n"))
}

// TestUpdateCompactsAsItGrows: frequent updates (one per hook event) keep
// the record file near the number of conversations, without a Compact call.
func TestUpdateCompactsAsItGrows(t *testing.T) {
	s, root := newStore(t)
	for i := 0; i < 10; i++ {
		if err := s.Put(conv.Conversation{ID: fmt.Sprintf("c%d", i), State: conv.StateWorking}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 5000; i++ {
		id := fmt.Sprintf("c%d", i%10)
		if _, err := s.Update(id, func(c *conv.Conversation) error { c.Summary = strconv.Itoa(i); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if n := dataLines(t, root); n > 400 {
		t.Fatalf("record file has %d lines for 10 conversations", n)
	}
	l, err := s.List(conv.Filter{})
	if err != nil || len(l) != 10 {
		t.Fatalf("%v %v", l, err)
	}
	for _, c := range l {
		if c.Summary < "4990" {
			t.Fatalf("lost the latest write of %s: %q", c.ID, c.Summary)
		}
	}
}

func TestPutCompactsAsItGrows(t *testing.T) {
	s, root := newStore(t)
	for i := 0; i < 5000; i++ {
		if err := s.Put(conv.Conversation{ID: "only", Summary: strconv.Itoa(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if n := dataLines(t, root); n > 400 {
		t.Fatalf("record file has %d lines for 1 conversation", n)
	}
	if c, _, _ := s.Get("only"); c.Summary != "4999" {
		t.Fatalf("%+v", c)
	}
}

func TestTxnSeesAllAndWritesChanged(t *testing.T) {
	s, _ := newStore(t)
	for _, id := range []string{"a", "b", "c"} {
		if err := s.Put(conv.Conversation{ID: id, State: conv.StateIdle}); err != nil {
			t.Fatal(err)
		}
	}
	err := s.Txn(func(all []conv.Conversation) ([]conv.Conversation, error) {
		if len(all) != 3 {
			t.Errorf("txn saw %d", len(all))
		}
		all[1].State = conv.StateQueued
		return []conv.Conversation{all[1]}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if b, _, _ := s.Get("b"); b.State != conv.StateQueued {
		t.Fatalf("b %+v", b)
	}
	if a, _, _ := s.Get("a"); a.State != conv.StateIdle {
		t.Fatalf("a %+v", a)
	}
	// An error writes nothing; an unknown id is refused.
	_ = s.Txn(func(all []conv.Conversation) ([]conv.Conversation, error) {
		all[0].State = conv.StateDone
		return all[:1], errors.New("no")
	})
	if a, _, _ := s.Get("a"); a.State != conv.StateIdle {
		t.Fatal("failed txn wrote")
	}
	if err := s.Txn(func([]conv.Conversation) ([]conv.Conversation, error) {
		return []conv.Conversation{{ID: "zz"}}, nil
	}); err == nil {
		t.Fatal("unknown id written")
	}
	// Concurrent transactions are serialised: increments are not lost.
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.Txn(func(all []conv.Conversation) ([]conv.Conversation, error) {
				for _, c := range all {
					if c.ID == "c" {
						c.IgnoredEvents++
						return []conv.Conversation{c}, nil
					}
				}
				return nil, nil
			})
		}()
	}
	wg.Wait()
	if c, _, _ := s.Get("c"); c.IgnoredEvents != 20 {
		t.Fatalf("lost updates: %d", c.IgnoredEvents)
	}
}

func TestInsertNewSkipsKnownNativeIDs(t *testing.T) {
	s, root := newStore(t)
	_ = s.Put(conv.Conversation{ID: "a", Tool: "claude", NativeID: "n1"})
	_ = s.Put(conv.Conversation{ID: "b", Tool: "claude", NativeID: "n2"})
	// b is removed: its native id still counts as known.
	if err := os.MkdirAll(filepath.Join(root, s.CuratedDir()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, s.CuratedPath("b")), []byte(`{"removed":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	known, err := s.NativeIDs("claude")
	if err != nil || !known["n1"] || !known["n2"] || len(known) != 2 {
		t.Fatalf("known %v %v", known, err)
	}
	added, err := s.InsertNew([]conv.Conversation{
		{ID: "c", Tool: "claude", NativeID: "n1"},
		{ID: "d", Tool: "claude", NativeID: "n2"},
		{ID: "e", Tool: "claude", NativeID: "n3"},
		{ID: "f", Tool: "codex", NativeID: "n1"},
		{ID: "g", Tool: "claude", NativeID: "n3"}, // duplicate within the batch
		{ID: "h", Tool: "claude"},                 // no native id
	})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range added {
		ids = append(ids, c.ID)
	}
	if strings.Join(ids, ",") != "e,f" {
		t.Fatalf("added %v", ids)
	}
	l, _ := s.List(conv.Filter{Archived: true})
	if len(l) != 3 {
		t.Fatalf("visible %+v", l)
	}
}

func TestInsertNewConcurrentNoDuplicates(t *testing.T) {
	s, _ := newStore(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = s.InsertNew([]conv.Conversation{{ID: fmt.Sprintf("id%d", i), Tool: "claude", NativeID: "same"}})
		}(i)
	}
	wg.Wait()
	if l, _ := s.List(conv.Filter{}); len(l) != 1 {
		t.Fatalf("records %+v", l)
	}
}

func TestFilterMatchExported(t *testing.T) {
	c := conv.Conversation{Project: "demo", Tags: []string{"x"}}
	if !(conv.Filter{Project: "demo", Tag: "x"}).Match(c) || (conv.Filter{Project: "api"}).Match(c) {
		t.Fatal("Match disagrees with List")
	}
}
