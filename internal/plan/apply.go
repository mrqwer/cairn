package plan

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
	"syscall"

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
	// OwnerOf returns the owning uid of a file; nil means the real owner.
	// Tests inject it because temp dirs cannot be root-owned.
	OwnerOf func(fs.FileInfo) (uid uint32, ok bool)
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
		if err := env.applyOne(ctx, a, inv, backupDir, opts.Extra); err != nil {
			runErr = fmt.Errorf("%s %s: %w", a.Kind, a.target(), err)
			break
		}
	}
	errs = append(errs, runErr)
	if needUser {
		errs = append(errs, env.UserInv.Save(env.Root, env.UserInvPath))
	}
	if needSys {
		if err := env.secureDir(path.Dir(env.SysInvPath), true); err != nil {
			errs = append(errs, err)
		} else {
			errs = append(errs, env.SysInv.Save(env.Root, env.SysInvPath))
		}
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
func (env Env) applyOne(ctx context.Context, a Action, inv *inventory.Inventory, backupDir string, extra []string) error {
	key := a.target()
	prev, managed := inv.Get(key)
	if a.NeedsRoot && (a.Kind == WriteFile || a.Kind == RemoveFile) {
		if err := env.secureDir(path.Dir(a.Path), a.Kind == WriteFile); err != nil {
			return err
		}
	}
	switch a.Kind {
	case WriteFile:
		e := inventory.Entry{Path: a.Path, Policy: a.Policy, Class: classOr(a.Class, inventory.DropIn), SHA256: fsx.SHA256(a.Content)}
		if managed {
			e.Backup, e.Previous, e.Upstream, e.UpstreamSHA256 = prev.Backup, prev.Previous, prev.Upstream, prev.UpstreamSHA256
		}
		if a.Upstream != "" {
			up, err := env.Root.ReadFile(a.Upstream)
			if err != nil {
				return fmt.Errorf("read upstream: %w", err)
			}
			e.Upstream, e.UpstreamSHA256 = a.Upstream, fsx.SHA256(up)
		}
		switch a.State {
		case Foreign:
			b, err := env.backup(a.Path, backupDir, a.NeedsRoot)
			if err != nil {
				return err
			}
			e.Backup = b
		case ModifiedByUser: // keep the user's edit, but uninstall restores the original
			if _, err := env.backup(a.Path, backupDir, a.NeedsRoot); err != nil {
				return err
			}
		}
		mode, err := fileMode(a.Mode, defaultFileMode)
		if err != nil {
			return err
		}
		if err := env.Root.WriteAtomic(a.Path, a.Content, mode); err != nil {
			return err
		}
		inv.Upsert(e)
	case RemoveFile:
		if managed && prev.Class == inventory.ManagedDir {
			return env.removeManagedDir(a.Path, inv, key)
		}
		if a.State == Foreign || a.State == ModifiedByUser {
			if _, err := env.backup(a.Path, backupDir, a.NeedsRoot); err != nil {
				return err
			}
		}
		if managed && prev.Backup != "" {
			data, err := env.Root.ReadFile(prev.Backup)
			if err != nil {
				return fmt.Errorf("restore backup: %w", err)
			}
			mode, err := fileMode(a.Mode, defaultFileMode)
			if err != nil {
				return err
			}
			if err := env.Root.WriteAtomic(a.Path, data, mode); err != nil {
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
			if err := env.makeDir(a, extra); err != nil {
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

// removeManagedDir removes a managed directory only when it is empty; a
// non-empty one stays, together with its inventory record.
func (env Env) removeManagedDir(p string, inv *inventory.Inventory, key string) error {
	full := env.Root.Path(p)
	st, err := os.Lstat(full)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		inv.Delete(key)
		return nil
	case err != nil:
		return err
	case !st.IsDir():
		return fmt.Errorf("%s is not a directory", p)
	}
	if err := os.Remove(full); err != nil {
		if errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) {
			return nil
		}
		return err
	}
	inv.Delete(key)
	return nil
}

func firstLine(b []byte) string {
	s, _, _ := strings.Cut(string(b), "\n")
	return s
}

// backup copies p into dir as "<p with / replaced by %>" (mode 0600) and
// returns the backup path. An existing backup with different content is
// never overwritten; a numbered name is used instead.
func (env Env) backup(p, dir string, asRoot bool) (string, error) {
	data, err := env.Root.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("backup %s: %w", p, err)
	}
	if asRoot {
		if err := env.secureDir(path.Dir(dir), true); err != nil {
			return "", err
		}
		if err := os.Mkdir(env.Root.Path(dir), 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return "", err
		}
		if err := env.secureDir(dir, false); err != nil {
			return "", err
		}
	} else {
		if err := env.Root.MkdirAll(path.Dir(dir), 0o755); err != nil {
			return "", err
		}
		if err := env.Root.MkdirAll(dir, 0o700); err != nil {
			return "", err
		}
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

// makeDir creates (if missing) and chmods the directory a.Path without
// following symlinks. As root, every parent must be root-owned and not
// group/world writable, and the parent of a config-derived (extra) path
// must already exist.
func (env Env) makeDir(a Action, extra []string) error {
	mode, err := fileMode(a.Mode, defaultDirMode)
	if err != nil {
		return err
	}
	p := path.Clean(a.Path)
	full := env.Root.Path(p)
	st, err := os.Lstat(full)
	switch {
	case err == nil && st.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("%s is a symlink; refusing to use it", p)
	case err == nil && !st.IsDir():
		return fmt.Errorf("%s exists and is not a directory", p)
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return err
	}
	missing := err != nil
	parent := path.Dir(p)
	switch {
	case !a.NeedsRoot:
		if err := env.Root.MkdirAll(parent, 0o755); err != nil {
			return err
		}
	case inExtra(p, extra):
		// Config-derived path: the parent must already exist and be trusted.
		if err := trustedChain(env.Root, parent, env.OwnerOf); err != nil {
			return err
		}
	default:
		if err := env.secureDir(parent, true); err != nil {
			return err
		}
	}
	if missing {
		if err := os.Mkdir(full, mode); err != nil {
			return err
		}
	}
	fd, err := syscall.Open(full, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", p, err)
	}
	defer func() { _ = syscall.Close(fd) }()
	if err := syscall.Fchmod(fd, uint32(mode)); err != nil {
		return fmt.Errorf("chmod %s: %w", p, err)
	}
	return nil
}

// secureDir is the root side's way to reach a directory: it walks from "/"
// to dir, Lstat-checking each component with trustedNode (no symlinks,
// root-owned, not group/world writable, a directory). With create, a missing
// component is made with os.Mkdir(0755) and verified before descending;
// without, the walk stops quietly at the first missing component.
func (env Env) secureDir(dir string, create bool) error {
	cur := "/"
	for _, c := range append([]string{""}, strings.Split(strings.Trim(path.Clean(dir), "/"), "/")...) {
		cur = path.Join(cur, c)
		if _, err := os.Lstat(env.Root.Path(cur)); errors.Is(err, fs.ErrNotExist) {
			if !create {
				return nil
			}
			if err := os.Mkdir(env.Root.Path(cur), 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
		}
		st, err := trustedNode(env.Root, cur, env.OwnerOf)
		if err != nil {
			return err
		}
		if !st.IsDir() {
			return fmt.Errorf("%s is not a directory", cur)
		}
	}
	return nil
}
