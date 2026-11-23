package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mrqwer/cairn/internal/agents/conv"
	"github.com/mrqwer/cairn/internal/agents/task"
)

// maxRecordBytes bounds a record text read from a file or a pipe.
const maxRecordBytes = 1 << 20

// recordInfo is what the draft record is made of.
type recordInfo struct {
	changed, commits, names []string
}

// collectRecordInfo reads the files and commits of the task's change from
// git while its branch exists. What cannot be read stays empty (the draft
// then says TODO): a missing detail never stops a finish.
func collectRecordInfo(g task.Git, t task.Task, cs []conv.Conversation) recordInfo {
	var in recordInfo
	r := t.Repos[0]
	base := r.BaseCommit
	if base == "" {
		base = r.Base
	}
	in.changed, _ = g.ChangedFiles(base, r.Branch)
	in.commits, _ = g.CommitIDs(base, r.Branch)
	for _, c := range cs {
		in.names = append(in.names, convTitle(c))
	}
	return in
}

// draftFile is the draft of t; the id is checked, it names a file.
func draftFile(e *agentsEnv, t task.Task) (string, error) {
	if !conv.ValidID(t.ID) {
		return "", fmt.Errorf("task id %q is not valid", t.ID)
	}
	return filepath.Join(e.dirs.State, "agents", "drafts", t.ID+".md"), nil
}

// saveDraft writes text as the draft of t: atomically, readable by the
// owner only. The drafts directory is private too.
func saveDraft(e *agentsEnv, t task.Task, text string) error {
	p, err := draftFile(e, t)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return wsRoot.WriteAtomic(p, []byte(text), 0o600)
}

// offerRecord runs after a successful finish by merge or pull request: it
// prints the draft, saves it for editing and says how to finish the record.
// The task is finished already, so nothing here may fail the command.
func offerRecord(cmd *cobra.Command, e *agentsEnv, t task.Task, in recordInfo) {
	out := cmd.OutOrStdout()
	draft := task.Draft(t, in.changed, in.commits, in.names)
	_, _ = fmt.Fprintf(out, "\ndraft record (nothing is saved to the knowledge notes yet):\n%s\n", indent(draft))
	p, perr := draftFile(e, t)
	if perr == nil {
		_, perr = os.Stat(p)
	}
	if perr == nil {
		_, _ = fmt.Fprintf(out, "a draft of this task already exists and was kept: %s\n", p)
	} else if err := saveDraft(e, t, draft); err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: the draft was not saved (%v); write the record yourself and run: cairn task record %s --text-file FILE\n", err, t.Slug)
		return
	} else {
		_, _ = fmt.Fprintf(out, "draft saved: %s\n", p)
	}
	_, _ = fmt.Fprintf(out, "fill the TODO lines, then: cairn task record %s --edit   (or --text-file FILE)\n", t.Slug)
}

func indent(s string) string {
	ls := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return "  " + strings.Join(ls, "\n  ")
}

// readLimited reads at most maxRecordBytes.
func readLimited(r io.Reader) (string, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxRecordBytes+1))
	if err != nil {
		return "", err
	}
	if len(b) > maxRecordBytes {
		return "", fmt.Errorf("the record text is longer than %d bytes", maxRecordBytes)
	}
	return string(b), nil
}

func newTaskRecordCmd() *cobra.Command {
	var edit bool
	var textFile string
	cmd := &cobra.Command{
		Use:   "record <id|slug> [--edit | --text-file FILE]",
		Short: "Check and save the finish record of a finished task",
		Long: "A finished task has a draft record (shown by task finish, kept in <State>/agents/drafts).\n" +
			"This checks the text against the quality gate (at most 120 words, at least 2 concrete\n" +
			"references such as files, commits or commands, no TODO and no filler phrases; extend the\n" +
			"filler list with tasks.gate_banned) and writes it to <knowledge.root>/<project>/<slug>.md\n" +
			"only when every check passes. --edit opens the draft in $VISUAL/$EDITOR first;\n" +
			"--text-file takes the text from FILE (- is standard input). Running it again redoes the\n" +
			"record; the write is recorded in history, so cairn undo restores the previous one.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if edit && cmd.Flags().Changed("text-file") {
				return errors.New("use either --edit or --text-file")
			}
			e, err := loadAgents()
			if err != nil {
				return err
			}
			st, err := loadTaskSettings(e.dirs)
			if err != nil {
				return err
			}
			// The tasks lock is taken only around reading the task and
			// writing the record, never while an editor is open.
			t, err := finishedTask(cmd, e, args[0])
			if err != nil {
				return err
			}
			var text string
			if cmd.Flags().Changed("text-file") {
				if text, err = readTextFile(cmd, textFile); err != nil {
					return err
				}
			} else if text, err = draftText(cmd, e, t, edit); err != nil {
				return err
			}
			unlock, err := lockTasks(ctxOf(cmd), e.dirs, taskLockWait)
			if err != nil {
				return err
			}
			defer unlock()
			// Whatever happened while the editor was open: check again.
			if t, err = findFinished(e, cmd, args[0]); err != nil {
				return err
			}
			return saveRecord(cmd, e, st, t, text)
		},
	}
	cmd.Flags().BoolVar(&edit, "edit", false, "edit the draft in $VISUAL/$EDITOR first")
	cmd.Flags().StringVar(&textFile, "text-file", "", "take the record text from this file (- is standard input)")
	return cmd
}

// finishedTask finds the task under the tasks lock and requires it done.
func finishedTask(cmd *cobra.Command, e *agentsEnv, ref string) (task.Task, error) {
	unlock, err := lockTasks(ctxOf(cmd), e.dirs, taskLockWait)
	if err != nil {
		return task.Task{}, err
	}
	defer unlock()
	return findFinished(e, cmd, ref)
}

// findFinished resolves ref and requires a done task (the lock is held).
func findFinished(e *agentsEnv, cmd *cobra.Command, ref string) (task.Task, error) {
	t, err := findTask(e, cmd.ErrOrStderr(), ref)
	if err != nil {
		return t, err
	}
	switch t.Status {
	case task.StatusDone:
		return t, nil
	case task.StatusAbandoned:
		return t, fmt.Errorf("task %s was abandoned; an abandoned task has no record", t.Slug)
	}
	return t, fmt.Errorf("task %s is not finished (%s); finish it with --merge or --pr first", t.Slug, t.Status)
}

func readTextFile(cmd *cobra.Command, name string) (string, error) {
	if name == "-" {
		return readLimited(cmd.InOrStdin())
	}
	f, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	return readLimited(f)
}

// draftText returns the saved draft of t, making it from git when it is
// missing and the branch still exists. With edit, the editor works on a
// copy: the draft is replaced (atomically) only by a finished edit, so a
// failed or interrupted editor never leaves it half written.
func draftText(cmd *cobra.Command, e *agentsEnv, t task.Task, edit bool) (string, error) {
	p, err := draftFile(e, t)
	if err != nil {
		return "", err
	}
	b, err := wsRoot.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		r := t.Repos[0]
		g, cancel := finishGit(ctxOf(cmd), r.Repo)
		defer cancel()
		if _, rerr := g.ResolveCommit("refs/heads/" + r.Branch); rerr != nil {
			return "", fmt.Errorf("task %s has no draft and its branch is gone; write the record and pass it with --text-file FILE", t.Slug)
		}
		in := collectRecordInfo(g, t, nil)
		b = []byte(task.Draft(t, in.changed, in.commits, nil))
		if err := saveDraft(e, t, string(b)); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	if !edit {
		return string(b), nil
	}
	tmp := filepath.Join(filepath.Dir(p), t.ID+".edit.md")
	if err := wsRoot.WriteAtomic(tmp, b, 0o600); err != nil {
		return "", err
	}
	defer func() { _ = wsRoot.Remove(tmp) }()
	argv := append(editorArgv(), tmp)
	if err := runEditor(argv); err != nil {
		return "", fmt.Errorf("editor %s: %w; the draft was not changed", argv[0], err)
	}
	after, err := wsRoot.ReadFile(tmp)
	if err != nil {
		return "", err
	}
	if len(after) > maxRecordBytes {
		return "", fmt.Errorf("the record text is longer than %d bytes", maxRecordBytes)
	}
	if err := wsRoot.WriteAtomic(p, after, 0o600); err != nil {
		return "", err
	}
	return string(after), nil
}

// saveRecord runs the gate, prints each check and writes the record only
// when all pass, through history so it can be undone.
func saveRecord(cmd *cobra.Command, e *agentsEnv, st taskSettings, t task.Task, text string) error {
	out := cmd.OutOrStdout()
	ok := true
	for _, c := range task.Gate(text, st.gateBanned...) {
		mark := "pass"
		if !c.OK {
			mark, ok = "fail", false
		}
		_, _ = fmt.Fprintf(out, "  %s  %s\n", mark, c.Text)
	}
	if !ok {
		return fmt.Errorf("the record of task %s is not saved: fix the failed checks and run: cairn task record %s --edit", t.Slug, t.Slug)
	}
	p, data, err := task.PrepareRecord(wsRoot, st.knowledgeRoot, t, text, st.gateBanned...)
	if err != nil {
		return err
	}
	// The record is a note for other tools and people to read (0644); the
	// draft is private work in progress (0600).
	ed := fileEdit{path: p, after: data, afterExists: true, perm: 0o644}
	if old, err := wsRoot.ReadFile(p); err == nil {
		ed.before, ed.beforeExists = old, true
	}
	if err := commitEdits(e.dirs, "task.record", "record task "+t.Slug, []fileEdit{ed}); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "record saved: %s\n", p)
	return nil
}
