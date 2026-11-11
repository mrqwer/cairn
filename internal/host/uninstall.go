package host

import (
	"slices"
	"sort"
	"strings"

	"github.com/mrqwer/cairn/internal/inventory"
	"github.com/mrqwer/cairn/internal/plan"
)

// enabledStates are `systemctl is-enabled` answers meaning the unit was
// already active before cairn enabled it; uninstall leaves such units alone.
var enabledStates = []string{
	"enabled", "enabled-runtime", "linked", "linked-runtime", "alias", "static",
	"indirect", "generated", "transient", "masked", "masked-runtime",
}

// UninstallPlan undoes what the inventories record: units cairn enabled are
// disabled (unless they were enabled before), managed files are removed (a
// backed-up original is restored), settings go back to their previous value,
// managed directories are removed when empty, then systemd reloads. Root
// actions come first. The system state directory, which holds the inventory,
// backups and event log, and boot entries rewritten by repair are kept.
func UninstallPlan(sysInv, userInv *inventory.Inventory) plan.Plan {
	p := plan.Plan{Mode: plan.ModeUninstall}
	p.Actions = append(uninstallSide(sysInv, true), uninstallSide(userInv, false)...)
	return p
}

func uninstallSide(inv *inventory.Inventory, root bool) []plan.Action {
	if inv == nil {
		return nil
	}
	var units, files, settings, dirs []plan.Action
	for _, e := range inv.Entries {
		base := plan.Action{NeedsRoot: root, Policy: e.Policy, Class: e.Class}
		switch e.Class {
		case inventory.DropIn, inventory.Mask:
			a := base
			a.Kind, a.Path, a.Mode = plan.RemoveFile, e.Path, 0o644
			files = append(files, a)
		case inventory.ManagedDir:
			if root && e.Path == plan.SysStateDir {
				continue
			}
			a := base
			a.Kind, a.Path = plan.RemoveFile, e.Path
			dirs = append(dirs, a)
		case inventory.UnitEnable:
			unit, ok := strings.CutPrefix(e.Path, "unit:")
			if !root {
				unit, ok = strings.CutPrefix(e.Path, "user-unit:")
			}
			if !ok || slices.Contains(enabledStates, e.Previous) {
				continue
			}
			a := base
			a.Kind, a.Unit, a.UserUnit = plan.DisableUnit, unit, !root
			units = append(units, a)
		case inventory.UserSetting:
			rest, ok := strings.CutPrefix(e.Path, "gsettings:")
			schema, key, ok2 := strings.Cut(rest, " ")
			if !ok || !ok2 || e.Previous == "" {
				continue
			}
			a := base
			a.Kind, a.Schema, a.Key, a.Value = plan.SetGSetting, schema, key, e.Previous
			settings = append(settings, a)
		}
	}
	// Deepest directories first.
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].Path > dirs[j].Path })
	out := slices.Concat(units, files, settings, dirs)
	if len(units)+len(files) > 0 {
		cmd := []string{"systemctl", "daemon-reload"}
		if !root {
			cmd = []string{"systemctl", "--user", "daemon-reload"}
		}
		out = append(out, plan.Action{Kind: plan.RunCommand, Cmd: cmd, NeedsRoot: root})
	}
	return out
}
