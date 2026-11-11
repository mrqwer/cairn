package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mrqwer/cairn/internal/config"
	"github.com/mrqwer/cairn/internal/execx"
	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/inventory"
	"github.com/mrqwer/cairn/internal/plan"
	"github.com/mrqwer/cairn/internal/xdg"
)

// hostDesired is the registration point for host policies; later tasks
// replace it with the real policy set.
var hostDesired = func(context.Context, *config.Config, string) ([]plan.Action, error) { return nil, nil }

// checkSelf and runSudo are seams for tests.
var (
	checkSelf = plan.CheckSelfOwnership
	runSudo   = sudoApplyRoot
)

var sysRoot = fsx.Root{Dir: "/"}

// hostState is everything the user-side host commands need.
type hostState struct {
	cfg   *config.Config
	dirs  xdg.Dirs
	self  string
	extra []string
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

// hostExtra lists config-derived paths the root side may create: the NVIDIA
// temporary directory, unless it is the vendor default /var/tmp.
func hostExtra(cfg *config.Config) []string {
	n := cfg.Host.GPU.Nvidia
	if !n.Manage || n.TemporaryFilePath == "" {
		return nil
	}
	p := filepath.Clean(n.TemporaryFilePath)
	if p == "/var/tmp" {
		return nil
	}
	return []string{p}
}

func loadHost(ctx context.Context) (*hostState, error) {
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
	desired, err := hostDesired(ctx, cfg, self)
	if err != nil {
		return nil, err
	}
	return &hostState{
		cfg:   cfg,
		dirs:  dirs,
		self:  self,
		extra: hostExtra(cfg),
		env: plan.Env{
			Root: sysRoot, Runner: execx.OSRunner{},
			UserInv: userInv, SysInv: sysInv,
			UserInvPath: userInvPath, SysInvPath: plan.SysInventoryPath,
			UserBackupDir: filepath.Join(dirs.State, "host", "backup"),
		},
		plan:  plan.ResolveAll(desired, sysRoot, userInv, sysInv),
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

// sudoApplyRoot runs `sudo <self> host apply-root --plan <file>`. This is the
// one place that uses os/exec instead of execx.Runner: sudo needs the
// terminal for its password prompt and the root side's output should stream
// to the user, while Runner only returns combined output after exit. The
// logic it triggers (plan.ApplyRoot) is Runner-based and tested.
func sudoApplyRoot(ctx context.Context, cmd *cobra.Command, self, file string) error {
	c := exec.CommandContext(ctx, "sudo", self, "host", "apply-root", "--plan", file)
	c.Stdin, c.Stdout, c.Stderr = cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()
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
			hs, err := loadHost(ctxOf(cmd))
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
			hs, err := loadHost(ctx)
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
			req := plan.NewRootRequest(hs.plan, hs.extra, hs.cfg.Host, force)
			file := filepath.Join(hs.dirs.Runtime, "host-plan.json")
			if err := plan.WriteRootRequest(sysRoot, file, req); err != nil {
				return err
			}
			defer func() { _ = sysRoot.Remove(file) }()
			return runSudo(ctx, cmd, hs.self, file)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask for confirmation")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite managed files modified by hand (a backup is kept)")
	return cmd
}

// newHostApplyRootCmd is the root half of apply. It runs under sudo, so it
// reads neither XDG directories nor config files: everything comes from the
// plan file, which it treats as untrusted input.
func newHostApplyRootCmd() *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:    "apply-root",
		Short:  "Apply the root part of a host plan (run by cairn host apply)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if os.Geteuid() != 0 {
				return errors.New("apply-root must run as root; use cairn host apply")
			}
			if !filepath.IsAbs(file) {
				return fmt.Errorf("--plan %q: must be an absolute path", file)
			}
			self, err := selfPath()
			if err != nil {
				return err
			}
			if err := checkSelf(sysRoot, self); err != nil {
				return err
			}
			req, err := plan.ReadRootRequest(sysRoot, file)
			if err != nil {
				return err
			}
			sysInv, err := inventory.Load(sysRoot, plan.SysInventoryPath)
			if err != nil {
				return err
			}
			env := plan.Env{Root: sysRoot, Runner: execx.OSRunner{}, SysInv: sysInv, SysInvPath: plan.SysInventoryPath}
			if err := plan.ApplyRoot(ctxOf(cmd), req, env); err != nil {
				return err
			}
			cmd.Println("root changes applied")
			return nil
		},
	}
	cmd.Flags().StringVar(&file, "plan", "", "plan file written by cairn host apply")
	_ = cmd.MarkFlagRequired("plan")
	return cmd
}
