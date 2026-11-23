package task

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/mrqwer/cairn/internal/fsx"
)

// now is the clock of record front matter (tests replace it).
var now = time.Now

// maxDraftFiles is how many changed files a draft lists before "and N more":
// the record is 120 words at most.
const maxDraftFiles = 10

func oneLine(s string) string {
	return strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return ' '
		}
		return r
	}, s)), " ")
}

func shortID(c string) string {
	f := strings.Fields(c)
	if len(f) == 0 || !isHex(f[0]) {
		return ""
	}
	if len(f[0]) > 7 {
		return f[0][:7]
	}
	return f[0]
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// Draft is the skeleton of a finish record in the shape of the prototype
// (Goal, Changed, Decision, Verify, Links). Changed lists the real files
// of the diff and Links the branch, the commit range (commits oldest first,
// full or short ids) and the conversations. What cannot be known is left as
// a TODO marker: nothing is invented, and the gate refuses a record that
// still has one.
func Draft(t Task, changed, commits, convNames []string) string {
	var files []string
	for _, f := range changed {
		if f != "" && !strings.ContainsFunc(f, func(r rune) bool { return r < ' ' || r == 0x7f || r == ',' }) {
			files = append(files, f)
		}
	}
	ch := "TODO"
	if len(files) > 0 {
		ch = strings.Join(files[:min(len(files), maxDraftFiles)], ", ")
		if len(files) > maxDraftFiles {
			ch += fmt.Sprintf(" and %d more", len(files)-maxDraftFiles)
		}
	}
	links := []string{}
	if len(t.Repos) > 0 && t.Repos[0].Branch != "" {
		links = append(links, "branch "+t.Repos[0].Branch)
	}
	var ids []string
	for _, c := range commits {
		if s := shortID(c); s != "" {
			ids = append(ids, s)
		}
	}
	switch len(ids) {
	case 0:
	case 1:
		links = append(links, "commit "+ids[0])
	default:
		links = append(links, "commits "+ids[0]+".."+ids[len(ids)-1])
	}
	var names []string
	for _, n := range convNames {
		if n = oneLine(n); n != "" && len(names) < 3 {
			names = append(names, n)
		}
	}
	if len(names) > 0 {
		links = append(links, "conversations "+strings.Join(names, ", "))
	}
	lk := "TODO"
	if len(links) > 0 {
		lk = strings.Join(links, ", ")
	}
	return "Goal: TODO\nChanged: " + ch + "\nDecision: TODO\nVerify: TODO\nLinks: " + lk + "\n"
}

// Anchors are the files text refers to: path-like references, cleaned
// (no "./", no trailing "." or "/"), in order, without repeats and without
// the task's branch, URLs, or anything with a ".." part.
func Anchors(text, branch string) []string {
	var out []string
	seen := map[string]bool{}
	for _, ix := range refRe.FindAllStringIndex(text, -1) {
		m := text[ix[0]:ix[1]]
		if !strings.Contains(m, "/") || strings.HasSuffix(text[:ix[0]], "//") {
			continue
		}
		m = strings.TrimRight(m, "./-")
		for strings.HasPrefix(m, "./") {
			m = m[2:]
		}
		ok := m != "" && m != branch
		for _, seg := range strings.Split(m, "/") {
			if seg == "" || seg == "." || seg == ".." {
				ok = false
			}
		}
		if ok && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

// RecordPath is where the record of t goes: <dir>/<project>/<slug>.md. dir
// must be a clean absolute path; the task's id, slug and project are
// validated, so the result cannot leave dir.
func RecordPath(dir string, t Task) (string, error) {
	if !cleanAbs(dir) {
		return "", fmt.Errorf("record directory %q is not a clean absolute path", dir)
	}
	if err := t.Validate(); err != nil {
		return "", fmt.Errorf("task is not valid: %w", err)
	}
	p := filepath.Join(dir, t.Project, t.Slug+".md")
	if rel, err := filepath.Rel(dir, p); err != nil || rel != path.Join(t.Project, t.Slug+".md") {
		return "", fmt.Errorf("record path %q is outside %s", p, dir)
	}
	return p, nil
}

// PrepareRecord checks the gate (with extra banned phrases) and returns the
// path and the content of the record without writing anything. It refuses
// when a check fails, and when p holds something that is not a record of
// this same task (another task's record, or a note of the user).
func PrepareRecord(root fsx.Root, dir string, t Task, text string, extra ...string) (string, []byte, error) {
	p, err := RecordPath(dir, t)
	if err != nil {
		return "", nil, err
	}
	var bad []string
	for _, c := range Gate(text, extra...) {
		if !c.OK {
			bad = append(bad, c.Text)
		}
	}
	if len(bad) > 0 {
		return "", nil, fmt.Errorf("the record does not pass the quality gate: %s", strings.Join(bad, "; "))
	}
	if old, err := root.ReadFile(p); err == nil {
		if recordTask(old) != t.ID {
			return "", nil, fmt.Errorf("%s exists and is not a record of task %s; not overwritten", p, t.ID)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", nil, err
	}
	branch := ""
	if len(t.Repos) > 0 {
		branch = t.Repos[0].Branch
	}
	var b strings.Builder
	b.WriteString("---\ntask: " + t.ID + "\nproject: " + t.Project + "\nbranch: " + branch + "\nanchors:")
	an := Anchors(text, branch)
	if len(an) == 0 {
		b.WriteString(" []")
	}
	b.WriteString("\n")
	for _, a := range an {
		b.WriteString("  - " + a + "\n")
	}
	b.WriteString("created: " + now().UTC().Format(time.RFC3339) + "\n---\n")
	b.WriteString(strings.TrimRight(text, " \t\r\n") + "\n")
	return p, []byte(b.String()), nil
}

// recordTask is the task id in the front matter of a record, or "".
func recordTask(data []byte) string {
	ls := strings.Split(string(data), "\n")
	if len(ls) == 0 || strings.TrimRight(ls[0], "\r") != "---" {
		return ""
	}
	for _, l := range ls[1:] {
		l = strings.TrimRight(l, "\r")
		if l == "---" {
			return ""
		}
		if v, ok := strings.CutPrefix(l, "task: "); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// WriteRecord writes the record of t for the knowledge module: refuses
// unless every check of the gate passes, then writes <dir>/<project>/<slug>.md
// atomically with a small front matter (task, project, branch, anchors,
// created) and returns its path.
func WriteRecord(root fsx.Root, dir string, t Task, text string, extra ...string) (string, error) {
	p, data, err := PrepareRecord(root, dir, t, text, extra...)
	if err != nil {
		return "", err
	}
	if err := root.WriteAtomic(p, data, 0o644); err != nil {
		return "", err
	}
	return p, nil
}
