package tmux

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/mrqwer/cairn/internal/execx"
)

func snapFake(sessions, windows, panes execx.Response) *execx.FakeRunner {
	return &execx.FakeRunner{Responses: map[string]execx.Response{
		"tmux -L s list-sessions -F " + sessionFormat:  sessions,
		"tmux -L s list-windows -a -F " + windowFormat: windows,
		"tmux -L s list-panes -a -F " + paneFormat:     panes,
	}}
}

func TestSnapshotParses(t *testing.T) {
	f := snapFake(
		execx.Response{Out: []byte("$0\tdemo\tdemo\t1\n$1\tscratch\t\t0\n$2\todd\tx/y\t0\n")},
		execx.Response{Out: []byte(
			"$0\t@1\t2\tlogs\tdemo/logs\t200\t50\n" +
				"$0\t@0\t1\teditor\tdemo/editor\t200\t50\n" +
				"$1\t@2\t0\tbash\t\t80\t24\n" +
				"$2\t@3\t0\tw\tdemo\t80\t24\n")},
		execx.Response{Out: []byte(
			"@0\t%1\t1\tdemo/editor\t\t101\t0\t98\t50\t1\t3\t/home/u/demo\tmake\n" +
				"@0\t%0\t0\tdemo/editor/1\t101\t0\t0\t100\t50\t0\t\t/home/u/demo\tnvim\n" +
				"@1\t%2\t0\tdemo/logs/1\t103\t0\t0\t200\t50\t0\t\t/home/u/my dir\ttail\n" +
				"@2\t%3\t0\t\t104\t0\t0\t80\t24\t0\t\t/home/u\tbash\n" +
				"@3\t%4\t0\tdemo\t105\t0\t0\t80\t24\t0\t\t/home/u\tbash\n" +
				// a foreign pane whose cwd contains a tab: skipped, not fatal
				"@2\t%5\t1\t\t106\t0\t0\t80\t24\t0\t\t/home/u/a\tb\tbash\n")},
	)
	snap, err := Client{R: f, Socket: "s"}.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := Snapshot{Sessions: []Session{
		{ID: "$0", Name: "demo", Key: "demo", Attached: 1, Windows: []Window{
			{ID: "@0", Key: "demo/editor", Name: "editor", Index: 1, W: 200, H: 50, Panes: []Pane{
				{ID: "%0", Key: "demo/editor/1", Index: 0, PID: 101, Left: 0, Top: 0, W: 100, H: 50, Cwd: "/home/u/demo", Cmd: "nvim"},
				// %1 shows the window's value through tmux's option fallback: unmanaged.
				{ID: "%1", Key: "", Index: 1, PID: 0, Left: 101, Top: 0, W: 98, H: 50, Dead: true, DeadStatus: 3, Cwd: "/home/u/demo", Cmd: "make"},
			}},
			{ID: "@1", Key: "demo/logs", Name: "logs", Index: 2, W: 200, H: 50, Panes: []Pane{
				{ID: "%2", Key: "demo/logs/1", PID: 103, W: 200, H: 50, Cwd: "/home/u/my dir", Cmd: "tail"},
			}},
		}},
		{ID: "$1", Name: "scratch", Windows: []Window{
			{ID: "@2", Name: "bash", W: 80, H: 24, Panes: []Pane{
				{ID: "%3", PID: 104, W: 80, H: 24, Cwd: "/home/u", Cmd: "bash"},
			}},
		}},
		// A session key with a slash is not a session marker; a window showing
		// the session's value is not a window marker.
		{ID: "$2", Name: "odd", Windows: []Window{
			{ID: "@3", Name: "w", W: 80, H: 24, Panes: []Pane{
				{ID: "%4", PID: 105, W: 80, H: 24, Cwd: "/home/u", Cmd: "bash"},
			}},
		}},
	}, Skipped: []string{"@2\t%5\t1\t\t106\t0\t0\t80\t24\t0\t\t/home/u/a\tb\tbash"}}
	if !reflect.DeepEqual(snap, want) {
		t.Fatalf("snapshot\n got %+v\nwant %+v", snap, want)
	}
}

func TestSnapshotNoServerIsEmpty(t *testing.T) {
	for _, msg := range []string{
		"no server running on /tmp/tmux-1000/s\n",
		"error connecting to /tmp/tmux-1000/s (No such file or directory)\n",
	} {
		f := snapFake(execx.Response{Out: []byte(msg), Err: errors.New("exit status 1")}, execx.Response{}, execx.Response{})
		snap, err := Client{R: f, Socket: "s"}.Snapshot(context.Background())
		if err != nil || len(snap.Sessions) != 0 {
			t.Fatalf("%q: %+v %v", msg, snap, err)
		}
		if len(f.Calls) != 1 {
			t.Fatalf("calls %v", f.Calls)
		}
	}
}

func TestSnapshotOtherErrorFails(t *testing.T) {
	f := snapFake(execx.Response{Out: []byte("unknown option\n"), Err: errors.New("exit status 1")}, execx.Response{}, execx.Response{})
	if _, err := (Client{R: f, Socket: "s"}).Snapshot(context.Background()); err == nil {
		t.Fatal("want error")
	}
}

func TestSnapshotMalformedLinesSkipped(t *testing.T) {
	f := snapFake(
		execx.Response{Out: []byte("$0\tdemo\tdemo\t1\n$1\tbad\n")},
		execx.Response{Out: []byte("$0\t@0\tx\teditor\tdemo/editor\t200\t50\n$0\t@1\t1\tmain\tdemo/main\t200\t50\n")},
		execx.Response{Out: []byte("@1\t%0\t0\tdemo/main/1\t1\t0\t0\t200\t50\t0\t\t/home/u\n/x\tsh\n")},
	)
	snap, err := Client{R: f, Socket: "s"}.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Sessions) != 1 || len(snap.Sessions[0].Windows) != 1 || snap.Sessions[0].Windows[0].Key != "demo/main" {
		t.Fatalf("snapshot %+v", snap)
	}
	if len(snap.Skipped) != 4 {
		t.Fatalf("skipped %q", snap.Skipped)
	}
}

func TestSnapshotConnectErrorNarrowed(t *testing.T) {
	f := snapFake(execx.Response{Out: []byte("error connecting to /tmp/tmux-1000/s (Permission denied)\n"), Err: errors.New("exit status 1")}, execx.Response{}, execx.Response{})
	if _, err := (Client{R: f, Socket: "s"}).Snapshot(context.Background()); err == nil {
		t.Fatal("permission error treated as no server")
	}
	f = snapFake(execx.Response{Out: []byte("error connecting to /tmp/tmux-1000/s (Connection refused)\n"), Err: errors.New("exit status 1")}, execx.Response{}, execx.Response{})
	if _, err := (Client{R: f, Socket: "s"}).Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotDefaultSocketHasNoFlag(t *testing.T) {
	f := &execx.FakeRunner{Responses: map[string]execx.Response{
		"tmux list-sessions -F " + sessionFormat: {Out: []byte("no server running on /tmp/tmux-1000/default\n"), Err: errors.New("exit status 1")},
	}}
	if _, err := (Client{R: f}).Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
}
