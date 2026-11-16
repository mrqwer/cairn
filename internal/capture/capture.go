// Package capture turns a live, unmanaged tmux session (as read by
// tmux.Snapshot) into an undefaulted project config. It is pure: it never
// talks to tmux or looks at the filesystem; the idle check is injected.
package capture

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mrqwer/cairn/internal/tmux"
	"github.com/mrqwer/cairn/internal/workspace"
	"github.com/mrqwer/cairn/internal/workspace/layout"
)

// Options tune a capture. ID defaults to the slug of the session name and
// Root to the most common directory of the panes. IsIdle reports whether a
// shell pane's process sits at its prompt; nil treats every shell as idle.
type Options struct {
	ID, Root string
	IsIdle   func(pid int) bool
}

// editors are the programs captured as role "editor" with run = [name].
var editors = map[string]bool{
	"nvim": true, "vim": true, "vi": true, "hx": true, "helix": true,
	"emacs": true, "nano": true, "code": true,
}

// Session converts s into a project config and notes for the user. The
// project is undefaulted (decision E3: it is meant for workspace.Marshal); a
// defaulted copy is validated and its errors are returned, so an invalid
// project is never handed out.
func Session(s tmux.Session, opts Options) (workspace.Project, []string, error) {
	if s.Key != "" {
		return workspace.Project{}, nil, fmt.Errorf("session %q is already managed by cairn (project %s); nothing to capture", s.Name, s.Key)
	}
	if len(s.Windows) == 0 {
		return workspace.Project{}, nil, fmt.Errorf("session %q has no windows", s.Name)
	}
	var notes []string
	p := workspace.Project{ID: opts.ID, Name: s.Name}
	if p.ID == "" {
		if p.ID = workspace.Slug(s.Name); p.ID == "" {
			return p, nil, fmt.Errorf("cannot derive a project id from session name %q; pass one with --id", s.Name)
		}
	}
	if p.ID != s.Name {
		if validSession(s.Name) {
			p.Session = s.Name
		} else {
			notes = append(notes, fmt.Sprintf("tmux session name %q is not a valid cairn session name; the managed session will be named %q", s.Name, p.ID))
		}
	}

	switch {
	case opts.Root == "":
		p.Root = defaultRoot(s)
	case !filepath.IsAbs(opts.Root):
		return p, nil, fmt.Errorf("root %q must be an absolute path", opts.Root)
	default:
		p.Root = filepath.Clean(opts.Root)
	}

	keys := map[string]bool{}
	for wi, w := range s.Windows {
		key := uniqueKey(windowKey(w.Name, wi+1), keys)
		win, wnotes := captureWindow(w, key, p.Root, opts.IsIdle)
		p.Windows = append(p.Windows, win)
		notes = append(notes, wnotes...)
	}

	if errs := validate(p); len(errs) > 0 {
		return workspace.Project{}, nil, fmt.Errorf("captured project is invalid: %s", strings.Join(errs, "; "))
	}
	return p, notes, nil
}

// validSession reports whether name passes the project session rules.
func validSession(name string) bool {
	for _, fe := range (workspace.Project{Session: name}).Validate() {
		if fe.Key == "session" {
			return false
		}
	}
	return true
}

// validate defaults a deep copy of p and returns its validation errors.
func validate(p workspace.Project) []string {
	c := p
	c.Windows = make([]workspace.Window, len(p.Windows))
	for i, w := range p.Windows {
		w.Panes = append([]workspace.Pane(nil), w.Panes...)
		c.Windows[i] = w
	}
	c.ApplyDefaults("")
	var out []string
	for _, fe := range c.Validate() {
		out = append(out, fe.Error())
	}
	return out
}

// windowKey is the slug of name, or "window-<pos>" when nothing is left.
func windowKey(name string, pos int) string {
	if k := workspace.Slug(name); k != "" {
		return k
	}
	return fmt.Sprintf("window-%d", pos)
}

// uniqueKey returns key, or key-2, key-3, ... (kept within 63 characters),
// whichever is not in used yet, and records it.
func uniqueKey(key string, used map[string]bool) string {
	cand := key
	for n := 2; used[cand]; n++ {
		suffix := fmt.Sprintf("-%d", n)
		base := key
		if len(base)+len(suffix) > 63 {
			base = strings.TrimRight(base[:63-len(suffix)], "-")
		}
		cand = base + suffix
	}
	used[cand] = true
	return cand
}

// captureWindow builds the config window. The split sequence comes from the
// pane rects (layout.FromRects + layout.Sequence); src maps each config pane
// to the tmux pane (in pane_index order) it was read from.
func captureWindow(w tmux.Window, key, root string, isIdle func(int) bool) (workspace.Window, []string) {
	panes := append([]tmux.Pane(nil), w.Panes...)
	sort.Slice(panes, func(i, j int) bool { return panes[i].Index < panes[j].Index })
	win := workspace.Window{Key: key, Name: w.Name}
	var notes []string

	// src[i] is the index into panes of config pane i+1.
	seq, src, err := sequence(panes, w.W, w.H)
	if err != nil {
		if overlapping(panes) {
			notes = append(notes, fmt.Sprintf("window %s: looks zoomed (a pane covers the window) — unzoom it (prefix+z) and capture again; panes recorded side by side", key))
		} else {
			notes = append(notes, fmt.Sprintf("window %s (%q): the layout cannot be rebuilt (%v); its panes are listed side by side, adjust split/size by hand", key, w.Name, err))
		}
		seq = make([]workspace.Pane, len(panes))
		src = make([]int, len(panes))
		for i := range panes {
			src[i] = i
			if i > 0 {
				seq[i] = workspace.Pane{Split: workspace.SplitH, From: i, Size: workspace.DefaultSize}
			}
		}
	}

	for i, pn := range seq {
		tp := panes[src[i]]
		pn.Cwd = relCwd(tp.Cwd, root)
		var note string
		pn.Role, pn.Run, note = role(tp, isIdle)
		if note != "" {
			notes = append(notes, fmt.Sprintf("window %s pane %d: %s", key, i+1, note))
		}
		win.Panes = append(win.Panes, pn)
	}
	return win, notes
}

// overlapping reports whether two pane rects share a cell, as when a zoomed
// pane is reported at full window size over the others.
func overlapping(panes []tmux.Pane) bool {
	for i, a := range panes {
		for _, b := range panes[i+1:] {
			if a.Left < b.Left+b.W && b.Left < a.Left+a.W && a.Top < b.Top+b.H && b.Top < a.Top+a.H {
				return true
			}
		}
	}
	return false
}

func fromRects(panes []tmux.Pane, w, h int) (*layout.Node, error) {
	rects := make([]layout.Rect, len(panes))
	for i, p := range panes {
		rects[i] = layout.Rect{Pane: i + 1, X: p.Left, Y: p.Top, W: p.W, H: p.H}
	}
	return layout.FromRects(rects, w, h)
}

// sequence returns the split sequence and the config-to-tmux mapping, or the
// FromRects error when the rects do not form a clean split tree.
func sequence(panes []tmux.Pane, w, h int) ([]workspace.Pane, []int, error) {
	tree, err := fromRects(panes, w, h)
	if err != nil {
		return nil, nil, err
	}
	leaves := layout.SequenceLeaves(tree)
	src := make([]int, len(leaves))
	for i, l := range leaves {
		src[i] = l - 1 // leaves are traversal positions, 1-based
	}
	return layout.Sequence(tree), src, nil
}

// role classifies a pane. Only editors get a run command: the argv of any
// other program is never guessed.
func role(p tmux.Pane, isIdle func(int) bool) (string, []string, string) {
	if p.Dead {
		return workspace.RoleShell, nil, "the pane is dead; captured as a shell"
	}
	cmd := filepath.Base(p.Cmd)
	switch {
	case tmux.IsShell(cmd):
		// A busy shell is still a shell: a command pane without run would
		// mean nothing to apply.
		if isIdle == nil || isIdle(p.PID) {
			return workspace.RoleShell, nil, ""
		}
		return workspace.RoleShell, nil, "shell was busy when captured; recorded as a plain shell"
	case editors[cmd]:
		return workspace.RoleEditor, []string{cmd}, ""
	}
	return workspace.RoleCommand, nil, fmt.Sprintf("running %q — add run = [...] to restart it automatically", p.Cmd)
}

// relCwd is cwd relative to root when it lies under root ("" for root
// itself), else the cleaned absolute cwd.
func relCwd(cwd, root string) string {
	if cwd == "" {
		return ""
	}
	cwd = filepath.Clean(cwd)
	if rel, ok := under(cwd, root); ok {
		if rel == "." {
			return ""
		}
		return rel
	}
	return cwd
}

// under reports whether path is root or inside it, with the relative path.
func under(path, root string) (string, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return rel, true
}

// defaultRoot is the deepest directory that holds the cwd of a strict
// majority of the panes. When that is "/" or a top-level directory (the
// panes disagree widely), it is the cwd of the first pane that has one.
func defaultRoot(s tmux.Session) string {
	var cwds []string
	for _, w := range s.Windows {
		panes := append([]tmux.Pane(nil), w.Panes...)
		sort.Slice(panes, func(i, j int) bool { return panes[i].Index < panes[j].Index })
		for _, p := range panes {
			if filepath.IsAbs(p.Cwd) {
				cwds = append(cwds, filepath.Clean(p.Cwd))
			}
		}
	}
	if len(cwds) == 0 {
		return "/"
	}
	count := map[string]int{}
	for _, c := range cwds {
		for d := c; ; d = filepath.Dir(d) {
			count[d]++
			if d == "/" {
				break
			}
		}
	}
	best := ""
	for d, n := range count {
		if 2*n <= len(cwds) {
			continue
		}
		if best == "" || depth(d) > depth(best) {
			best = d
		}
	}
	if depth(best) < 2 {
		return cwds[0]
	}
	return best
}

// depth is the number of path components of an absolute clean path.
func depth(p string) int {
	if p == "/" || p == "" {
		return 0
	}
	return strings.Count(p, "/")
}

// ErrNoSession is returned by Find when no session has the exact name.
var ErrNoSession = errors.New("no such tmux session")

// Find returns the session named exactly name (never a prefix match).
func Find(snap tmux.Snapshot, name string) (tmux.Session, error) {
	for _, s := range snap.Sessions {
		if s.Name == name {
			return s, nil
		}
	}
	return tmux.Session{}, ErrNoSession
}

// Unmanaged returns the sessions without a cairn marker.
func Unmanaged(snap tmux.Snapshot) []tmux.Session {
	var out []tmux.Session
	for _, s := range snap.Sessions {
		if s.Key == "" {
			out = append(out, s)
		}
	}
	return out
}
