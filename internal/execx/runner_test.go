package execx_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mrqwer/cairn/internal/execx"
)

func TestFakeRunnerRecordsCalls(t *testing.T) {
	f := &execx.FakeRunner{Responses: map[string]execx.Response{
		"systemctl is-active x": {Out: []byte("active\n")},
		"false":                 {Err: errors.New("boom")},
	}}
	out, err := f.Run(context.Background(), "systemctl", "is-active", "x")
	if err != nil || string(out) != "active\n" {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := f.Run(context.Background(), "false"); err == nil {
		t.Fatal("want error")
	}
	want := [][]string{{"systemctl", "is-active", "x"}, {"false"}}
	if !reflect.DeepEqual(f.Calls, want) {
		t.Fatalf("calls %v", f.Calls)
	}
}

func TestFakeRunnerUnknownCommandErrors(t *testing.T) {
	f := &execx.FakeRunner{}
	_, err := f.Run(context.Background(), "rm", "-rf", "x")
	if err == nil || !strings.Contains(err.Error(), "rm -rf x") {
		t.Fatalf("err %v", err)
	}
}

func TestOSRunner(t *testing.T) {
	out, err := execx.OSRunner{}.Run(context.Background(), "echo", "hi")
	if err != nil || string(out) != "hi\n" {
		t.Fatalf("%q %v", out, err)
	}
}

func TestOSRunnerEnv(t *testing.T) {
	out, err := execx.OSRunner{Env: []string{"CAIRN_X=1"}}.Run(context.Background(), "env")
	if err != nil || string(out) != "CAIRN_X=1\n" {
		t.Fatalf("%q %v", out, err)
	}
}

func TestOSRunnerNilEnvInherits(t *testing.T) {
	t.Setenv("CAIRN_INHERIT", "yes")
	out, err := execx.OSRunner{}.Run(context.Background(), "env")
	if err != nil || !strings.Contains(string(out), "CAIRN_INHERIT=yes\n") {
		t.Fatalf("%q %v", out, err)
	}
}

func TestOSRunnerDir(t *testing.T) {
	dir := t.TempDir()
	out, err := execx.OSRunner{Dir: dir}.Run(context.Background(), "pwd")
	if err != nil || strings.TrimSpace(string(out)) != dir {
		t.Fatalf("%q %v", out, err)
	}
}

func TestOSRunnerStreams(t *testing.T) {
	var out, errOut strings.Builder
	r := execx.OSRunner{Stdin: strings.NewReader("in\n"), Stdout: &out, Stderr: &errOut}
	b, err := r.Run(context.Background(), "sh", "-c", "cat; echo e >&2")
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 0 || out.String() != "in\n" || errOut.String() != "e\n" {
		t.Fatalf("returned %q, out %q, err %q", b, out.String(), errOut.String())
	}
}

func TestOSRunnerInterruptSignal(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	r := execx.OSRunner{Interrupt: syscall.SIGTERM, Dir: dir}
	_, _ = r.Run(ctx, "sh", "-c", "trap 'echo term > got; exit 1' TERM; sleep 5 >/dev/null 2>&1 & wait")
	b, _ := os.ReadFile(filepath.Join(dir, "got"))
	if strings.TrimSpace(string(b)) != "term" {
		t.Fatalf("child was not sent the signal: %q", b)
	}
}
