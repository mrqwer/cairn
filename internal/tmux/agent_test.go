package tmux

import (
	"context"
	"testing"

	"github.com/mrqwer/cairn/internal/execx"
)

func TestSessionAttachedArgs(t *testing.T) {
	ctx := context.Background()
	f := &execx.FakeRunner{Responses: map[string]execx.Response{
		"tmux -u -S /tmp/cairn-test/s display-message -p -t %3 #{session_attached}": {Out: []byte("2\n")},
		"tmux -u -S /tmp/cairn-test/s display-message -p -t %4 #{session_attached}": {Out: []byte("0\n")},
	}}
	c := Client{R: f, SocketFile: "/tmp/cairn-test/s", Socket: "ignored"}
	if ok, err := c.SessionAttached(ctx, "%3"); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	if ok, err := c.SessionAttached(ctx, "%4"); err != nil || ok {
		t.Fatalf("%v %v", ok, err)
	}
	if _, err := c.SessionAttached(ctx, "demo"); err == nil {
		t.Fatal("accepted a non-pane target")
	}
}

func TestSelectPaneArgs(t *testing.T) {
	f := &execx.FakeRunner{Responses: map[string]execx.Response{
		"tmux -u -L s select-window -t %7": {},
		"tmux -u -L s select-pane -t %7":   {},
	}}
	if err := (Client{R: f, Socket: "s"}).SelectPane(context.Background(), "%7"); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 2 {
		t.Fatalf("calls %v", f.Calls)
	}
	if err := (Client{R: f, Socket: "s"}).SelectPane(context.Background(), "demo"); err == nil {
		t.Fatal("accepted a non-pane target")
	}
}
