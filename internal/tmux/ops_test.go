package tmux

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mrqwer/cairn/internal/execx"
)

func fake(resp map[string]string) *execx.FakeRunner {
	f := &execx.FakeRunner{Responses: map[string]execx.Response{}}
	for k, v := range resp {
		f.Responses[k] = execx.Response{Out: []byte(v)}
	}
	return f
}

// okCalls registers empty successful responses for each command line.
func okCalls(f *execx.FakeRunner, calls [][]string) {
	for _, c := range calls {
		k := strings.Join(c, " ")
		if _, ok := f.Responses[k]; !ok {
			f.Responses[k] = execx.Response{}
		}
	}
}

func TestNewSessionArgs(t *testing.T) {
	want := [][]string{
		{"tmux", "-u", "-L", "s", "new-session", "-d", "-P", "-F", "#{session_id} #{window_id} #{pane_id}",
			"-s", "demo", "-x", "200", "-y", "50", "-n", "editor", "-c", "/home/u/demo",
			"-e", "A=1", "-e", "B=2"},
		{"tmux", "-u", "-L", "s", "set", "-p", "-t", "%9", "remain-on-exit", "on"},
		{"tmux", "-u", "-L", "s", "set", "-t", "$3", "@cairn_s", "demo"},
		{"tmux", "-u", "-L", "s", "set", "-w", "-t", "@7", "@cairn_w", "demo/editor"},
		{"tmux", "-u", "-L", "s", "set", "-w", "-t", "@7", "automatic-rename", "off"},
		{"tmux", "-u", "-L", "s", "set", "-p", "-t", "%9", "@cairn_p", "demo/editor/1"},
		{"tmux", "-u", "-L", "s", "respawn-pane", "-k", "-t", "%9", "-c", "/home/u/demo",
			"-e", "A=1", "-e", "B=2", "--", "sh", "-c", `echo "a b" $HOME`},
	}
	f := fake(map[string]string{strings.Join(want[0], " "): "$3 @7 %9\n"})
	okCalls(f, want[1:])
	c := Client{R: f, Socket: "s"}
	sid, wid, pid, err := c.NewSession(context.Background(), "demo", 0, 0, WindowSpec{
		Key: "demo/editor", Name: "editor",
		PaneSpec: PaneSpec{
			Key: "demo/editor/1", Cwd: "/home/u/demo",
			Argv: []string{"sh", "-c", `echo "a b" $HOME`},
			Env:  map[string]string{"B": "2", "A": "1"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if sid != "$3" || wid != "@7" || pid != "%9" {
		t.Fatalf("ids %q %q %q", sid, wid, pid)
	}
	if !reflect.DeepEqual(f.Calls, want) {
		t.Fatalf("calls\n got %q\nwant %q", f.Calls, want)
	}
	for _, call := range f.Calls {
		if slices.Contains(call, "send-keys") {
			t.Fatalf("send-keys used: %q", call)
		}
	}
}

func TestNewSessionExplicitSize(t *testing.T) {
	want := [][]string{
		{"tmux", "-u", "new-session", "-d", "-P", "-F", "#{session_id} #{window_id} #{pane_id}", "-s", "demo", "-x", "120", "-y", "40", "-n", "main"},
		{"tmux", "-u", "set", "-t", "$0", "@cairn_s", "demo"},
		{"tmux", "-u", "set", "-w", "-t", "@1", "@cairn_w", "demo/main"},
		{"tmux", "-u", "set", "-w", "-t", "@1", "automatic-rename", "off"},
	}
	f := fake(map[string]string{strings.Join(want[0], " "): "$0 @1 %2"})
	okCalls(f, want[1:])
	if _, _, _, err := (Client{R: f}).NewSession(context.Background(), "demo", 120, 40, WindowSpec{Key: "demo/main", Name: "main"}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.Calls, want) {
		t.Fatalf("calls\n got %q\nwant %q", f.Calls, want)
	}
}

func TestEscSemi(t *testing.T) {
	// tmux drops a trailing ';' as a command separator; one '\' before it keeps
	// it, and a '\' already there must survive (verified on 3.8).
	cases := map[string]string{
		"":     "",
		"a":    "a",
		`a\`:   `a\`,
		"a;b":  "a;b",
		"a;":   `a\;`,
		";":    `\;`,
		"a;;":  `a;\;`,
		`a\;`:  `a\\;`,
		`a\\;`: `a\\\;`,
	}
	for in, want := range cases {
		if got := escSemi(in); got != want {
			t.Errorf("escSemi(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUserValuesEscaped(t *testing.T) {
	want := [][]string{
		{"tmux", "-u", "new-session", "-d", "-P", "-F", "#{session_id} #{window_id} #{pane_id}",
			"-s", `s##1\;`, "-x", "200", "-y", "50", "-n", `a##{x}\;`, "-c", `/home/u/c##p\;`,
			"-e", `K=#v\;`},
		{"tmux", "-u", "set", "-p", "-t", "%2", "remain-on-exit", "on"},
		{"tmux", "-u", "set", "-t", "$0", "@cairn_s", "demo"},
		{"tmux", "-u", "set", "-w", "-t", "@1", "@cairn_w", "demo/main"},
		{"tmux", "-u", "set", "-w", "-t", "@1", "automatic-rename", "off"},
		{"tmux", "-u", "set", "-p", "-t", "%2", "@cairn_p", "demo/main/1"},
		{"tmux", "-u", "respawn-pane", "-k", "-t", "%2", "-c", `/home/u/c##p\;`,
			"-e", `K=#v\;`, "--", "sh", "-c", `echo hi\;`, `#{x}`},
	}
	f := fake(map[string]string{strings.Join(want[0], " "): "$0 @1 %2"})
	okCalls(f, want[1:])
	_, _, _, err := Client{R: f}.NewSession(context.Background(), "s#1;", 0, 0, WindowSpec{
		Key: "demo/main", Name: "a#{x};",
		PaneSpec: PaneSpec{Key: "demo/main/1", Cwd: "/home/u/c#p;", Env: map[string]string{"K": "#v;"},
			Argv: []string{"sh", "-c", "echo hi;", "#{x}"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.Calls, want) {
		t.Fatalf("calls\n got %q\nwant %q", f.Calls, want)
	}
}

func TestKeyAndArgvValidation(t *testing.T) {
	ctx := context.Background()
	f := &execx.FakeRunner{}
	c := Client{R: f}
	for _, key := range []string{"", "demo", "demo/a/b", "/a", "demo/"} {
		if _, _, _, err := c.NewSession(ctx, "demo", 0, 0, WindowSpec{Key: key, Name: "a"}); err == nil {
			t.Errorf("NewSession key %q accepted", key)
		}
		if _, _, err := c.NewWindow(ctx, "$0", WindowSpec{Key: key, Name: "a"}); err == nil {
			t.Errorf("NewWindow key %q accepted", key)
		}
	}
	bad := PaneSpec{Key: "demo/a/1", Argv: []string{"echo", ""}}
	if _, _, _, err := c.NewSession(ctx, "demo", 0, 0, WindowSpec{Key: "demo/a", Name: "a", PaneSpec: bad}); err == nil {
		t.Error("empty argv element accepted by NewSession")
	}
	if _, _, err := c.NewWindow(ctx, "$0", WindowSpec{Key: "demo/a", Name: "a", PaneSpec: bad}); err == nil {
		t.Error("empty argv element accepted by NewWindow")
	}
	if _, err := c.SplitPane(ctx, "%1", "h", 50, bad); err == nil {
		t.Error("empty argv element accepted by SplitPane")
	}
	if err := c.RespawnPane(ctx, "%1", []string{""}); err == nil {
		t.Error("empty argv element accepted by RespawnPane")
	}
	if _, err := c.SplitPane(ctx, "%1", "h", 50, PaneSpec{Key: "demo/a"}); err == nil {
		t.Error("pane key with one slash accepted")
	}
	if len(f.Calls) != 0 {
		t.Fatalf("tmux called: %q", f.Calls)
	}
}

func TestSessionIDByKey(t *testing.T) {
	f := snapFake(
		execx.Response{Out: []byte("$0\tdemo-old\t\t0\n$4\tdemo\tdemo\t0\n")},
		execx.Response{}, execx.Response{},
	)
	c := Client{R: f, Socket: "s"}
	id, ok, err := c.SessionIDByKey(context.Background(), "demo")
	if err != nil || !ok || id != "$4" {
		t.Fatalf("%q %v %v", id, ok, err)
	}
	if _, ok, err := c.SessionIDByKey(context.Background(), "web"); err != nil || ok {
		t.Fatalf("web: %v %v", ok, err)
	}
}

func TestNewWindowArgs(t *testing.T) {
	want := [][]string{
		{"tmux", "-u", "-L", "s", "new-window", "-d", "-P", "-F", "#{window_id} #{pane_id}", "-t", "$3:", "-n", "logs",
			"-c", "/home/u/demo"},
		{"tmux", "-u", "-L", "s", "set", "-p", "-t", "%4", "remain-on-exit", "on"},
		{"tmux", "-u", "-L", "s", "set", "-w", "-t", "@2", "@cairn_w", "demo/logs"},
		{"tmux", "-u", "-L", "s", "set", "-w", "-t", "@2", "automatic-rename", "off"},
		{"tmux", "-u", "-L", "s", "set", "-p", "-t", "%4", "@cairn_p", "demo/logs/1"},
		{"tmux", "-u", "-L", "s", "respawn-pane", "-k", "-t", "%4", "-c", "/home/u/demo", "--", "tail", "-f", "log.txt"},
	}
	f := fake(map[string]string{strings.Join(want[0], " "): "@2 %4\n"})
	okCalls(f, want[1:])
	wid, pid, err := Client{R: f, Socket: "s"}.NewWindow(context.Background(), "$3", WindowSpec{
		Key: "demo/logs", Name: "logs",
		PaneSpec: PaneSpec{Key: "demo/logs/1", Cwd: "/home/u/demo", Argv: []string{"tail", "-f", "log.txt"}},
	})
	if err != nil || wid != "@2" || pid != "%4" {
		t.Fatalf("%q %q %v", wid, pid, err)
	}
	if !reflect.DeepEqual(f.Calls, want) {
		t.Fatalf("calls\n got %q\nwant %q", f.Calls, want)
	}
}

func TestSingleElementRunIsShellString(t *testing.T) {
	want := [][]string{
		{"tmux", "-u", "-L", "s", "split-window", "-d", "-P", "-F", "#{pane_id}", "-t", "%9", "-v", "-l", "30%",
			"-c", "/home/u/demo"},
		{"tmux", "-u", "-L", "s", "set", "-p", "-t", "%10", "remain-on-exit", "on"},
		{"tmux", "-u", "-L", "s", "set", "-p", "-t", "%10", "@cairn_p", "demo/editor/2"},
		{"tmux", "-u", "-L", "s", "respawn-pane", "-k", "-t", "%10", "-c", "/home/u/demo", "--", "npm run dev && echo 'done'"},
	}
	f := fake(map[string]string{strings.Join(want[0], " "): "%10\n"})
	okCalls(f, want[1:])
	id, err := Client{R: f, Socket: "s"}.SplitPane(context.Background(), "%9", "v", 30,
		PaneSpec{Key: "demo/editor/2", Cwd: "/home/u/demo", Argv: []string{"npm run dev && echo 'done'"}})
	if err != nil || id != "%10" {
		t.Fatalf("%q %v", id, err)
	}
	if !reflect.DeepEqual(f.Calls, want) {
		t.Fatalf("calls\n got %q\nwant %q", f.Calls, want)
	}
}

func TestSplitPaneWithoutCommand(t *testing.T) {
	want := [][]string{
		{"tmux", "-u", "split-window", "-d", "-P", "-F", "#{pane_id}", "-t", "%1", "-h", "-l", "50%", "-c", "/home/u", "-e", "K=v"},
		{"tmux", "-u", "set", "-p", "-t", "%2", "remain-on-exit", "on"},
	}
	f := fake(map[string]string{strings.Join(want[0], " "): "%2\n"})
	okCalls(f, want[1:])
	if _, err := (Client{R: f}).SplitPane(context.Background(), "%1", "h", 50,
		PaneSpec{Cwd: "/home/u", Env: map[string]string{"K": "v"}, RemainOnExit: true}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.Calls, want) {
		t.Fatalf("calls\n got %q\nwant %q", f.Calls, want)
	}
}

func TestSplitPaneBadDir(t *testing.T) {
	f := &execx.FakeRunner{}
	if _, err := (Client{R: f}).SplitPane(context.Background(), "%1", "x", 50, PaneSpec{}); err == nil || len(f.Calls) != 0 {
		t.Fatalf("err %v calls %v", err, f.Calls)
	}
}

func TestSimpleOps(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		run  func(c Client) error
		want []string
	}{
		{func(c Client) error { return c.SelectLayout(ctx, "@1", "abcd,80x24,0,0,1") }, []string{"select-layout", "-t", "@1", "abcd,80x24,0,0,1"}},
		{func(c Client) error { return c.SetOption(ctx, "session", "$1", "@cairn_s", "demo") }, []string{"set", "-t", "$1", "@cairn_s", "demo"}},
		{func(c Client) error { return c.SetOption(ctx, "window", "@1", "@cairn_w", "demo/a") }, []string{"set", "-w", "-t", "@1", "@cairn_w", "demo/a"}},
		{func(c Client) error { return c.SetOption(ctx, "pane", "%1", "@cairn_p", "demo/a/1") }, []string{"set", "-p", "-t", "%1", "@cairn_p", "demo/a/1"}},
		{func(c Client) error { return c.UnsetOption(ctx, "session", "$1", "@cairn_s") }, []string{"set", "-u", "-t", "$1", "@cairn_s"}},
		{func(c Client) error { return c.UnsetOption(ctx, "window", "@1", "@cairn_w") }, []string{"set", "-u", "-w", "-t", "@1", "@cairn_w"}},
		{func(c Client) error { return c.UnsetOption(ctx, "pane", "%1", "@cairn_p") }, []string{"set", "-u", "-p", "-t", "%1", "@cairn_p"}},
		{func(c Client) error { return c.RespawnPane(ctx, "%1", nil) }, []string{"respawn-pane", "-t", "%1"}},
		{func(c Client) error { return c.RespawnPane(ctx, "%1", []string{"go", "run", "."}) }, []string{"respawn-pane", "-t", "%1", "--", "go", "run", "."}},
		{func(c Client) error { return c.SwapPanes(ctx, "%1", "%2") }, []string{"swap-pane", "-d", "-s", "%1", "-t", "%2"}},
		{func(c Client) error { return c.KillWindow(ctx, "@1") }, []string{"kill-window", "-t", "@1"}},
		{func(c Client) error { return c.KillPane(ctx, "%1") }, []string{"kill-pane", "-t", "%1"}},
		{func(c Client) error { return c.KillSession(ctx, "$1") }, []string{"kill-session", "-t", "$1"}},
		{func(c Client) error { return c.SetOption(ctx, "pane", "%1", "@x", "v;") }, []string{"set", "-p", "-t", "%1", "@x", `v\;`}},
		{func(c Client) error { return c.RespawnPane(ctx, "%1", []string{"echo", "a;"}) }, []string{"respawn-pane", "-t", "%1", "--", "echo", `a\;`}},
		{func(c Client) error { return c.SwitchClient(ctx, "demo") }, []string{"switch-client", "-t", "=demo"}},
		{func(c Client) error { return c.SwitchClient(ctx, "$4") }, []string{"switch-client", "-t", "$4"}},
	}
	for _, tc := range cases {
		want := append([]string{"tmux", "-u", "-L", "s"}, tc.want...)
		f := &execx.FakeRunner{Responses: map[string]execx.Response{strings.Join(want, " "): {}}}
		if err := tc.run(Client{R: f, Socket: "s"}); err != nil {
			t.Fatalf("%q: %v", want, err)
		}
		if !reflect.DeepEqual(f.Calls, [][]string{want}) {
			t.Fatalf("calls %q want %q", f.Calls, want)
		}
	}
	if err := (Client{R: &execx.FakeRunner{}}).SetOption(ctx, "global", "x", "a", "b"); err == nil {
		t.Fatal("bad scope accepted")
	}
	if err := (Client{R: &execx.FakeRunner{}}).UnsetOption(ctx, "global", "x", "a"); err == nil {
		t.Fatal("bad unset scope accepted")
	}
}

var errExit = errors.New("exit status 1")

func TestOpErrorIncludesOutput(t *testing.T) {
	f := &execx.FakeRunner{Responses: map[string]execx.Response{
		"tmux -u kill-pane -t %1": {Out: []byte("can't find pane: %1\n"), Err: errExit},
	}}
	err := Client{R: f}.KillPane(context.Background(), "%1")
	if err == nil || !strings.Contains(err.Error(), "can't find pane") {
		t.Fatalf("err %v", err)
	}
}

func TestEnvStripped(t *testing.T) {
	t.Setenv("TMUX", "/tmp/tmux-1000/default,1,0")
	t.Setenv("TMUX_PANE", "%1")
	t.Setenv("CAIRN_KEEP", "1")
	check := func(r execx.Runner) {
		t.Helper()
		os, ok := r.(execx.OSRunner)
		if !ok {
			t.Fatalf("runner %T", r)
		}
		if !slices.Contains(os.Env, "CAIRN_KEEP=1") {
			t.Fatalf("env lost: %q", os.Env)
		}
		for _, kv := range os.Env {
			if strings.HasPrefix(kv, "TMUX=") || strings.HasPrefix(kv, "TMUX_PANE=") {
				t.Fatalf("leaked %q", kv)
			}
		}
	}
	c := NewClient("s")
	if c.Socket != "s" {
		t.Fatalf("socket %q", c.Socket)
	}
	check(c.R)
	check(c.runner())
	// A plain OSRunner (nil Env) is cleaned at call time too.
	check(Client{R: execx.OSRunner{}}.runner())
	check(Client{R: execx.OSRunner{Env: []string{"CAIRN_KEEP=1", "TMUX=x"}}}.runner())
}

func TestAttachCommand(t *testing.T) {
	if got := AttachCommand("s", "demo"); !reflect.DeepEqual(got, []string{"tmux", "-L", "s", "attach-session", "-t", "=demo"}) {
		t.Fatalf("%q", got)
	}
	if got := AttachArgs("", "demo"); !reflect.DeepEqual(got, []string{"tmux", "attach-session", "-t", "=demo"}) {
		t.Fatalf("%q", got)
	}
}

// cancelRunner wraps a FakeRunner: it cancels the caller's context when it
// sees a command starting with cancelOn, and fails every later command whose
// context is already cancelled (as exec.CommandContext would).
type cancelRunner struct {
	f        *execx.FakeRunner
	cancelOn string
	cancel   context.CancelFunc
}

func (r *cancelRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		r.f.Calls = append(r.f.Calls, append([]string{name}, args...))
		return nil, err
	}
	out, err := r.f.Run(ctx, name, args...)
	if len(args) > 1 && args[0] == "-u" && args[1] == r.cancelOn {
		r.cancel()
	}
	return out, err
}

func TestNewSessionKillsSessionWhenMarkerFails(t *testing.T) {
	newSess := []string{"tmux", "-u", "new-session", "-d", "-P", "-F", "#{session_id} #{window_id} #{pane_id}", "-s", "demo", "-x", "200", "-y", "50", "-n", "main"}
	marker := []string{"tmux", "-u", "set", "-t", "$3", "@cairn_s", "demo"}
	kill := []string{"tmux", "-u", "kill-session", "-t", "$3"}
	f := fake(map[string]string{strings.Join(newSess, " "): "$3 @7 %9"})
	f.Responses[strings.Join(marker, " ")] = execx.Response{Err: errors.New("boom")}
	okCalls(f, [][]string{kill})
	_, _, _, err := Client{R: f}.NewSession(context.Background(), "demo", 0, 0, WindowSpec{Key: "demo/main", Name: "main"})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err %v", err)
	}
	if want := [][]string{newSess, marker, kill}; !reflect.DeepEqual(f.Calls, want) {
		t.Fatalf("calls\n got %q\nwant %q", f.Calls, want)
	}
}

func TestNewSessionCancelledAfterCreateCleansUp(t *testing.T) {
	newSess := []string{"tmux", "-u", "new-session", "-d", "-P", "-F", "#{session_id} #{window_id} #{pane_id}", "-s", "demo", "-x", "200", "-y", "50", "-n", "main"}
	rest := [][]string{
		{"tmux", "-u", "set", "-t", "$3", "@cairn_s", "demo"},
		{"tmux", "-u", "set", "-w", "-t", "@7", "@cairn_w", "demo/main"},
		{"tmux", "-u", "set", "-w", "-t", "@7", "automatic-rename", "off"},
		{"tmux", "-u", "kill-session", "-t", "$3"},
	}
	f := fake(map[string]string{strings.Join(newSess, " "): "$3 @7 %9"})
	okCalls(f, rest)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &cancelRunner{f: f, cancelOn: "new-session", cancel: cancel}
	_, _, _, err := Client{R: r}.NewSession(ctx, "demo", 0, 0, WindowSpec{Key: "demo/main", Name: "main"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	// The marker steps run detached from the cancellation; the session is
	// then removed, so no unmarked session is left behind.
	if want := append([][]string{newSess}, rest...); !reflect.DeepEqual(f.Calls, want) {
		t.Fatalf("calls\n got %q\nwant %q", f.Calls, want)
	}
}
