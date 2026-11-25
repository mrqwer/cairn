package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	"github.com/spf13/cobra"

	agentsdoctor "github.com/mrqwer/cairn/internal/agents/doctor"
	"github.com/mrqwer/cairn/internal/doctor"
	"github.com/mrqwer/cairn/internal/ui"
)

// agentLookPath finds a tool's binary for `agents doctor` (a seam for tests).
var agentLookPath = exec.LookPath

// probeAgents probes the adapters (only tool, when set) against the
// installed tools. Every probe command runs in an empty temp dir, which is
// removed. An adapter file that does not load is a fail finding (only
// without a tool filter).
func probeAgents(ctx context.Context, tool string) ([]agentsdoctor.Finding, error) {
	e, err := loadAgents()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(e.adapters))
	for n := range e.adapters {
		names = append(names, n)
	}
	sort.Strings(names)
	if tool != "" {
		if _, ok := e.adapters[tool]; !ok {
			return nil, fmt.Errorf("unknown tool %q (adapters: %v)", tool, names)
		}
		names = []string{tool}
	}
	var out []agentsdoctor.Finding
	if tool == "" {
		for _, le := range e.lerrs {
			out = append(out, agentsdoctor.Finding{Tool: filepath.Base(le.File), Check: "load",
				Level: agentsdoctor.LevelFail, Detail: le.Err.Error()})
		}
	}
	tmp, err := os.MkdirTemp("", "cairn-agents-doctor-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	// Own process group (killed whole on timeout), minimal environment.
	run := agentsdoctor.Runner{Dir: tmp, Env: agentsdoctor.MinimalEnv(os.Getenv, tmp)}
	for _, n := range names {
		out = append(out, agentsdoctor.Probe(ctx, e.adapters[n], run, agentLookPath)...)
		out = append(out, e.sandboxFinding(e.adapters[n])...)
	}
	if tool == "" {
		out = append(out, e.profileFindings()...)
	}
	return out, nil
}

func newAgentsDoctorCmd() *cobra.Command {
	var tool string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Probe the installed tools against their adapters (non-destructive)",
		Long: "Checks each tool without starting a conversation: the binary, its version,\n" +
			"that the flags and subcommands the adapter uses are in its --help, that the\n" +
			"list output parses and that the hook file is valid. Exit code 1 only on a fail.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fs, err := probeAgents(ctxOf(cmd), tool)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if asJSON {
				if fs == nil {
					fs = []agentsdoctor.Finding{}
				}
				if err := writeJSON(w, fs); err != nil {
					return err
				}
			} else {
				st, styled := styleFor(w)
				for _, f := range fs {
					lvl := ui.Pad(f.Level, 4)
					if styled {
						switch f.Level {
						case agentsdoctor.LevelOK:
							lvl = st.Green(lvl)
						case agentsdoctor.LevelWarn:
							lvl = st.Yellow(lvl)
						case agentsdoctor.LevelFail:
							lvl = st.Red(lvl)
						default:
							lvl = st.Dim(lvl)
						}
					}
					_, _ = fmt.Fprintf(w, "%s  %s %s %s\n", lvl, ui.Pad(f.Tool, 8), ui.Pad(f.Check, 15), f.Detail)
				}
			}
			for _, f := range fs {
				if f.Level == agentsdoctor.LevelFail {
					return &ExitError{Code: 1}
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&tool, "tool", "", "probe only this tool")
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	return cmd
}

// agentsChecks are the agents section of `cairn doctor`: the probe
// findings as results (fail is critical, skip is skip).
var agentsChecks = func(ctx context.Context) ([]doctor.Check, error) {
	fs, err := probeAgents(ctx, "")
	if err != nil {
		return nil, err
	}
	checks := make([]doctor.Check, 0, len(fs))
	for _, f := range fs {
		r := doctor.Result{Status: doctor.OK, Evidence: f.Detail}
		switch f.Level {
		case agentsdoctor.LevelWarn:
			r.Status, r.Fix = doctor.Warn, "check the adapter against this tool version: cairn agents doctor --tool "+f.Tool
			switch {
			case f.Tool == "profiles" && f.Check == "risk":
				r.Fix = "remove i_understand_the_risk from the project file unless you mean it (cairn project edit <id>)"
			case f.Check == "sandbox":
				r.Fix = "set a sandboxed mode in the tool's own config, or launch with a profile that sets sandbox"
			case f.Tool == "profiles":
				r.Fix = "fix profiles.toml in the config dir (see: cairn agents profile ls)"
			}
		case agentsdoctor.LevelFail:
			r.Status, r.Fix = doctor.Crit, "cairn agents doctor --tool "+f.Tool
			if f.Check == "load" {
				r.Fix = "fix the adapter file " + f.Tool + " in the tools config dir"
			}
		case agentsdoctor.LevelSkip:
			r.Status = doctor.Skip
		}
		id, title := "agents."+f.Tool+"."+f.Check, "Agent tool "+f.Tool+" "+f.Check
		checks = append(checks, check{id, title, func(context.Context) doctor.Result { return r }})
	}
	return checks, nil
}
