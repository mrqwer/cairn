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
	return root
}
