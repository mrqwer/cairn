// Package workspace models per-project tmux workspace configuration:
// the TOML schema, loading, validation, built-in templates and marshalling.
package workspace

import (
	"path/filepath"
	"regexp"
	"strings"
)

// Pane roles, restart policies, split directions and project statuses.
const (
	RoleShell   = "shell"
	RoleEditor  = "editor"
	RoleCommand = "command"
	RoleService = "service"

	RestartNever     = "never"
	RestartOnFailure = "on-failure"
	RestartAlways    = "always"

	SplitH = "h"
	SplitV = "v"

	StatusActive   = "active"
	StatusPaused   = "paused"
	StatusArchived = "archived"

	DefaultSize = 50
)

// Pane is one tmux pane inside a window.
type Pane struct {
	Role    string   `toml:"role"`
	Cwd     string   `toml:"cwd"`
	Run     []string `toml:"run"`
	Restart string   `toml:"restart"`
	Split   string   `toml:"split"`
	From    int      `toml:"from"`
	Size    int      `toml:"size"`
	Title   string   `toml:"title"`
}

// Window is one tmux window; panes are created by a split sequence.
type Window struct {
	Key   string `toml:"key"`
	Name  string `toml:"name"`
	Panes []Pane `toml:"panes"`
}

// Project is the config of one managed tmux session.
type Project struct {
	ID      string   `toml:"id"`
	Name    string   `toml:"name"`
	Root    string   `toml:"root"`
	Session string   `toml:"session"`
	Status  string   `toml:"status"`
	Tags    []string `toml:"tags"`
	Windows []Window `toml:"windows"`
}

var (
	slugRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	nonSlugRe = regexp.MustCompile(`[^a-z0-9_-]+`)
)

// Slug lowercases s, replaces runs of characters outside [a-z0-9_-] with
// "-", trims "-" and truncates to 63 characters.
func Slug(s string) string {
	s = nonSlugRe.ReplaceAllString(strings.ToLower(s), "-")
	s = strings.Trim(s, "-")
	if len(s) > 63 {
		s = strings.TrimRight(s[:63], "-")
	}
	return s
}

// DefaultRestart is the restart policy implied by a pane role.
func DefaultRestart(role string) string {
	if role == RoleService {
		return RestartOnFailure
	}
	return RestartNever
}

// ApplyDefaults fills in omitted values: session, status, root expansion
// of "~/" via home, window key/name, a default shell pane per empty window,
// and per pane role, restart, cwd, from and size.
func (p *Project) ApplyDefaults(home string) {
	if p.Session == "" {
		p.Session = p.ID
	}
	if p.Status == "" {
		p.Status = StatusActive
	}
	if p.Root == "~" {
		p.Root = home
	} else if strings.HasPrefix(p.Root, "~/") {
		p.Root = filepath.Join(home, p.Root[2:])
	}
	for wi := range p.Windows {
		w := &p.Windows[wi]
		if w.Key == "" {
			w.Key = Slug(w.Name)
		}
		if w.Name == "" {
			w.Name = w.Key
		}
		if len(w.Panes) == 0 {
			w.Panes = []Pane{{}}
		}
		for i := range w.Panes {
			pn := &w.Panes[i]
			if pn.Role == "" {
				pn.Role = RoleShell
			}
			if pn.Restart == "" {
				pn.Restart = DefaultRestart(pn.Role)
			}
			switch {
			case pn.Cwd == "":
				pn.Cwd = p.Root
			case !filepath.IsAbs(pn.Cwd) && p.Root != "":
				pn.Cwd = filepath.Join(p.Root, pn.Cwd)
			}
			if i > 0 {
				if pn.From == 0 {
					pn.From = i
				}
				if pn.Size == 0 {
					pn.Size = DefaultSize
				}
			}
		}
	}
}
