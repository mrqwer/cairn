package host

import (
	"strings"
	"testing"

	"github.com/mrqwer/cairn/internal/config"
	"github.com/mrqwer/cairn/internal/detect"
	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/inventory"
	"github.com/mrqwer/cairn/internal/plan"
)

func describe(a plan.Action) string {
	s := string(a.Kind) + " " + a.Path + a.Unit + strings.Join(a.Cmd, " ")
	if a.Kind == plan.SetGSetting {
		s += a.Schema + " " + a.Key + "=" + a.Value
	}
	if a.NeedsRoot {
		s += " [root]"
	}
	return s
}

func TestUninstallPlan(t *testing.T) {
	sysInv := &inventory.Inventory{}
	for _, e := range []inventory.Entry{
		{Path: plan.SysStateDir, Class: inventory.ManagedDir},
		{Path: "/home/.nvidia-suspend", Class: inventory.ManagedDir},
		{Path: "/etc/modprobe.d/nvidia-utils.conf", Class: inventory.Mask, Backup: plan.BackupDir + "/x"},
		{Path: "/etc/systemd/sleep.conf.d/50-cairn.conf", Class: inventory.DropIn},
		{Path: "/boot/loader/entries/arch.conf", Class: inventory.Repaired},
		{Path: "unit:nvidia-resume.service", Class: inventory.UnitEnable, Previous: "enabled"},
		{Path: "unit:cairn-x.service", Class: inventory.UnitEnable, Previous: "disabled"},
	} {
		sysInv.Upsert(e)
	}
	userInv := &inventory.Inventory{}
	for _, e := range []inventory.Entry{
		{Path: "/config/systemd/user/cairn-host-notify.path", Class: inventory.DropIn},
		{Path: "user-unit:cairn-host-notify.path", Class: inventory.UnitEnable, Previous: "disabled"},
		{Path: "gsettings:org.gnome.settings-daemon.plugins.power sleep-inactive-battery-type", Class: inventory.UserSetting, Previous: "'suspend'"},
	} {
		userInv.Upsert(e)
	}
	p := UninstallPlan(sysInv, userInv)
	var got []string
	for _, a := range p.Actions {
		got = append(got, describe(a))
	}
	want := []string{
		"DisableUnit cairn-x.service [root]",
		"RemoveFile /etc/modprobe.d/nvidia-utils.conf [root]",
		"RemoveFile /etc/systemd/sleep.conf.d/50-cairn.conf [root]",
		"RemoveFile /home/.nvidia-suspend [root]",
		"RunCommand systemctl daemon-reload [root]",
		"DisableUnit cairn-host-notify.path",
		"RemoveFile /config/systemd/user/cairn-host-notify.path",
		"SetGSetting org.gnome.settings-daemon.plugins.power sleep-inactive-battery-type='suspend'",
		"RunCommand systemctl --user daemon-reload",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if p.Mode != plan.ModeUninstall {
		t.Fatalf("mode %q", p.Mode)
	}
	for _, a := range p.Actions {
		if a.NeedsRoot {
			if err := plan.CheckRootAction(a, []string{"/home/.nvidia-suspend"}, plan.ModeUninstall); err != nil {
				t.Errorf("root allowlist rejects %s: %v", describe(a), err)
			}
		} else if err := plan.CheckUserAction(a); err != nil {
			t.Errorf("user allowlist rejects %s: %v", describe(a), err)
		}
	}
	if len(UninstallPlan(nil, nil).Actions) != 0 {
		t.Fatal("empty inventories must give an empty plan")
	}
}

func TestPacmanHookContent(t *testing.T) {
	a := PacmanHook("/usr/local/bin/cairn")
	want := "# Managed by cairn host (sleep). Do not edit; run \"cairn host ...\"\n" + `[Trigger]
Operation = Upgrade
Type = Package
Target = linux
Target = linux-lts
Target = linux-zen
Target = linux-hardened
Target = nvidia*

[Action]
Description = cairn: recording that a reboot is required for safe hibernation
When = PostTransaction
Exec = /usr/local/bin/cairn host record-sleep reboot-required
`
	if string(a.Content) != want || a.Path != "/etc/pacman.d/hooks/50-cairn-reboot-required.hook" || !a.NeedsRoot {
		t.Fatalf("%s\n%q", a.Path, a.Content)
	}
	if err := plan.CheckRootAction(a, nil, plan.ModeApply); err != nil {
		t.Fatal(err)
	}
}

func TestUninstallNotesRepairs(t *testing.T) {
	root := fsx.Root{Dir: t.TempDir()}
	inv := &inventory.Inventory{}
	inv.Upsert(inventory.Entry{Path: "/boot/loader/entries/a.conf", Class: inventory.Repaired, Backup: plan.BackupDir + "/%boot%loader%entries%a.conf"})
	inv.Upsert(inventory.Entry{Path: "/etc/systemd/sleep.conf.d/50-cairn.conf", Class: inventory.DropIn})
	if err := inv.Save(root, plan.SysInventoryPath); err != nil {
		t.Fatal(err)
	}
	notes := Builder{Root: root}.Notes(plan.ModeUninstall, config.Host{}, detect.System{})
	want := "repair not reverted: /boot/loader/entries/a.conf (backup: " + plan.BackupDir + "/%boot%loader%entries%a.conf)"
	if len(notes) != 1 || notes[0] != want {
		t.Fatalf("notes %q", notes)
	}
	for _, a := range UninstallPlan(inv, nil).Actions {
		if a.Path == "/boot/loader/entries/a.conf" {
			t.Fatal("uninstall touches a repaired file")
		}
	}
}
