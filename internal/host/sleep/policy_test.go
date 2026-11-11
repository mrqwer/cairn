package sleep_test

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrqwer/cairn/internal/config"
	"github.com/mrqwer/cairn/internal/detect"
	"github.com/mrqwer/cairn/internal/host/sleep"
	"github.com/mrqwer/cairn/internal/plan"
)

const (
	self    = "/usr/local/bin/cairn"
	userDir = "/home/u/.config/systemd/user"
)

var update = flag.Bool("update", false, "rewrite golden files")

func defaults(t *testing.T) config.Sleep {
	t.Helper()
	span, err := config.ParseDuration("15min")
	if err != nil {
		t.Fatal(err)
	}
	return config.Sleep{
		Lid: "suspend-then-hibernate", LidAC: "suspend-then-hibernate",
		CriticalBattery: "hibernate", IdleBattery: "hibernate", HibernateAfter: span,
	}
}

func fixture() detect.System {
	return detect.System{Desktop: "gnome", MemSleepDeep: true, UPowerDropIns: true}
}

func goldenName(p string) string {
	return strings.ReplaceAll(strings.TrimPrefix(p, "/"), "/", "__") + ".conf"
}

func find(acts []plan.Action, kind plan.Kind, path string) *plan.Action {
	for i := range acts {
		if acts[i].Kind == kind && acts[i].Path == path {
			return &acts[i]
		}
	}
	return nil
}

func TestSleepActionsGolden(t *testing.T) {
	acts := sleep.Actions(defaults(t), fixture(), self, userDir)
	n := 0
	for _, a := range acts {
		if a.Kind != plan.WriteFile {
			continue
		}
		n++
		g := filepath.Join("..", "..", "..", "testdata", "sleep", "golden", goldenName(a.Path))
		if *update {
			if err := os.WriteFile(g, a.Content, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(g)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a.Content, want) {
			t.Errorf("%s differs from golden:\n%s", a.Path, a.Content)
		}
	}
	if n != 10 {
		t.Errorf("%d files generated, want 10", n)
	}
}

func TestDeterministic(t *testing.T) {
	a := sleep.Actions(defaults(t), fixture(), self, userDir)
	b := sleep.Actions(defaults(t), fixture(), self, userDir)
	if len(a) != len(b) {
		t.Fatal("length differs")
	}
	for i := range a {
		if a[i].Kind != b[i].Kind || a[i].Path != b[i].Path || !bytes.Equal(a[i].Content, b[i].Content) {
			t.Errorf("action %d differs", i)
		}
	}
}

func TestAllActionsPassAllowlist(t *testing.T) {
	for _, a := range sleep.Actions(defaults(t), fixture(), self, userDir) {
		var err error
		if a.NeedsRoot {
			err = plan.CheckRootAction(a, nil, plan.ModeApply)
		} else {
			err = plan.CheckUserAction(a)
		}
		if err != nil {
			t.Errorf("%s %s %v: %v", a.Kind, a.Path, a.Cmd, err)
		}
		if a.Policy != sleep.Policy {
			t.Errorf("%s %s: policy %q", a.Kind, a.Path, a.Policy)
		}
	}
}

func TestNoUPowerDropInWhenUnsupported(t *testing.T) {
	sys := fixture()
	sys.UPowerDropIns = false
	if find(sleep.Actions(defaults(t), sys, self, userDir), plan.WriteFile, "/etc/UPower/UPower.conf.d/50-cairn.conf") != nil {
		t.Error("UPower drop-in generated")
	}
}

func TestNoGSettingOutsideGnome(t *testing.T) {
	sys := fixture()
	sys.Desktop = "kde"
	for _, a := range sleep.Actions(defaults(t), sys, self, userDir) {
		if a.Kind == plan.SetGSetting {
			t.Error("gsetting generated outside gnome")
		}
	}
	got := gsetting(sleep.Actions(defaults(t), fixture(), self, userDir), "sleep-inactive-battery-type")
	if got == nil || got.Value != "'hibernate'" || got.NeedsRoot {
		t.Errorf("gsetting = %+v", got)
	}
}

func gsetting(acts []plan.Action, key string) *plan.Action {
	for i := range acts {
		if acts[i].Kind == plan.SetGSetting && acts[i].Key == key {
			return &acts[i]
		}
	}
	return nil
}

func TestIdleACFollowsLidAC(t *testing.T) {
	for lidAC, want := range map[string]string{
		"suspend-then-hibernate": "'suspend'", "suspend": "'suspend'", "hibernate": "'hibernate'", "ignore": "'nothing'",
	} {
		cfg := defaults(t)
		cfg.LidAC = lidAC
		got := gsetting(sleep.Actions(cfg, fixture(), self, userDir), "sleep-inactive-ac-type")
		if got == nil || got.Value != want || got.Schema != "org.gnome.settings-daemon.plugins.power" || got.NeedsRoot {
			t.Errorf("%s: %+v", lidAC, got)
		}
	}
}

func TestMemorySleepModeOnlyWhenDeep(t *testing.T) {
	const p = "/etc/systemd/sleep.conf.d/50-cairn.conf"
	if a := find(sleep.Actions(defaults(t), fixture(), self, userDir), plan.WriteFile, p); a == nil || !strings.Contains(string(a.Content), "MemorySleepMode=deep") {
		t.Error("missing MemorySleepMode=deep")
	}
	sys := fixture()
	sys.MemSleepDeep = false
	if a := find(sleep.Actions(defaults(t), sys, self, userDir), plan.WriteFile, p); a == nil || strings.Contains(string(a.Content), "MemorySleepMode") {
		t.Error("MemorySleepMode present without deep")
	}
}

func TestCriticalBatteryMapping(t *testing.T) {
	for in, want := range map[string]string{"hibernate": "Hibernate", "hybrid-sleep": "HybridSleep", "poweroff": "PowerOff"} {
		cfg := defaults(t)
		cfg.CriticalBattery = in
		a := find(sleep.Actions(cfg, fixture(), self, userDir), plan.WriteFile, "/etc/UPower/UPower.conf.d/50-cairn.conf")
		if a == nil || !strings.Contains(string(a.Content), "CriticalPowerAction="+want+"\n") {
			t.Errorf("%s: %v", in, a)
		}
	}
}
