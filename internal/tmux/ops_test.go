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
		{"tmux", "-L", "s", "new-session", "-d", "-P", "-F", "#{session_id} #{window_id} #{pane_id}",
			"-s", "demo", "-x", "200", "-y", "50", "-n", "editor", "-c", "/home/u/demo",
			"-e", "A=1", "-e", "B=2", "--", "sh", "-c", `echo "a b" $HOME`},
		{"tmux", "-L", "s", "set", "-p", "-t", "%9", "remain-on-exit", "on"},
		{"tmux", "-L", "s", "set", "-t", "$3", "@cairn_s", "demo"},
		{"tmux", "-L", "s", "set", "-w", "-t", "@7", "@cairn_w", "demo/editor"},
		{"tmux", "-L", "s", "set", "-w", "-t", "@7", "automatic-rename", "off"},
		{"tmux", "-L", "s", "set", "-p", "-t", "%9", "@cairn_p", "demo/editor/1"},
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
	first := "tmux new-session -d -P -F #{session_id} #{window_id} #{pane_id} -s demo -x 120 -y 40 -n main"
	want := [][]string{
		{"tmux", "new-session", "-d", "-P", "-F", "#{session_id} #{window_id} #{pane_id}", "-s", "demo", "-x", "120", "-y", "40", "-n", "main"},
		{"tmux", "set", "-w", "-t", "@1", "automatic-rename", "off"},
	}
	f := fake(map[string]string{first: "$0 @1 %2"})
	okCalls(f, want[1:])
	if _, _, _, err := (Client{R: f}).NewSession(context.Background(), "demo", 120, 40, WindowSpec{Name: "main"}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.Calls, want) {
		t.Fatalf("calls\n got %q\nwant %q", f.Calls, want)
	}
}

func TestNewWindowArgs(t *testing.T) {
	want := [][]string{
		{"tmux", "-L", "s", "new-window", "-d", "-P", "-F", "#{window_id} #{pane_id}", "-t", "$3:", "-n", "logs",
			"-c", "/home/u/demo", "--", "tail", "-f", "log.txt"},
		{"tmux", "-L", "s", "set", "-p", "-t", "%4", "remain-on-exit", "on"},
		{"tmux", "-L", "s", "set", "-w", "-t", "@2", "@cairn_w", "demo/logs"},
		{"tmux", "-L", "s", "set", "-w", "-t", "@2", "automatic-rename", "off"},
		{"tmux", "-L", "s", "set", "-p", "-t", "%4", "@cairn_p", "demo/logs/1"},
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
		{"tmux", "-L", "s", "split-window", "-d", "-P", "-F", "#{pane_id}", "-t", "%9", "-v", "-l", "30%",
			"-c", "/home/u/demo", "--", "npm run dev && echo 'done'"},
		{"tmux", "-L", "s", "set", "-p", "-t", "%10", "remain-on-exit", "on"},
		{"tmux", "-L", "s", "set", "-p", "-t", "%10", "@cairn_p", "demo/editor/2"},
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
		{"tmux", "split-window", "-d", "-P", "-F", "#{pane_id}", "-t", "%1", "-h", "-l", "50%", "-c", "/home/u", "-e", "K=v"},
		{"tmux", "set", "-p", "-t", "%2", "remain-on-exit", "on"},
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
		{func(c Client) error { return c.RespawnPane(ctx, "%1", nil) }, []string{"respawn-pane", "-k", "-t", "%1"}},
		{func(c Client) error { return c.RespawnPane(ctx, "%1", []string{"go", "run", "."}) }, []string{"respawn-pane", "-k", "-t", "%1", "--", "go", "run", "."}},
		{func(c Client) error { return c.SwapPanes(ctx, "%1", "%2") }, []string{"swap-pane", "-d", "-s", "%1", "-t", "%2"}},
		{func(c Client) error { return c.KillWindow(ctx, "@1") }, []string{"kill-window", "-t", "@1"}},
		{func(c Client) error { return c.KillPane(ctx, "%1") }, []string{"kill-pane", "-t", "%1"}},
		{func(c Client) error { return c.KillSession(ctx, "$1") }, []string{"kill-session", "-t", "$1"}},
		{func(c Client) error { return c.SwitchClient(ctx, "demo") }, []string{"switch-client", "-t", "demo"}},
	}
	for _, tc := range cases {
		want := append([]string{"tmux", "-L", "s"}, tc.want...)
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
}

var errExit = errors.New("exit status 1")

func TestOpErrorIncludesOutput(t *testing.T) {
	f := &execx.FakeRunner{Responses: map[string]execx.Response{
		"tmux kill-pane -t %1": {Out: []byte("can't find pane: %1\n"), Err: errExit},
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
	if got := AttachCommand("s", "demo"); !reflect.DeepEqual(got, []string{"tmux", "-L", "s", "attach-session", "-t", "demo"}) {
		t.Fatalf("%q", got)
	}
	if got := AttachArgs("", "demo"); !reflect.DeepEqual(got, []string{"tmux", "attach-session", "-t", "demo"}) {
		t.Fatalf("%q", got)
	}
}
