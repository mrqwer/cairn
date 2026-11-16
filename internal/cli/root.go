// Package cli defines the cairn command tree.
package cli

import (
	"github.com/spf13/cobra"
)

// Version is injected at build time with -ldflags -X.
var Version = "dev"

// NewRootCmd builds the root cairn command.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "cairn",
		Short:         "cairn: declarative host and tooling management",
		SilenceUsage:  true,
		SilenceErrors: true,
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
		newWsStatusCmd(), newHistoryCmd(), newUndoCmd())
	return root
}
