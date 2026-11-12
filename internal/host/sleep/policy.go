// Package sleep turns the sleep configuration into plan actions: logind,
// sleep and UPower drop-ins, the hibernate fallback, the system-sleep hook and
// the user-side notification units.
package sleep

import (
	"io/fs"
	"path"
	"strings"

	"github.com/mrqwer/cairn/internal/config"
	"github.com/mrqwer/cairn/internal/detect"
	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/inventory"
	"github.com/mrqwer/cairn/internal/plan"
)

// Policy is the policy name recorded on every action and in file headers.
const Policy = "sleep"

const (
	hostStateDir    = plan.SysStateDir
	gsSchema        = "org.gnome.settings-daemon.plugins.power"
	gsKey           = "sleep-inactive-battery-type"
	gsKeyAC         = "sleep-inactive-ac-type"
	fallbackUnit    = "cairn-sleep-fallback.service"
	notifyPathUnit  = "cairn-host-notify.path"
	notifySvcUnit   = "cairn-host-notify.service"
	bootCheckUnit   = "cairn-host-boot-check.service"
	systemSleepHook = "/usr/lib/systemd/system-sleep/50-cairn"
)

// idleAC maps host.sleep.lid_ac to GNOME's idle action on AC power, so idle
// does what closing the lid on AC does (GNOME has no suspend-then-hibernate;
// with HibernateOnACPower=no that stays suspended anyway).
var idleAC = map[string]string{
	"suspend-then-hibernate": "suspend",
	"suspend":                "suspend",
	"hibernate":              "hibernate",
	"ignore":                 "nothing",
}

var upowerAction = map[string]string{
	"hibernate":    "Hibernate",
	"hybrid-sleep": "HybridSleep",
	"poweroff":     "PowerOff",
}

// Actions returns the sleep policy actions in a fixed order: root files, user
// units, gsetting, daemon-reloads, unit enables. self is the absolute path of
// the cairn binary; userUnitDir is the user systemd unit directory.
func Actions(cfg config.Sleep, sys detect.System, self, userUnitDir string) []plan.Action {
	var out []plan.Action
	rootFile := func(p, content string, mode fs.FileMode) {
		out = append(out, plan.Action{
			Kind: plan.WriteFile, Path: p, Content: []byte(content), Mode: mode,
			NeedsRoot: true, Class: inventory.DropIn, Policy: Policy,
		})
	}
	userFile := func(name, content string) {
		out = append(out, plan.Action{
			Kind: plan.WriteFile, Path: path.Join(userUnitDir, name), Content: []byte(content),
			Mode: 0o644, Class: inventory.DropIn, Policy: Policy,
		})
	}
	hdr := fsx.Header(Policy)

	rootFile("/etc/systemd/logind.conf.d/50-cairn.conf",
		hdr+"[Login]\nHandleLidSwitch="+cfg.Lid+"\nHandleLidSwitchExternalPower="+cfg.LidAC+"\n", 0o644)

	sl := hdr + "[Sleep]\nAllowSuspendThenHibernate=yes\nHibernateDelaySec=" + cfg.HibernateAfter.SystemdSpan() +
		"\nSuspendEstimationSec=15min\nHibernateOnACPower=no\n"
	if sys.MemSleepDeep {
		sl += "MemorySleepMode=deep\n"
	}
	rootFile("/etc/systemd/sleep.conf.d/50-cairn.conf", sl, 0o644)

	// The fallback unit first: the drop-ins below name it in OnFailure=.
	rootFile("/etc/systemd/system/"+fallbackUnit,
		hdr+"[Unit]\nDescription=cairn: suspend when hibernation is refused\n\n[Service]\nType=oneshot\nExecStart="+
			self+" host record-sleep fallback\nExecStart=/usr/bin/systemctl suspend\n", 0o644)

	hook := hdr + "[Unit]\nOnFailure=" + fallbackUnit + "\n\n[Service]\nExecStartPre=" + self + " host sleep-check --record\n"
	rootFile("/etc/systemd/system/systemd-hibernate.service.d/50-cairn.conf", hook, 0o644)
	rootFile("/etc/systemd/system/systemd-suspend-then-hibernate.service.d/50-cairn.conf", hook, 0o644)

	rootFile(systemSleepHook,
		"#!/bin/sh\n"+hdr+"exec "+self+" host record-sleep \"$1\" \"$2\"\n", 0o755)

	if sys.UPowerDropIns {
		rootFile("/etc/UPower/UPower.conf.d/50-cairn.conf",
			hdr+"[UPower]\nCriticalPowerAction="+upowerAction[cfg.CriticalBattery]+"\n", 0o644)
	}

	out = append(out, plan.Action{
		Kind: plan.MakeDir, Path: hostStateDir, Mode: 0o755,
		NeedsRoot: true, Class: inventory.ManagedDir, Policy: Policy,
	})

	userFile(notifyPathUnit, hdr+"[Unit]\nDescription=cairn: watch host events\n\n[Path]\nPathChanged="+
		hostStateDir+"/events.jsonl\n\n[Install]\nWantedBy=default.target\n")
	userFile(notifySvcUnit, hdr+"[Unit]\nDescription=cairn: show host notifications\n\n[Service]\nType=oneshot\nExecStart="+
		self+" host notify\n")
	userFile(bootCheckUnit, hdr+"[Unit]\nDescription=cairn: check the last sleep cycle after login\n\n[Service]\nType=oneshot\nExecStart="+
		self+" host boot-check\n\n[Install]\nWantedBy=default.target\n")

	if sys.Desktop == "gnome" {
		out = append(out, plan.Action{
			Kind: plan.SetGSetting, Schema: gsSchema, Key: gsKey,
			Value: "'" + strings.ReplaceAll(cfg.IdleBattery, "'", "") + "'",
			Class: inventory.UserSetting, Policy: Policy,
		})
		if v, ok := idleAC[cfg.LidAC]; ok {
			out = append(out, plan.Action{
				Kind: plan.SetGSetting, Schema: gsSchema, Key: gsKeyAC, Value: "'" + v + "'",
				Class: inventory.UserSetting, Policy: Policy,
			})
		}
	}

	out = append(out,
		plan.Action{Kind: plan.RunCommand, Cmd: []string{"systemctl", "daemon-reload"}, NeedsRoot: true, Policy: Policy},
		plan.Action{Kind: plan.RunCommand, Cmd: []string{"systemctl", "--user", "daemon-reload"}, Policy: Policy},
	)
	for _, u := range []string{notifyPathUnit, bootCheckUnit} {
		out = append(out, plan.Action{Kind: plan.EnableUnit, Unit: u, UserUnit: true, Class: inventory.UnitEnable, Policy: Policy})
	}
	return out
}
