package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/mrqwer/cairn/internal/config"
	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/plan"
)

func runHost(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	cmd := NewRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	err := cmd.Execute()
	return buf.String(), err
}

func hostTestEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	for env, sub := range map[string]string{
		"XDG_CONFIG_HOME": "config", "XDG_STATE_HOME": "state", "XDG_DATA_HOME": "data",
		"XDG_CACHE_HOME": "cache", "XDG_RUNTIME_DIR": "run",
	} {
		t.Setenv(env, filepath.Join(dir, sub))
	}
	return dir
}

func TestHostPlanAndApply(t *testing.T) {
	dir := hostTestEnv(t)
	target := filepath.Join(dir, "config/systemd/user/cairn-test.service")
	oldDesired, oldCheck, oldSudo := hostDesired, checkSelf, runSudo
	t.Cleanup(func() { hostDesired, checkSelf, runSudo = oldDesired, oldCheck, oldSudo })
	hostDesired = func(context.Context, *config.Config, string) ([]plan.Action, error) {
		return []plan.Action{
			{Kind: plan.WriteFile, Path: target, Content: []byte("[Unit]\n"), Reason: "test unit"},
			{Kind: plan.WriteFile, Path: "/etc/systemd/sleep.conf.d/50-cairn.conf", Content: []byte("cairn test\n"), NeedsRoot: true},
		}, nil
	}
	checkSelf = func(fsx.Root, string) error { return nil }
	var req plan.RootRequest
	var planFile string
	sudoCalls := 0
	runSudo = func(_ context.Context, _ *cobra.Command, _, file string) error {
		sudoCalls++
		planFile = file
		st, err := os.Stat(file)
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("plan file %s: %v %v", file, st, err)
		}
		req, err = plan.ReadRootRequest(fsx.Root{Dir: "/"}, file)
		return err
	}

	out, err := runHost(t, "", "host", "plan")
	if err != nil || !strings.Contains(out, target) || !strings.Contains(out, "create") || !strings.Contains(out, "needs root") {
		t.Fatalf("plan: %v\n%s", err, out)
	}

	out, err = runHost(t, "n\n", "host", "apply")
	if err != nil || !strings.Contains(out, "Apply these changes? [y/N]") || !strings.Contains(out, "aborted") {
		t.Fatalf("declined apply: %v\n%s", err, out)
	}
	if _, err := os.Stat(target); err == nil || sudoCalls != 0 {
		t.Fatal("declined apply changed something")
	}

	if out, err = runHost(t, "", "host", "apply", "--yes"); err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("user file not written: %v", err)
	}
	if sudoCalls != 1 || req.Mode != plan.ModeApply || len(req.Actions) != 1 || !req.Actions[0].NeedsRoot {
		t.Fatalf("root request: calls=%d %+v", sudoCalls, req)
	}
	if planFile != filepath.Join(dir, "run/cairn/host-plan.json") {
		t.Fatalf("plan file at %s", planFile)
	}
	if _, err := os.Stat(planFile); err == nil {
		t.Fatal("plan file left behind")
	}
	if _, err := os.Stat(filepath.Join(dir, "state/cairn/host/managed.json")); err != nil {
		t.Fatalf("user inventory not saved: %v", err)
	}
}

func TestApplyRootRequiresRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root")
	}
	_, err := runHost(t, "", "host", "apply-root", "--plan", "/nonexistent/host-plan.json")
	if err == nil || !strings.Contains(err.Error(), "must run as root") {
		t.Fatalf("err=%v", err)
	}
}
