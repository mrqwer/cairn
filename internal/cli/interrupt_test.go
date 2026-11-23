package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
)

func TestInteractiveNeedsARealTerminal(t *testing.T) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = null.Close() }()
	if interactive(null) {
		t.Error("/dev/null counts as a terminal")
	}
	if interactive(strings.NewReader("y\n")) {
		t.Error("a reader counts as a terminal")
	}
	pr, pw, _ := os.Pipe()
	defer func() { _ = pr.Close(); _ = pw.Close() }()
	if interactive(pr) {
		t.Error("a pipe counts as a terminal")
	}
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	defer func() { _ = master.Close() }()
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Skipf("no pty available: %v", err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	defer func() { _ = slave.Close() }()
	if !interactive(slave) {
		t.Error("a pty slave is not recognised as a terminal")
	}
}

func TestAskAbortsOnCancelledContext(t *testing.T) {
	pr, pw := io.Pipe() // never answers
	defer func() { _ = pw.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	var out strings.Builder
	done := make(chan error, 1)
	go func() {
		_, err := ask(ctx, pr, &out, "Go?")
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	if !Prompting() {
		t.Error("Prompting() should be true while waiting")
	}
	cancel()
	select {
	case err := <-done:
		ee, ok := err.(*ExitError)
		if !ok || ee.Code != 130 {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ask did not return after cancellation")
	}
	if !strings.Contains(out.String(), "nothing changed") {
		t.Errorf("output %q", out.String())
	}
	if Prompting() {
		t.Error("Prompting() still true")
	}
}

func TestAskAnswers(t *testing.T) {
	for in, want := range map[string]bool{"y\n": true, "YES\n": true, "n\n": false, "\n": false, "": false} {
		got, err := ask(context.Background(), strings.NewReader(in), io.Discard, "q")
		if err != nil || got != want {
			t.Errorf("ask(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := ask(func() context.Context { c, f := context.WithCancel(context.Background()); f(); return c }(), strings.NewReader("y\n"), io.Discard, "q"); err == nil {
		t.Error("an already cancelled context must not be answered")
	}
}

func TestInterruptMessageOnlyForApplyingCommands(t *testing.T) {
	root := NewRootCmd()
	cases := map[string]bool{
		"up": true, "apply": true, "adopt": true, "undo": true, "host apply": true, "history restore": true,
		"capture": false, // without --write
		"plan":    false, "status": false, "doctor": false, "project ls": false, "config validate": false,
		"history": false, "history show": false, "host plan": false,
		"agents new": true, "agents resume": true, "agents archive": true, "agents rm": true,
		"agents ls": false, "agents pane": false,
	}
	for path, want := range cases {
		c, _, err := root.Find(strings.Fields(path))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if got := mayBeMidApply(c); got != want {
			t.Errorf("%q: mayBeMidApply = %v, want %v", path, got, want)
		}
	}
	c, _, _ := root.Find([]string{"capture"})
	if err := c.Flags().Set("write", "true"); err != nil {
		t.Fatal(err)
	}
	if !mayBeMidApply(c) {
		t.Error("capture --write can be mid-apply")
	}
	if mayBeMidApply(nil) || mayBeMidApply(&cobra.Command{Use: "x"}) {
		t.Error("unknown command must not announce")
	}
}

func TestRecordedUnitWithMissingFileNeedsConfirmation(t *testing.T) {
	dir, _ := svcEnv(t)
	unit := filepath.Join(dir, "config/systemd/user/cairn-tmux.service")
	if out, err := runHost(t, "", "tmux", "service", "install", "--yes", "--socket", "a"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if err := os.Remove(unit); err != nil {
		t.Fatal(err)
	}
	out, err := runHost(t, "", "tmux", "service", "install", "--yes", "--socket", "a")
	if err == nil || !strings.Contains(err.Error(), "unknown") || !strings.Contains(out, "socket is unknown") {
		t.Fatalf("err=%v out=%s", err, out)
	}
	if _, err := os.Stat(unit); err == nil {
		t.Error("unit rewritten despite the refusal")
	}
	if out, err := runHost(t, "y\n", "tmux", "service", "install", "--socket", "a"); err != nil {
		t.Fatalf("interactive: %v\n%s", err, out)
	}
	if _, err := os.Stat(unit); err != nil {
		t.Error("unit not restored after confirming")
	}
	if err := os.Remove(unit); err != nil {
		t.Fatal(err)
	}
	if out, err := runHost(t, "", "tmux", "service", "install", "--yes", "--force", "--socket", "a"); err != nil {
		t.Fatalf("force: %v\n%s", err, out)
	}
}
