package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/mrqwer/cairn/internal/config"
	"github.com/mrqwer/cairn/internal/detect"
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
	rootAct := plan.Action{Kind: plan.WriteFile, Path: "/etc/systemd/sleep.conf.d/50-cairn.conf", Content: []byte("cairn test\n"), NeedsRoot: true}
	oldDesired, oldCheck, oldSudo, oldDetect, oldBuilder := hostDesired, checkSelf, runSudo, detectSystem, plan.RootBuilder
	t.Cleanup(func() {
		hostDesired, checkSelf, runSudo, detectSystem, plan.RootBuilder = oldDesired, oldCheck, oldSudo, oldDetect, oldBuilder
	})
	detectSystem = func(context.Context) (detect.System, error) { return detect.System{Desktop: "gnome"}, nil }
	hostDesired = func(_ context.Context, _ *config.Config, sys detect.System, _ string) ([]plan.Action, error) {
		if sys.Desktop != "gnome" {
			t.Errorf("hostDesired got sys %+v", sys)
		}
		return []plan.Action{{Kind: plan.WriteFile, Path: target, Content: []byte("[Unit]\n"), Reason: "test unit"}}, nil
	}
	plan.RootBuilder = func(mode string, _ config.Host, _ detect.System, _ string) ([]plan.Action, error) {
		if mode != plan.ModeApply {
			t.Errorf("builder mode %q", mode)
		}
		return []plan.Action{rootAct}, nil
	}
	checkSelf = func(fsx.Root, string) error { return nil }
	type sudoCall struct {
		mode       string
		yes, force bool
		req        plan.RootRequest
	}
	var calls []sudoCall
	runSudo = func(_ context.Context, _ *cobra.Command, _, mode string, yes, force bool, request []byte) error {
		req, err := plan.ReadRootRequest(bytes.NewReader(request))
		calls = append(calls, sudoCall{mode, yes, force, req})
		return err
	}

	out, err := runHost(t, "", "host", "plan")
	if err != nil || !strings.Contains(out, target) || !strings.Contains(out, "create") ||
		!strings.Contains(out, "preview (exact plan is computed by root at apply time):") ||
		!strings.Contains(out, "/etc/systemd/sleep.conf.d/50-cairn.conf") {
		t.Fatalf("plan: %v\n%s", err, out)
	}

	out, err = runHost(t, "n\n", "host", "apply")
	if err != nil || !strings.Contains(out, "Apply these changes? [y/N]") || !strings.Contains(out, "aborted") {
		t.Fatalf("declined apply: %v\n%s", err, out)
	}
	if _, err := os.Stat(target); err == nil || len(calls) != 0 {
		t.Fatal("declined apply changed something")
	}

	if out, err = runHost(t, "", "host", "apply", "--yes", "--force"); err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Root changes will be shown and confirmed by the privileged step.") ||
		strings.Contains(out, "/etc/systemd/sleep.conf.d/50-cairn.conf") {
		t.Fatalf("apply must not show the root plan itself:\n%s", out)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("user file not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "state/cairn/host/managed.json")); err != nil {
		t.Fatalf("user inventory not saved: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("sudo calls: %d", len(calls))
	}
	c := calls[0]
	if c.mode != plan.ModeApply || !c.yes || !c.force || c.req.Mode != plan.ModeApply {
		t.Fatalf("sudo call %+v", c)
	}

	// With user-level work done, root work alone asks no user-side question.
	calls = nil
	out, err = runHost(t, "", "host", "apply")
	if err != nil || strings.Contains(out, "Apply these changes?") || len(calls) != 1 || calls[0].yes {
		t.Fatalf("root-only apply: calls=%+v err=%v\n%s", calls, err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "run/cairn/host-plan.json")); err == nil {
		t.Fatal("a plan file was written")
	}
}

func TestHostDesiredMustNotReturnRootActions(t *testing.T) {
	hostTestEnv(t)
	oldDesired, oldDetect := hostDesired, detectSystem
	t.Cleanup(func() { hostDesired, detectSystem = oldDesired, oldDetect })
	detectSystem = func(context.Context) (detect.System, error) { return detect.System{}, nil }
	hostDesired = func(context.Context, *config.Config, detect.System, string) ([]plan.Action, error) {
		return []plan.Action{{Kind: plan.WriteFile, Path: "/etc/x", NeedsRoot: true}}, nil
	}
	if _, err := runHost(t, "", "host", "plan"); err == nil || !strings.Contains(err.Error(), "RootBuilder") {
		t.Fatalf("err=%v", err)
	}
}

func TestApplyRootRequiresRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root")
	}
	_, err := runHost(t, "{}", "host", "apply-root", "--mode", "apply")
	if err == nil || !strings.Contains(err.Error(), "must run as root") {
		t.Fatalf("err=%v", err)
	}
}

func TestHostApplyRootFailureLeavesUserSideAlone(t *testing.T) {
	_, calls := wireEnv(t)
	target := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "systemd/user/cairn-test.service")
	hostDesired = func(context.Context, *config.Config, detect.System, string) ([]plan.Action, error) {
		return []plan.Action{{Kind: plan.WriteFile, Path: target, Content: []byte("[Unit]\n")}}, nil
	}
	withRootBuilder(t, plan.Action{Kind: plan.WriteFile, Path: "/etc/systemd/sleep.conf.d/50-cairn.conf", Content: []byte("x\n"), NeedsRoot: true})
	runSudo = func(context.Context, *cobra.Command, string, string, bool, bool, []byte) error {
		*calls = append(*calls, sudoRec{})
		return errors.New("exit status 1")
	}
	out, err := runHost(t, "", "host", "apply", "--yes")
	if err == nil || !strings.Contains(err.Error(), "user-level changes were not applied") || len(*calls) != 1 {
		t.Fatalf("err=%v calls=%d\n%s", err, len(*calls), out)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("user file written although the root step failed")
	}
}

func TestHostPlanAndApplyPrintSleepBlockers(t *testing.T) {
	_, _ = wireEnv(t)
	hostDesired = func(context.Context, *config.Config, detect.System, string) ([]plan.Action, error) { return nil, nil }
	withRootBuilder(t, plan.Action{Kind: plan.RunCommand, Cmd: []string{"systemctl", "daemon-reload"}, NeedsRoot: true})
	want := "hibernation will be refused at sleep entry until: Hibernation swap is active — " +
		"create and enable a swap file or partition at least as large as RAM, then set host.sleep.hibernation_swap.path"
	for _, args := range [][]string{{"host", "plan"}, {"host", "apply", "--yes"}} {
		out, err := runHost(t, "", args...)
		if err != nil || !strings.Contains(out, want) || !strings.Contains(out, "until: Running kernel matches installed modules — reboot") {
			t.Fatalf("%v: err=%v\n%s", args, err, out)
		}
	}
}
