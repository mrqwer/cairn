package cli

import (
	"errors"
	"os"

	"github.com/spf13/cobra"

	"github.com/mrqwer/cairn/internal/config"
	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/xdg"
)

func loadConfig() (*config.Config, config.Provenance, error) {
	if err := config.CheckEnv(os.Environ()); err != nil {
		return nil, nil, err
	}
	dirs, err := xdg.Resolve(os.Getenv)
	if err != nil {
		return nil, nil, err
	}
	return config.Load(fsx.Root{Dir: "/"}, dirs, os.Getenv)
}

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Inspect cairn configuration"}
	cmd.AddCommand(&cobra.Command{
		Use:   "validate",
		Short: "Validate the merged configuration",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, _, err := loadConfig()
			if err != nil {
				return err
			}
			errs := cfg.Validate()
			for _, e := range errs {
				cmd.PrintErrf("%s: %s\n", e.Key, e.Msg)
			}
			if len(errs) > 0 {
				return errors.New("configuration is invalid")
			}
			cmd.Println("configuration ok")
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "explain <key>",
		Short: "Show the effective value of a key and the layer that set it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, prov, err := loadConfig()
			if err != nil {
				return err
			}
			line, err := config.Explain(cfg, prov, args[0])
			if err != nil {
				return err
			}
			cmd.Println(line)
			return nil
		},
	})
	return cmd
}
