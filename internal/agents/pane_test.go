package agents_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrqwer/cairn/internal/agents"
	"github.com/mrqwer/cairn/internal/agents/adapter"
	"github.com/mrqwer/cairn/internal/agents/conv"
	"github.com/mrqwer/cairn/internal/agents/testtool"
	"github.com/mrqwer/cairn/internal/fsx"
)

// syncBuf is a goroutine-safe output buffer.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type paneFixture struct {
	store   conv.Store
	tool    string
	log     string
	env     agents.PaneEnv
	out     *syncBuf
	closed  atomic.Int32
	archive atomic.Int32
}

func newPaneFixture(t *testing.T, exit int, keys string) *paneFixture {
	t.Helper()
	dir := t.TempDir()
	f := &paneFixture{store: conv.Store{Dir: "/state/agents", Root: fsx.Root{Dir: dir}}, out: &syncBuf{}}
	f.tool = testtool.Write(t, dir, testtool.Spec{Name: "fake", ExitCode: exit})
	f.log = filepath.Join(dir, "fake.log")
	a := adapter.Adapter{Name: "fake",
		Capabilities: adapter.Capabilities{PreassignID: true, ResumeByID: true, Hooks: "none"},
		Commands: map[string][]string{
			"start":  {f.tool, "start", "{id}", "--name", "{name}"},
			"resume": {f.tool, "resume", "{native_id}"},
		},
		Env: adapter.Env{Set: map[string]string{"FAKE_SET": "1"}}}
	f.env = agents.PaneEnv{
		Store:    f.store,
		Adapter:  func(name string) (adapter.Adapter, bool) { return a, name == "fake" },
		Attached: func(context.Context) (bool, error) { return true, nil },
		Run: func(_ context.Context, argv, env []string, dir string) (int, error) {
			c := exec.Command(argv[0], argv[1:]...)
			c.Env, c.Dir = env, dir
			err := c.Run()
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return ee.ExitCode(), nil
			}
			return 0, err
		},
		Keys:    strings.NewReader(keys),
		Out:     f.out,
		Poll:    5 * time.Millisecond,
		Close:   func(conv.Conversation) error { f.closed.Add(1); return nil },
		Archive: func(conv.Conversation) error { f.archive.Add(1); return nil },
	}
	return f
}

func (f *paneFixture) put(t *testing.T, c conv.Conversation) {
	t.Helper()
	if c.Tool == "" {
		c.Tool = "fake"
	}
	if err := f.store.Put(c); err != nil {
		t.Fatal(err)
	}
}

func (f *paneFixture) argvs(t *testing.T) [][]string {
	t.Helper()
	b, err := os.ReadFile(f.log)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out [][]string
	var cur []string
	in := false
	for _, l := range strings.Split(string(b), "\n") {
		switch {
		case l == "ARGV:":
			in, cur = true, nil
		case l == "ENV:":
			in = false
			out = append(out, cur)
		case in:
			cur = append(cur, strings.TrimPrefix(l, "  "))
		}
	}
	return out
}

func TestPaneLazyDoesNotStartDetached(t *testing.T) {
	f := newPaneFixture(t, 0, "x")
	f.put(t, conv.Conversation{ID: "C1", NativeID: uuid, Name: "lazy one", State: conv.StateStarting})
	var polls atomic.Int32
	f.env.Attached = func(context.Context) (bool, error) { polls.Add(1); return false, nil }
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_ = agents.RunPane(ctx, f.env, "C1")
	if got := f.argvs(t); len(got) != 0 {
		t.Fatalf("tool started while detached: %v", got)
	}
	if polls.Load() < 3 {
		t.Fatalf("attach polled %d times", polls.Load())
	}
	if !strings.Contains(f.out.String(), "lazy one") {
		t.Fatalf("pane does not show the name:\n%s", f.out.String())
	}
}

func TestPaneStartsOnAttach(t *testing.T) {
	f := newPaneFixture(t, 0, "x")
	f.put(t, conv.Conversation{ID: "C1", NativeID: uuid, Name: "fix login", State: conv.StateStarting})
	var n atomic.Int32
	f.env.Attached = func(context.Context) (bool, error) { return n.Add(1) > 3, nil }
	if err := agents.RunPane(context.Background(), f.env, "C1"); err != nil {
		t.Fatal(err)
	}
	got := f.argvs(t)
	if len(got) != 1 || strings.Join(got[0], " ") != "start "+uuid+" --name fix login" {
		t.Fatalf("argv %q", got)
	}
	if n.Load() < 4 {
		t.Fatalf("started before attach (polls %d)", n.Load())
	}
	c, _, _ := f.store.Get("C1")
	if len(c.LaunchArgv) == 0 || c.State != conv.StateStopped || c.StateSource != conv.SourceObserved {
		t.Fatalf("record %+v", c)
	}
	if f.closed.Load() != 1 {
		t.Fatal("x did not close")
	}
	b, _ := os.ReadFile(f.log)
	if !strings.Contains(string(b), "FAKE_SET=1") || !strings.Contains(string(b), "CAIRN_CONVERSATION=C1") {
		t.Fatalf("env not applied:\n%s", b)
	}
}

func TestPaneStartRequestSkipsWait(t *testing.T) {
	f := newPaneFixture(t, 0, "x")
	f.put(t, conv.Conversation{ID: "C1", NativeID: uuid, Name: "n", State: conv.StateStarting})
	f.env.Attached = func(context.Context) (bool, error) { return false, nil }
	if err := agents.RequestStart(f.store, "C1"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := agents.RunPane(ctx, f.env, "C1"); err != nil {
		t.Fatal(err)
	}
	if len(f.argvs(t)) != 1 {
		t.Fatal("start request ignored")
	}
}

func TestPaneUsesResumeWhenNativeKnown(t *testing.T) {
	f := newPaneFixture(t, 0, "x")
	f.put(t, conv.Conversation{ID: "C1", NativeID: uuid, Name: "n", State: conv.StateIdle,
		LaunchArgv: []string{"fake", "start"}})
	f.env.Now = true
	if err := agents.RunPane(context.Background(), f.env, "C1"); err != nil {
		t.Fatal(err)
	}
	got := f.argvs(t)
	if len(got) != 1 || strings.Join(got[0], " ") != "resume "+uuid {
		t.Fatalf("argv %q", got)
	}
}

func TestPaneRefusesSecondResume(t *testing.T) {
	f := newPaneFixture(t, 0, "x")
	f.put(t, conv.Conversation{ID: "C1", NativeID: uuid, Name: "busy", State: conv.StateIdle, LaunchArgv: []string{"x"}})
	rel, err := f.store.AcquireLock("fake", uuid)
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	f.env.Now = true
	if err := agents.RunPane(context.Background(), f.env, "C1"); err != nil {
		t.Fatal(err)
	}
	if got := f.argvs(t); len(got) != 0 {
		t.Fatalf("tool started although the conversation is locked: %v", got)
	}
	if out := f.out.String(); !strings.Contains(out, "pid "+strconv.Itoa(os.Getpid())) {
		t.Fatalf("message does not name the holder:\n%s", out)
	}
}

func TestPaneConcurrentResumeStartsOnce(t *testing.T) {
	f := newPaneFixture(t, 0, "")
	f.put(t, conv.Conversation{ID: "C1", NativeID: uuid, Name: "n", State: conv.StateIdle, LaunchArgv: []string{"x"}})
	// The fake tool sleeps so both panes overlap.
	slow := filepath.Join(filepath.Dir(f.tool), "slow")
	if err := os.WriteFile(slow, []byte("#!/bin/sh\nsleep 0.3\nexec "+f.tool+" \"$@\"\n"), 0o755); err != nil { //nolint:gosec // test tool
		t.Fatal(err)
	}
	f.env.Now = true
	base := f.env.Adapter
	f.env.Adapter = func(n string) (adapter.Adapter, bool) {
		a, ok := base(n)
		a.Commands = map[string][]string{"start": {slow, "start", "{id}"}, "resume": {slow, "resume", "{native_id}"}}
		return a, ok
	}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			env := f.env
			env.Keys = strings.NewReader("x")
			_ = agents.RunPane(context.Background(), env, "C1")
		}()
	}
	wg.Wait()
	if got := f.argvs(t); len(got) != 1 {
		t.Fatalf("tool started %d times: %v", len(got), got)
	}
}

func TestPaneExitShowsMenu(t *testing.T) {
	f := newPaneFixture(t, 3, "?ra")
	f.put(t, conv.Conversation{ID: "C1", NativeID: uuid, Name: "menu", State: conv.StateStarting})
	f.env.Now = true
	if err := agents.RunPane(context.Background(), f.env, "C1"); err != nil {
		t.Fatal(err)
	}
	out := f.out.String()
	for _, want := range []string{"exited (status 3)", "r resume", "x close", "a archive"} {
		if !strings.Contains(out, want) {
			t.Fatalf("menu lacks %q:\n%s", want, out)
		}
	}
	got := f.argvs(t)
	if len(got) != 2 || got[0][0] != "start" || got[1][0] != "resume" {
		t.Fatalf("r should resume: %q", got)
	}
	if f.archive.Load() != 1 || f.closed.Load() != 0 {
		t.Fatalf("archive=%d close=%d", f.archive.Load(), f.closed.Load())
	}
	c, _, _ := f.store.Get("C1")
	if c.State != conv.StateFailed {
		t.Fatalf("state %q", c.State)
	}
}

func TestPaneEOFLeavesQuietly(t *testing.T) {
	f := newPaneFixture(t, 0, "")
	f.put(t, conv.Conversation{ID: "C1", NativeID: uuid, Name: "n", State: conv.StateStarting})
	f.env.Now = true
	if err := agents.RunPane(context.Background(), f.env, "C1"); err != nil {
		t.Fatal(err)
	}
	if f.closed.Load() != 0 || f.archive.Load() != 0 {
		t.Fatal("EOF must not unbind")
	}
}

func TestPaneUnknownConversation(t *testing.T) {
	f := newPaneFixture(t, 0, "x")
	if err := agents.RunPane(context.Background(), f.env, "NOPE"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.out.String(), "NOPE") || len(f.argvs(t)) != 0 {
		t.Fatalf("out:\n%s", f.out.String())
	}
}

func TestPaneDiscoversNativeID(t *testing.T) {
	f := newPaneFixture(t, 0, "x")
	home := t.TempDir()
	cwd := t.TempDir()
	tool := filepath.Join(t.TempDir(), "disco")
	script := "#!/bin/sh\nd=" + home + "/sessions/day\nmkdir -p \"$d\"\nprintf '{\"cwd\":\"%s\"}\\n' \"$PWD\" > \"$d/rollout-x-" + uuid + ".jsonl\"\nsleep 1.2\n"
	if err := os.WriteFile(tool, []byte(script), 0o755); err != nil { //nolint:gosec // test tool
		t.Fatal(err)
	}
	a := dirDiffAdapter()
	a.Commands["start"] = []string{tool}
	f.env.Adapter = func(string) (adapter.Adapter, bool) { return a, true }
	f.env.Root, f.env.Home, f.env.Now = fsx.Root{Dir: "/"}, home, true
	f.put(t, conv.Conversation{ID: "C1", Name: "d", Cwd: cwd, State: conv.StateStarting})
	if err := agents.RunPane(context.Background(), f.env, "C1"); err != nil {
		t.Fatal(err)
	}
	c, _, _ := f.store.Get("C1")
	if c.NativeID != uuid {
		t.Fatalf("native id %q\n%s", c.NativeID, f.out.String())
	}
}

func TestStaleStartRequestIgnored(t *testing.T) {
	f := newPaneFixture(t, 0, "x")
	f.put(t, conv.Conversation{ID: "C1", NativeID: uuid, Name: "n", State: conv.StateStarting})
	if err := agents.RequestStart(f.store, "C1"); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Minute)
	p := filepath.Join(f.store.Root.Dir, f.store.Dir, "start", "C1")
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	f.env.Attached = func(context.Context) (bool, error) { return false, nil }
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = agents.RunPane(ctx, f.env, "C1")
	if len(f.argvs(t)) != 0 {
		t.Fatal("stale start request started the tool")
	}
}
