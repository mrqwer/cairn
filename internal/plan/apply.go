package plan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/mrqwer/cairn/internal/config"
	"github.com/mrqwer/cairn/internal/events"
	"github.com/mrqwer/cairn/internal/execx"
	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/inventory"
)

// ErrModifiedByUser is returned when managed files changed since cairn wrote
// them and Force is not set.
var ErrModifiedByUser = errors.New("modified since cairn wrote them (re-run with --force to overwrite; a backup is kept)")

// Env is where and how a plan is applied.
type Env struct {
	Root                    fsx.Root
	Runner                  execx.Runner
	UserInv, SysInv         *inventory.Inventory
	UserInvPath, SysInvPath string
	Events                  events.Log
	// UserBackupDir receives backups of foreign user-level files, since the
	// system backup directory is root-only. Empty means BackupDir.
	UserBackupDir string
}

// Options select and constrain what Apply executes. Extra is passed to
// AllowedRootPath for root actions.
type Options struct {
	Force              bool
	RootOnly, UserOnly bool
	Extra              []string
}

// Apply executes the pending actions of p in order. Every selected action is
// validated first (root paths and commands against the allowlist, user
// commands against theirs); nothing runs unless all pass. Foreign files are
// backed up before being overwritten or removed. The inventories of the sides
// that ran are saved even when an action fails, so completed work stays
// recorded.
func Apply(ctx context.Context, p Plan, env Env, opts Options) error {
	mode := p.Mode
	if mode == "" {
		mode = ModeApply
	}
	var sel []Action
	for _, a := range p.Actions {
		if a.State == Unchanged || (opts.RootOnly && !a.NeedsRoot) || (opts.UserOnly && a.NeedsRoot) {
			continue
		}
		sel = append(sel, a)
	}
	var errs []error
	var modified []string
	needUser, needSys := false, false
	for _, a := range sel {
		var err error
		if a.NeedsRoot {
			needSys = true
			err = CheckRootAction(a, opts.Extra, mode)
		} else {
			needUser = true
			err = CheckUserAction(a)
		}
		if err != nil {
			errs = append(errs, err)
		}
		if a.State == ModifiedByUser {
			modified = append(modified, a.target())
		}
	}
	if needUser && env.UserInv == nil || needSys && env.SysInv == nil {
		errs = append(errs, errors.New("plan: inventory not loaded"))
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	if len(modified) > 0 && !opts.Force {
		return fmt.Errorf("%w: %s", ErrModifiedByUser, strings.Join(modified, ", "))
	}

	userBackup := env.UserBackupDir
	if userBackup == "" {
		userBackup = BackupDir
	}
	var runErr error
	for _, a := range sel {
		inv, backupDir := env.UserInv, userBackup
		if a.NeedsRoot {
			inv, backupDir = env.SysInv, BackupDir
		}
		if err := env.applyOne(ctx, a, inv, backupDir); err != nil {
			runErr = fmt.Errorf("%s %s: %w", a.Kind, a.target(), err)
			break
		}
	}
	errs = append(errs, runErr)
	if needUser {
		errs = append(errs, env.UserInv.Save(env.Root, env.UserInvPath))
	}
	if needSys {
		errs = append(errs, env.SysInv.Save(env.Root, env.SysInvPath))
	}
	return errors.Join(errs...)
}

func classOr(c, def inventory.Class) inventory.Class {
	if c == "" {
		return def
	}
	return c
}

func (env Env) run(ctx context.Context, cmd ...string) ([]byte, error) {
	out, err := env.Runner.Run(ctx, cmd[0], cmd[1:]...)
	if err != nil {
		return out, fmt.Errorf("%s: %w: %s", strings.Join(cmd, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func systemctl(a Action, verb string) []string {
	if a.UserUnit {
		return []string{"systemctl", "--user", verb, a.Unit}
	}
	return []string{"systemctl", verb, a.Unit}
}

// applyOne executes a and records the result in inv. Semantics beyond the
// obvious: RemoveFile of a managed file with a backup restores the backup;
// SetGSetting back to the recorded previous value un-manages the setting;
// MakeDir on a pre-existing directory only records it (no chmod).
func (env Env) applyOne(ctx context.Context, a Action, inv *inventory.Inventory, backupDir string) error {
	key := a.target()
	prev, managed := inv.Get(key)
	switch a.Kind {
	case WriteFile:
		e := inventory.Entry{Path: a.Path, Policy: a.Policy, Class: classOr(a.Class, inventory.DropIn), SHA256: fsx.SHA256(a.Content)}
		if managed {
			e.Backup, e.Previous, e.Upstream, e.UpstreamSHA256 = prev.Backup, prev.Previous, prev.Upstream, prev.UpstreamSHA256
		}
		switch a.State {
		case Foreign:
			b, err := env.backup(a.Path, backupDir)
			if err != nil {
				return err
			}
			e.Backup = b
		case ModifiedByUser: // keep the user's edit, but uninstall restores the original
			if _, err := env.backup(a.Path, backupDir); err != nil {
				return err
			}
		}
		if err := env.Root.WriteAtomic(a.Path, a.Content, modeOr(a.Mode, defaultFileMode)); err != nil {
			return err
		}
		inv.Upsert(e)
	case RemoveFile:
		if a.State == Foreign || a.State == ModifiedByUser {
			if _, err := env.backup(a.Path, backupDir); err != nil {
				return err
			}
		}
		if managed && prev.Backup != "" {
			data, err := env.Root.ReadFile(prev.Backup)
			if err != nil {
				return fmt.Errorf("restore backup: %w", err)
			}
			if err := env.Root.WriteAtomic(a.Path, data, modeOr(a.Mode, defaultFileMode)); err != nil {
				return err
			}
		} else if err := env.Root.Remove(a.Path); err != nil {
			return err
		}
		inv.Delete(key)
	case MakeDir:
		e := inventory.Entry{Path: a.Path, Policy: a.Policy, Class: classOr(a.Class, inventory.ManagedDir)}
		if managed {
			e.Previous = prev.Previous
		}
		if a.State == Foreign {
			e.Previous = preexisting
		}
		if e.Previous != preexisting {
			mode := modeOr(a.Mode, defaultDirMode)
			if err := env.Root.MkdirAll(a.Path, mode); err != nil {
				return err
			}
			if err := os.Chmod(env.Root.Path(a.Path), mode); err != nil {
				return err
			}
		}
		inv.Upsert(e)
	case EnableUnit:
		previous := prev.Previous
		if !managed {
			// is-enabled exits non-zero for disabled units; its output is the state.
			out, _ := env.Runner.Run(ctx, "systemctl", systemctl(a, "is-enabled")[1:]...)
			if previous = strings.TrimSpace(firstLine(out)); previous == "" {
				previous = "unknown"
			}
		}
		if _, err := env.run(ctx, systemctl(a, "enable")...); err != nil {
			return err
		}
		inv.Upsert(inventory.Entry{Path: key, Policy: a.Policy, Class: classOr(a.Class, inventory.UnitEnable), Previous: previous})
	case DisableUnit:
		if _, err := env.run(ctx, systemctl(a, "disable")...); err != nil {
			return err
		}
		inv.Delete(key)
	case SetGSetting:
		if managed && a.Value == prev.Previous {
			if _, err := env.run(ctx, "gsettings", "set", a.Schema, a.Key, a.Value); err != nil {
				return err
			}
			inv.Delete(key)
			return nil
		}
		previous := prev.Previous
		if !managed {
			out, err := env.run(ctx, "gsettings", "get", a.Schema, a.Key)
			if err != nil {
				return err
			}
			previous = strings.TrimSpace(string(out))
		}
		if _, err := env.run(ctx, "gsettings", "set", a.Schema, a.Key, a.Value); err != nil {
			return err
		}
		inv.Upsert(inventory.Entry{Path: key, Policy: a.Policy, Class: classOr(a.Class, inventory.UserSetting),
			SHA256: fsx.SHA256([]byte(a.Value)), Previous: previous})
	case RunCommand:
		if len(a.Cmd) == 0 {
			return errors.New("empty command")
		}
		if _, err := env.run(ctx, a.Cmd...); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown action kind %q", a.Kind)
	}
	return nil
}

func firstLine(b []byte) string {
	s, _, _ := strings.Cut(string(b), "\n")
	return s
}

// backup copies p into dir as "<p with / replaced by %>" (mode 0600) and
// returns the backup path. An existing backup with different content is
// never overwritten; a numbered name is used instead.
func (env Env) backup(p, dir string) (string, error) {
	data, err := env.Root.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("backup %s: %w", p, err)
	}
	if err := env.Root.MkdirAll(path.Dir(dir), 0o755); err != nil {
		return "", err
	}
	if err := env.Root.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	base := dir + "/" + strings.ReplaceAll(p, "/", "%")
	for i := 0; ; i++ {
		name := base
		if i > 0 {
			name = fmt.Sprintf("%s.%d", base, i)
		}
		old, err := env.Root.ReadFile(name)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return name, env.Root.WriteAtomic(name, data, 0o600)
		case err != nil:
			return "", err
		case bytes.Equal(old, data):
			return name, nil
		}
	}
}

const (
	requestVersion = 1
	maxRequestSize = 16 << 20
)

// RootRequest is what the user-side `cairn host apply` hands to
// `cairn host apply-root` through sudo. The root side treats it as untrusted:
// it re-validates every field and re-resolves the actions itself, and needs
// no XDG environment or config files.
type RootRequest struct {
	Version int         `json:"version"`
	Mode    string      `json:"mode"`
	Force   bool        `json:"force,omitempty"`
	Extra   []string    `json:"extra,omitempty"`
	Host    config.Host `json:"host"`
	Actions []Action    `json:"actions"`
}

// NewRootRequest builds the request for the root actions of p (all of them,
// including unchanged ones, since the root side re-resolves).
func NewRootRequest(p Plan, extra []string, host config.Host, force bool) RootRequest {
	mode := p.Mode
	if mode == "" {
		mode = ModeApply
	}
	r := RootRequest{Version: requestVersion, Mode: mode, Force: force, Extra: extra, Host: host}
	for _, a := range p.Actions {
		if a.NeedsRoot {
			r.Actions = append(r.Actions, a)
		}
	}
	return r
}

// Validate checks the whole request; any failure rejects all of it.
func (r RootRequest) Validate() error {
	if r.Version != requestVersion {
		return fmt.Errorf("plan request version %d, want %d", r.Version, requestVersion)
	}
	if r.Mode != ModeApply && r.Mode != ModeRepair {
		return fmt.Errorf("plan request mode %q is not %q or %q", r.Mode, ModeApply, ModeRepair)
	}
	errs := []error{checkExtra(r.Extra)}
	cfg := config.Config{Host: r.Host}
	for _, e := range cfg.Validate() {
		errs = append(errs, e)
	}
	for _, a := range r.Actions {
		if !a.NeedsRoot {
			errs = append(errs, fmt.Errorf("%s %s: user-level action in root request", a.Kind, a.target()))
			continue
		}
		errs = append(errs, CheckRootAction(a, r.Extra, r.Mode))
	}
	return errors.Join(errs...)
}

// WriteRootRequest stores r at p with mode 0600 (parent created 0700).
func WriteRootRequest(root fsx.Root, p string, r RootRequest) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := root.MkdirAll(path.Dir(p), 0o700); err != nil {
		return err
	}
	return root.WriteAtomic(p, data, 0o600)
}

// ReadRootRequest loads a request written by WriteRootRequest. The file must
// be a regular, not group/world-writable file; parse errors do not echo its
// content (it is read with root privileges).
func ReadRootRequest(root fsx.Root, p string) (RootRequest, error) {
	var r RootRequest
	st, err := root.Stat(p)
	if err != nil {
		return r, fmt.Errorf("plan file: %w", err)
	}
	if !st.Mode().IsRegular() || st.Size() > maxRequestSize || st.Mode().Perm()&0o022 != 0 {
		return r, fmt.Errorf("plan file %s: must be a regular file, at most %d bytes, not group/world writable", p, maxRequestSize)
	}
	data, err := root.ReadFile(p)
	if err != nil {
		return r, fmt.Errorf("plan file: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return RootRequest{}, fmt.Errorf("plan file %s is not a valid cairn plan", p)
	}
	return r, nil
}

// HostSnapshot is the content of HostConfigPath for host.
func HostSnapshot(host config.Host) ([]byte, error) {
	data, err := json.MarshalIndent(host, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// SnapshotStale reports whether HostConfigPath differs from host.
func SnapshotStale(root fsx.Root, host config.Host) bool {
	want, err := HostSnapshot(host)
	if err != nil {
		return true
	}
	got, err := root.ReadFile(HostConfigPath)
	return err != nil || !bytes.Equal(got, want)
}

// ApplyRoot is the root side of apply: validate everything, re-resolve the
// actions against the system inventory, write the host config snapshot for
// later root-side commands, then apply the root actions.
func ApplyRoot(ctx context.Context, r RootRequest, env Env) error {
	if err := r.Validate(); err != nil {
		return fmt.Errorf("rejected plan: %w", err)
	}
	if env.SysInv == nil {
		return errors.New("plan: system inventory not loaded")
	}
	p := Resolve(r.Actions, env.Root, env.SysInv)
	p.Mode = r.Mode
	if m := p.ModifiedPaths(); len(m) > 0 && !r.Force {
		return fmt.Errorf("%w: %s", ErrModifiedByUser, strings.Join(m, ", "))
	}
	snap, err := HostSnapshot(r.Host)
	if err != nil {
		return err
	}
	if err := env.Root.MkdirAll(SysStateDir, 0o755); err != nil {
		return err
	}
	if err := env.Root.WriteAtomic(HostConfigPath, snap, 0o644); err != nil {
		return err
	}
	return Apply(ctx, p, env, Options{Force: r.Force, RootOnly: true, Extra: r.Extra})
}
