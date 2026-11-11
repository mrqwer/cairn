package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mrqwer/cairn/internal/config"
	"github.com/mrqwer/cairn/internal/detect"
	"github.com/mrqwer/cairn/internal/host/sleep"
	"github.com/mrqwer/cairn/internal/xdg"
)

const swapKey = "host.sleep.hibernation_swap.path"

// tomlString quotes s as a TOML basic string; ok is false for values that
// would need escapes.
func tomlString(s string) (string, bool) {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r == '"' || r == '\\' {
			return "", false
		}
	}
	return `"` + s + `"`, true
}

// initHostConfig renders the detected host values that differ from the
// defaults and returns the keys it set.
func initHostConfig(sys detect.System, def *config.Config) (string, []string) {
	var b strings.Builder
	b.WriteString("# cairn configuration, created by `cairn init --host` from detected values.\n")
	b.WriteString("# Only values that differ from the built-in defaults are set here.\n")
	var keys []string
	if swap, ok := sleep.SelectSwap(config.Sleep{}, sys); ok && swap.Path != def.Host.Sleep.HibernationSwap.Path {
		if q, ok := tomlString(swap.Path); ok {
			b.WriteString("\n[host.sleep.hibernation_swap]\npath = " + q + "\n")
			keys = append(keys, swapKey)
		}
	}
	return b.String(), keys
}

func newInitCmd() *cobra.Command {
	var hostFlag bool
	cmd := &cobra.Command{
		Use:   "init --host",
		Short: "Create a user configuration from detected values (never overwrites)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !hostFlag {
				return errors.New("nothing to initialise: pass --host")
			}
			dirs, err := xdg.Resolve(os.Getenv)
			if err != nil {
				return err
			}
			path := filepath.Join(dirs.Config, "cairn.toml")
			if _, err := sysRoot.Stat(path); err == nil {
				cmd.Printf("%s already exists; leaving it unchanged\n", path)
				return nil
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			sys, err := detectSystem(ctxOf(cmd))
			if err != nil {
				return err
			}
			def, err := config.Defaults()
			if err != nil {
				return err
			}
			content, keys := initHostConfig(sys, def)
			if err := sysRoot.WriteAtomic(path, []byte(content), 0o644); err != nil {
				return fmt.Errorf("write %s: %w", path, err)
			}
			cmd.Printf("wrote %s\n", path)
			for _, k := range keys {
				cmd.Printf("  detected %s; check it with: cairn config explain %s\n", k, k)
			}
			cmd.Println("Everything else uses the defaults; see any value with: cairn config explain <key> (e.g. host.sleep.lid)")
			return nil
		},
	}
	cmd.Flags().BoolVar(&hostFlag, "host", false, "write detected host values")
	return cmd
}
