package execx_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

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
