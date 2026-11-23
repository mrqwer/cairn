package agents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mrqwer/cairn/internal/agents/adapter"
	"github.com/mrqwer/cairn/internal/agents/conv"
	"github.com/mrqwer/cairn/internal/execx"
	"github.com/mrqwer/cairn/internal/fsx"
)

// PaneEnv is what the pane wrapper needs from its surroundings; every side
// effect goes through it so tests can drive the wrapper without tmux.
type PaneEnv struct {
	Store   conv.Store
	Adapter func(name string) (adapter.Adapter, bool)
	// ProfileArgs returns the tool flags of the conversation's profile (nil
	// is no flags).
	ProfileArgs func(conv.Conversation, adapter.Adapter) []string
	// Attached reports whether the pane's tmux session has a client (R9).
	Attached func(ctx context.Context) (bool, error)
	// Run runs the tool in the pane's terminal and returns its exit status.
	Run func(ctx context.Context, argv, env []string, dir string) (int, error)
	// Keys is the menu input, read one byte at a time; RawKeys, when set,
	// switches the terminal to single-key input and returns the restore.
	Keys    io.Reader
	RawKeys func() (restore func())
	Out     io.Writer
	// Poll is the attach polling interval (default 500 ms).
	Poll time.Duration
	// Now starts the tool without waiting for a client.
	Now bool
	// Close and Archive remove the pane binding (one history event each);
	// Archive also archives the conversation.
	Close, Archive func(conv.Conversation) error
	// Discovery of tool-assigned ids.
	Root   fsx.Root
	Home   string
	Runner execx.Runner
	// DiscoverFor bounds how long discovery is tried after a launch
	// (default 15 s).
	DiscoverFor time.Duration
	Clock       func() time.Time
}

func (e PaneEnv) now() time.Time {
	if e.Clock != nil {
		return e.Clock()
	}
	return time.Now()
}

func (e PaneEnv) printf(format string, a ...any) { _, _ = fmt.Fprintf(e.Out, format, a...) }

// RequestStart asks the pane of conversation id to start its tool without
// waiting for an attached client (agents new --now, agents resume).
func RequestStart(s conv.Store, id string) error {
	if err := s.Root.MkdirAll(s.Dir+"/start", 0o700); err != nil {
		return err
	}
	return s.Root.WriteAtomic(startPath(s, id), []byte("1\n"), 0o600)
}

func startPath(s conv.Store, id string) string {
	return s.Dir + "/start/" + strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, id)
}

// startRequestTTL bounds how long a start request waits for its pane, so a
// request left behind does not start the tool unattended after a reboot.
const startRequestTTL = time.Minute

// takeStartRequest consumes a pending start request; a stale one is
// discarded.
func takeStartRequest(s conv.Store, id string) bool {
	p := startPath(s, id)
	fi, err := s.Root.Stat(p)
	if err != nil {
		return false
	}
	fresh := time.Since(fi.ModTime()) < startRequestTTL
	return s.Root.Remove(p) == nil && fresh
}

// LockKey is the per-conversation lock: the tool's own id when known (so two
// records of one native conversation also exclude each other), else cairn's.
func LockKey(c conv.Conversation) string {
	if c.NativeID != "" {
		return c.NativeID
	}
	return "cairn-" + c.ID
}

// fresh reports a conversation that was never launched.
func fresh(c conv.Conversation) bool {
	return len(c.LaunchArgv) == 0 && c.State == conv.StateStarting
}

// RunPane is "cairn agents pane <id>": it shows the conversation, waits for
// the session to be attached (unless Now or a start request), starts or
// resumes the tool under the conversation lock, and when the tool exits
// offers r resume / x close / a archive and stays (R7). It returns when the
// user closes or archives, when the key input ends, or when ctx is done
// while waiting.
func RunPane(ctx context.Context, env PaneEnv, id string) error {
	if env.Poll <= 0 {
		env.Poll = 500 * time.Millisecond
	}
	c, ok, err := env.Store.Get(id)
	if err != nil {
		return err
	}
	if !ok {
		env.printf("conversation %s not found (removed or archived elsewhere)\n", id)
		return menu(env, conv.Conversation{ID: id}, false)
	}
	a, ok := env.Adapter(c.Tool)
	if !ok {
		env.printf("%s: no adapter for tool %q (see cairn agents doctor)\n", title(c), c.Tool)
		return menu(env, c, false)
	}
	env.printf("%s\n", header(c))
	if !env.Now && !takeStartRequest(env.Store, id) {
		env.printf("waiting for the session to be attached; the tool starts then\n")
		if err := waitAttach(ctx, env, id); err != nil {
			return err
		}
	}
	for {
		// Whether it ran or was not started (locked, gone, failed), the
		// same choices follow; r tries again.
		c := launch(ctx, env, a, id)
		if err := menu(env, c, true); err != errRetry {
			return err
		}
	}
}

func title(c conv.Conversation) string {
	if c.Name != "" {
		return c.Name
	}
	return c.ID
}

func header(c conv.Conversation) string {
	parts := []string{title(c), c.Tool}
	if c.State != "" {
		parts = append(parts, c.State)
	}
	if c.Project != "" {
		parts = append(parts, c.Project)
	}
	return strings.Join(parts, " · ")
}

func waitAttach(ctx context.Context, env PaneEnv, id string) error {
	t := time.NewTicker(env.Poll)
	defer t.Stop()
	for {
		if takeStartRequest(env.Store, id) {
			return nil
		}
		if ok, err := env.Attached(ctx); err == nil && ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// launch runs the tool once under the conversation lock.
func launch(ctx context.Context, env PaneEnv, a adapter.Adapter, id string) conv.Conversation {
	c, ok, err := env.Store.Get(id)
	if err != nil || !ok {
		env.printf("conversation %s is gone: %v\n", id, err)
		return conv.Conversation{ID: id}
	}
	release, err := env.Store.AcquireLock(c.Tool, LockKey(c))
	if err != nil {
		var el conv.ErrLocked
		if errors.As(err, &el) {
			env.printf("%s is already open in another pane: %v; not starting it here\n", title(c), err)
		} else {
			env.printf("%s: cannot take the conversation lock: %v\n", title(c), err)
		}
		return c
	}
	defer release()

	var profile []string
	if env.ProfileArgs != nil {
		profile = env.ProfileArgs(c, a)
	}
	var argv []string
	var disc *Discovery
	if c.NativeID != "" && !fresh(c) {
		argv, err = BuildResume(a, c, profile)
	} else {
		if c.NativeID == "" && !fresh(c) {
			env.printf("the tool's id for %s is unknown; starting a new conversation\n", title(c))
		}
		argv, disc, err = BuildStart(a, c, profile)
	}
	if err != nil {
		env.printf("%s: %v\n", title(c), err)
		return c
	}

	var stopDiscovery func()
	if disc != nil {
		stopDiscovery = startDiscovery(ctx, env, a, c)
	}
	started := env.now()
	if c, err = env.Store.Update(id, func(r *conv.Conversation) error {
		r.LaunchArgv = argv
		r.LastActivityAt = started
		if r.State == "" || r.State == conv.StateStopped || r.State == conv.StateFailed || r.State == conv.StateDone {
			r.State = conv.StateStarting
		}
		return nil
	}); err != nil {
		env.printf("%s: %v\n", id, err)
		if stopDiscovery != nil {
			stopDiscovery()
		}
		return conv.Conversation{ID: id}
	}
	code, runErr := env.Run(context.WithoutCancel(ctx), argv, toolEnv(a, c), c.Cwd)
	if stopDiscovery != nil {
		stopDiscovery()
	}
	state := conv.StateStopped
	switch {
	case runErr != nil:
		env.printf("%s: %v\n", strings.Join(argv, " "), runErr)
		state = conv.StateFailed
	case code != 0:
		state = conv.StateFailed
	}
	env.printf("\n%s exited (status %d)\n", title(c), code)
	if u, err := env.Store.Update(id, func(r *conv.Conversation) error {
		if r.ArchivedAt == nil {
			r.State, r.StateSource = state, conv.SourceObserved
		}
		r.LastActivityAt = env.now()
		return nil
	}); err == nil {
		c = u
	}
	return c
}

// toolEnv is the pane's environment adjusted by the adapter, plus the
// conversation id for hooks.
func toolEnv(a adapter.Adapter, c conv.Conversation) []string {
	unset := map[string]bool{"CAIRN_CONVERSATION": true}
	for _, k := range a.Env.Unset {
		unset[k] = true
	}
	for k := range a.Env.Set {
		unset[k] = true
	}
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if !unset[k] {
			out = append(out, kv)
		}
	}
	for k, v := range a.Env.Set {
		out = append(out, k+"="+v)
	}
	return append(out, "CAIRN_CONVERSATION="+c.ID)
}

// startDiscovery serialises launches in one cwd with a short lock and polls
// Discover while the tool runs; the returned func stops it and releases the
// lock. A found id is written to the record at once.
func startDiscovery(ctx context.Context, env PaneEnv, a adapter.Adapter, c conv.Conversation) func() {
	sum := sha256.Sum256([]byte(c.Cwd))
	key := hex.EncodeToString(sum[:8])
	var release func()
	for deadline := time.Now().Add(10 * time.Second); ; {
		r, err := env.Store.AcquireLock("discover", key)
		if err == nil {
			release = r
			break
		}
		if time.Now().After(deadline) {
			env.printf("another launch in %s is still being identified; the id of %s may need cairn agents import\n", c.Cwd, title(c))
			return func() {}
		}
		time.Sleep(100 * time.Millisecond)
	}
	before, err := TakeSnapshot(ctx, env.Root, a, env.Runner, env.Home)
	if err != nil {
		release()
		env.printf("discovery for %s unavailable: %v\n", title(c), err)
		return func() {}
	}
	started := env.now()
	limit := env.DiscoverFor
	if limit <= 0 {
		limit = 15 * time.Second
	}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		defer release()
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		end := time.After(limit)
		for {
			select {
			case <-done:
				return
			case <-end:
				return
			case <-t.C:
			}
			id, err := Discover(ctx, env.Root, a, env.Runner, before, started, c.Cwd)
			switch {
			case err == nil:
				_, _ = env.Store.Update(c.ID, func(r *conv.Conversation) error { r.NativeID = id; return nil })
				return
			case errors.Is(err, ErrNotDiscovered):
				continue
			default:
				env.printf("could not identify the conversation of %s: %v\n", title(c), err)
				return
			}
		}
	}()
	return func() {
		select {
		case <-done:
		default:
			close(done)
		}
		<-finished
	}
}

var errRetry = errors.New("retry")

// menu offers the after-exit choices and reads one key. canResume hides r
// when there is nothing to resume.
func menu(env PaneEnv, c conv.Conversation, canResume bool) error {
	for {
		if canResume {
			env.printf("r resume · x close · a archive\n")
		} else {
			env.printf("x close · a archive\n")
		}
		k, err := readKey(env)
		if err != nil {
			return nil // input closed: leave the pane as it is
		}
		switch k {
		case 'r':
			if canResume {
				return errRetry
			}
		case 'x':
			if env.Close == nil {
				return nil
			}
			if err := env.Close(c); err != nil {
				env.printf("close failed: %v\n", err)
				continue
			}
			return nil
		case 'a':
			if env.Archive == nil {
				return nil
			}
			if err := env.Archive(c); err != nil {
				env.printf("archive failed: %v\n", err)
				continue
			}
			return nil
		}
	}
}

func readKey(env PaneEnv) (byte, error) {
	if env.RawKeys != nil {
		restore := env.RawKeys()
		defer restore()
	}
	var b [1]byte
	for {
		n, err := env.Keys.Read(b[:])
		if n == 1 {
			if b[0] == '\n' || b[0] == '\r' {
				continue
			}
			return b[0], nil
		}
		if err != nil {
			return 0, err
		}
	}
}
