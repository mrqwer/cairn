package config_test

import (
	"testing"

	"github.com/mrqwer/cairn/internal/config"
	"github.com/mrqwer/cairn/internal/fsx"
)

func loadUser(t *testing.T, body string) *config.Config {
	t.Helper()
	root := fsx.Root{Dir: t.TempDir()}
	write(t, root, "/home/u/.config/cairn/cairn.toml", body)
	cfg, _, err := config.Load(root, testDirs, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func keys(errs []config.FieldError) map[string]bool {
	m := map[string]bool{}
	for _, e := range errs {
		m[e.Key] = true
	}
	return m
}

func TestValidate(t *testing.T) {
	cfg := loadUser(t, `[host.sleep]
lid = "sleep"
hibernate_after = "30s"
[host.sleep.hibernation_swap]
path = "swap/file"
`)
	got := keys(cfg.Validate())
	for _, k := range []string{"host.sleep.lid", "host.sleep.hibernate_after", "host.sleep.hibernation_swap.path"} {
		if !got[k] {
			t.Errorf("missing FieldError for %s (got %v)", k, got)
		}
	}
	if len(got) != 3 {
		t.Errorf("unexpected extra errors: %v", got)
	}
}

func TestValidateOtherFields(t *testing.T) {
	cfg := loadUser(t, `[host.sleep]
lid_ac = "nap"
critical_battery = "suspend"
idle_battery = "poweroff"
hibernate_after = "25h"
[host.gpu.nvidia]
temporary_file_path = "tmp"
`)
	got := keys(cfg.Validate())
	for _, k := range []string{"host.sleep.lid_ac", "host.sleep.critical_battery", "host.sleep.idle_battery",
		"host.sleep.hibernate_after", "host.gpu.nvidia.temporary_file_path"} {
		if !got[k] {
			t.Errorf("missing FieldError for %s", k)
		}
	}
}

func TestValidateBounds(t *testing.T) {
	for _, in := range []string{"1min", "24h"} {
		cfg := loadUser(t, "[host.sleep]\nhibernate_after = \""+in+"\"\n")
		if errs := cfg.Validate(); len(errs) != 0 {
			t.Errorf("%s rejected: %v", in, errs)
		}
	}
}
