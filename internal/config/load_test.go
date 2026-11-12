package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrqwer/cairn/internal/config"
	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/xdg"
)

var testDirs = xdg.Dirs{Home: "/home/u", Config: "/home/u/.config/cairn"}

func write(t *testing.T, root fsx.Root, p, content string) {
	t.Helper()
	if err := root.WriteAtomic(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestDefaultsOnly(t *testing.T) {
	root := fsx.Root{Dir: t.TempDir()}
	cfg, prov, err := config.Load(root, testDirs, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host.Sleep.HibernateAfter.Duration != 15*time.Minute {
		t.Errorf("hibernate_after = %v", cfg.Host.Sleep.HibernateAfter)
	}
	if cfg.Host.Sleep.Lid != "suspend-then-hibernate" {
		t.Errorf("lid = %q", cfg.Host.Sleep.Lid)
	}
	if !cfg.Host.GPU.Nvidia.Manage || cfg.Host.GPU.Nvidia.TemporaryFilePath != "/var/tmp" {
		t.Errorf("nvidia = %+v", cfg.Host.GPU.Nvidia)
	}
	if got := prov["host.sleep.lid"]; got.Layer != "defaults" {
		t.Errorf("origin = %+v", got)
	}
	if errs := cfg.Validate(); len(errs) != 0 {
		t.Errorf("defaults invalid: %v", errs)
	}
}

func TestLayerOrder(t *testing.T) {
	root := fsx.Root{Dir: t.TempDir()}
	write(t, root, "/etc/cairn/cairn.toml", "[host.sleep]\nlid = \"suspend\"\nlid_ac = \"suspend\"\n")
	write(t, root, "/home/u/.config/cairn/cairn.toml", "[host.sleep]\nlid = \"hibernate\"\n")
	cfg, prov, err := config.Load(root, testDirs, env(map[string]string{"CAIRN_HOST_SLEEP_LID": "ignore"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host.Sleep.Lid != "ignore" || prov["host.sleep.lid"].Layer != "env" {
		t.Errorf("lid = %q origin %+v", cfg.Host.Sleep.Lid, prov["host.sleep.lid"])
	}
	if prov["host.sleep.lid"].File != "CAIRN_HOST_SLEEP_LID" {
		t.Errorf("env origin file = %q", prov["host.sleep.lid"].File)
	}
	if cfg.Host.Sleep.LidAC != "suspend" || prov["host.sleep.lid_ac"].Layer != "system" {
		t.Errorf("lid_ac = %q origin %+v", cfg.Host.Sleep.LidAC, prov["host.sleep.lid_ac"])
	}
	if prov["host.sleep.lid_ac"].File != "/etc/cairn/cairn.toml" {
		t.Errorf("system file = %q", prov["host.sleep.lid_ac"].File)
	}
}

func TestUserLayerBeatsSystem(t *testing.T) {
	root := fsx.Root{Dir: t.TempDir()}
	write(t, root, "/etc/cairn/cairn.toml", "[host.sleep]\nlid = \"suspend\"\n")
	write(t, root, "/home/u/.config/cairn/cairn.toml", "[host.sleep]\nlid = \"hibernate\"\n")
	cfg, prov, err := config.Load(root, testDirs, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host.Sleep.Lid != "hibernate" || prov["host.sleep.lid"].Layer != "user" {
		t.Errorf("lid = %q origin %+v", cfg.Host.Sleep.Lid, prov["host.sleep.lid"])
	}
}

func TestProfileLayer(t *testing.T) {
	root := fsx.Root{Dir: t.TempDir()}
	write(t, root, "/home/u/.config/cairn/cairn.toml", "profile = \"laptop\"\n")
	write(t, root, "/home/u/.config/cairn/profiles/laptop.toml",
		"[host.sleep.hibernation_swap]\npath = \"/swap/file\"\n")
	cfg, prov, err := config.Load(root, testDirs, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Profile != "laptop" || cfg.Host.Sleep.HibernationSwap.Path != "/swap/file" {
		t.Errorf("cfg = %+v", cfg)
	}
	if prov["host.sleep.hibernation_swap.path"].Layer != "profile" {
		t.Errorf("origin = %+v", prov["host.sleep.hibernation_swap.path"])
	}
}

func TestProfileFromEnv(t *testing.T) {
	root := fsx.Root{Dir: t.TempDir()}
	write(t, root, "/home/u/.config/cairn/profiles/desk.toml", "[host.sleep]\nlid = \"ignore\"\n")
	cfg, _, err := config.Load(root, testDirs, env(map[string]string{"CAIRN_PROFILE": "desk"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Profile != "desk" || cfg.Host.Sleep.Lid != "ignore" {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestProfileNameCannotEscape(t *testing.T) {
	root := fsx.Root{Dir: t.TempDir()}
	_, _, err := config.Load(root, testDirs, env(map[string]string{"CAIRN_PROFILE": "../x"}))
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestUnknownKeyIsError(t *testing.T) {
	root := fsx.Root{Dir: t.TempDir()}
	write(t, root, "/home/u/.config/cairn/cairn.toml", "[host.sleep]\nlidd = \"suspend\"\n")
	_, _, err := config.Load(root, testDirs, env(nil))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"/home/u/.config/cairn/cairn.toml", "host.sleep.lidd"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

func TestMalformedTOMLNamesFile(t *testing.T) {
	root := fsx.Root{Dir: t.TempDir()}
	write(t, root, "/etc/cairn/cairn.toml", "[host.sleep\n")
	_, _, err := config.Load(root, testDirs, env(nil))
	if err == nil || !strings.Contains(err.Error(), "/etc/cairn/cairn.toml") {
		t.Fatalf("err = %v", err)
	}
}

func TestWrongTypeNamesFile(t *testing.T) {
	root := fsx.Root{Dir: t.TempDir()}
	write(t, root, "/home/u/.config/cairn/cairn.toml", "[host.gpu.nvidia]\nmanage = \"yes\"\n")
	_, _, err := config.Load(root, testDirs, env(nil))
	if err == nil || !strings.Contains(err.Error(), "host.gpu.nvidia.manage") {
		t.Fatalf("err = %v", err)
	}
}

func TestBadDurationNamesKey(t *testing.T) {
	root := fsx.Root{Dir: t.TempDir()}
	write(t, root, "/home/u/.config/cairn/cairn.toml", "[host.sleep]\nhibernate_after = \"soon\"\n")
	_, _, err := config.Load(root, testDirs, env(nil))
	if err == nil || !strings.Contains(err.Error(), "host.sleep.hibernate_after") ||
		!strings.Contains(err.Error(), "cairn.toml") {
		t.Fatalf("err = %v", err)
	}
}

func TestEnvTypedValues(t *testing.T) {
	root := fsx.Root{Dir: t.TempDir()}
	cfg, _, err := config.Load(root, testDirs, env(map[string]string{
		"CAIRN_HOST_GPU_NVIDIA_MANAGE":           "false",
		"CAIRN_HOST_SLEEP_HIBERNATE_AFTER":       "2h",
		"CAIRN_HOST_SLEEP_HIBERNATION_SWAP_PATH": "/s",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host.GPU.Nvidia.Manage || cfg.Host.Sleep.HibernateAfter.Duration != 2*time.Hour ||
		cfg.Host.Sleep.HibernationSwap.Path != "/s" {
		t.Errorf("cfg = %+v", cfg.Host)
	}
	_, _, err = config.Load(root, testDirs, env(map[string]string{"CAIRN_HOST_GPU_NVIDIA_MANAGE": "maybe"}))
	if err == nil || !strings.Contains(err.Error(), "CAIRN_HOST_GPU_NVIDIA_MANAGE") {
		t.Fatalf("err = %v", err)
	}
}

func TestCheckEnv(t *testing.T) {
	ok := []string{"CAIRN_PROFILE=x", "CAIRN_HOST_SLEEP_LID=ignore", "PATH=/bin", "CAIRN_OTHER=1"}
	if err := config.CheckEnv(ok); err != nil {
		t.Errorf("unexpected: %v", err)
	}
	err := config.CheckEnv([]string{"CAIRN_HOST_SLEEP_LIDD=x"})
	if err == nil || !strings.Contains(err.Error(), "CAIRN_HOST_SLEEP_LIDD") {
		t.Errorf("err = %v", err)
	}
}

func TestDuration(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		span string
	}{
		{"15min", 15 * time.Minute, "15min"},
		{"60min", time.Hour, "1h"},
		{"2h", 2 * time.Hour, "2h"},
		{"30s", 30 * time.Second, "30s"},
		{"1h 30min", 90 * time.Minute, "1h 30min"},
		{"15m", 15 * time.Minute, "15min"},
	}
	for _, c := range cases {
		d, err := config.ParseDuration(c.in)
		if err != nil || d.Duration != c.want {
			t.Errorf("%q: %v %v", c.in, d, err)
			continue
		}
		if got := d.SystemdSpan(); got != c.span {
			t.Errorf("%q span = %q, want %q", c.in, got, c.span)
		}
	}
	for _, bad := range []string{"", "soon", "15", "min", "-5min", "5parsecs"} {
		if _, err := config.ParseDuration(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestExplain(t *testing.T) {
	root := fsx.Root{Dir: t.TempDir()}
	cfg, prov, err := config.Load(root, testDirs, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	got, err := config.Explain(cfg, prov, "host.sleep.hibernate_after")
	if err != nil || got != "15min  defaults  (embedded)" {
		t.Errorf("got %q err %v", got, err)
	}
	if _, err := config.Explain(cfg, prov, "host.nope"); err == nil {
		t.Error("unknown key accepted")
	}
}

func TestTestdataFixture(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "config", "user.toml"))
	if err != nil {
		t.Fatal(err)
	}
	root := fsx.Root{Dir: t.TempDir()}
	write(t, root, "/home/u/.config/cairn/cairn.toml", string(b))
	cfg, _, err := config.Load(root, testDirs, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host.Sleep.CriticalBattery != "poweroff" {
		t.Errorf("cfg = %+v", cfg.Host.Sleep)
	}
}
