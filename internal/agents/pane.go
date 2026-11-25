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
	"sync"
	"time"

	"github.com/mrqwer/cairn/internal/agents/adapter"
	"github.com/mrqwer/cairn/internal/agents/conv"
	"github.com/mrqwer/cairn/internal/agents/task"
	"github.com/mrqwer/cairn/internal/execx"
	"github.com/mrqwer/cairn/internal/fsx"
)

// PaneEnv is what the pane wrapper needs from its surroundings; every side
// effect goes through it so tests can drive the wrapper without tmux.
type PaneEnv struct {
	Store   conv.Store
	Adapter func(name string) (adapter.Adapter, bool)
	// ProfileArgs returns the tool flags of the conversation's profile (nil
	// is no flags); an error (an unknown profile, risky flags the project
	// has not acknowledged) is printed and the tool is not started.
	ProfileArgs func(conv.Conversation, adapter.Adapter) ([]string, error)
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
	// DiscoverFor is how long a launch holds the per-cwd discovery lock
	// (default 15 s); discovery itself goes on while the tool runs.
	DiscoverFor time.Duration
	Clock       func() time.Time
	// Hooks returns the tool arguments that make the tool report its state
	// to cairn event (hooks.Inject); nil or an error runs the tool without,
	// and its state is then only observed.
	Hooks func(adapter.Adapter, conv.Conversation) ([]string, error)
	// Activity returns when the pane last showed output (nil: no idle
	// observation); polled every ObservePoll (default 10 s) while the tool
	// runs, for Observe.
	Activity    func() (time.Time, error)
	ObservePoll time.Duration
	// Queue, when set, is the concurrency limit a launch waits for.
	Queue *task.Queue
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

// AcquireConvLocks takes the conversation's locks: always the one on
// cairn's id (stable for the whole life of the conversation), and also the
// one on the tool's own id when it is known, so two records of one native
// conversation exclude each other. Either held is conv.ErrLocked.
func AcquireConvLocks(s conv.Store, c conv.Conversation) (release func(), err error) {
	rel, err := s.AcquireLock(c.Tool, "cairn-"+c.ID)
	if err != nil {
		return nil, err
	}
	if c.NativeID == "" {
		return rel, nil
	}
	relNative, err := s.AcquireLock(c.Tool, c.NativeID)
	if err != nil {
		rel()
		return nil, err
	}
	return func() { relNative(); rel() }, nil
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
		if env.Queue != nil {
			// This tool no longer takes a slot: the next queued one may.
			_, _, _ = Promote(env.Store, *env.Queue, env.now(), 2*time.Second)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
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
	release, err := AcquireConvLocks(env.Store, c)
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
	if env.Queue != nil {
		if c, ok = waitSlot(ctx, env, c); !ok {
			return c
		}
	}

	var profile []string
	if env.ProfileArgs != nil {
		if profile, err = env.ProfileArgs(c, a); err != nil {
			env.printf("%s: not started: %v\n", title(c), err)
			return c
		}
	}
	var argv []string
	var disc *Discovery
	if ShouldResume(env.Root, env.Home, a, c) {
		argv, err = BuildResume(a, c, profile)
	} else {
		if c.NativeID == "" && len(c.LaunchArgv) > 0 {
			env.printf("the tool's id for %s is unknown; starting a new conversation\n", title(c))
		}
		argv, disc, err = BuildStart(a, c, profile)
	}
	if err != nil {
		env.printf("%s: %v\n", title(c), err)
		return c
	}
	argv = withHooks(env, a, c, argv)

	var d *discovery
	if disc != nil {
		if d, err = startDiscovery(ctx, env, a, c); err != nil {
			return c // ctx done while waiting: the tool is not started
		}
		defer d.releaseNative()
	}
	started := env.now()
	if c, err = env.Store.Update(id, func(r *conv.Conversation) error {
		r.LaunchArgv = argv
		r.LastActivityAt = started
		if r.ArchivedAt == nil {
			// Whatever an earlier run left (even a hook state after a
			// crash), a launch starts over; hooks take it from here.
			r.State, r.StateSource, r.WaitingReason = conv.StateStarting, conv.SourceObserved, ""
		}
		return nil
	}); err != nil {
		env.printf("%s: %v\n", id, err)
		if d != nil {
			d.stop()
		}
		return conv.Conversation{ID: id}
	}
	if c.Cwd != "" {
		if fi, err := os.Stat(c.Cwd); err != nil || !fi.IsDir() {
			wd, _ := os.Getwd()
			env.printf("note: the working directory %s is missing; the tool starts in %s\n", c.Cwd, wd)
		}
	}
	stopObserve := observe(env, a, id)
	code, runErr := env.Run(context.WithoutCancel(ctx), argv, toolEnv(a, c), c.Cwd)
	stopObserve()
	if d != nil {
		d.stop()
	}
	status := code
	if runErr != nil {
		env.printf("%s: %v\n", strings.Join(argv, " "), runErr)
		status = -1
	}
	env.printf("\n%s exited (status %d)\n", title(c), code)
	if u, err := env.Store.Update(id, func(r *conv.Conversation) error {
		*r = Observe(*r, true, status, 0, env.now())
		r.LastActivityAt = env.now()
		return nil
	}); err == nil {
		c = u
	}
	return c
}

// waitSlot holds the launch, with the conversation lock taken, until the
// concurrency queue admits it (AdmitLaunch), polling every env.Poll and
// showing the queue position when it changes. false when ctx ended or the
// store failed: the tool is not started.
func waitSlot(ctx context.Context, env PaneEnv, c conv.Conversation) (conv.Conversation, bool) {
	shown := -1
	for {
		ok, pos, err := AdmitLaunch(env.Store, *env.Queue, c.ID, env.now(), shown >= 0)
		if errors.Is(err, ErrDequeued) {
			env.printf("%s left the queue; its tool was not started\n", title(c))
			return c, false
		}
		if err != nil {
			env.printf("%s: %v\n", title(c), err)
			return c, false
		}
		if ok {
			if shown >= 0 {
				env.printf("a slot is free; starting %s\n", title(c))
			}
			if u, found, err := env.Store.Get(c.ID); err == nil && found {
				c = u
			}
			return c, true
		}
		if pos != shown {
			env.printf("queued: position %d (at most %d agents run at once); %s starts when one finishes\n", pos, env.Queue.Max, title(c))
			shown = pos
		}
		select {
		case <-ctx.Done():
			return c, false
		case <-time.After(env.Poll):
		}
	}
}

// withHooks puts the tool arguments of env.Hooks right after the binary
// (argv[0]), where every tool reads options. A failure is reported and the
// tool runs without hooks: its state is then observed.
func withHooks(env PaneEnv, a adapter.Adapter, c conv.Conversation, argv []string) []string {
	if env.Hooks == nil || len(argv) == 0 {
		return argv
	}
	extra, err := env.Hooks(a, c)
	if err != nil {
		env.printf("note: state hooks for %s are unavailable (%v); its state is observed instead\n", title(c), err)
		return argv
	}
	return WithHookArgs(argv, extra)
}

// WithHookArgs puts hook arguments right after the binary (argv[0]), before
// any subcommand, where every tool reads options.
func WithHookArgs(argv, extra []string) []string {
	if len(extra) == 0 || len(argv) == 0 {
		return argv
	}
	out := make([]string, 0, len(argv)+len(extra))
	out = append(out, argv[0])
	out = append(out, extra...)
	return append(out, argv[1:]...)
}

// observe polls env.Activity while the tool runs and applies Observe for
// an idle or active pane; records are written only on a change. The
// returned func stops it and waits.
func observe(env PaneEnv, a adapter.Adapter, id string) (stop func()) {
	// Hooks that never report working (codex notify: turn complete only)
	// cannot end their own idle; then fresh output may.
	reviveIdle := !a.HooksReport(conv.StateWorking)
	if env.Activity == nil {
		return func() {}
	}
	poll := env.ObservePoll
	if poll <= 0 {
		poll = 10 * time.Second
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(poll)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
			}
			last, err := env.Activity()
			if err != nil {
				continue
			}
			now := env.now()
			_, _ = env.Store.Update(id, func(r *conv.Conversation) error {
				in := *r
				if reviveIdle && in.StateSource == conv.SourceHook && in.State == conv.StateIdle {
					in.StateSource = conv.SourceObserved
				}
				o := Observe(in, false, 0, now.Sub(last), now)
				if o.State == r.State {
					return conv.ErrNoChange
				}
				*r = o
				return nil
			})
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done); wg.Wait() }) }
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

// discovery finds the native id of a conversation started without one.
type discovery struct {
	env     PaneEnv
	a       adapter.Adapter
	c       conv.Conversation
	before  Snapshot
	started time.Time

	mu         sync.Mutex
	found      bool
	relNative  func()
	releaseCwd func()
	done       chan struct{}
	finished   chan struct{}
}

// startDiscovery serialises launches in one cwd with a short lock (waiting
// for another launch is ctx-aware and announced), snapshots, and polls
// Discover for as long as the tool runs. The cwd lock is released once the
// id is found or after DiscoverFor (default 15 s); polling continues, and
// the ambiguity rule keeps a later overlap from guessing. A found id is
// recorded at once and its native lock taken. An error means ctx ended
// while waiting.
func startDiscovery(ctx context.Context, env PaneEnv, a adapter.Adapter, c conv.Conversation) (*discovery, error) {
	sum := sha256.Sum256([]byte(c.Cwd))
	key := hex.EncodeToString(sum[:8])
	d := &discovery{env: env, a: a, c: c, done: make(chan struct{}), finished: make(chan struct{})}
	announced := false
	for deadline := time.Now().Add(10 * time.Second); ; {
		r, err := env.Store.AcquireLock("discover", key)
		if err == nil {
			d.releaseCwd = r
			break
		}
		if !announced {
			env.printf("waiting for another launch in %s to be identified\n", c.Cwd)
			announced = true
		}
		if time.Now().After(deadline) {
			env.printf("still busy; %s starts now and its id may stay unknown\n", title(c))
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	before, err := TakeSnapshot(ctx, env.Root, a, env.Runner, env.Home)
	if err != nil {
		d.releaseCwdLock()
		env.printf("discovery for %s unavailable: %v\n", title(c), err)
		close(d.finished)
		d.found = true // nothing more to try
		return d, nil
	}
	d.before, d.started = before, env.now()
	window := env.DiscoverFor
	if window <= 0 {
		window = 15 * time.Second
	}
	go func() {
		defer close(d.finished)
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		cwdWindow := time.After(window)
		for {
			select {
			case <-d.done:
				return
			case <-cwdWindow:
				d.releaseCwdLock()
				continue
			case <-t.C:
			}
			if d.try(ctx) {
				return
			}
		}
	}()
	return d, nil
}

// try runs Discover once; true when finished (found or a hard error).
func (d *discovery) try(ctx context.Context) bool {
	id, err := Discover(ctx, d.env.Root, d.a, d.env.Runner, d.before, d.started, d.c.Cwd)
	switch {
	case err == nil:
		d.record(id)
		return true
	case errors.Is(err, ErrNotDiscovered):
		return false
	default:
		d.env.printf("could not identify the conversation of %s: %v\n", title(d.c), err)
		d.mu.Lock()
		d.found = true
		d.mu.Unlock()
		d.releaseCwdLock()
		return true
	}
}

func (d *discovery) record(id string) {
	_, _ = d.env.Store.Update(d.c.ID, func(r *conv.Conversation) error { r.NativeID = id; return nil })
	rel, err := d.env.Store.AcquireLock(d.c.Tool, id)
	if err != nil {
		d.env.printf("warning: the conversation %s is also open elsewhere: %v\n", id, err)
	}
	d.mu.Lock()
	d.found, d.relNative = true, rel
	d.mu.Unlock()
	d.releaseCwdLock()
}

func (d *discovery) releaseCwdLock() {
	d.mu.Lock()
	r := d.releaseCwd
	d.releaseCwd = nil
	d.mu.Unlock()
	if r != nil {
		r()
	}
}

// stop ends polling after the tool exited and makes one last attempt, for a
// tool that wrote its session just before exiting.
func (d *discovery) stop() {
	select {
	case <-d.done:
	default:
		close(d.done)
	}
	<-d.finished
	d.mu.Lock()
	found := d.found
	d.mu.Unlock()
	if !found {
		if !d.try(context.Background()) {
			d.env.printf("the tool's id for %s was not found; it will start as a new conversation next time\n", title(d.c))
		}
	}
	d.releaseCwdLock()
}

func (d *discovery) releaseNative() {
	d.mu.Lock()
	r := d.relNative
	d.relNative = nil
	d.mu.Unlock()
	if r != nil {
		r()
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
