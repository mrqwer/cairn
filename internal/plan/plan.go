// Package plan turns desired host actions into a resolved plan, renders it and
// applies it, recording every managed object in the inventory.
package plan

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/inventory"
)

// Kind is the type of change an action makes.
type Kind string

const (
	WriteFile   Kind = "WriteFile"
	RemoveFile  Kind = "RemoveFile"
	MakeDir     Kind = "MakeDir"
	EnableUnit  Kind = "EnableUnit"
	DisableUnit Kind = "DisableUnit"
	SetGSetting Kind = "SetGSetting"
	RunCommand  Kind = "RunCommand"
)

// State is the resolved relation between an action and the current system.
type State string

const (
	Create         State = "create"
	Update         State = "update"
	Unchanged      State = "unchanged"
	ModifiedByUser State = "modified-by-user"
	Foreign        State = "foreign" // exists, not in inventory
)

// Plan modes. Repair mode widens the root allowlist (see AllowedRootPath).
const (
	ModeApply  = "apply"
	ModeRepair = "repair"
)

// System-side locations owned by cairn host.
const (
	SysStateDir      = "/var/lib/cairn/host"
	SysInventoryPath = SysStateDir + "/managed.json"
	HostConfigPath   = SysStateDir + "/config.json"
	BackupDir        = SysStateDir + "/backup"
)

const (
	defaultFileMode fs.FileMode = 0o644
	defaultDirMode  fs.FileMode = 0o755
	preexisting                 = "preexisting"
)

// Action is one desired change. Which fields matter depends on Kind:
// files use Path/Content/Mode, units Unit/UserUnit, settings
// Schema/Key/Value, commands Cmd.
type Action struct {
	Kind      Kind            `json:"kind"`
	Path      string          `json:"path,omitempty"`
	Content   []byte          `json:"content,omitempty"`
	Mode      fs.FileMode     `json:"mode,omitempty"`
	Unit      string          `json:"unit,omitempty"`
	UserUnit  bool            `json:"user_unit,omitempty"`
	Schema    string          `json:"schema,omitempty"`
	Key       string          `json:"key,omitempty"`
	Value     string          `json:"value,omitempty"`
	Cmd       []string        `json:"cmd,omitempty"`
	NeedsRoot bool            `json:"needs_root,omitempty"`
	Class     inventory.Class `json:"class,omitempty"`
	Policy    string          `json:"policy,omitempty"`
	Reason    string          `json:"reason,omitempty"`
	// Upstream is the vendor file a masking WriteFile shadows; Apply records
	// its path and content hash in the inventory.
	Upstream string `json:"upstream,omitempty"`
	State    State  `json:"state,omitempty"`
}

// Plan is an ordered list of resolved actions.
type Plan struct {
	Mode    string
	Actions []Action
}

// Pending returns the actions apply would execute.
func (p Plan) Pending() []Action {
	var out []Action
	for _, a := range p.Actions {
		if a.State != Unchanged {
			out = append(out, a)
		}
	}
	return out
}

// ModifiedPaths lists the targets changed by the user since cairn wrote them.
func (p Plan) ModifiedPaths() []string {
	var out []string
	for _, a := range p.Actions {
		if a.State == ModifiedByUser {
			out = append(out, a.target())
		}
	}
	return out
}

// target is the inventory key of the object an action manages.
func (a Action) target() string {
	switch a.Kind {
	case EnableUnit, DisableUnit:
		if a.UserUnit {
			return "user-unit:" + a.Unit
		}
		return "unit:" + a.Unit
	case SetGSetting:
		return "gsettings:" + a.Schema + " " + a.Key
	case RunCommand:
		return strings.Join(a.Cmd, " ")
	default:
		return a.Path
	}
}

// fileMode returns m, or def when m is zero. Bits beyond the permission bits
// (setuid, setgid, sticky, type bits) are an error rather than silently
// dropped.
func fileMode(m, def fs.FileMode) (fs.FileMode, error) {
	if m == 0 {
		return def, nil
	}
	if m&^fs.ModePerm != 0 {
		return 0, fmt.Errorf("mode %v: only permission bits are allowed", m)
	}
	return m, nil
}

// samePerm reports whether fi's permissions equal the action's mode.
func samePerm(fi fs.FileInfo, m, def fs.FileMode) bool {
	want, err := fileMode(m, def)
	return err == nil && fi.Mode().Perm() == want
}

// Resolve sets each action's State by comparing it with disk and inv. A
// RunCommand is pending only when another action on the same side (user or
// root) is pending, so an up-to-date system yields no pending actions.
func Resolve(desired []Action, root fsx.Root, inv *inventory.Inventory) Plan {
	p := Plan{Mode: ModeApply}
	pendingSide := map[bool]bool{}
	for _, a := range desired {
		if a.Kind != RunCommand {
			a.State = resolveOne(a, root, inv)
			if a.State != Unchanged {
				pendingSide[a.NeedsRoot] = true
			}
		}
		p.Actions = append(p.Actions, a)
	}
	for i := range p.Actions {
		if a := &p.Actions[i]; a.Kind == RunCommand {
			a.State = Unchanged
			if pendingSide[a.NeedsRoot] {
				a.State = Update
			}
		}
	}
	return p
}

// ResolveAll resolves user-level actions against userInv and root actions
// against sysInv, keeping the original order.
func ResolveAll(desired []Action, root fsx.Root, userInv, sysInv *inventory.Inventory) Plan {
	var user, sys []Action
	for _, a := range desired {
		if a.NeedsRoot {
			sys = append(sys, a)
		} else {
			user = append(user, a)
		}
	}
	u, s := Resolve(user, root, userInv), Resolve(sys, root, sysInv)
	out := Plan{Mode: ModeApply}
	for _, a := range desired {
		if a.NeedsRoot {
			out.Actions = append(out.Actions, s.Actions[0])
			s.Actions = s.Actions[1:]
		} else {
			out.Actions = append(out.Actions, u.Actions[0])
			u.Actions = u.Actions[1:]
		}
	}
	return out
}

func resolveOne(a Action, root fsx.Root, inv *inventory.Inventory) State {
	e, managed := inv.Get(a.target())
	switch a.Kind {
	case WriteFile:
		data, err := root.ReadFile(a.Path)
		if errors.Is(err, fs.ErrNotExist) {
			return Create
		}
		if err != nil {
			return Update // unreadable here; the applying side re-resolves
		}
		if !managed {
			return Foreign
		}
		cur, want := fsx.SHA256(data), fsx.SHA256(a.Content)
		if cur != e.SHA256 {
			if cur == want {
				return Update // refresh the inventory record
			}
			return ModifiedByUser
		}
		if cur != want {
			return Update
		}
		if st, err := root.Stat(a.Path); err != nil || !samePerm(st, a.Mode, defaultFileMode) {
			return Update
		}
		return Unchanged
	case RemoveFile:
		data, err := root.ReadFile(a.Path)
		if errors.Is(err, fs.ErrNotExist) {
			if managed {
				return Update // drop the stale record, restore a backup if any
			}
			return Unchanged
		}
		if err != nil {
			return Update
		}
		if !managed {
			return Foreign
		}
		if fsx.SHA256(data) != e.SHA256 {
			return ModifiedByUser
		}
		return Update
	case MakeDir:
		st, err := os.Lstat(root.Path(a.Path))
		if errors.Is(err, fs.ErrNotExist) {
			return Create
		}
		if err != nil || !st.IsDir() {
			return Update // symlink, file or unreadable: apply reports the error
		}
		if !managed {
			return Foreign
		}
		if e.Previous != preexisting && !samePerm(st, a.Mode, defaultDirMode) {
			return Update
		}
		return Unchanged
	case EnableUnit:
		if managed {
			return Unchanged
		}
		return Create
	case DisableUnit:
		if managed {
			return Update
		}
		return Unchanged
	case SetGSetting:
		switch {
		case !managed:
			return Create
		case e.SHA256 == fsx.SHA256([]byte(a.Value)):
			return Unchanged
		default:
			return Update
		}
	}
	return Update
}
