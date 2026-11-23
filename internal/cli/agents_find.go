package cli

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mrqwer/cairn/internal/agents/adapter"
	"github.com/mrqwer/cairn/internal/agents/conv"
	"github.com/mrqwer/cairn/internal/agents/index"
	"github.com/mrqwer/cairn/internal/ui"
)

// agentIndex is the transcript index in <State>/agents/index.
func (e *agentsEnv) agentIndex() index.Index {
	return index.Index{Dir: filepath.Join(e.dirs.State, "agents", "index"), Root: wsRoot, Home: e.dirs.Home}
}

func (e *agentsEnv) adapterList() []adapter.Adapter {
	out := make([]adapter.Adapter, 0, len(e.adapters))
	for _, a := range e.adapters {
		out = append(out, a)
	}
	return out
}

// printSkips lists what was not indexed or imported, with a count.
func printSkips(w io.Writer, skips []index.Skip) {
	if len(skips) == 0 {
		return
	}
	_, _ = fmt.Fprintf(w, "%d skipped:\n", len(skips))
	for _, s := range skips {
		_, _ = fmt.Fprintf(w, "  %s: %s\n", s.Path, s.Reason)
	}
}

func newAgentsImportCmd() *cobra.Command {
	var tool string
	var dryRun, asJSON bool
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Adopt existing conversations of the configured tools (records only, unnamed)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := loadAgents()
			if err != nil {
				return err
			}
			if tool != "" {
				if _, ok := e.adapter(tool); !ok {
					return fmt.Errorf("unknown tool %q", tool)
				}
			}
			opt := index.ImportOptions{Home: e.dirs.Home, Tool: tool, DryRun: dryRun}
			if ws, err := loadWorkspace(); err == nil {
				opt.ProjectOf = func(cwd string) string {
					if p, ok := projectOfDir(ws, cwd); ok {
						return p.ID
					}
					return ""
				}
			}
			rep, err := index.Import(cmd.Context(), wsRoot, e.adapterList(), e.store, opt)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(out, rep)
			}
			verb := "imported"
			if dryRun {
				verb = "would import"
			}
			_, _ = fmt.Fprintf(out, "%s %d conversations (%d already recorded)\n", verb, len(rep.Created), rep.Existing)
			for _, c := range rep.Created {
				project := c.Project
				if project == "" {
					project = "-"
				}
				_, _ = fmt.Fprintf(out, "  %s %s %s %s\n", ui.Pad(c.ID, 26), ui.Pad(c.Tool, 8), ui.Pad(project, 14), c.Cwd)
			}
			printSkips(out, rep.Skipped)
			if len(rep.Created) > 0 && !dryRun {
				_, _ = fmt.Fprintln(out, "name them with: cairn agents name <id>; open with: cairn agents resume <id>")
			}
			return nil
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&tool, "tool", "", "only this tool")
	fl.BoolVar(&dryRun, "dry-run", false, "show what would be imported, record nothing")
	fl.BoolVar(&asJSON, "json", false, "machine-readable output")
	return cmd
}

type findJSON struct {
	Hits     []index.Hit  `json:"hits"`
	Skipped  []index.Skip `json:"skipped"`
	Warnings []index.Skip `json:"warnings"`
}

func newAgentsFindCmd() *cobra.Command {
	var f conv.Filter
	var since string
	var limit int
	var asJSON, reindex bool
	cmd := &cobra.Command{
		Use:   "find <words...>",
		Short: "Search conversation transcripts (every word must match)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := strings.Join(args, " ")
			if strings.TrimSpace(q) == "" {
				return errors.New("empty query")
			}
			if since != "" {
				t, err := parseSince(since, time.Now())
				if err != nil {
					return err
				}
				f.Since = t
			}
			e, err := loadAgents()
			if err != nil {
				return err
			}
			ix := e.agentIndex()
			if reindex {
				if err := ix.Reset(); err != nil {
					return err
				}
			}
			all, err := e.store.List(conv.Filter{Archived: true})
			if err != nil {
				return err
			}
			rep, err := ix.Update(cmd.Context(), all, index.Parsers(), e.adapterList())
			if err != nil {
				return err
			}
			hits, err := ix.Search(q, f, limit)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				if hits == nil {
					hits = []index.Hit{}
				}
				warnings := rep.Warnings
				if warnings == nil {
					warnings = []index.Skip{}
				}
				return writeJSON(out, findJSON{Hits: hits, Skipped: rep.Skipped, Warnings: warnings})
			}
			st, styled := styleFor(out)
			if len(hits) == 0 {
				_, _ = fmt.Fprintln(out, "no matches")
			}
			for _, h := range hits {
				name := h.Name
				if name == "" {
					name = "(unnamed)"
				}
				project := h.Project
				if project == "" {
					project = "-"
				}
				_, _ = fmt.Fprintf(out, "%s %s %s %s %s\n", ui.Pad(h.ID, 26), ui.Pad(name, 28), ui.Pad(h.Tool, 8),
					ui.Pad(project, 14), h.At.Local().Format("2006-01-02 15:04"))
				snip := "    " + h.Snippet
				if styled {
					snip = st.Dim(snip)
				}
				_, _ = fmt.Fprintln(out, snip)
			}
			if len(hits) > 0 {
				_, _ = fmt.Fprintln(out, "open with: cairn agents resume <id>")
			}
			printSkips(out, rep.Skipped)
			return nil
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.Project, "project", "", "only this project")
	fl.StringVar(&f.Task, "task", "", "only this task")
	fl.StringVar(&f.Tool, "tool", "", "only this tool")
	fl.StringVar(&f.Tag, "tag", "", "only conversations with this tag")
	fl.BoolVar(&f.Archived, "archived", false, "include archived conversations")
	fl.StringVar(&since, "since", "", "only active since: 7d, 36h, 90m or a date 2006-01-02")
	fl.IntVar(&limit, "limit", 20, "at most this many results")
	fl.BoolVar(&reindex, "reindex", false, "read every transcript again")
	fl.BoolVar(&asJSON, "json", false, "machine-readable output")
	return cmd
}

// parseSince reads a --since value: a positive age in days (7d) or a Go
// duration (36h, 90m), counted back from now, or a local date.
func parseSince(s string, now time.Time) (time.Time, error) {
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return t, nil
	}
	if n, ok := strings.CutSuffix(s, "d"); ok {
		if d, err := strconv.Atoi(n); err == nil && d > 0 && n[0] != '+' {
			return now.Add(-time.Duration(d) * 24 * time.Hour), nil
		}
	} else if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return now.Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("--since %q: want an age like 7d, 36h, 90m or a date like 2006-01-02", s)
}
