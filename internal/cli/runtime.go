package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mrqwer/cairn/internal/reconcile"
	"github.com/mrqwer/cairn/internal/tmux"
	"github.com/mrqwer/cairn/internal/ui"
	"github.com/mrqwer/cairn/internal/workspace"
)

// wsRun is a loaded workspace with a tmux driver and a snapshot.
type wsRun struct {
	ws     *wsState
	drv    wsDriver
	socket string
	snap   tmux.Snapshot
}

func prepare(cmd *cobra.Command) (*wsRun, error) {
	ws, err := loadWorkspace()
	if err != nil {
		return nil, err
	}
	socket := socketName(cmd)
	drv, err := newDriver(socket)
	if err != nil {
		return nil, err
	}
	snap, err := drv.Snapshot(ctxOf(cmd))
	if err != nil {
		return nil, err
	}
	return &wsRun{ws: ws, drv: drv, socket: socket, snap: snap}, nil
}

// plan runs the reconciler; PaneIdle is wired so --prune can work.
func (r *wsRun) plan(only string, prune bool) []reconcile.Op {
	return reconcile.Plan(r.ws.projects, r.snap, reconcile.Options{Prune: prune, Only: only, PaneIdle: r.drv.PaneIdle})
}

// checkOnly validates a <project> argument. A project that exists but is
// not active gives a note (and nothing to plan); a missing one an error.
func (r *wsRun) checkOnly(only string) (note string, err error) {
	if only == "" {
		return "", nil
	}
	p, err := r.ws.requireProject(only)
	if err != nil {
		return "", err
	}
	if p.Status != workspace.StatusActive {
		return fmt.Sprintf("project %s is %s", only, p.Status), nil
	}
	return "", nil
}

func nonDestructive(ops []reconcile.Op) []reconcile.Op {
	out := make([]reconcile.Op, 0, len(ops))
	for _, op := range ops {
		if !op.Destructive {
			out = append(out, op)
		}
	}
	return out
}

func actionable(ops []reconcile.Op) bool {
	for _, op := range ops {
		if !op.IsNote() {
			return true
		}
	}
	return false
}

func opSymbol(st ui.Style, styled bool, op reconcile.Op) string {
	sym, paint := "!", st.Yellow
	switch {
	case op.Destructive || strings.HasPrefix(op.Kind, "kill-"):
		sym, paint = "-", st.Red
	case strings.HasPrefix(op.Kind, "create-"):
		sym, paint = "+", st.Green
	case op.Kind == reconcile.KindRespawnPane:
		sym, paint = "↻", st.Cyan
	case op.Kind == reconcile.KindSetLayout || op.Kind == reconcile.KindRekeyPanes:
		sym, paint = "~", st.Cyan
	case strings.HasPrefix(op.Kind, "skip-"):
		sym, paint = "⊘", st.Dim
	}
	if styled {
		return paint(sym)
	}
	return sym
}

func opText(op reconcile.Op) string {
	if op.Kind == reconcile.KindSetLayout {
		return "window " + op.WindowKey + ": set layout"
	}
	return op.Detail
}

// renderPlan prints ops grouped per project; "nothing to do" when nothing
// would change (notes are still shown).
func renderPlan(w io.Writer, ops []reconcile.Op) {
	st, styled := styleFor(w)
	cur := ""
	for _, op := range ops {
		if op.Project != cur {
			cur = op.Project
			if styled {
				_, _ = fmt.Fprintf(w, "\n %s\n", st.Bold(cur))
			} else {
				_, _ = fmt.Fprintf(w, "%s:\n", cur)
			}
		}
		kind := ui.Pad(op.Kind, 18)
		if styled {
			kind = st.Dim(kind)
		}
		_, _ = fmt.Fprintf(w, "   %s %s %s\n", opSymbol(st, styled, op), kind, opText(op))
	}
	if !actionable(ops) {
		if styled {
			_, _ = fmt.Fprintf(w, "\n %s\n", st.Green("nothing to do"))
		} else {
			_, _ = fmt.Fprintln(w, "nothing to do")
		}
	}
}

// notRun reports an op that never started because the command was
// interrupted.
func notRun(r reconcile.Result) bool {
	return r.Err == context.Canceled
}

// interruptedLine is the one line printed instead of a failure per op that
// did not run after Ctrl-C.
func interruptedLine(w io.Writer, n int) {
	_, _ = fmt.Fprintf(w, "interrupted: %s not run\n", ui.Plural(n, "operation", "operations"))
}

// renderResults prints the outcome of each op that ran (not notes). Ops that
// did not run because of an interrupt are summed up in one line. It returns
// nil, an ExitError with code 1 when an op failed, or code 130 when
// interrupted.
func renderResults(w io.Writer, results []reconcile.Result) error {
	st, styled := styleFor(w)
	failed, skippedN := false, 0
	for _, r := range results {
		if r.Note != "" {
			mark := "!"
			if styled {
				mark = st.Yellow("!")
			}
			_, _ = fmt.Fprintf(w, " %s %s: %s\n", mark, r.Op.Project, r.Note)
			continue
		}
		if notRun(r) {
			skippedN++
			continue
		}
		if r.Op.IsNote() {
			continue
		}
		mark, text := "ok  ", r.Op.Project+": "+opText(r.Op)
		if styled {
			mark = st.Green("✓")
		}
		if r.Err != nil {
			failed = true
			mark = "FAIL"
			if styled {
				mark = st.Red("✗")
			}
			text += ": " + r.Err.Error()
		}
		_, _ = fmt.Fprintf(w, " %s %s\n", mark, text)
	}
	if skippedN > 0 {
		interruptedLine(w, skippedN)
		return &ExitError{Code: exitInterrupted}
	}
	if failed {
		return &ExitError{Code: 1}
	}
	return nil
}

// JSON output. In --json mode a command writes exactly one JSON document to
// stdout; every array is present ([] when empty). Field names are stable:
//
//	plan --json:   {"ops": [op], "load_errors": [load_error], "notes": [string]}
//	  op:          {"project", "kind", "target", "window", "pane", "detail",
//	                "destructive": bool, "note": bool}
//	  load_error:  {"file", "error"} — a project file that could not be loaded
//	  notes:       e.g. "project demo is archived" (nothing planned for it)
//	status --json: {"projects": [row], "load_errors": [load_error]}
//	  row:         {"id", "session", "running": bool, "windows",
//	                "missing_windows", "missing_panes", "dead_panes", "extra",
//	                "problem": "" or why the project is skipped}

// planJSON is the output of plan --json.
type planJSON struct {
	Ops        []opJSON        `json:"ops"`
	LoadErrors []loadErrorJSON `json:"load_errors"`
	Notes      []string        `json:"notes"`
}

type opJSON struct {
	Project     string `json:"project"`
	Kind        string `json:"kind"`
	Target      string `json:"target"`
	Window      string `json:"window"`
	Pane        string `json:"pane"`
	Detail      string `json:"detail"`
	Destructive bool   `json:"destructive"`
	Note        bool   `json:"note"`
}

type loadErrorJSON struct {
	File  string `json:"file"`
	Error string `json:"error"`
}

func loadErrorsJSON(ws *wsState) []loadErrorJSON {
	out := []loadErrorJSON{}
	for _, le := range ws.loadErrs {
		out = append(out, loadErrorJSON{File: le.File, Error: le.Err.Error()})
	}
	return out
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func optionalArg(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return ""
}

func newWsPlanCmd() *cobra.Command {
	var prune, asJSON bool
	cmd := &cobra.Command{
		Use:   "plan [<project>]",
		Short: "Show what cairn apply would change in tmux",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := prepare(cmd)
			if err != nil {
				return err
			}
			only := optionalArg(args)
			note, err := r.checkOnly(only)
			if err != nil {
				return err
			}
			var ops []reconcile.Op
			if note == "" {
				ops = r.plan(only, prune)
			}
			if asJSON {
				pj := planJSON{Ops: []opJSON{}, LoadErrors: loadErrorsJSON(r.ws), Notes: []string{}}
				if note != "" {
					pj.Notes = append(pj.Notes, note)
				}
				for _, op := range ops {
					pj.Ops = append(pj.Ops, opJSON{Project: op.Project, Kind: op.Kind, Target: op.Target,
						Window: op.WindowKey, Pane: op.PaneKey, Detail: op.Detail, Destructive: op.Destructive, Note: op.IsNote()})
				}
				return writeJSON(cmd.OutOrStdout(), pj)
			}
			printLoadErrors(cmd.ErrOrStderr(), r.ws.loadErrs)
			if note != "" {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s; nothing to do (resume it: cairn project resume %s)\n", note, only)
				return nil
			}
			renderPlan(cmd.OutOrStdout(), ops)
			return nil
		},
	}
	cmd.Flags().BoolVar(&prune, "prune", false, "also plan removal of managed windows and panes that are no longer configured (idle shells only)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	return cmd
}

func newWsApplyCmd() *cobra.Command {
	var prune, yes bool
	cmd := &cobra.Command{
		Use:   "apply [<project>]",
		Short: "Make tmux match the project configs",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			unlock, err := lockApply(cmd)
			if err != nil {
				return err
			}
			defer func() { unlock() }()
			r, err := prepare(cmd)
			if err != nil {
				return err
			}
			only := optionalArg(args)
			out := cmd.OutOrStdout()
			note, err := r.checkOnly(only)
			if err != nil {
				return err
			}
			if note != "" {
				_, _ = fmt.Fprintf(out, "%s; nothing to do (resume it: cairn project resume %s)\n", note, only)
				return nil
			}
			printLoadErrors(cmd.ErrOrStderr(), r.ws.loadErrs)
			ops := r.plan(only, prune)
			renderPlan(out, ops)
			if !actionable(ops) {
				return nil
			}
			destructive := false
			for _, op := range ops {
				destructive = destructive || op.Destructive
			}
			// Apply re-verifies every object before touching it (E13), so
			// the gap between plan and confirmation is safe.
			if destructive && !yes {
				if !interactive(cmd.InOrStdin()) {
					_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "destructive changes need confirmation: re-run with --yes")
					return &ExitError{Code: 1}
				}
				// Not held while waiting for the user: Apply re-verifies
				// everything, and a login "cairn up" must not wait on a prompt.
				unlock()
				ok, err := ask(ctxOf(cmd), cmd.InOrStdin(), out, "Apply?")
				if err != nil {
					return err
				}
				if !ok {
					_, _ = fmt.Fprintln(out, "nothing changed")
					return nil
				}
				// Re-take the lock; a failure must not clobber unlock with nil
				// (the deferred call would panic).
				relock, lerr := lockApply(cmd)
				if lerr != nil {
					unlock = func() {}
					return lerr
				}
				unlock = relock
			}
			_, _ = fmt.Fprintln(out)
			return renderResults(out, reconcile.Apply(ctxOf(cmd), r.drv, r.ws.projects, ops))
		},
	}
	cmd.Flags().BoolVar(&prune, "prune", false, "remove managed windows and panes that are no longer configured (idle shells only; asks first)")
	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask before destructive changes")
	return cmd
}

// isSkip reports a note that means the project was not applied at all.
func isSkip(op reconcile.Op) bool {
	switch op.Kind {
	case reconcile.KindSkipMissingRoot, reconcile.KindSkipNameClash, reconcile.KindSkipInvalid:
		return true
	}
	return false
}

func newWsUpCmd() *cobra.Command {
	var prune bool
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Start every active project (non-interactive, never removes anything)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			unlock, err := lockApply(cmd)
			if err != nil {
				return err
			}
			defer unlock()
			r, err := prepare(cmd)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if prune {
				_, _ = fmt.Fprintln(out, "note: up never removes anything; to prune, run: cairn apply --prune")
			}
			ops := nonDestructive(r.plan("", false))
			results := reconcile.Apply(ctxOf(cmd), r.drv, r.ws.projects, ops)
			byProject := map[string][]reconcile.Result{}
			for _, res := range results {
				byProject[res.Op.Project] = append(byProject[res.Op.Project], res)
			}
			var n, created, updated, unchanged, skipped, failed, interruptedN int
			var problems []string
			for _, p := range r.ws.projects {
				if p.Status != workspace.StatusActive {
					continue
				}
				n++
				var isCreated, isUpdated, isSkipped, isFailed bool
				for _, res := range byProject[p.ID] {
					switch {
					case notRun(res):
						interruptedN++
					case res.Note != "":
						problems = append(problems, fmt.Sprintf("! %s: %s", p.ID, res.Note))
					case res.Err != nil:
						isFailed = true
						problems = append(problems, fmt.Sprintf("✗ %s: %s: %v", p.ID, opText(res.Op), res.Err))
					case isSkip(res.Op):
						isSkipped = true
						problems = append(problems, fmt.Sprintf("⊘ %s: %s", p.ID, res.Op.Detail))
					case res.Op.IsNote():
						problems = append(problems, fmt.Sprintf("! %s: %s", p.ID, res.Op.Detail))
					case res.Op.Kind == reconcile.KindCreateSession:
						isCreated = true
					default:
						isUpdated = true
					}
				}
				switch {
				case isFailed:
					failed++
				case isSkipped:
					skipped++
				case isCreated:
					created++
				case isUpdated:
					updated++
				default:
					unchanged++
				}
			}
			parts := []string{ui.Plural(n, "project", "projects"), fmt.Sprintf("%d created", created)}
			if updated > 0 {
				parts = append(parts, fmt.Sprintf("%d updated", updated))
			}
			parts = append(parts, fmt.Sprintf("%d unchanged", unchanged))
			if skipped > 0 {
				parts = append(parts, fmt.Sprintf("%d skipped", skipped))
			}
			parts = append(parts, fmt.Sprintf("%d failed", failed))
			summary := strings.Join(parts, " · ")
			st, styled := styleFor(out)
			if styled {
				summary = st.Bold(summary)
			}
			_, _ = fmt.Fprintln(out, summary)
			for _, pr := range problems {
				_, _ = fmt.Fprintln(out, "  "+pr)
			}
			printLoadErrors(out, r.ws.loadErrs)
			if interruptedN > 0 {
				interruptedLine(out, interruptedN)
				return &ExitError{Code: exitInterrupted}
			}
			if failed > 0 {
				return &ExitError{Code: 1}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&prune, "prune", false, "ignored: up never removes anything")
	_ = cmd.Flags().MarkHidden("prune")
	return cmd
}

func newWsOpenCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "open <project>",
		Short: "Start a project if needed and switch to (or attach) its session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			unlock, err := lockApply(cmd)
			if err != nil {
				return err
			}
			defer unlock()
			ws, err := loadWorkspace()
			if err != nil {
				return err
			}
			p, err := ws.requireProject(id)
			if err != nil {
				return err
			}
			errOut := cmd.ErrOrStderr()
			if p.Status != workspace.StatusActive {
				_, _ = fmt.Fprintf(errOut, "project %s is %s; resume it first: cairn project resume %s\n", id, p.Status, id)
				return &ExitError{Code: 1}
			}
			socket := socketName(cmd)
			drv, err := newDriver(socket)
			if err != nil {
				return err
			}
			ctx := ctxOf(cmd)
			snap, err := drv.Snapshot(ctx)
			if err != nil {
				return err
			}
			r := &wsRun{ws: ws, drv: drv, socket: socket, snap: snap}
			ops := nonDestructive(r.plan(id, false))
			for _, op := range ops {
				if isSkip(op) {
					_, _ = fmt.Fprintf(errOut, "cannot open %s: %s\n", id, op.Detail)
					return &ExitError{Code: 1}
				}
				if op.IsNote() {
					_, _ = fmt.Fprintf(errOut, "note: %s\n", op.Detail)
				}
			}
			if actionable(ops) {
				if err := renderResults(cmd.OutOrStdout(), reconcile.Apply(ctx, drv, ws.projects, ops)); err != nil {
					return err
				}
			}
			sid, ok, err := drv.SessionIDByKey(ctx, id)
			if err != nil {
				return err
			}
			if !ok {
				_, _ = fmt.Fprintf(errOut, "the session of %s was not found after apply; see: cairn plan %s\n", id, id)
				return &ExitError{Code: 1}
			}
			return switchOrAttach(cmd, drv, socket, p.Session, sid)
		},
	}
}

// switchOrAttach brings the user to session sid. Inside tmux: never nest
// (Review Focus 5). Switch the invoking client, identified by its own
// TMUX/TMUX_PANE, to the session id — only when it is a client of the
// target server; a client of another server is never touched. Outside tmux
// the process is replaced by tmux attach.
func switchOrAttach(cmd *cobra.Command, drv wsDriver, socket, session, sid string) error {
	ctx := ctxOf(cmd)
	if here := tmuxClientSocket(); here != "" {
		target, err := drv.SocketPath(ctx)
		if err != nil {
			return err
		}
		if filepath.Clean(here) != filepath.Clean(target) {
			attach := "tmux "
			if socket != "" {
				attach += "-L " + socket + " "
			}
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "you are inside another tmux server (%s); run: %sattach -t =%s\n", here, attach, session)
			return &ExitError{Code: 1}
		}
		return drv.SwitchClientHere(ctx, sid, os.Getenv("TMUX"), os.Getenv("TMUX_PANE"))
	}
	return execProcess(tmux.AttachCommand(socket, sid))
}

// statusJSON is the output of status --json.
type statusJSON struct {
	Projects   []statusRow     `json:"projects"`
	LoadErrors []loadErrorJSON `json:"load_errors"`
}

// statusRow is one project in status --json.
type statusRow struct {
	ID             string `json:"id"`
	Session        string `json:"session"`
	Running        bool   `json:"running"`
	Windows        int    `json:"windows"`
	MissingWindows int    `json:"missing_windows"`
	MissingPanes   int    `json:"missing_panes"`
	DeadPanes      int    `json:"dead_panes"`
	Extra          int    `json:"extra"`
	Problem        string `json:"problem"`
}

func newWsStatusCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the live state of every active project",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := prepare(cmd)
			if err != nil {
				return err
			}
			ops := r.plan("", false)
			rows := []statusRow{}
			for _, p := range r.ws.projects {
				if p.Status != workspace.StatusActive {
					continue
				}
				row := statusRow{ID: p.ID, Session: p.Session, Windows: len(p.Windows)}
				for _, s := range r.snap.Sessions {
					row.Running = row.Running || s.Key == p.ID
				}
				for _, op := range ops {
					if op.Project != p.ID {
						continue
					}
					switch {
					case op.Kind == reconcile.KindCreateSession:
						row.MissingWindows = len(p.Windows)
					case op.Kind == reconcile.KindCreateWindow:
						row.MissingWindows++
					case op.Kind == reconcile.KindCreatePane:
						row.MissingPanes++
					case op.Kind == reconcile.KindRespawnPane || op.Kind == reconcile.KindReportDead:
						row.DeadPanes++
					case op.Kind == reconcile.KindReportExtra:
						row.Extra++
					case isSkip(op):
						row.Problem = op.Detail
					}
				}
				rows = append(rows, row)
			}
			out := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(out, statusJSON{Projects: rows, LoadErrors: loadErrorsJSON(r.ws)})
			}
			printLoadErrors(cmd.ErrOrStderr(), r.ws.loadErrs)
			if len(rows) == 0 {
				_, _ = fmt.Fprintln(out, "no active projects")
				return nil
			}
			st, styled := styleFor(out)
			_, _ = fmt.Fprintf(out, "%s %s %s %s %s %s %s\n", ui.Pad("ID", 16), ui.Pad("SESSION", 16), ui.Pad("STATE", 8),
				ui.Pad("WINDOWS", 8), ui.Pad("DEAD", 5), ui.Pad("EXTRA", 6), "PROBLEM")
			for _, row := range rows {
				state := "stopped"
				if row.Running {
					state = "running"
				}
				stateCol := ui.Pad(state, 8)
				if styled {
					if row.Running {
						stateCol = st.Green(stateCol)
					} else {
						stateCol = st.Dim(stateCol)
					}
				}
				windows := fmt.Sprintf("%d/%d", row.Windows-row.MissingWindows, row.Windows)
				problem := row.Problem
				if problem == "" && row.MissingPanes > 0 {
					problem = ui.Plural(row.MissingPanes, "pane missing", "panes missing")
				}
				_, _ = fmt.Fprintf(out, "%s %s %s %s %s %s %s\n", ui.Pad(row.ID, 16), ui.Pad(row.Session, 16), stateCol,
					ui.Pad(windows, 8), ui.Pad(fmt.Sprint(row.DeadPanes), 5), ui.Pad(fmt.Sprint(row.Extra), 6), problem)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	return cmd
}
