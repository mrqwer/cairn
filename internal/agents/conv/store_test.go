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
	if strings.TrimSpace(string(b)) != strconv.Itoa(os.Getpid()) {
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
