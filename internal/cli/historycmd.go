package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mrqwer/cairn/internal/history"
	"github.com/mrqwer/cairn/internal/xdg"
)

func openHistory() (history.Store, error) {
	dirs, err := xdg.Resolve(os.Getenv)
	if err != nil {
		return history.Store{}, err
	}
	return historyStore(dirs), nil
}

func newHistoryCmd() *cobra.Command {
	var n int
	cmd := &cobra.Command{
		Use:   "history",
		Short: "List recorded config changes (newest first)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := openHistory()
			if err != nil {
				return err
			}
			all, err := s.List(0)
			if err != nil {
				return err
			}
			undone := history.UndoneIDs(all)
			out := cmd.OutOrStdout()
			st, styled := styleFor(out)
			if len(all) == 0 {
				_, _ = fmt.Fprintln(out, "no history yet")
				return nil
			}
			for i, e := range all {
				if n > 0 && i >= n {
					break
				}
				mark := ""
				if undone[e.ID] {
					mark = "  (undone)"
				}
				ts := e.TS.Local().Format("2006-01-02 15:04")
				if styled {
					_, _ = fmt.Fprintf(out, "%s  %s  %-16s %s%s\n", st.Bold(e.ID), st.Dim(ts), e.Kind, e.Summary, st.Dim(mark))
					continue
				}
				_, _ = fmt.Fprintf(out, "%s  %s  %-16s %s%s\n", e.ID, ts, e.Kind, e.Summary, mark)
			}
			return nil
		},
	}
	cmd.Flags().IntVarP(&n, "limit", "n", 20, "number of events to show (0 = all)")
	cmd.AddCommand(&cobra.Command{
		Use:   "show <id>",
		Short: "Show the file changes of an event",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openHistory()
			if err != nil {
				return err
			}
			e, err := s.Get(args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "%s  %s  %s  %s\n", e.ID, e.TS.Local().Format("2006-01-02 15:04:05"), e.Kind, e.Summary)
			for _, f := range e.Files {
				_, _ = fmt.Fprint(out, history.Diff(f))
			}
			return nil
		},
	}, &cobra.Command{
		Use:   "restore <id>",
		Short: "Put the files of an event back to the state it produced",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openHistory()
			if err != nil {
				return err
			}
			e, err := s.Restore(args[0])
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "restored (event %s): %s\nRun: cairn apply   (config only; tmux was not changed)\n", e.ID, e.Summary)
			return nil
		},
	})
	return cmd
}

func newUndoCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "undo [<id>]",
		Short: "Undo a config change (the latest one by default)",
		Long: "Undo a config change (the latest one by default).\n\n" +
			"Undoing a task new also removes its worktree (only when nothing in it is work) and its branch\n" +
			"(only without new commits). --force lets ignored files in the worktree go as well (they are\n" +
			"listed); with an already undone task new it retries that removal.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openHistory()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			id := optionalArg(args)
			if id == "" {
				all, err := s.List(0)
				if err != nil {
					return err
				}
				undone := history.UndoneIDs(all)
				for _, e := range all {
					if e.Kind != history.KindUndo && e.Kind != history.KindRestore && !undone[e.ID] {
						id = e.ID
						break
					}
				}
				if id == "" {
					_, _ = fmt.Fprintln(out, "nothing to undo")
					return nil
				}
			}
			undoing, _ := s.Get(id)
			e, err := s.Undo(id)
			switch {
			case errors.Is(err, history.ErrAlreadyApplied):
				if force && undoing.Kind == "task.new" {
					afterTaskUndo(cmd, undoing, true)
					return nil
				}
				_, _ = fmt.Fprintf(out, "event %s is already undone; nothing to do\n", id)
				return nil
			case errors.Is(err, history.ErrConflict):
				w := cmd.ErrOrStderr()
				_, _ = fmt.Fprintf(w, "cannot undo event %s: %v\n", id, err)
				_, _ = fmt.Fprintf(w, "The files changed after that event; nothing was written. See: cairn history show %s\n", id)
				_, _ = fmt.Fprintln(w, "To put back the state a recorded event produced, use: cairn history restore <id>")
				return &ExitError{Code: 1}
			case err != nil:
				return err
			}
			_, _ = fmt.Fprintf(out, "%s\nRun: cairn apply   (config only; tmux was not changed)\n", e.Summary)
			if undoing.Kind == "task.new" {
				afterTaskUndo(cmd, undoing, force)
			}
			if undoing.Kind == "adopt" && len(undoing.Files) == 1 {
				project := strings.TrimSuffix(filepath.Base(undoing.Files[0].Path), ".toml")
				_, _ = fmt.Fprintf(out, "markers on the live session remain; run: cairn adopt --release %s\n", project)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "for a task new: also remove ignored files in its worktree (listed)")
	return cmd
}
