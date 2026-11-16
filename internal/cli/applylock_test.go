package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mrqwer/cairn/internal/reconcile"
)

func countCalls(fd *fakeDriver, prefix string) int {
	n := 0
	for _, c := range fd.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func TestConcurrentUpCreatesTheWindowOnce(t *testing.T) {
	dir, fd := wsTestEnv(t)
	addProject(t, "demo", dir)
	mustRun(t, "", "up")
	file := filepath.Join(dir, "config", "cairn", "projects", "demo.toml")
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, append(b, []byte("\n[[windows]]\nkey = \"logs\"\nname = \"logs\"\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	fd.calls = nil
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := NewRootCmd()
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs([]string{"up"})
			errs[i] = cmd.Execute()
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := countCalls(fd, "NewWindow"); n != 1 {
		t.Fatalf("NewWindow called %d times: %v", n, fd.calls)
	}
}

func TestApplyLockContentionNamesTheLockFile(t *testing.T) {
	dir, fd := wsTestEnv(t)
	addProject(t, "demo", dir)
	old := applyLockWait
	applyLockWait = 100 * time.Millisecond
	t.Cleanup(func() { applyLockWait = old })

	held := NewRootCmd()
	unlock, err := lockApply(held)
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"up"}, {"apply", "--yes"}, {"open", "demo"}, {"adopt", "x"}} {
		out, err := runHost(t, "", args...)
		if err == nil || !strings.Contains(err.Error(), "apply.lock") || !strings.Contains(err.Error(), "another cairn") {
			t.Fatalf("%v: err=%v out=%s", args, err, out)
		}
	}
	// plan, status and a dry-run adopt only read: no lock needed.
	for _, args := range [][]string{{"plan"}, {"status"}} {
		if out, err := runHost(t, "", args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	if fd.called("NewSession") {
		t.Fatalf("something was applied while the lock was held: %v", fd.calls)
	}
	unlock()
	unlock() // idempotent
	if out, err := runHost(t, "", "up"); err != nil {
		t.Fatalf("after release: %v\n%s", err, out)
	}
}

func TestRenderResultsInterruptedIsOneLine(t *testing.T) {
	var b strings.Builder
	op := func(k string) reconcile.Op { return reconcile.Op{Kind: k, Project: "demo", Detail: k} }
	err := renderResults(&b, []reconcile.Result{
		{Op: op(reconcile.KindCreateSession)},
		{Op: op(reconcile.KindCreateWindow), Err: context.Canceled},
		{Op: op(reconcile.KindCreatePane), Err: context.Canceled},
	})
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 130 {
		t.Fatalf("err %v", err)
	}
	out := b.String()
	if strings.Contains(out, "FAIL") || !strings.Contains(out, "interrupted: 2 operations not run") || strings.Count(out, "interrupted") != 1 {
		t.Fatalf("output:\n%s", out)
	}
	b.Reset()
	err = renderResults(&b, []reconcile.Result{{Op: op(reconcile.KindCreateSession), Err: errors.New("boom")}})
	if !errors.As(err, &ee) || ee.Code != 1 || !strings.Contains(b.String(), "FAIL") {
		t.Fatalf("err %v\n%s", err, b.String())
	}
	b.Reset()
	if err := renderResults(&b, []reconcile.Result{{Op: op(reconcile.KindCreateWindow), Note: "window demo/logs already exists (@1); not created twice"}}); err != nil || !strings.Contains(b.String(), "not created twice") {
		t.Fatalf("note: %v\n%s", err, b.String())
	}
}

func TestApplyLockHeldByAnotherProcess(t *testing.T) {
	dir, _ := wsTestEnv(t)
	old := applyLockWait
	applyLockWait = 100 * time.Millisecond
	t.Cleanup(func() { applyLockWait = old })
	path := filepath.Join(dir, "state", "cairn", "workspace", "apply.lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil { // another open file description, as another process holds it
		t.Fatal(err)
	}
	if _, err := lockApply(NewRootCmd()); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("err %v", err)
	}
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
	unlock, err := lockApply(NewRootCmd())
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}

// readerFunc lets a test act at the moment the prompt reads the answer.
type readerFunc func(p []byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// The lock is released while the prompt waits; if another cairn takes it
// meanwhile, apply must fail with the lock message, not panic.
func TestApplyPruneRelockFailureIsAnErrorNotAPanic(t *testing.T) {
	dir, fd := wsTestEnv(t)
	addProject(t, "demo", dir)
	mustRun(t, "", "up")
	s := fd.session(t, "demo")
	s.windows = append(s.windows, &fWindow{id: "@90", key: "demo/old", name: "old",
		panes: []*fPane{{id: "%90", key: "demo/old/1", pid: 990, cmd: "bash"}}})
	fd.idle = true
	old := applyLockWait
	applyLockWait = 100 * time.Millisecond
	t.Cleanup(func() { applyLockWait = old })
	interactive = func(io.Reader) bool { return true }
	t.Cleanup(func() { interactive = func(io.Reader) bool { return false } })

	var unlock func()
	answered := false
	in := readerFunc(func(p []byte) (int, error) {
		if answered {
			return 0, io.EOF
		}
		answered = true
		var err error
		if unlock, err = lockApply(NewRootCmd()); err != nil { // another cairn wins the lock
			t.Error(err)
		}
		return copy(p, "y\n"), nil
	})
	cmd := NewRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetIn(in)
	cmd.SetArgs([]string{"apply", "--prune"})
	err := cmd.Execute()
	if unlock != nil {
		unlock()
	}
	if err == nil || !strings.Contains(err.Error(), "apply.lock") || fd.called("Kill") {
		t.Fatalf("err=%v kills=%v\n%s", err, fd.calls, buf.String())
	}
}
