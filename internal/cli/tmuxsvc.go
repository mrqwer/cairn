package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/mrqwer/cairn/internal/host"
	"github.com/mrqwer/cairn/internal/inventory"
	"github.com/mrqwer/cairn/internal/plan"
	"github.com/mrqwer/cairn/internal/workspace"
	"github.com/mrqwer/cairn/internal/xdg"
)

func newTmuxCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "tmux", Short: "Manage the tmux server integration"}
	svc := &cobra.Command{Use: "service", Short: "Optional user service: tmux server plus restore on login"}
	svc.AddCommand(newSvcInstallCmd(), newSvcUninstallCmd(), newSvcStatusCmd())
	cmd.AddCommand(svc)
	return cmd
}

// svcState is the user-side environment of the tmux service commands.
type svcState struct {
	dirs    xdg.Dirs
	unitDir string
	invPath string
	inv     *inventory.Inventory
	env     plan.Env
}

func loadSvc() (*svcState, error) {
	dirs, err := xdg.Resolve(os.Getenv)
	if err != nil {
		return nil, err
	}
	invPath := filepath.Join(dirs.State, "workspace", "managed.json")
	inv, err := inventory.Load(wsRoot, invPath)
	if err != nil {
		return nil, err
	}
	return &svcState{
		dirs: dirs, unitDir: host.UserUnitDir(dirs), invPath: invPath, inv: inv,
		env: plan.Env{
			Root: wsRoot, Runner: hostRunner, UserInv: inv, UserInvPath: invPath,
			UserBackupDir: filepath.Join(dirs.State, "workspace", "backup"),
		},
	}, nil
}

func (s *svcState) render(w io.Writer, p plan.Plan, title string) {
	if st, ok := styleFor(w); ok {
		plan.RenderSection(w, p, st, title, "", s.dirs.Home)
		return
	}
	if len(p.Pending()) > 0 {
		plan.Render(w, p)
		return
	}
	_, _ = fmt.Fprintln(w, "nothing to do")
}

// applyPlan shows p, confirms unless yes, then applies it user-side.
func (s *svcState) applyPlan(cmd *cobra.Command, p plan.Plan, title string, yes, force bool) error {
	out := cmd.OutOrStdout()
	s.render(out, p, title)
	if len(p.Pending()) == 0 {
		return nil
	}
	if !yes && !confirm(cmd.InOrStdin(), out) {
		_, _ = fmt.Fprintln(out, "aborted, nothing changed")
		return nil
	}
	return plan.Apply(ctxOf(cmd), p, s.env, plan.Options{UserOnly: true, Force: force})
}

// recordedSocket is the socket of the installed, cairn-managed unit.
func (s *svcState) recordedSocket() (string, bool) {
	path := filepath.Join(s.unitDir, workspace.TmuxUnit)
	if _, ok := s.inv.Get(path); !ok {
		return "", false
	}
	b, err := wsRoot.ReadFile(path)
	if err != nil {
		return "", false
	}
	return workspace.RecordedSocket(b), true
}

func socketLabel(s string) string {
	if s == "" {
		return "(default)"
	}
	return s
}

func newSvcInstallCmd() *cobra.Command {
	var yes, force bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install and enable cairn-tmux.service and cairn-up.service (nothing is started)",
		Long: `Install and enable cairn-tmux.service and cairn-up.service for --socket
(or $CAIRN_TMUX_SOCKET). Nothing is started now; they run at your next login.

There is one set of units per user: installing for a different socket
replaces the existing ones and needs --force or an interactive confirmation.
Prefer an explicit --socket: its unit stops its own server with kill-server.
On the default socket the unit never runs kill-server, but if it started the
server itself (no tmux was running at login) stopping the unit or logging out
ends that server and its sessions.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			self, err := selfPath()
			if err != nil {
				return err
			}
			socket := socketName(cmd)
			if err := workspace.CheckServiceInput(self, socket); err != nil {
				return err
			}
			s, err := loadSvc()
			if err != nil {
				return err
			}
			p := plan.Resolve(workspace.ServiceActions(self, socket, s.unitDir), wsRoot, s.inv)
			if old, ok := s.recordedSocket(); ok && old != socket {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "replaces units for socket %s (the new units run %s)\n", socketLabel(old), socketLabel(socket))
				if yes && !force {
					return fmt.Errorf("existing units were installed for socket %s; re-run with --force or without --yes to replace them", socketLabel(old))
				}
			}
			if err := s.applyPlan(cmd, p, "tmux user service", yes, force); err != nil {
				return err
			}
			if len(p.Pending()) > 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "installed and enabled; it starts at your next login (nothing was started now)")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "apply without asking")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite unit files changed by hand (a backup is kept)")
	return cmd
}

func newSvcUninstallCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Disable the units and remove exactly the files cairn recorded",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := loadSvc()
			if err != nil {
				return err
			}
			p := host.UninstallPlan(nil, s.inv)
			if len(p.Actions) == 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "nothing to uninstall")
				return nil
			}
			return s.applyPlan(cmd, p, "remove tmux user service", yes, false)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "apply without asking")
	return cmd
}

func newSvcStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether the service units exist, are enabled and active",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := loadSvc()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, u := range workspace.QueryUnits(ctxOf(cmd), hostRunner, wsRoot, s.unitDir) {
				file := "missing"
				if u.File {
					file = "present"
				}
				_, _ = fmt.Fprintf(out, "%-20s file: %-8s enabled: %-10s active: %s\n", u.Unit, file, u.Enabled, u.Active)
			}
			return nil
		},
	}
}
