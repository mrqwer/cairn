// Package wsdoctor holds the doctor checks of the workspace engine. It lives
// beside package workspace because it needs the reconciler, which imports
// workspace.
package wsdoctor

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/mrqwer/cairn/internal/doctor"
	"github.com/mrqwer/cairn/internal/reconcile"
	"github.com/mrqwer/cairn/internal/tmux"
	"github.com/mrqwer/cairn/internal/workspace"
)

// Check IDs.
const (
	IDTmux    = "workspace.tmux"
	IDConfig  = "workspace.config"
	IDRoots   = "workspace.roots"
	IDDrift   = "workspace.drift"
	IDService = "workspace.service"
)

type check struct {
	id, title string
	run       func() doctor.Result
}

func (c check) ID() string    { return c.id }
func (c check) Title() string { return c.title }
func (c check) Run(context.Context) doctor.Result {
	r := c.run()
	r.ID, r.Title = c.id, c.title
	return r
}

// Checks returns the workspace checks (tmux, config, roots, drift). All of
// them only read what they are given: snap is a snapshot, rootExists a
// filesystem probe. tmuxVersion is the version text of `tmux -V` ("" when
// tmux is missing).
func Checks(ps []workspace.Project, errs []workspace.LoadError, snap tmux.Snapshot, tmuxVersion string, rootExists func(string) bool) []doctor.Check {
	return []doctor.Check{
		check{IDTmux, "tmux available", func() doctor.Result { return tmuxResult(tmuxVersion) }},
		check{IDConfig, "Workspace config", func() doctor.Result { return configResult(ps, errs) }},
		check{IDRoots, "Project roots", func() doctor.Result { return rootsResult(ps, rootExists) }},
		check{IDDrift, "Workspace in sync", func() doctor.Result { return driftResult(ps, snap, tmuxVersion != "", rootExists) }},
	}
}

// Failed is a check that reports an error met while gathering its input.
func Failed(id, title string, st doctor.Status, err error) doctor.Check {
	return check{id, title, func() doctor.Result { return doctor.Result{Status: st, Evidence: err.Error()} }}
}

var versionRe = regexp.MustCompile(`(\d+)\.(\d+)`)

func tmuxResult(ver string) doctor.Result {
	ver = strings.TrimSpace(ver)
	if ver == "" {
		return doctor.Result{Status: doctor.Crit, Evidence: "tmux not found in PATH", Fix: "install tmux 3.2 or newer"}
	}
	m := versionRe.FindStringSubmatch(ver)
	if m == nil {
		return doctor.Result{Status: doctor.Warn, Evidence: fmt.Sprintf("cannot read the tmux version from %q; 3.2 or newer is required", ver)}
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	if major < 3 || (major == 3 && minor < 2) {
		return doctor.Result{Status: doctor.Warn, Evidence: fmt.Sprintf("tmux %s is older than 3.2", ver), Fix: "upgrade tmux to 3.2 or newer"}
	}
	return doctor.Result{Status: doctor.OK, Evidence: "tmux " + ver}
}

func configResult(ps []workspace.Project, errs []workspace.LoadError) doctor.Result {
	var bad []string
	for _, e := range errs {
		bad = append(bad, e.Error())
	}
	for _, p := range ps {
		for _, fe := range p.Validate() {
			bad = append(bad, fmt.Sprintf("projects/%s.toml: %s", p.ID, fe))
		}
	}
	if len(bad) > 0 {
		return doctor.Result{Status: doctor.Crit, Evidence: strings.Join(bad, "; "), Fix: "fix the listed files, or cairn undo the last change"}
	}
	return doctor.Result{Status: doctor.OK, Evidence: fmt.Sprintf("%d projects loaded", len(ps))}
}

func rootsResult(ps []workspace.Project, rootExists func(string) bool) doctor.Result {
	var missing []string
	for _, p := range ps {
		if p.Status == workspace.StatusActive && p.Root != "" && !rootExists(p.Root) {
			missing = append(missing, fmt.Sprintf("%s: %s", p.ID, p.Root))
		}
	}
	if len(missing) > 0 {
		return doctor.Result{Status: doctor.Warn, Evidence: "missing root: " + strings.Join(missing, "; "),
			Fix: "create the directory, or cairn project edit <id>, or cairn project archive <id>"}
	}
	return doctor.Result{Status: doctor.OK, Evidence: "all roots exist"}
}

func driftResult(ps []workspace.Project, snap tmux.Snapshot, haveTmux bool, rootExists func(string) bool) doctor.Result {
	if !haveTmux {
		return doctor.Result{Status: doctor.Unknown, Evidence: "tmux is missing, nothing to compare"}
	}
	drifting, attention := map[string]bool{}, map[string]bool{}
	for _, op := range reconcile.Plan(ps, snap, reconcile.Options{RootExists: rootExists}) {
		switch {
		case !op.IsNote():
			drifting[op.Project] = true
		case op.Kind == reconcile.KindSkipNameClash || op.Kind == reconcile.KindSkipLayout:
			attention[op.Project] = true
		}
	}
	for id := range drifting {
		delete(attention, id)
	}
	var evidence, fix []string
	if n := len(drifting); n > 0 {
		evidence = append(evidence, fmt.Sprintf("%d %s `cairn apply`", n, plural(n, "project needs", "projects need")))
		fix = append(fix, "cairn plan, then cairn apply")
	}
	if n := len(attention); n > 0 {
		evidence = append(evidence, fmt.Sprintf("%d %s attention: cairn plan", n, plural(n, "project needs", "projects need")))
		if len(fix) == 0 {
			fix = append(fix, "cairn plan")
		}
	}
	if len(evidence) == 0 {
		return doctor.Result{Status: doctor.OK, Evidence: "tmux matches the config"}
	}
	return doctor.Result{Status: doctor.Warn, Evidence: strings.Join(evidence, "; "), Fix: strings.Join(fix, "")}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// ServiceCheck reports whether the optional tmux user service is installed
// (see workspace.QueryUnits). It is never worse than a warning.
func ServiceCheck(units []workspace.UnitStatus) doctor.Check {
	return check{IDService, "tmux user service", func() doctor.Result {
		installed := false
		for _, u := range units {
			installed = installed || u.File
		}
		if !installed {
			return doctor.Result{Status: doctor.Warn,
				Evidence: "optional: cairn tmux service install keeps tmux alive across logouts and restores on login",
				Fix:      "cairn tmux service install --socket <name>"}
		}
		var off []string
		for _, u := range units {
			if !u.File || !strings.HasPrefix(u.Enabled, "enabled") {
				off = append(off, u.Unit)
			}
		}
		if len(off) > 0 {
			return doctor.Result{Status: doctor.Warn, Evidence: "not installed or not enabled: " + strings.Join(off, ", "),
				Fix: "cairn tmux service install"}
		}
		return doctor.Result{Status: doctor.OK, Evidence: "units installed and enabled"}
	}}
}
