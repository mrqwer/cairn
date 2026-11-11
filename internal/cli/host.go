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
	"github.com/mrqwer/cairn/internal/inventory"
	"github.com/mrqwer/cairn/internal/plan"
	"github.com/mrqwer/cairn/internal/xdg"
)

// hostDesired is the registration point for user-level host policies; later
// tasks replace it. Root-level actions come only from plan.RootBuilder, so
// apply-root can regenerate them itself.
var hostDesired = func(context.Context, *config.Config, detect.System, string) ([]plan.Action, error) {
	return nil, nil
}

// detectSystem, checkSelf and runSudo are seams for tests.
var (
	detectSystem = func(ctx context.Context) (detect.System, error) {
		return detect.Detect(ctx, sysRoot, execx.OSRunner{}, os.Getenv)
	}
	checkSelf = plan.CheckSelfOwnership
	runSudo   = sudoApplyRoot
)

var sysRoot = fsx.Root{Dir: "/"}

// hostState is everything the user-side host commands need.
type hostState struct {
	cfg   *config.Config
	self  string
	env   plan.Env
	plan  plan.Plan
	stale bool // the root-side host config snapshot differs from cfg.Host
}

func ctxOf(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

func selfPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

func loadHost(ctx context.Context, mode string) (*hostState, error) {
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
	desired, err := hostDesired(ctx, cfg, sys, self)
	if err != nil {
		return nil, err
	}
	for _, a := range desired {
		if a.NeedsRoot {
			return nil, fmt.Errorf("policy bug: root action %s %s must come from plan.RootBuilder", a.Kind, a.Path)
		}
	}
	rootActs, err := plan.BuildRoot(mode, cfg.Host, sys, self)
	if err != nil {
		return nil, err
	}
	p := plan.ResolveAll(append(desired, rootActs...), sysRoot, userInv, sysInv)
	p.Mode = mode
	return &hostState{
		cfg:  cfg,
		self: self,
		env: plan.Env{
			Root: sysRoot, Runner: execx.OSRunner{},
			UserInv: userInv, SysInv: sysInv,
			UserInvPath: userInvPath, SysInvPath: plan.SysInventoryPath,
			UserBackupDir: filepath.Join(dirs.State, "host", "backup"),
		},
		plan:  p,
		stale: plan.SnapshotStale(sysRoot, cfg.Host),
	}, nil
}

func (hs *hostState) pending() (user, root bool) {
	for _, a := range hs.plan.Pending() {
		if a.NeedsRoot {
			root = true
		} else {
			user = true
		}
	}
	return user, root || hs.stale
}

func (hs *hostState) render(w io.Writer) {
	if len(hs.plan.Pending()) > 0 || !hs.stale {
		plan.Render(w, hs.plan)
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

// sudoApplyRoot runs `sudo <self> host apply-root --mode <m> --expect <hash>`
// with the request on stdin (sudo reads the password from the tty). This is
// the one place that uses os/exec instead of execx.Runner: sudo needs the
// terminal and the root side's output should stream to the user, while
// Runner only returns combined output after exit. The logic it triggers
// (plan.ApplyRoot) is Runner-based and tested.
func sudoApplyRoot(ctx context.Context, cmd *cobra.Command, self, mode, expect string, force bool, request []byte) error {
	args := []string{self, "host", "apply-root", "--mode", mode, "--expect", expect}
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
	cmd.AddCommand(newHostPlanCmd(), newHostApplyCmd(), newHostApplyRootCmd())
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
			hs.render(cmd.OutOrStdout())
			return nil
		},
	}
}

func newHostApplyCmd() *cobra.Command {
	var yes, force bool
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Apply the host plan (root parts through sudo)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := ctxOf(cmd)
			hs, err := loadHost(ctx, plan.ModeApply)
			if err != nil {
				return err
			}
			hs.render(cmd.OutOrStdout())
			userNeeded, rootNeeded := hs.pending()
			if !userNeeded && !rootNeeded {
				return nil
			}
			if m := hs.plan.ModifiedPaths(); len(m) > 0 && !force {
				return fmt.Errorf("%w: %s", plan.ErrModifiedByUser, strings.Join(m, ", "))
			}
			if rootNeeded {
				if err := checkSelf(sysRoot, hs.self); err != nil {
					return err
				}
			}
			if !yes && !confirm(cmd.InOrStdin(), cmd.OutOrStdout()) {
				cmd.Println("aborted")
				return nil
			}
			if userNeeded {
				if err := plan.Apply(ctx, hs.plan, hs.env, plan.Options{Force: force, UserOnly: true}); err != nil {
					return err
				}
			}
			if !rootNeeded {
				return nil
			}
			req, err := json.Marshal(plan.NewRootRequest(hs.plan.Mode, hs.cfg.Host))
			if err != nil {
				return err
			}
			return runSudo(ctx, cmd, hs.self, hs.plan.Mode, plan.Hash(hs.plan.Actions), force, req)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask for confirmation")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite managed files modified by hand (a backup is kept)")
	return cmd
}

// newHostApplyRootCmd is the root half of apply. It runs under sudo and reads
// neither XDG directories nor config files. Its only unprivileged input is a
// small request on stdin (mode, host config) plus the approved plan hash on
// argv; it regenerates the actions itself and runs them only if their hash
// matches.
func newHostApplyRootCmd() *cobra.Command {
	var mode, expect string
	var force bool
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
			sysInv, err := inventory.Load(sysRoot, plan.SysInventoryPath)
			if err != nil {
				return err
			}
			env := plan.Env{Root: sysRoot, Runner: execx.OSRunner{}, SysInv: sysInv, SysInvPath: plan.SysInventoryPath}
			opts := plan.RootOptions{Expect: expect, Force: force, Sys: sys, Self: self}
			if err := plan.ApplyRoot(ctx, req, env, opts); err != nil {
				return err
			}
			cmd.Println("root changes applied")
			return nil
		},
	}
	cmd.Flags().StringVar(&mode, "mode", "", "plan mode (apply or repair)")
	cmd.Flags().StringVar(&expect, "expect", "", "hash of the approved root plan")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite managed files modified by hand")
	_ = cmd.MarkFlagRequired("mode")
	_ = cmd.MarkFlagRequired("expect")
	return cmd
}
