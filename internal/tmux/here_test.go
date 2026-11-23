package tmux

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/mrqwer/cairn/internal/execx"
)

func TestSocketPath(t *testing.T) {
	ctx := context.Background()
	f := fake(map[string]string{"tmux -u -L s display-message -p #{socket_path}": "/tmp/tmux-1/s\n"})
	p, err := Client{R: f, Socket: "s"}.SocketPath(ctx)
	if err != nil || p != "/tmp/tmux-1/s" {
		t.Fatalf("SocketPath = %q, %v", p, err)
	}
	f = &execx.FakeRunner{Responses: map[string]execx.Response{
		"tmux -u -L s display-message -p #{socket_path}": {Out: []byte("no server running on /tmp/tmux-1/s\n"), Err: errors.New("exit status 1")},
	}}
	if _, err := (Client{R: f, Socket: "s"}).SocketPath(ctx); err == nil || !strings.Contains(err.Error(), "no tmux server") {
		t.Fatalf("no server: %v", err)
	}
}

func TestSwitchClientHereArgs(t *testing.T) {
	ctx := context.Background()
	f := &execx.FakeRunner{Responses: map[string]execx.Response{"tmux -u -L s switch-client -t $4": {}}}
	if err := (Client{R: f, Socket: "s"}).SwitchClientHere(ctx, "$4", "/tmp/tmux-1/s,10,0", "%2"); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("calls %v", f.Calls)
	}
	for _, bad := range []struct{ id, env string }{{"demo", "/tmp/tmux-1/s,10,0"}, {"$4", ""}} {
		if err := (Client{R: f, Socket: "s"}).SwitchClientHere(ctx, bad.id, bad.env, ""); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	if len(f.Calls) != 1 {
		t.Fatalf("refused calls ran: %v", f.Calls)
	}
}

// The switch runs with the invoking client's TMUX and TMUX_PANE (exactly
// once each), everything else of the environment kept.
func TestSwitchClientHereEnv(t *testing.T) {
	t.Setenv("TMUX", "/tmp/tmux-1/other,1,0")
	t.Setenv("TMUX_PANE", "%9")
	t.Setenv("CAIRN_KEEP", "1")
	for _, c := range []Client{NewClient("s"), {R: execx.OSRunner{}, Socket: "s"}} {
		r, ok := c.hereRunner("/tmp/tmux-1/s,10,0", "%2").(execx.OSRunner)
		if !ok {
			t.Fatalf("runner %T", r)
		}
		var tm []string
		for _, kv := range r.Env {
			if strings.HasPrefix(kv, "TMUX=") || strings.HasPrefix(kv, "TMUX_PANE=") {
				tm = append(tm, kv)
			}
		}
		if !slices.Equal(tm, []string{"TMUX=/tmp/tmux-1/s,10,0", "TMUX_PANE=%2"}) || !slices.Contains(r.Env, "CAIRN_KEEP=1") {
			t.Fatalf("env %q", r.Env)
		}
	}
	// The ordinary runner still strips them.
	if r := NewClient("s").runner().(execx.OSRunner); slices.ContainsFunc(r.Env, func(kv string) bool { return strings.HasPrefix(kv, "TMUX=") }) {
		t.Fatalf("runner leaked TMUX: %q", r.Env)
	}
}
