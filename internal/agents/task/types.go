// Package task holds cairn tasks: a titled unit of work on its own branch,
// optionally in its own git worktree, with the git operations, include-file
// copying, setup approval and the agent concurrency queue they need.
package task

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/pelletier/go-toml/v2"

	"github.com/mrqwer/cairn/internal/agents/conv"
	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/xdg"
)

// Task statuses.
const (
	StatusOpen      = "open"
	StatusReview    = "review"
	StatusDone      = "done"
	StatusAbandoned = "abandoned"
)

// Isolation modes: a worktree of its own, or the main checkout. Ask is a
// config value only (tasks.default_isolation), never stored in a task.
const (
	IsolationWorktree = "worktree"
	IsolationHere     = "here"
	IsolationAsk      = "ask"
)

// MaxSlug is the longest slug, in runes (all ASCII).
const MaxSlug = 40

// Repo is one repository of a task (R6: a list; M2 uses one entry). Base is
// the ref the branch was made from as given (a branch name, or a commit),
// BaseCommit what it resolved to: "new commits" are counted from there.
// Worktree is empty for a task in the main checkout.
type Repo struct {
	Repo       string `toml:"repo"`
	Base       string `toml:"base"`
	BaseCommit string `toml:"base_commit"`
	Branch     string `toml:"branch"`
	Worktree   string `toml:"worktree,omitempty"`
	// Return is what the main checkout had checked out before a task
	// --here switched it (a branch, or a commit when detached).
	Return string `toml:"return,omitempty"`
}

// Task is desired state at <Config>/tasks/<id>.toml, written through
// history so it can be undone.
type Task struct {
	ID        string    `toml:"id"`
	Title     string    `toml:"title"`
	Slug      string    `toml:"slug"`
	Project   string    `toml:"project"`
	Repos     []Repo    `toml:"repos"`
	Isolation string    `toml:"isolation"`
	Status    string    `toml:"status"`
	CreatedAt time.Time `toml:"created_at"`
}

// Window is the key of the tmux window of a task in its project.
func (t Task) Window() string { return "task-" + t.Slug }

var (
	nonSlug   = regexp.MustCompile(`[^a-z0-9]+`)
	slugRe    = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	prefixRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*(/[A-Za-z0-9][A-Za-z0-9_-]*)*$`)
	shaRe     = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	projectRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
)

// Slug makes a branch- and file-safe name of a title: lower case, runs of
// anything outside [a-z0-9] become one "-", at most MaxSlug runes, no "-"
// at either end. A title with no Latin letter or digit (Cyrillic, say)
// gives "task-" and 8 hex digits of its sha256, stable per title; one
// without any letter or digit gives "task". The title itself stays the display name.
func Slug(title string) string {
	s := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(title), "-"), "-")
	if len(s) > MaxSlug {
		s = strings.TrimRight(s[:MaxSlug], "-")
	}
	if s != "" {
		return s
	}
	if t := strings.TrimSpace(title); strings.IndexFunc(t, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) >= 0 {
		sum := sha256.Sum256([]byte(t))
		return "task-" + hex.EncodeToString(sum[:4])
	}
	return "task"
}

// ValidSlug reports whether s is a slug as Slug and UniqueBranch make them.
func ValidSlug(s string) bool { return len(s) <= MaxSlug && slugRe.MatchString(s) }

// UniqueBranch returns slug, or slug-2, slug-3, ... (cut to stay within
// MaxSlug), and its branch <prefix>/<slug>: the first for which taken is
// false. taken sees both, so a used worktree directory or task slug counts
// like an existing branch.
func UniqueBranch(prefix, slug string, taken func(branch, slug string) bool) (string, string) {
	for n := 1; ; n++ {
		s := slug
		if n > 1 {
			suf := "-" + strconv.Itoa(n)
			base := slug
			if len(base)+len(suf) > MaxSlug {
				base = strings.TrimRight(base[:MaxSlug-len(suf)], "-")
			}
			s = base + suf
		}
		b := prefix + "/" + s
		if !taken(b, s) {
			return s, b
		}
	}
}

// ValidBranchPrefix accepts "/"-separated components of [A-Za-z0-9_-] that
// do not start with "-" or "_": nothing git would reject or read as an
// option.
func ValidBranchPrefix(p string) bool { return prefixRe.MatchString(p) }

// ValidBranch accepts a task branch: a valid prefix plus a slug.
func ValidBranch(b string) bool {
	i := strings.LastIndexByte(b, '/')
	return i > 0 && ValidBranchPrefix(b[:i]) && ValidSlug(b[i+1:])
}

// CheckRef rejects a ref given by the user that git could read as an
// option or that cannot be a single revision: empty, a leading "-",
// whitespace or control characters, or "..".
func CheckRef(r string) error {
	switch {
	case r == "":
		return errors.New("empty ref")
	case strings.HasPrefix(r, "-"):
		return fmt.Errorf("ref %q must not start with \"-\"", r)
	case strings.ContainsFunc(r, func(c rune) bool { return c <= ' ' || c == 0x7f }):
		return fmt.Errorf("ref %q contains whitespace or control characters", r)
	case strings.Contains(r, ".."):
		return fmt.Errorf("ref %q must name one commit, not a range", r)
	}
	return nil
}

// Validate checks a task as read from its file: every value that reaches a
// path, a git argument or a tmux name is checked here.
func (t Task) Validate() error {
	var probs []string
	add := func(f string, a ...any) { probs = append(probs, fmt.Sprintf(f, a...)) }
	if !conv.ValidID(t.ID) {
		add("id %q is not a valid id", t.ID)
	}
	if !ValidSlug(t.Slug) {
		add("slug %q is not a valid slug", t.Slug)
	}
	if !projectRe.MatchString(t.Project) {
		add("project %q is not a valid project id", t.Project)
	}
	if strings.ContainsFunc(t.Title, func(c rune) bool { return c < ' ' || c == 0x7f }) {
		add("title contains control characters")
	}
	switch t.Status {
	case StatusOpen, StatusReview, StatusDone, StatusAbandoned:
	default:
		add("status %q is not one of open, review, done, abandoned", t.Status)
	}
	switch t.Isolation {
	case IsolationWorktree, IsolationHere:
	default:
		add("isolation %q is not worktree or here", t.Isolation)
	}
	if len(t.Repos) == 0 {
		add("no repos")
	}
	for i, r := range t.Repos {
		if !cleanAbs(r.Repo) {
			add("repos[%d].repo %q is not a clean absolute path", i, r.Repo)
		}
		if !ValidBranch(r.Branch) {
			add("repos[%d].branch %q is not a task branch", i, r.Branch)
		}
		if r.Base != "" && CheckRef(r.Base) != nil {
			add("repos[%d].base %q is not a usable ref", i, r.Base)
		}
		if r.BaseCommit != "" && !shaRe.MatchString(r.BaseCommit) {
			add("repos[%d].base_commit %q is not a commit id", i, r.BaseCommit)
		}
		if r.Return != "" && (CheckRef(r.Return) != nil || t.Isolation != IsolationHere) {
			add("repos[%d].return %q is not usable here", i, r.Return)
		}
		switch {
		case t.Isolation == IsolationHere && r.Worktree != "":
			add("repos[%d].worktree is set for a task in the main checkout", i)
		case t.Isolation == IsolationWorktree && !cleanAbs(r.Worktree):
			add("repos[%d].worktree %q is not a clean absolute path", i, r.Worktree)
		}
	}
	if len(probs) > 0 {
		return errors.New(strings.Join(probs, "; "))
	}
	return nil
}

func cleanAbs(p string) bool {
	return filepath.IsAbs(p) && filepath.Clean(p) == p &&
		!strings.ContainsFunc(p, func(c rune) bool { return c < ' ' || c == 0x7f })
}

// Encode renders the task file.
func (t Task) Encode() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("# cairn task (see: cairn task ls); cairn undo reverts its creation\n")
	if err := toml.NewEncoder(&buf).Encode(t); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Decode reads a task file strictly (unknown keys are errors) and
// validates it.
func Decode(data []byte) (Task, error) {
	var t Task
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		return Task{}, err
	}
	return t, t.Validate()
}

// Dir is where task files live.
func Dir(d xdg.Dirs) string { return filepath.Join(d.Config, "tasks") }

// Path is the file of task id; id must be valid (conv.ValidID).
func Path(d xdg.Dirs, id string) string { return filepath.Join(Dir(d), id+".toml") }

// LoadError is a task file that could not be read.
type LoadError struct {
	File string
	Err  error
}

func (e LoadError) Error() string { return e.File + ": " + e.Err.Error() }

// Load reads every task file, sorted by id. A broken file is reported in
// the second result; the others stay usable. A file whose id differs from
// its name is broken.
func Load(root fsx.Root, d xdg.Dirs) ([]Task, []LoadError, error) {
	ms, err := filepath.Glob(root.Path(filepath.Join(Dir(d), "*.toml")))
	if err != nil {
		return nil, nil, err
	}
	var ts []Task
	var lerrs []LoadError
	for _, m := range ms {
		rel := filepath.Join(Dir(d), filepath.Base(m))
		data, err := root.ReadFile(rel)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		t, err := Decode(data)
		if err == nil && t.ID+".toml" != filepath.Base(m) {
			err = fmt.Errorf("id %q does not match the file name", t.ID)
		}
		if err != nil {
			lerrs = append(lerrs, LoadError{File: rel, Err: err})
			continue
		}
		ts = append(ts, t)
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i].ID < ts[j].ID })
	return ts, lerrs, nil
}

// Find resolves ref as a task id, else a unique slug, else a unique id
// prefix of at least 4 characters.
func Find(ts []Task, ref string) (Task, error) {
	for _, t := range ts {
		if t.ID == ref {
			return t, nil
		}
	}
	pick := func(match func(Task) bool) (Task, bool, error) {
		var hits []string
		var found Task
		for _, t := range ts {
			if match(t) {
				hits = append(hits, t.Project+"/"+t.Slug+" ("+t.ID+")")
				found = t
			}
		}
		switch len(hits) {
		case 0:
			return Task{}, false, nil
		case 1:
			return found, true, nil
		}
		return Task{}, false, fmt.Errorf("%q matches several tasks: %s", ref, strings.Join(hits, ", "))
	}
	if t, ok, err := pick(func(t Task) bool { return t.Slug == ref }); ok || err != nil {
		return t, err
	}
	if len(ref) >= 4 {
		up := strings.ToUpper(ref)
		if t, ok, err := pick(func(t Task) bool { return strings.HasPrefix(t.ID, up) }); ok || err != nil {
			return t, err
		}
	}
	return Task{}, fmt.Errorf("no task %q (see: cairn task ls)", ref)
}
