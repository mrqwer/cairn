// Package host assembles the host policies (sleep, NVIDIA, package hook,
// repair, uninstall) into plans and runs a host operation on both sides:
// the user's own actions, then the privileged step that root computes and
// confirms itself.
package host

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/mrqwer/cairn/internal/config"
	"github.com/mrqwer/cairn/internal/detect"
	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/host/gpu"
	"github.com/mrqwer/cairn/internal/host/repair"
	"github.com/mrqwer/cairn/internal/host/sleep"
	"github.com/mrqwer/cairn/internal/inventory"
	"github.com/mrqwer/cairn/internal/plan"
	"github.com/mrqwer/cairn/internal/xdg"
)

// PacmanHookPath records a pending reboot after kernel or NVIDIA upgrades.
const PacmanHookPath = "/etc/pacman.d/hooks/50-cairn-reboot-required.hook"

// PacmanHook returns the package hook action; self is the cairn binary.
func PacmanHook(self string) plan.Action {
	content := fsx.Header(sleep.Policy) + `[Trigger]
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
Exec = ` + self + " host record-sleep reboot-required\n"
	return plan.Action{
		Kind: plan.WriteFile, Path: PacmanHookPath, Content: []byte(content), Mode: 0o644,
		NeedsRoot: true, Class: inventory.DropIn, Policy: sleep.Policy,
		Reason: "record kernel/driver upgrades that need a reboot",
	}
}

// DesiredActions returns the root-level actions of the apply plan: the root
// part of sleep.Actions, gpu.NvidiaActions (vendorNvidia is the content of
// the vendor modprobe file), enabling the present NVIDIA vendor sleep units
// (nvidiaUnits) and, on pacman systems, the reboot-required hook; root
// daemon-reloads come last. User-level actions come from UserActions, since
// they need the user's unit directory.
func DesiredActions(cfg *config.Config, sys detect.System, self string, vendorNvidia []byte, nvidiaUnits []string) []plan.Action {
	var out, cmds []plan.Action
	for _, a := range sleep.Actions(cfg.Host.Sleep, sys, self, "") {
		switch {
		case !a.NeedsRoot:
		case a.Kind == plan.RunCommand:
			cmds = append(cmds, a)
		default:
			out = append(out, a)
		}
	}
	out = append(out, gpu.NvidiaActions(cfg.Host.GPU.Nvidia, sys, vendorNvidia)...)
	out = append(out, gpu.VendorUnitActions(cfg.Host.GPU.Nvidia, sys, nvidiaUnits)...)
	if sys.PkgManager == "pacman" {
		out = append(out, PacmanHook(self))
	}
	return append(out, cmds...)
}

// UserActions returns the user-level actions of the apply plan.
func UserActions(cfg *config.Config, sys detect.System, self, userUnitDir string) []plan.Action {
	var out []plan.Action
	for _, a := range sleep.Actions(cfg.Host.Sleep, sys, self, userUnitDir) {
		if !a.NeedsRoot {
			out = append(out, a)
		}
	}
	return out
}

// UserUnitDir is the user's systemd unit directory: systemd/user next to
// cairn's own config directory.
func UserUnitDir(d xdg.Dirs) string {
	return filepath.Join(filepath.Dir(d.Config), "systemd", "user")
}

// Builder generates root's actions with its own view of the filesystem. Its
// Build method is what plan.RootBuilder is set to, so it is the one place
// root actions come from.
type Builder struct {
	Root fsx.Root
	// Probes compute the resume target for repair; a nil Offset means
	// repair cannot compute it.
	Probes sleep.Probes
}

// Build returns the root actions for mode: the apply policies, the repair
// plan, or the removals recorded in the system inventory.
func (b Builder) Build(mode string, h config.Host, sys detect.System, self string) ([]plan.Action, error) {
	switch mode {
	case plan.ModeApply:
		var vendor []byte
		var units []string
		if sys.GPU == "nvidia" && h.GPU.Nvidia.Manage {
			v, err := b.Root.ReadFile(gpu.VendorUpstream)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
			vendor, units = v, gpu.PresentUnits(b.Root)
		}
		return DesiredActions(&config.Config{Host: h}, sys, self, vendor, units), nil
	case plan.ModeRepair:
		if b.Probes.Offset == nil {
			return nil, errors.New("repair needs a resume offset function")
		}
		return repair.Plan(b.Root, sys, h.Sleep, b.Probes).Actions, nil
	case plan.ModeUninstall:
		inv, err := inventory.Load(b.Root, plan.SysInventoryPath)
		if err != nil {
			return nil, err
		}
		extra := plan.ExtraPaths(h)
		var out []plan.Action
		for _, a := range UninstallPlan(inv, nil).Actions {
			// A directory from an older temporary_file_path is not allowed
			// any more; it stays recorded rather than blocking uninstall.
			if plan.CheckRootAction(a, extra, plan.ModeUninstall) == nil {
				out = append(out, a)
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("unknown mode %q", mode)
}

// Notes returns the report-only findings for mode. Repair: why boot entries
// were not updated, and the problems repair only reports. Uninstall: every
// repair recorded in the system inventory, which uninstall does not revert.
func (b Builder) Notes(mode string, h config.Host, sys detect.System) []string {
	if mode == plan.ModeUninstall {
		inv, err := inventory.Load(b.Root, plan.SysInventoryPath)
		if err != nil {
			return []string{err.Error()}
		}
		var out []string
		for _, e := range inv.Entries {
			if e.Class == inventory.Repaired {
				backup := e.Backup
				if backup == "" {
					backup = "none"
				}
				out = append(out, fmt.Sprintf("repair not reverted: %s (backup: %s)", e.Path, backup))
			}
		}
		return out
	}
	if mode != plan.ModeRepair {
		return nil
	}
	if b.Probes.Offset == nil {
		return repair.Report(b.Root, sys)
	}
	return repair.Plan(b.Root, sys, h.Sleep, b.Probes).Notes
}

// PlanBoth resolves a host operation for display: userActs against the user
// inventory, and root's actions from plan.BuildRoot against the system
// inventory. The root part is a preview: apply-root recomputes it. Repair
// has no preview, since only root can compute it.
func PlanBoth(mode string, h config.Host, sys detect.System, self string, userActs []plan.Action,
	userEnv, rootEnv plan.Env) (user, root plan.Plan, err error) {
	for _, a := range userActs {
		if a.NeedsRoot {
			return user, root, fmt.Errorf("policy bug: root action %s %s must come from plan.RootBuilder", a.Kind, a.Path)
		}
	}
	user = plan.Resolve(userActs, userEnv.Root, userEnv.UserInv)
	user.Mode = mode
	root.Mode = mode
	if mode == plan.ModeRepair {
		return user, root, nil
	}
	acts, err := plan.BuildRoot(mode, h, sys, self)
	if err != nil {
		return user, root, err
	}
	root = plan.Resolve(acts, rootEnv.Root, rootEnv.SysInv)
	root.Mode = mode
	return user, root, nil
}

// Privileged runs the root step for a request; the CLI goes through sudo,
// tests call ApplyRootSide directly.
type Privileged func(context.Context, plan.RootRequest) error

// CheckRootPreview validates, on the user side, the request that would be
// sent to root and every action of root's preview, so an invalid request or
// a policy bug stops the operation before anything is applied. Root
// validates everything again itself.
func CheckRootPreview(req plan.RootRequest, preview plan.Plan) error {
	if err := req.Validate(); err != nil {
		return fmt.Errorf("invalid root request: %w", err)
	}
	extra := plan.ExtraPaths(req.Host)
	var errs []error
	for _, a := range preview.Actions {
		errs = append(errs, plan.CheckRootAction(a, extra, req.Mode))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid root plan: %w", err)
	}
	return nil
}

// ApplyBoth runs a host operation on both sides after CheckRootPreview.
// user holds the user-level actions (already shown and confirmed), root the
// user side's preview of root's plan; privileged nil skips the root step.
//
// On apply the privileged step runs first and user actions only after it
// succeeded: user settings such as idle hibernation must never take effect
// without root's sleep-entry guard. On uninstall the order is reversed, so
// the guard is removed last. When the root step fails or is declined on
// apply, nothing is applied on the user side and the error says so.
func ApplyBoth(ctx context.Context, user, root plan.Plan, userEnv plan.Env, req plan.RootRequest, force bool, privileged Privileged) error {
	if err := CheckRootPreview(req, root); err != nil {
		return err
	}
	applyUser := func() error {
		if len(user.Pending()) == 0 {
			return nil
		}
		return plan.Apply(ctx, user, userEnv, plan.Options{Force: force, UserOnly: true})
	}
	runRoot := func() error {
		if privileged == nil {
			return nil
		}
		return privileged(ctx, req)
	}
	if req.Mode == plan.ModeUninstall {
		if err := applyUser(); err != nil {
			return err
		}
		return runRoot()
	}
	if err := runRoot(); err != nil {
		if len(user.Pending()) > 0 {
			return fmt.Errorf("%w; user-level changes were not applied", err)
		}
		return err
	}
	return applyUser()
}

// ApplyRootSide is the body of apply-root: load the system inventory from
// env.Root and run plan.ApplyRoot.
func ApplyRootSide(ctx context.Context, req plan.RootRequest, env plan.Env, opts plan.RootOptions) error {
	inv, err := inventory.Load(env.Root, plan.SysInventoryPath)
	if err != nil {
		return err
	}
	env.SysInv, env.SysInvPath = inv, plan.SysInventoryPath
	return plan.ApplyRoot(ctx, req, env, opts)
}
