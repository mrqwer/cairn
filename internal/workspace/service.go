package workspace

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/mrqwer/cairn/internal/execx"
	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/inventory"
	"github.com/mrqwer/cairn/internal/plan"
)

// Service policy and unit names. The base session keeps the tmux server
// alive while no workspace session exists; it is not cairn-managed (no
// marker), so cairn never reconciles or kills it.
const (
	ServicePolicy = "workspace"
	TmuxUnit      = "cairn-tmux.service"
	UpUnit        = "cairn-up.service"
	baseSession   = "cairn-base"
)

var socketName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)

// CheckServiceInput validates the values embedded in the unit files: the
// cairn binary (absolute, host-module safety rule) and the optional tmux
// socket name.
func CheckServiceInput(self, socket string) error {
	if !filepath.IsAbs(self) {
		return fmt.Errorf("cairn binary path %q must be absolute", self)
	}
	if err := plan.CheckSelfPath(self); err != nil {
		return err
	}
	if socket != "" && !socketName.MatchString(socket) {
		return fmt.Errorf("socket name %q is invalid: use letters, digits, . _ - and do not start with . or -", socket)
	}
	return nil
}

// ServiceActions returns the user-level actions that install the tmux server
// unit and the restore-on-login unit in userUnitDir, reload systemd and
// enable both (never start them). Invalid self or socket (see
// CheckServiceInput) yields nil.
func ServiceActions(self, socket, userUnitDir string) []plan.Action {
	if CheckServiceInput(self, socket) != nil {
		return nil
	}
	tmux := "/usr/bin/env tmux"
	up := self + " up"
	if socket != "" {
		tmux += " -L " + socket
		up += " --socket " + socket
	}
	// Without a socket the unit shares the owner's default tmux server, which
	// may predate it and hold hand-made sessions: never kill-server there.
	// KillMode=control-group already ends a server the unit started itself.
	stop := ""
	if socket != "" {
		stop = "ExecStop=" + tmux + " kill-server\n"
	}
	hdr := fsx.Header(ServicePolicy)
	tmuxUnit := hdr + `[Unit]
Description=tmux server for cairn workspaces

[Service]
Type=forking
ExecStart=` + tmux + ` -f /dev/null new-session -d -s ` + baseSession + `
ExecStartPost=` + tmux + ` set -g exit-empty off
` + stop + `KillMode=control-group

[Install]
WantedBy=default.target
`
	upUnit := hdr + `[Unit]
Description=Restore cairn workspaces
After=` + TmuxUnit + `
Requires=` + TmuxUnit + `

[Service]
Type=oneshot
ExecStart=` + up + `

[Install]
WantedBy=default.target
`
	file := func(name, content, reason string) plan.Action {
		return plan.Action{
			Kind: plan.WriteFile, Path: filepath.Join(userUnitDir, name), Content: []byte(content),
			Mode: 0o644, Class: inventory.DropIn, Policy: ServicePolicy, Reason: reason,
		}
	}
	enable := func(unit string) plan.Action {
		return plan.Action{Kind: plan.EnableUnit, Unit: unit, UserUnit: true, Class: inventory.UnitEnable, Policy: ServicePolicy}
	}
	return []plan.Action{
		file(TmuxUnit, tmuxUnit, "run a tmux server as a user service"),
		file(UpUnit, upUnit, "restore workspaces on login"),
		{Kind: plan.RunCommand, Cmd: []string{"systemctl", "--user", "daemon-reload"}, Policy: ServicePolicy},
		enable(TmuxUnit),
		enable(UpUnit),
	}
}

// UnitStatus is what status reports for one unit. Enabled and Active are the
// first words systemctl printed, or "unknown".
type UnitStatus struct {
	Unit            string
	File            bool
	Enabled, Active string
}

var unitStates = map[string]bool{
	"enabled": true, "enabled-runtime": true, "linked": true, "linked-runtime": true,
	"alias": true, "static": true, "indirect": true, "generated": true, "transient": true,
	"masked": true, "masked-runtime": true, "disabled": true, "not-found": true,
	"active": true, "inactive": true, "failed": true, "activating": true,
	"deactivating": true, "reloading": true, "maintenance": true, "refreshing": true,
}

// ParseUnitState reads the answer of `systemctl is-enabled|is-active`: the
// first line when it is a known state, else "unknown".
func ParseUnitState(out []byte) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	line = strings.TrimSpace(line)
	if unitStates[line] {
		return line
	}
	return "unknown"
}

// QueryUnits reports the file and systemd state of both service units. A
// non-zero exit from systemctl is a normal answer (disabled, inactive,
// not-found), not an error.
func QueryUnits(ctx context.Context, r execx.Runner, root fsx.Root, userUnitDir string) []UnitStatus {
	var out []UnitStatus
	for _, u := range []string{TmuxUnit, UpUnit} {
		st := UnitStatus{Unit: u, File: root.Exists(filepath.Join(userUnitDir, u))}
		query := func(verb string) string {
			o, _ := r.Run(ctx, "systemctl", "--user", verb, u)
			return ParseUnitState(o)
		}
		st.Enabled, st.Active = query("is-enabled"), query("is-active")
		out = append(out, st)
	}
	return out
}

// RecordedSocket returns the tmux socket name a generated cairn-tmux.service
// was written for ("" for the default server).
func RecordedSocket(unit []byte) string {
	for _, line := range strings.Split(string(unit), "\n") {
		if rest, ok := strings.CutPrefix(line, "ExecStart=/usr/bin/env tmux "); ok {
			if f := strings.Fields(rest); len(f) > 1 && f[0] == "-L" {
				return f[1]
			}
		}
	}
	return ""
}
