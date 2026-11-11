package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mrqwer/cairn/internal/config"
	"github.com/mrqwer/cairn/internal/detect"
	"github.com/mrqwer/cairn/internal/execx"
	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/host"
	"github.com/mrqwer/cairn/internal/inventory"
	"github.com/mrqwer/cairn/internal/plan"
	"github.com/mrqwer/cairn/internal/xdg"
)

// hostDesired returns the user-level actions of the apply plan (a seam for
// tests). Root-level actions come only from plan.RootBuilder, so apply-root
// can regenerate them itself.
var hostDesired = func(_ context.Context, cfg *config.Config, sys detect.System, self string) ([]plan.Action, error) {
	dirs, err := xdg.Resolve(os.Getenv)
	if err != nil {
		return nil, err
	}
	return host.UserActions(cfg, sys, self, host.UserUnitDir(dirs)), nil
}

// The root builder is registered here and nowhere else: it reads the system
// through sysRoot at call time, so apply-root and the user-side preview use
// the same policies.
func init() {
	plan.RootBuilder = func(mode string, h config.Host, sys detect.System, self string) ([]plan.Action, error) {
		return host.Builder{Root: sysRoot, Offset: offsetOf}.Build(mode, h, sys, self)
	}
	plan.RootNotes = func(mode string, h config.Host, sys detect.System) []string {
		return host.Builder{Root: sysRoot, Offset: offsetOf}.Notes(mode, h, sys)
	}
}

// detectSystem, checkSelf and runSudo are seams for tests.
var (
	detectSystem = func(ctx context.Context) (detect.System, error) {
		return detect.Detect(ctx, sysRoot, execx.OSRunner{}, os.Getenv)
	}
	checkSelf = plan.CheckSelfOwnership
	runSudo   = sudoApplyRoot
	exePath   = os.Executable
	// hostRunner runs the user side's commands (systemctl --user, gsettings).
	hostRunner execx.Runner = execx.OSRunner{}
)

var sysRoot = fsx.Root{Dir: "/"}

// hostState is everything the user-side host commands need.
type hostState struct {
	cfg  *config.Config
	self string
	env  plan.Env
	user plan.Plan // user-level actions, resolved
	root plan.Plan // preview of root's plan
	// stale: the root-side host config snapshot differs from cfg.Host.
	stale bool
}

func ctxOf(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

// unsafeSelfChars may not appear in the cairn binary path: it is embedded in
// unit files (where % is a specifier and whitespace splits arguments) and in
// shell and pacman hook lines.
const unsafeSelfChars = " \t\n\r\v\f%\"'\\$`;"

// checkSelfPath rejects binary paths that cannot be embedded safely.
func checkSelfPath(p string) error {
	if i := strings.IndexAny(p, unsafeSelfChars); i >= 0 {
		return fmt.Errorf("cairn binary path %q contains %q, which cannot be used in unit files and hooks; "+
			"install cairn to a path without whitespace, %%, quotes, backslash, $, backtick or ;", p, p[i])
	}
	return nil
}

func selfPath() (string, error) {
	exe, err := exePath()
	if err != nil {
		return "", err
	}
	p, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", err
	}
	if err := checkSelfPath(p); err != nil {
		return "", err
	}
	return p, nil
}

func loadValidConfig() (*config.Config, error) {
	cfg, _, err := loadConfig()
	if err != nil {
		return nil, err
	}
	if errs := cfg.Validate(); len(errs) > 0 {
		all := make([]error, len(errs))
		for i, e := range errs {
			all[i] = e
		}
		return nil, fmt.Errorf("configuration is invalid: %w", errors.Join(all...))
	}
	return cfg, nil
}

func loadHost(ctx context.Context, mode string) (*hostState, error) {
	cfg, err := loadValidConfig()
	if err != nil {
		return nil, err
	}
	dirs, err := xdg.Resolve(os.Getenv)
	if err != nil {
		return nil, err
	}
	self, err := selfPath()
	if err != nil {
		return nil, err
	}
	userInvPath := filepath.Join(dirs.State, "host", "managed.json")
	userInv, err := inventory.Load(sysRoot, userInvPath)
	if err != nil {
		return nil, err
	}
	sysInv, err := inventory.Load(sysRoot, plan.SysInventoryPath)
	if err != nil {
		return nil, err
	}
	sys, err := detectSystem(ctx)
	if err != nil {
		return nil, err
	}
	var desired []plan.Action
	switch mode {
	case plan.ModeApply:
		if desired, err = hostDesired(ctx, cfg, sys, self); err != nil {
			return nil, err
		}
	case plan.ModeUninstall:
		desired = host.UninstallPlan(nil, userInv).Actions
	}
	env := plan.Env{
		Root: sysRoot, Runner: hostRunner,
		UserInv: userInv, SysInv: sysInv,
		UserInvPath: userInvPath, SysInvPath: plan.SysInventoryPath,
		UserBackupDir: filepath.Join(dirs.State, "host", "backup"),
	}
	user, root, err := host.PlanBoth(mode, cfg.Host, sys, self, desired, env, env)
	if err != nil {
		return nil, err
	}
	return &hostState{
		cfg: cfg, self: self, env: env, user: user, root: root,
		stale: mode == plan.ModeApply && plan.SnapshotStale(sysRoot, cfg.Host),
	}, nil
}

// rootStep reports whether the privileged step must run: whenever root has
// any action for this config (R11: never skipped on the strength of the
// user-side preview, which cannot see root-only facts) or the snapshot is
// stale.
func (hs *hostState) rootStep() bool { return len(hs.root.Actions) > 0 || hs.stale }

func (hs *hostState) renderUser(w io.Writer) {
	if len(hs.user.Pending()) > 0 {
		plan.Render(w, hs.user)
		return
	}
	_, _ = fmt.Fprintln(w, "user-level: nothing to do")
}

// renderRootPreview shows the user side's guess at the root plan. Root
// recomputes it with its own view of the system and confirms it itself.
func (hs *hostState) renderRootPreview(w io.Writer) {
	_, _ = fmt.Fprintln(w, "preview (exact plan is computed by root at apply time):")
	if len(hs.root.Pending()) > 0 {
		plan.Render(w, hs.root)
	} else if !hs.stale {
		_, _ = fmt.Fprintln(w, "needs root: nothing to do")
	}
	if hs.stale {
		_, _ = fmt.Fprintf(w, "needs root: update host config snapshot %s\n", plan.HostConfigPath)
	}
}

func confirm(in io.Reader, out io.Writer) bool {
	_, _ = fmt.Fprint(out, "Apply these changes? [y/N] ")
	line, _ := bufio.NewReader(in).ReadString('\n')
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes"
}

// sudoApplyRoot runs `sudo <self> host apply-root --mode <m> [--yes]
// [--force]` with the request on stdin; sudo and apply-root's confirmation
// both use the terminal directly. This is the one place that uses os/exec
// instead of execx.Runner: sudo needs the terminal and the root side's output
// should stream to the user, while Runner only returns combined output after
// exit. The logic it triggers (plan.ApplyRoot) is Runner-based and tested.
func sudoApplyRoot(ctx context.Context, cmd *cobra.Command, self, mode string, yes, force bool, request []byte) error {
	args := []string{self, "host", "apply-root", "--mode", mode}
	if yes {
		args = append(args, "--yes")
	}
	if force {
		args = append(args, "--force")
	}
	c := exec.CommandContext(ctx, "sudo", args...)
	c.Stdin, c.Stdout, c.Stderr = bytes.NewReader(request), cmd.OutOrStdout(), cmd.ErrOrStderr()
	if err := c.Run(); err != nil {
		return fmt.Errorf("sudo %s host apply-root: %w", self, err)
	}
	return nil
}

func newHostCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "host", Short: "Manage host settings"}
	cmd.AddCommand(newHostPlanCmd(), newHostApplyCmd(), newHostUninstallCmd(), newHostRepairCmd(),
		newHostApplyRootCmd(), newSleepCheckCmd(), newRecordSleepCmd(), newBootCheckCmd(), newNotifyCmd())
	return cmd
}

func newHostPlanCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "plan",
		Short: "Show what cairn host apply would change",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			hs, err := loadHost(ctxOf(cmd), plan.ModeApply)
			if err != nil {
				return err
			}
			hs.renderUser(cmd.OutOrStdout())
			hs.renderRootPreview(cmd.OutOrStdout())
			return nil
		},
	}
}

// runHostOp is apply and uninstall: show the user plan, validate the request
// and root's preview, confirm the user side, then run both through
// host.ApplyBoth (on apply the privileged step first, user actions only after
// it succeeded). The privileged step always runs when root has anything for
// this config; root shows and confirms its own plan.
func runHostOp(cmd *cobra.Command, mode string, yes, force bool) error {
	ctx := ctxOf(cmd)
	hs, err := loadHost(ctx, mode)
	if err != nil {
		return err
	}
	hs.renderUser(cmd.OutOrStdout())
	userNeeded := len(hs.user.Pending()) > 0
	rootNeeded := hs.rootStep()
	if !userNeeded && !rootNeeded {
		return nil
	}
	if m := hs.user.ModifiedPaths(); len(m) > 0 && !force {
		return fmt.Errorf("%w: %s", plan.ErrModifiedByUser, strings.Join(m, ", "))
	}
	req := plan.NewRootRequest(mode, hs.cfg.Host)
	if err := host.CheckRootPreview(req, hs.root); err != nil {
		return err
	}
	if rootNeeded {
		if err := checkSelf(sysRoot, hs.self); err != nil {
			return err
		}
	}
	if userNeeded && !yes && !confirm(cmd.InOrStdin(), cmd.OutOrStdout()) {
		cmd.Println("aborted")
		return nil
	}
	var priv host.Privileged
	if rootNeeded {
		cmd.Println("Root changes will be shown and confirmed by the privileged step.")
		priv = sudoPrivileged(cmd, hs.self, yes, force)
	}
	return host.ApplyBoth(ctx, hs.user, hs.root, hs.env, req, force, priv)
}

func sudoPrivileged(cmd *cobra.Command, self string, yes, force bool) host.Privileged {
	return func(ctx context.Context, req plan.RootRequest) error {
		data, err := json.Marshal(req)
		if err != nil {
			return err
		}
		return runSudo(ctx, cmd, self, req.Mode, yes, force, data)
	}
}

func newHostApplyCmd() *cobra.Command {
	var yes, force bool
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Apply the host plan (root parts through sudo)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runHostOp(cmd, plan.ModeApply, yes, force)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask for confirmation")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite managed files modified by hand (a backup is kept)")
	return cmd
}

func newHostUninstallCmd() *cobra.Command {
	var yes, force bool
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove everything cairn host manages and restore what it replaced",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runHostOp(cmd, plan.ModeUninstall, yes, force)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask for confirmation")
	cmd.Flags().BoolVar(&force, "force", false, "remove managed files modified by hand (a backup is kept)")
	return cmd
}

func newHostRepairCmd() *cobra.Command {
	var yes, force bool
	cmd := &cobra.Command{
		Use:   "repair",
		Short: "Remove conflicting hibernate hooks and fix boot entry resume settings (through sudo)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadValidConfig()
			if err != nil {
				return err
			}
			self, err := selfPath()
			if err != nil {
				return err
			}
			if err := checkSelf(sysRoot, self); err != nil {
				return err
			}
			cmd.Println("The repair plan is computed, shown and confirmed by the privileged step.")
			return sudoPrivileged(cmd, self, yes, force)(ctxOf(cmd), plan.NewRootRequest(plan.ModeRepair, cfg.Host))
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask for confirmation")
	cmd.Flags().BoolVar(&force, "force", false, "rewrite boot entries changed since the last repair (a backup is kept)")
	return cmd
}

// newHostApplyRootCmd is the root half of apply, uninstall and repair. It
// runs under sudo and reads neither XDG directories nor config files. Its
// only unprivileged input is a small request on stdin (mode, host config).
// It regenerates the actions with its own detection, prints them and asks
// on /dev/tty (never stdin) unless --yes.
func newHostApplyRootCmd() *cobra.Command {
	var mode string
	var yes, force bool
	cmd := &cobra.Command{
		Use:    "apply-root",
		Short:  "Apply the root part of a host plan (run by cairn host apply)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if os.Geteuid() != 0 {
				return errors.New("apply-root must run as root; use cairn host apply")
			}
			self, err := selfPath()
			if err != nil {
				return err
			}
			if err := checkSelf(sysRoot, self); err != nil {
				return err
			}
			req, err := plan.ReadRootRequest(cmd.InOrStdin())
			if err != nil {
				return err
			}
			if req.Mode != mode {
				return fmt.Errorf("request mode %q does not match --mode %q", req.Mode, mode)
			}
			ctx := ctxOf(cmd)
			sys, err := detectSystem(ctx)
			if err != nil {
				return err
			}
			env := plan.Env{Root: sysRoot, Runner: execx.OSRunner{}}
			opts := plan.RootOptions{Yes: yes, Force: force, Sys: sys, Self: self, Out: cmd.ErrOrStderr()}
			if !yes {
				// stdin carries the request; the answer comes from the terminal.
				if tty, err := os.Open("/dev/tty"); err == nil {
					defer func() { _ = tty.Close() }()
					opts.TTY = tty
				}
			}
			return host.ApplyRootSide(ctx, req, env, opts)
		},
	}
	cmd.Flags().StringVar(&mode, "mode", "", "plan mode (apply, repair or uninstall)")
	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask for confirmation")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite managed files modified by hand")
	_ = cmd.MarkFlagRequired("mode")
	return cmd
}
