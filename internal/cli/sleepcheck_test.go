package cli

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrqwer/cairn/internal/detect"
	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/plan"
)

type scEnv struct {
	root  fsx.Root
	sys   detect.System
	off   uint64
	offE  error
	dev   string
	root0 bool
}

func setupSC(t *testing.T) *scEnv {
	t.Helper()
	e := &scEnv{
		root: fsx.Root{Dir: t.TempDir()},
		sys: detect.System{
			Swaps:                []detect.Swap{{Path: "/swapfile", Type: "file", SizeKiB: 1 << 30, Priority: -2}},
			Resume:               detect.Resume{Device: "259:3", Offset: 100},
			KernelModulesPresent: true,
			InitramfsResume:      detect.Yes,
		},
		off: 100, dev: "259:3", root0: true,
	}
	if err := e.root.MkdirAll(plan.SysStateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	oRoot, oDet, oOff, oDev, oIs, oOwn := sysRoot, detectSystem, offsetOf, deviceOf, isRoot, stateOwnerOf
	t.Cleanup(func() {
		sysRoot, detectSystem, offsetOf, deviceOf, isRoot, stateOwnerOf = oRoot, oDet, oOff, oDev, oIs, oOwn
	})
	sysRoot = e.root
	detectSystem = func(context.Context) (detect.System, error) { return e.sys, nil }
	offsetOf = func(string) (uint64, error) { return e.off, e.offE }
	deviceOf = func(string) (string, error) { return e.dev, nil }
	isRoot = func() bool { return e.root0 }
	stateOwnerOf = func(fs.FileInfo) (uint32, bool) { return 0, true }
	return e
}

func runSC(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := NewRootCmd()
	var o, e bytes.Buffer
	cmd.SetOut(&o)
	cmd.SetErr(&e)
	cmd.SetArgs(append([]string{"host", "sleep-check"}, args...))
	err = cmd.Execute()
	return o.String(), e.String(), err
}

func exitCode(err error) int {
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	if err != nil {
		return -1
	}
	return 0
}

func eventLog(t *testing.T, e *scEnv) string {
	b, _ := e.root.ReadFile(plan.SysStateDir + "/events.jsonl")
	return string(b)
}

func TestSleepCheckMissingConfigUsesDefaultsWithNote(t *testing.T) {
	e := setupSC(t)
	out, errOut, err := runSC(t, "--record")
	if exitCode(err) != 0 {
		t.Fatalf("err=%v out=%s", err, out)
	}
	if !strings.Contains(errOut, "note: no "+plan.HostConfigPath+", using built-in defaults") {
		t.Fatalf("stderr=%q", errOut)
	}
	if !strings.Contains(out, "using built-in defaults") {
		t.Fatalf("evidence lacks note: %s", out)
	}
	if !e.root.Exists(plan.SysStateDir + "/sleep-check.json") {
		t.Fatal("record not written")
	}
}

func TestSleepCheckMalformedConfigFails(t *testing.T) {
	e := setupSC(t)
	if err := e.root.WriteAtomic(plan.HostConfigPath, []byte("{nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := runSC(t, "--record")
	if err == nil || exitCode(err) == 0 {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(eventLog(t, e), `"error"`) {
		t.Fatalf("no error event: %q", eventLog(t, e))
	}
}

func TestSleepCheckValidConfigUsed(t *testing.T) {
	e := setupSC(t)
	cfg := `{"Sleep":{"HibernationSwap":{"Path":"/not-active"}}}`
	if err := e.root.WriteAtomic(plan.HostConfigPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	out, errOut, err := runSC(t, "--record")
	if exitCode(err) != 1 || !strings.Contains(out, "/not-active is not an active swap") {
		t.Fatalf("err=%v out=%s", err, out)
	}
	if strings.Contains(errOut, "built-in defaults") {
		t.Fatalf("defaults note printed: %s", errOut)
	}
	if !strings.Contains(eventLog(t, e), "sleep.swap.present") {
		t.Fatalf("event missing: %q", eventLog(t, e))
	}
}

func TestSleepCheckRecordExitCodes(t *testing.T) {
	e := setupSC(t)
	if _, _, err := runSC(t, "--record"); exitCode(err) != 0 {
		t.Fatalf("all ok: %v", err)
	}
	e.off = 999
	if _, _, err := runSC(t, "--record"); exitCode(err) != 1 {
		t.Fatalf("crit: %v", err)
	}
	if !strings.Contains(eventLog(t, e), "sleep.check.failed") || !strings.Contains(eventLog(t, e), "sleep.resume.match") {
		t.Fatalf("event: %q", eventLog(t, e))
	}
	// Non-record keeps the doctor exit code.
	if _, _, err := runSC(t); exitCode(err) != 2 {
		t.Fatalf("plain crit: %v", err)
	}
}

func TestSleepCheckUnverifiedResumeFailsClosedAsRoot(t *testing.T) {
	e := setupSC(t)
	e.offE = errors.New("operation not supported")
	out, _, err := runSC(t, "--record")
	if exitCode(err) != 1 || !strings.Contains(out, "could not verify: operation not supported") {
		t.Fatalf("err=%v out=%s", err, out)
	}
	if !strings.Contains(eventLog(t, e), "sleep.resume.match") {
		t.Fatalf("event: %q", eventLog(t, e))
	}
	// Non-root keeps Unknown and exits 0.
	e.root0 = false
	if _, _, err := runSC(t, "--record"); exitCode(err) != 0 {
		t.Fatalf("non-root: %v", err)
	}
}

func TestSleepCheckUntrustedStateDir(t *testing.T) {
	e := setupSC(t)
	real := filepath.Join(e.root.Dir, "elsewhere")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	host := e.root.Path(plan.SysStateDir)
	if err := os.RemoveAll(host); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, host); err != nil {
		t.Fatal(err)
	}
	_, errOut, err := runSC(t, "--record")
	if exitCode(err) != 1 || !strings.Contains(errOut, "state directory not trusted") {
		t.Fatalf("err=%v stderr=%s", err, errOut)
	}
	if entries, _ := os.ReadDir(real); len(entries) != 0 {
		t.Fatalf("wrote through the symlink: %v", entries)
	}
}
