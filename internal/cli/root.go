// Package cli defines the cairn command tree.
package cli

import (
	"github.com/spf13/cobra"

	"github.com/mrqwer/cairn/internal/reconcile"
)

// Agent panes run this executable (read on every plan, so test seams on
// exePath apply).
func init() { reconcile.AgentCommand = agentSelf }

// Version is injected at build time with -ldflags -X.
var Version = "dev"

// NewRootCmd builds the root cairn command.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "cairn",
		Short:         "cairn: declarative host and tooling management",
		SilenceUsage:  true,
		SilenceErrors: true,
		// main cancels the context on SIGINT/SIGTERM; a command that has
		// not started yet does not start.
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			running.Store(cmd)
			return ctxOf(cmd).Err()
		},
	}
	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print the cairn version",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			cmd.Printf("cairn %s\n", Version)
		},
	})
	root.AddCommand(newConfigCmd())
	root.AddCommand(newDoctorCmd())
	root.AddCommand(newHostCmd())
	root.AddCommand(newInitCmd())
	root.PersistentFlags().String("socket", "", "tmux server socket name (tmux -L); default $CAIRN_TMUX_SOCKET, else tmux's default server")
	root.AddCommand(newProjectCmd(), newWsPlanCmd(), newWsApplyCmd(), newWsUpCmd(), newWsOpenCmd(),
		newWsStatusCmd(), newHistoryCmd(), newUndoCmd(), newCaptureCmd(), newAdoptCmd(), newTmuxCmd(), newAgentsCmd())
	return root
}
