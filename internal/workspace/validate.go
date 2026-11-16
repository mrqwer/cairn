package workspace

import (
	"fmt"
	"path/filepath"
	"strings"
)

// FieldError is one validation problem; Key is a dotted path into the config.
type FieldError struct{ Key, Msg string }

func (e FieldError) Error() string { return e.Key + ": " + e.Msg }

// Validate checks a defaulted project (see ApplyDefaults).
func (p Project) Validate() []FieldError {
	var errs []FieldError
	add := func(key, format string, a ...any) {
		errs = append(errs, FieldError{key, fmt.Sprintf(format, a...)})
	}
	ctl := func(key, val string) {
		if hasControl(val) {
			add(key, "%q must not contain control characters", val)
		}
	}
	ctl("id", p.ID)
	ctl("name", p.Name)
	ctl("session", p.Session)
	if !slugRe.MatchString(p.ID) {
		add("id", "%q must match [a-z0-9][a-z0-9_-]{0,62}", p.ID)
	}
	if !slugRe.MatchString(p.Session) {
		add("session", "%q must match [a-z0-9][a-z0-9_-]{0,62}", p.Session)
	}
	if strings.ContainsAny(p.Session, ":.") || strings.IndexFunc(p.Session, isSpace) >= 0 {
		add("session", "%q must not contain ':', '.' or whitespace", p.Session)
	}
	if !filepath.IsAbs(p.Root) {
		add("root", "%q must be an absolute path (or start with ~/)", p.Root)
	}
	switch p.Status {
	case StatusActive, StatusPaused, StatusArchived:
	default:
		add("status", "%q must be active, paused or archived", p.Status)
	}
	if len(p.Windows) == 0 {
		add("windows", "at least one window is required")
	}
	seen := map[string]bool{}
	for wi, w := range p.Windows {
		wk := fmt.Sprintf("windows[%d]", wi)
		if !slugRe.MatchString(w.Key) {
			add(wk+".key", "%q must match [a-z0-9][a-z0-9_-]{0,62}", w.Key)
		} else if seen[w.Key] {
			add(wk+".key", "duplicate window key %q", w.Key)
		}
		seen[w.Key] = true
		ctl(wk+".key", w.Key)
		ctl(wk+".name", w.Name)
		if len(w.Panes) == 0 {
			add(wk+".panes", "at least one pane is required")
		}
		for i, pn := range w.Panes {
			validatePane(wk, i, pn, add)
			pk := fmt.Sprintf("%s.panes[%d]", wk, i)
			ctl(pk+".role", pn.Role)
			ctl(pk+".cwd", pn.Cwd)
			ctl(pk+".title", pn.Title)
			for j, r := range pn.Run {
				ctl(fmt.Sprintf("%s.run[%d]", pk, j), r)
			}
		}
	}
	return errs
}

// hasControl reports whether s holds a byte below 0x20 or 0x7f.
func hasControl(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0
}

func isSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f'
}

func validatePane(wk string, i int, pn Pane, add func(string, string, ...any)) {
	pk := fmt.Sprintf("%s.panes[%d]", wk, i)
	switch pn.Role {
	case RoleShell, RoleEditor, RoleCommand, RoleService:
	default:
		add(pk+".role", "%q must be shell, editor, command or service", pn.Role)
	}
	switch pn.Restart {
	case RestartNever, RestartOnFailure, RestartAlways:
	default:
		add(pk+".restart", "%q must be never, on-failure or always", pn.Restart)
	}
	if strings.HasPrefix(pn.Cwd, "~") {
		add(pk+".cwd", "%q: \"~\" is not expanded in a pane cwd; use an absolute path or a path relative to the project root", pn.Cwd)
	}
	for j, r := range pn.Run {
		if r == "" {
			add(fmt.Sprintf("%s.run[%d]", pk, j), "must not be empty")
		}
	}
	if i == 0 {
		if pn.Split != "" {
			add(pk+".split", "the first pane must not have a split")
		}
		return
	}
	if pn.Split != SplitH && pn.Split != SplitV {
		add(pk+".split", "%q must be h or v", pn.Split)
	}
	if pn.From <= 0 || pn.From > i {
		add(pk+".from", "%d must be between 1 and %d", pn.From, i)
	}
	if pn.Size < 5 || pn.Size > 95 {
		add(pk+".size", "%d must be between 5 and 95", pn.Size)
	}
}
