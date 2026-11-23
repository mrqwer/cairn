package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mrqwer/cairn/internal/agents/adapter"
	"github.com/mrqwer/cairn/internal/agents/conv"
	agentsdoctor "github.com/mrqwer/cairn/internal/agents/doctor"
	"github.com/mrqwer/cairn/internal/agents/profile"
	"github.com/mrqwer/cairn/internal/agents/task"
	"github.com/mrqwer/cairn/internal/execx"
	"github.com/mrqwer/cairn/internal/ui"
	"github.com/mrqwer/cairn/internal/workspace"
)

// Seams for tests: how ccusage is found and run.
var (
	usageLookPath              = exec.LookPath
	usageRunner   execx.Runner = stdoutRunner{}
)

// stdoutRunner runs a command without a shell and returns its standard
// output only (a tool's notes on stderr stay out of its JSON).
type stdoutRunner struct{}

func (stdoutRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	c := exec.CommandContext(ctx, name, args...)
	c.WaitDelay = 5 * time.Second
	return c.Output()
}

// loadProfiles reads <Config>/profiles.toml.
func (e *agentsEnv) loadProfiles() (map[string]profile.Profile, []profile.LoadError, error) {
	return profile.Load(wsRoot, e.dirs)
}

// launchSetup is the profile a launch uses and what it passes.
type launchSetup struct {
	name, src   string
	args        []string
	unsupported []string
	risky       []string
}

// launchProfile resolves the profile of a launch in project p (the
// conversation's own, else the task's, else the project's default),
// refuses an unknown one and risky flags the project has not acknowledged,
// and maps it to the tool's arguments.
func (e *agentsEnv) launchProfile(a adapter.Adapter, p workspace.Project, convProfile, taskProfile string) (launchSetup, error) {
	var ls launchSetup
	ls.name, ls.src = profile.Resolve(convProfile, taskProfile, p.Agents.DefaultProfile)
	var pr profile.Profile
	if ls.name != "" {
		ps, lerrs, err := e.loadProfiles()
		if err != nil {
			return ls, err
		}
		var ok bool
		if pr, ok = ps[ls.name]; !ok {
			for _, le := range lerrs {
				if le.Profile == ls.name || le.Profile == "" {
					return ls, fmt.Errorf("profile %s (from the %s) cannot be used: %v", ls.name, ls.src, le)
				}
			}
			names := make([]string, 0, len(ps))
			for n := range ps {
				names = append(names, n)
			}
			sort.Strings(names)
			return ls, fmt.Errorf("unknown profile %q (from the %s; defined: %s in %s)", ls.name, ls.src,
				orNone(strings.Join(names, ", ")), profile.Path(e.dirs))
		}
	}
	if err := profile.Authorize(pr, a, p); err != nil {
		return ls, err
	}
	ls.args, ls.unsupported = profile.Args(a, pr)
	ls.risky = profile.RiskyFlags(a, pr)
	return ls, nil
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// report prints what the profile of a new conversation does.
func (ls launchSetup) report(w io.Writer, tool, project string) {
	if ls.name != "" {
		_, _ = fmt.Fprintf(w, "profile %s (from the %s): %s\n", ls.name, ls.src, ui.SafeLine(orNone(strings.Join(ls.args, " "))))
	}
	if len(ls.unsupported) > 0 {
		_, _ = fmt.Fprintf(w, "note: %s cannot express %s; left out\n", tool, strings.Join(ls.unsupported, ", "))
	}
	if len(ls.risky) > 0 {
		_, _ = fmt.Fprintf(w, "! %s is launched with %s, which skips its permission checks or sandbox (allowed by i_understand_the_risk in project %s)\n",
			tool, strings.Join(ls.risky, ", "), project)
	}
}

// checkProfileFlag validates a --profile value before anything is created.
func checkProfileFlag(name string) error {
	if name != "" && !profile.ValidName(name) {
		return fmt.Errorf("--profile %q is not a profile name ([a-z0-9][a-z0-9_-]{0,62})", ui.SafeLine(name))
	}
	return nil
}

// paneProfileArgs is the pane wrapper's ProfileArgs: the launch resolves
// the profile again from the current project and task files, so a
// withdrawn acknowledgement stops a risky launch; the record's risk mark
// follows what is passed.
func (e *agentsEnv) paneProfileArgs(out io.Writer) func(conv.Conversation, adapter.Adapter) ([]string, error) {
	return func(c conv.Conversation, a adapter.Adapter) ([]string, error) {
		p := workspace.Project{ID: c.Project}
		if ws, err := loadWorkspace(); err == nil {
			if wp, ok := ws.project(c.Project); ok {
				p = wp
			}
		}
		taskProfile := ""
		if c.Task != "" {
			if ts, _, err := task.Load(wsRoot, e.dirs); err == nil {
				if tk, err := task.Find(ts, c.Task); err == nil {
					taskProfile = tk.Profile
				}
			}
		}
		ls, err := e.launchProfile(a, p, c.Profile, taskProfile)
		if err != nil {
			return nil, err
		}
		if len(ls.unsupported) > 0 {
			_, _ = fmt.Fprintf(out, "note: %s cannot express %s; left out\n", a.Name, strings.Join(ls.unsupported, ", "))
		}
		risk := len(ls.risky) > 0
		if risk {
			_, _ = fmt.Fprintf(out, "! launched with %s (allowed by i_understand_the_risk in project %s)\n", strings.Join(ls.risky, ", "), p.ID)
		}
		if risk != c.Risk {
			_, _ = e.store.Update(c.ID, func(r *conv.Conversation) error { r.Risk = risk; return nil })
		}
		return ls.args, nil
	}
}

// profileFindings are the profile part of agents doctor: problems in
// profiles.toml and each project that acknowledges risky flags.
func (e *agentsEnv) profileFindings() []agentsdoctor.Finding {
	var out []agentsdoctor.Finding
	if _, lerrs, err := e.loadProfiles(); err != nil {
		out = append(out, agentsdoctor.Finding{Tool: "profiles", Check: "load", Level: agentsdoctor.LevelWarn, Detail: err.Error()})
	} else {
		for _, le := range lerrs {
			out = append(out, agentsdoctor.Finding{Tool: "profiles", Check: "load", Level: agentsdoctor.LevelWarn, Detail: ui.SafeLine(le.Error())})
		}
	}
	if ws, err := loadWorkspace(); err == nil {
		for _, p := range ws.projects {
			if p.Agents.IUnderstandTheRisk {
				out = append(out, agentsdoctor.Finding{Tool: "profiles", Check: "risk", Level: agentsdoctor.LevelWarn,
					Detail: "! project " + p.ID + " allows flags that skip permission checks (i_understand_the_risk = true)"})
			}
		}
	}
	return out
}

func newAgentsProfileCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "profile",
		Short: "Agent profiles: list them and set a project's default",
	}
	cmd.AddCommand(newAgentsProfileLsCmd(), newAgentsProfileDefaultCmd())
	return cmd
}

type profileToolJSON struct {
	Tool        string   `json:"tool"`
	Args        []string `json:"args"`
	Unsupported []string `json:"unsupported"`
	Risky       []string `json:"risky"`
}

type profileJSON struct {
	Name     string            `json:"name"`
	Settings map[string]string `json:"settings"`
	Tools    []profileToolJSON `json:"tools"`
}

func newAgentsProfileLsCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List profiles and what each passes to each tool (! marks flags that skip permission checks)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := loadAgents()
			if err != nil {
				return err
			}
			ps, lerrs, err := e.loadProfiles()
			if err != nil {
				return err
			}
			names := make([]string, 0, len(ps))
			for n := range ps {
				names = append(names, n)
			}
			sort.Strings(names)
			tools := make([]string, 0, len(e.adapters))
			for n := range e.adapters {
				tools = append(tools, n)
			}
			sort.Strings(tools)
			rows := make([]profileJSON, 0, len(names))
			for _, n := range names {
				r := profileJSON{Name: n, Settings: ps[n].Settings}
				for _, t := range tools {
					a := e.adapters[t]
					args, uns := profile.Args(a, ps[n])
					r.Tools = append(r.Tools, profileToolJSON{Tool: t, Args: nonNil(args), Unsupported: nonNil(uns),
						Risky: nonNil(profile.RiskyFlags(a, ps[n]))})
				}
				rows = append(rows, r)
			}
			out := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(out, rows)
			}
			if len(rows) == 0 {
				_, _ = fmt.Fprintf(out, "no profiles (define them in %s)\n", profile.Path(e.dirs))
			}
			for _, r := range rows {
				var kv []string
				for _, k := range profile.Keys {
					if v, ok := r.Settings[k]; ok {
						kv = append(kv, k+"="+v)
					}
				}
				_, _ = fmt.Fprintf(out, "%s  %s\n", r.Name, ui.SafeLine(strings.Join(kv, " ")))
				for _, t := range r.Tools {
					badge := " "
					if len(t.Risky) > 0 {
						badge = "!"
					}
					line := fmt.Sprintf("  %s %s %s", badge, ui.Pad(ui.SafeLine(t.Tool), 8), ui.SafeLine(orNone(strings.Join(t.Args, " "))))
					if len(t.Unsupported) > 0 {
						line += "  (cannot express: " + strings.Join(t.Unsupported, ", ") + ")"
					}
					_, _ = fmt.Fprintln(out, line)
				}
			}
			if ws, err := loadWorkspace(); err == nil {
				for _, p := range ws.projects {
					if p.Agents.DefaultProfile == "" && !p.Agents.IUnderstandTheRisk {
						continue
					}
					line := "project " + p.ID
					if p.Agents.DefaultProfile != "" {
						line += ": default " + p.Agents.DefaultProfile
					}
					if p.Agents.IUnderstandTheRisk {
						line += "  ! i_understand_the_risk = true"
					}
					_, _ = fmt.Fprintln(out, line)
				}
			}
			for _, le := range lerrs {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "problem:", ui.SafeLine(le.Error()))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	return cmd
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func newAgentsProfileDefaultCmd() *cobra.Command {
	var project string
	var clear bool
	cmd := &cobra.Command{
		Use:   "default [<profile>] --project <id>",
		Short: "Set (or --clear) the profile a project's conversations use unless they name one (undoable)",
		Long: "Writes default_profile under [agents] in the project file, keeping its comments, as\n" +
			"one cairn history event (cairn undo reverts it). i_understand_the_risk is never set by\n" +
			"a command: edit the project file (cairn project edit <id>) if you mean it.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			if (name == "") == !clear {
				return errors.New("give a profile name or --clear")
			}
			e, err := loadAgents()
			if err != nil {
				return err
			}
			ws, err := loadWorkspace()
			if err != nil {
				return err
			}
			p, err := projectFor(ws, project)
			if err != nil {
				return err
			}
			if name != "" {
				if err := checkProfileFlag(name); err != nil {
					return err
				}
				ps, _, err := e.loadProfiles()
				if err != nil {
					return err
				}
				if _, ok := ps[name]; !ok {
					return fmt.Errorf("unknown profile %q (see: cairn agents profile ls)", name)
				}
			}
			unlock, err := lockApply(cmd)
			if err != nil {
				return err
			}
			defer unlock()
			f, warn, err := projectEdit(e.dirs, p.ID, func(raw *workspace.Project, before []byte) func(workspace.Project) ([]byte, bool) {
				raw.Agents.DefaultProfile = name
				return func(want workspace.Project) ([]byte, bool) {
					return accept(setDefaultProfileText(string(before), name), want)
				}
			})
			if err != nil {
				return err
			}
			if warn != "" {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), warn)
			}
			summary := "clear the default profile of " + p.ID
			if name != "" {
				summary = "default profile of " + p.ID + ": " + name
			}
			if err := commitEdits(e.dirs, "agents.profile", summary, []fileEdit{f}); err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), summary)
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project (default: the one containing the current directory)")
	cmd.Flags().BoolVar(&clear, "clear", false, "remove the project's default profile")
	return cmd
}

// setDefaultProfileText sets (or, with name "", removes) default_profile in
// the [agents] table of a project file's text, leaving every other line as
// it is; [agents] is added at the end when missing. name is a validated
// profile name, so it needs no escaping.
func setDefaultProfileText(text, name string) string {
	lines := strings.SplitAfter(text, "\n")
	header, set := -1, -1
	in := false
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "[") {
			rest, isAgents := strings.CutPrefix(t, "[agents]")
			in = isAgents && (rest == "" || strings.HasPrefix(strings.TrimSpace(rest), "#"))
			if in {
				header = i
			}
			continue
		}
		if in && set < 0 {
			if k, _, ok := strings.Cut(t, "="); ok && strings.TrimSpace(k) == "default_profile" {
				set = i
			}
		}
	}
	line := `default_profile = "` + name + `"` + "\n"
	switch {
	case set >= 0 && name == "":
		lines = append(lines[:set], lines[set+1:]...)
	case set >= 0:
		lines[set] = line
	case name == "":
	case header >= 0:
		lines = append(lines[:header+1], append([]string{line}, lines[header+1:]...)...)
	default:
		if text != "" && !strings.HasSuffix(text, "\n") {
			lines = append(lines, "\n")
		}
		lines = append(lines, "\n[agents]\n", line)
	}
	return strings.Join(lines, "")
}

// usageGroup is one row of agents usage.
type usageGroup struct {
	Key           string  `json:"key"`
	Conversations int     `json:"conversations"`
	WithData      int     `json:"with_data"`
	Tokens        int64   `json:"tokens"`
	CostUSD       float64 `json:"cost_usd"`
}

type usageJSON struct {
	By       string       `json:"by"`
	Sources  []string     `json:"sources"`
	Estimate bool         `json:"estimate"`
	Groups   []usageGroup `json:"groups"`
	Warnings []string     `json:"warnings"`
}

func newAgentsUsageCmd() *cobra.Command {
	var by string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "usage",
		Short: "Tokens and estimated cost per tool, project or task (from the tool's records or ccusage, if installed)",
		Long: "Sums usage over all conversations, archived ones included. Figures come from what a\n" +
			"tool recorded, else from ccusage (optional: run as `ccusage session --json --offline`,\n" +
			"without a shell, stopped after 30 s). Dollar figures are estimates, not a bill. Without\n" +
			"ccusage the usage is reported as unavailable; that is not an error.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			key := map[string]func(conv.Conversation) string{
				"tool":    func(c conv.Conversation) string { return c.Tool },
				"project": func(c conv.Conversation) string { return c.Project },
				"task":    func(c conv.Conversation) string { return c.Task },
			}[by]
			if key == nil {
				return fmt.Errorf("--by must be tool, project or task, not %q", ui.SafeLine(by))
			}
			e, err := loadAgents()
			if err != nil {
				return err
			}
			cs, err := e.store.List(conv.Filter{Archived: true})
			if err != nil {
				return err
			}
			us, werr := profile.CollectUsage(ctxOf(cmd), usageRunner, usageLookPath, cs)
			res := usageJSON{By: by, Estimate: true, Groups: []usageGroup{}, Sources: []string{}, Warnings: []string{}}
			if werr != nil {
				res.Warnings = append(res.Warnings, werr.Error())
			}
			groups := map[string]*usageGroup{}
			sources := map[string]bool{}
			for _, c := range cs {
				k := key(c)
				if k == "" {
					k = "(no " + by + ")"
				}
				g := groups[k]
				if g == nil {
					g = &usageGroup{Key: k}
					groups[k] = g
				}
				g.Conversations++
				u := us[c.ID]
				if u.Source == profile.SourceNative || u.Source == profile.SourceCcusage {
					g.WithData++
					g.Tokens += u.Tokens
					g.CostUSD += u.CostUSD
					sources[u.Source] = true
				}
			}
			for _, g := range groups {
				res.Groups = append(res.Groups, *g)
			}
			sort.Slice(res.Groups, func(i, j int) bool { return res.Groups[i].Key < res.Groups[j].Key })
			for s := range sources {
				res.Sources = append(res.Sources, s)
			}
			sort.Strings(res.Sources)
			out := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(out, res)
			}
			for _, w := range res.Warnings {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "warning:", ui.SafeLine(w))
			}
			if len(res.Sources) == 0 {
				_, _ = fmt.Fprintln(out, "usage: unavailable (no tool recorded any, and ccusage is not installed or knows none of these conversations; ccusage is optional)")
				return nil
			}
			st, styled := styleFor(out)
			head := fmt.Sprintf("%s %s %s %s", ui.Pad(strings.ToUpper(by), 28), ui.Pad("CONVERSATIONS", 14), ui.Pad("TOKENS", 12), "COST (estimate)")
			if styled {
				head = st.Dim(head)
			}
			_, _ = fmt.Fprintln(out, head)
			for _, g := range res.Groups {
				convs := fmt.Sprintf("%d/%d", g.WithData, g.Conversations)
				_, _ = fmt.Fprintf(out, "%s %s %s ~$%.2f\n", ui.Pad(ui.SafeLine(g.Key), 28), ui.Pad(convs, 14), ui.Pad(fmt.Sprint(g.Tokens), 12), g.CostUSD)
			}
			_, _ = fmt.Fprintf(out, "source: %s; conversations with data / all; costs are estimates, not a bill\n", strings.Join(res.Sources, ", "))
			return nil
		},
	}
	cmd.Flags().StringVar(&by, "by", "tool", "group by tool, project or task")
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	return cmd
}
