package index

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"github.com/mrqwer/cairn/internal/agents"
	"github.com/mrqwer/cairn/internal/agents/adapter"
	"github.com/mrqwer/cairn/internal/agents/conv"
	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/ui"
)

const (
	manifestFile = "manifest.json"
	lockFile     = ".lock"
	textExt      = ".txt"
	// DefaultMaxFileSize bounds a transcript the index reads; a larger
	// one is skipped and reported.
	DefaultMaxFileSize = 256 << 20
	// MaxText bounds the indexed text of one conversation; the rest of a
	// longer transcript is not indexed.
	MaxText = 8 << 20
)

// Index is the redacted full-text index: one text file per conversation,
// <Dir>/<id>.txt, and manifest.json mapping each id to the transcript it
// was read from. Dir is an absolute path resolved under Root, created 0700;
// every file is 0600. Home expands "~/" in adapter locators.
type Index struct {
	Dir  string
	Root fsx.Root
	Home string
	// MaxFileSize overrides DefaultMaxFileSize when positive.
	MaxFileSize int64
	// LockWait bounds the wait for another process updating the index
	// (DefaultLockWait when zero); OnBusy, when set, is called once when
	// the wait starts, so the caller can say why it is waiting.
	LockWait time.Duration
	OnBusy   func()
}

// DefaultLockWait is how long Update, Purge and Include wait for the index
// lock before giving up with ErrBusy.
const DefaultLockWait = 30 * time.Second

// ErrBusy is returned when another process kept the index locked for
// longer than LockWait.
var ErrBusy = errors.New("the index is busy (another find or index command is updating it)")

const excludedFile = "excluded.json"

// redactionKey identifies the rules an entry was redacted under: the
// redactor version and the adapter's secret_fields. An entry under other
// rules is redacted again.
func redactionKey(fields []string) string {
	fs := append([]string(nil), fields...)
	sort.Strings(fs)
	sum := sha256.Sum256([]byte(strings.Join(fs, "\x00")))
	return RedactorVersion + ":" + hex.EncodeToString(sum[:6])
}

// Entry is the manifest record of one indexed conversation: the transcript
// it came from (path, size, mtime, parser) and the record fields search
// filters on, refreshed on every Update. Error is set when the transcript
// could not be indexed, so an unchanged bad file is reported again without
// being read again.
type Entry struct {
	Path         string     `json:"path"`
	Size         int64      `json:"size"`
	MTime        time.Time  `json:"mtime"`
	Parser       string     `json:"parser"`
	ParsedAt     time.Time  `json:"parsedAt"`
	Error        string     `json:"error,omitempty"`
	SkippedLines int        `json:"skippedLines,omitempty"`
	Tool         string     `json:"tool,omitempty"`
	Project      string     `json:"project,omitempty"`
	Task         string     `json:"task,omitempty"`
	Name         string     `json:"name,omitempty"`
	State        string     `json:"state,omitempty"`
	Tags         []string   `json:"tags,omitempty"`
	ArchivedAt   *time.Time `json:"archivedAt,omitempty"`
	LastActivity time.Time  `json:"lastActivity"`
	// Redaction is the redactionKey the text was written under.
	Redaction string `json:"redaction,omitempty"`
}

// Skip is a transcript (or a whole tool, Path "tool <name>") that was not
// indexed or imported, and why.
type Skip struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Report is the outcome of Update. Warnings are transcripts indexed with
// some unreadable lines left out.
type Report struct {
	Indexed   int    `json:"indexed"`
	Unchanged int    `json:"unchanged"`
	Skipped   []Skip `json:"skipped"`
	Warnings  []Skip `json:"warnings,omitempty"`
}

// Hit is one search result.
type Hit struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Tool    string    `json:"tool"`
	Project string    `json:"project"`
	State   string    `json:"state"`
	At      time.Time `json:"last_activity_at"`
	Snippet string    `json:"snippet"`
}

func (ix Index) file(name string) string { return ix.Dir + "/" + name }

func (ix Index) maxFile() int64 {
	if ix.MaxFileSize > 0 {
		return ix.MaxFileSize
	}
	return DefaultMaxFileSize
}

// ensureDir creates Dir 0700 (tightening an existing one) and refuses a
// Dir that is a symlink or not a directory.
func (ix Index) ensureDir() error {
	if !strings.HasPrefix(ix.Dir, "/") {
		return fmt.Errorf("index dir %q is not absolute", ix.Dir)
	}
	if err := ix.Root.MkdirAll(ix.Dir, 0o700); err != nil {
		return err
	}
	fi, err := ix.Root.Lstat(ix.Dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("index dir %s is not a directory", ix.Dir)
	}
	return os.Chmod(ix.Root.Path(ix.Dir), 0o700)
}

// withLock runs fn holding the index lock, so concurrent updates do not
// lose each other's manifest entries.
func (ix Index) withLock(ctx context.Context, fn func() error) error {
	if err := ix.ensureDir(); err != nil {
		return err
	}
	f, err := os.OpenFile(ix.Root.Path(ix.file(lockFile)), os.O_RDWR|os.O_CREATE|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	wait := ix.LockWait
	if wait <= 0 {
		wait = DefaultLockWait
	}
	deadline := time.Now().Add(wait)
	told := false
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			return err
		}
		if !told && ix.OnBusy != nil {
			ix.OnBusy()
		}
		told = true
		if time.Now().After(deadline) {
			return ErrBusy
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	defer func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }()
	return fn()
}

// load reads the manifest; a missing or unreadable one is empty, so
// everything is indexed again.
func (ix Index) load() map[string]Entry {
	m := map[string]Entry{}
	b, err := ix.Root.ReadFile(ix.file(manifestFile))
	if err != nil || json.Unmarshal(b, &m) != nil {
		return map[string]Entry{}
	}
	for id := range m {
		if !conv.ValidID(id) {
			delete(m, id)
		}
	}
	return m
}

func (ix Index) save(m map[string]Entry) error {
	b, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return err
	}
	return ix.Root.WriteAtomic(ix.file(manifestFile), append(b, '\n'), 0o600)
}

func (ix Index) removeText(id string) error { return ix.Root.Remove(ix.file(id + textExt)) }

func setMeta(e *Entry, c conv.Conversation) {
	e.Tool, e.Project, e.Task, e.Name, e.State = c.Tool, c.Project, c.Task, c.Name, c.State
	e.Tags, e.ArchivedAt, e.LastActivity = c.Tags, c.ArchivedAt, c.LastActivityAt
}

func (e Entry) conv(id string) conv.Conversation {
	return conv.Conversation{ID: id, Tool: e.Tool, Project: e.Project, Task: e.Task, Name: e.Name, State: e.State,
		Tags: e.Tags, ArchivedAt: e.ArchivedAt, LastActivityAt: e.LastActivity}
}

// Update brings the index in line with convs (all recorded conversations,
// archived included): a transcript is read only when its path, size, mtime
// or parser changed; conversations no longer in convs leave the index.
// Transcripts are found through the adapter's history.locator by native
// id, never through a stored path. A tool whose parser is unknown, an
// unsafe id, a non-regular or oversized file, and a parse failure are
// reported in Skipped and never fail the update. When ctx ends, the work
// done so far is kept and ctx's error returned.
func (ix Index) Update(ctx context.Context, convs []conv.Conversation, parsers map[string]Parser, adapters []adapter.Adapter) (Report, error) {
	rep := Report{Skipped: []Skip{}}
	err := ix.withLock(ctx, func() error {
		ix.removeStaleTemps()
		m := ix.load()
		excluded, err := ix.excluded()
		if err != nil {
			return err
		}
		byTool := map[string]adapter.Adapter{}
		for _, a := range adapters {
			byTool[a.Name] = a
		}
		toolErr := map[string]error{}
		seen := map[string]bool{}
		for _, c := range convs {
			if err := ctx.Err(); err != nil {
				if serr := ix.save(m); serr != nil {
					return serr
				}
				return err
			}
			a, ok := byTool[c.Tool]
			if c.NativeID == "" || !ok || a.History.Locator == "" {
				continue // nothing on disk to index
			}
			if c.Project != "" && excluded[c.Project] {
				continue // purged: stays out until included again
			}
			if !conv.ValidID(c.ID) {
				rep.Skipped = append(rep.Skipped, Skip{Path: c.ID, Reason: "conversation id is not usable"})
				continue
			}
			if !agents.ValidNativeID(c.NativeID) {
				rep.Skipped = append(rep.Skipped, Skip{Path: c.ID, Reason: fmt.Sprintf("native id %q is not usable", c.NativeID)})
				continue
			}
			p, err := ParserFor(parsers, a.History.Parser)
			if err != nil {
				if _, done := toolErr[a.Name]; !done {
					toolErr[a.Name] = err
					rep.Skipped = append(rep.Skipped, Skip{Path: "tool " + a.Name, Reason: err.Error()})
				}
				continue
			}
			path, ok := newest(ix.Root, ix.Home, a, c.NativeID)
			if !ok {
				continue // no transcript (yet): nothing to index
			}
			seen[c.ID] = true
			e, read, skip, warn := ix.indexOne(c, path, p, a.History.SecretFields, m[c.ID])
			m[c.ID] = e
			switch {
			case skip != nil:
				rep.Skipped = append(rep.Skipped, *skip)
			case read:
				rep.Indexed++
			default:
				rep.Unchanged++
			}
			if warn != nil {
				rep.Warnings = append(rep.Warnings, *warn)
			}
		}
		for id := range m {
			if !seen[id] {
				delete(m, id)
				if err := ix.removeText(id); err != nil {
					return err
				}
			}
		}
		if err := ix.save(m); err != nil {
			return err
		}
		return ix.removeOrphans(m)
	})
	return rep, err
}

// newest is the most recently modified transcript of nativeID.
func newest(root fsx.Root, home string, a adapter.Adapter, nativeID string) (string, bool) {
	ms, err := agents.TranscriptPaths(root, home, a, nativeID)
	if err != nil || len(ms) == 0 {
		return "", false
	}
	best, bestT := "", time.Time{}
	for _, p := range ms {
		fi, err := os.Lstat(p)
		if err != nil {
			continue
		}
		if best == "" || fi.ModTime().After(bestT) {
			best, bestT = p, fi.ModTime()
		}
	}
	return best, best != ""
}

// indexOne (re)indexes c from path when it changed since old; read says
// whether it was read, skip is set when the transcript is not indexed.
func (ix Index) indexOne(c conv.Conversation, path string, p Parser, fields []string, old Entry) (e Entry, read bool, skip, warn *Skip) {
	fail := func(e Entry, reason string) (Entry, bool, *Skip, *Skip) {
		e.Error = reason
		_ = ix.removeText(c.ID)
		return e, false, &Skip{Path: path, Reason: reason}, nil
	}
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		e = Entry{Path: path, Parser: p.Name()}
		setMeta(&e, c)
		if errors.Is(err, unix.ELOOP) {
			return fail(e, "not a regular file (symlink)")
		}
		return fail(e, err.Error())
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		e = Entry{Path: path, Parser: p.Name()}
		setMeta(&e, c)
		return fail(e, err.Error())
	}
	rkey := redactionKey(fields)
	if old.Path == path && old.Size == fi.Size() && old.MTime.Equal(fi.ModTime()) && old.Parser == p.Name() &&
		old.Redaction == rkey && (old.Error != "" || ix.Root.Exists(ix.file(c.ID+textExt))) {
		e = old
		setMeta(&e, c)
		if old.Error != "" {
			return e, false, &Skip{Path: path, Reason: old.Error}, nil
		}
		return e, false, nil, nil
	}
	e = Entry{Path: path, Size: fi.Size(), MTime: fi.ModTime(), Parser: p.Name(), Redaction: rkey}
	setMeta(&e, c)
	if !fi.Mode().IsRegular() {
		return fail(e, "not a regular file")
	}
	if fi.Size() > ix.maxFile() {
		return fail(e, fmt.Sprintf("too large (%d bytes, limit %d)", fi.Size(), ix.maxFile()))
	}
	tr, err := p.Parse(io.LimitReader(f, ix.maxFile()))
	if err != nil {
		return fail(e, p.Name()+": "+err.Error())
	}
	if err := ix.Root.WriteAtomic(ix.file(c.ID+textExt), plainText(tr, fields), 0o600); err != nil {
		return fail(e, "write index: "+err.Error())
	}
	e.ParsedAt = time.Now().UTC()
	e.SkippedLines = tr.Skipped
	if tr.Skipped > 0 {
		warn = &Skip{Path: path, Reason: fmt.Sprintf("%d unreadable lines left out", tr.Skipped)}
	}
	return e, true, nil, warn
}

// plainText is the redacted text of tr, at most MaxText bytes, one
// message per paragraph without role labels (they would match every
// search). Terminal control sequences are removed and each message is
// redacted on its own before the cut, so no cut can expose part of a
// secret.
func plainText(tr Transcript, fields []string) []byte {
	var b bytes.Buffer
	for _, m := range tr.Messages {
		b.WriteString(RedactFields(ui.SafeText(m.Text), fields))
		b.WriteString("\n\n")
		if b.Len() > MaxText {
			break
		}
	}
	out := b.Bytes()
	if len(out) > MaxText {
		// The cut may split a rune; drop the partial one.
		out = bytes.ToValidUTF8(out[:MaxText], nil)
	}
	return out
}

// removeOrphans deletes text files no manifest entry refers to.
func (ix Index) removeOrphans(m map[string]Entry) error {
	ents, err := os.ReadDir(ix.Root.Path(ix.Dir))
	if err != nil {
		return err
	}
	for _, de := range ents {
		id, ok := strings.CutSuffix(de.Name(), textExt)
		if !ok || !conv.ValidID(id) {
			continue
		}
		if _, keep := m[id]; !keep {
			if err := ix.removeText(id); err != nil {
				return err
			}
		}
	}
	return nil
}

// removeStaleTemps deletes temp files a crashed write left behind
// (".<name>.tmp*"). Every write runs under the index lock, which the
// caller holds, so none is in progress.
func (ix Index) removeStaleTemps() {
	ents, err := os.ReadDir(ix.Root.Path(ix.Dir))
	if err != nil {
		return
	}
	for _, de := range ents {
		if n := de.Name(); strings.HasPrefix(n, ".") && strings.Contains(n, ".tmp") && de.Type().IsRegular() {
			_ = ix.Root.Remove(ix.file(n))
		}
	}
}

// Reset forgets what was indexed, so the next Update reads every
// transcript again.
func (ix Index) Reset() error {
	return ix.withLock(context.Background(), func() error { return ix.Root.Remove(ix.file(manifestFile)) })
}

// excluded is the set of purged projects.
func (ix Index) excluded() (map[string]bool, error) {
	b, err := ix.Root.ReadFile(ix.file(excludedFile))
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	var list []string
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("%s: %w", excludedFile, err)
	}
	out := map[string]bool{}
	for _, p := range list {
		out[p] = true
	}
	return out, nil
}

func (ix Index) saveExcluded(set map[string]bool) error {
	list := make([]string, 0, len(set))
	for p := range set {
		list = append(list, p)
	}
	sort.Strings(list)
	b, err := json.Marshal(list)
	if err != nil {
		return err
	}
	return ix.Root.WriteAtomic(ix.file(excludedFile), append(b, '\n'), 0o600)
}

// Excluded lists the purged projects, sorted.
func (ix Index) Excluded() ([]string, error) {
	set, err := ix.excluded()
	if err != nil {
		return nil, err
	}
	list := make([]string, 0, len(set))
	for p := range set {
		list = append(list, p)
	}
	sort.Strings(list)
	return list, nil
}

func checkProject(project string) error {
	if project == "" || len(project) > 128 || ui.SafeLine(project) != project {
		return fmt.Errorf("project %q is not usable", project)
	}
	return nil
}

// Include takes a purged project back: the next Update indexes it again.
func (ix Index) Include(project string) error {
	if err := checkProject(project); err != nil {
		return err
	}
	return ix.withLock(context.Background(), func() error {
		set, err := ix.excluded()
		if err != nil {
			return err
		}
		delete(set, project)
		return ix.saveExcluded(set)
	})
}

// Purge removes the index data of every conversation of project and keeps
// the project out of later updates until Include.
func (ix Index) Purge(project string) error {
	if err := checkProject(project); err != nil {
		return err
	}
	return ix.withLock(context.Background(), func() error {
		set, err := ix.excluded()
		if err != nil {
			return err
		}
		set[project] = true
		if err := ix.saveExcluded(set); err != nil {
			return err
		}
		m := ix.load()
		for id, e := range m {
			if e.Project == project {
				delete(m, id)
				if err := ix.removeText(id); err != nil {
					return err
				}
			}
		}
		return ix.save(m)
	})
}

// snippetRadius is how much text a hit shows on each side of the match.
const snippetRadius = 80

// Search returns the conversations whose name or indexed text contains
// every whitespace-separated term of q (case-insensitive) and that pass f,
// most recently active first, at most limit (20 when limit <= 0). It scans
// the text files; it does not update the index.
func (ix Index) Search(q string, f conv.Filter, limit int) ([]Hit, error) {
	terms := strings.Fields(q)
	if len(terms) == 0 {
		return nil, errors.New("empty query")
	}
	if limit <= 0 {
		limit = 20
	}
	res := make([]*regexp.Regexp, len(terms))
	for i, t := range terms {
		res[i] = regexp.MustCompile(`(?i)` + regexp.QuoteMeta(t))
	}
	m := ix.load()
	var hits []Hit
	for id, e := range m {
		if e.Error != "" {
			continue
		}
		c := e.conv(id)
		if !f.Match(c) {
			continue
		}
		text, err := ix.readText(id)
		if err != nil {
			continue
		}
		at := -1
		ok := true
		for i, re := range res {
			loc := re.FindIndex(text)
			if loc == nil && !re.MatchString(e.Name) {
				ok = false
				break
			}
			if loc != nil && (at < 0 || i == 0) {
				at = loc[0]
			}
		}
		if !ok {
			continue
		}
		hits = append(hits, Hit{ID: id, Name: e.Name, Tool: e.Tool, Project: e.Project, State: e.State,
			At: e.LastActivity, Snippet: snippet(text, max(at, 0))})
	}
	sort.Slice(hits, func(i, j int) bool {
		if !hits[i].At.Equal(hits[j].At) {
			return hits[i].At.After(hits[j].At)
		}
		return hits[i].ID < hits[j].ID
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// readText reads one text file, refusing anything but a regular file of at
// most MaxText bytes.
func (ix Index) readText(id string) ([]byte, error) {
	p := ix.Root.Path(ix.file(id + textExt))
	f, err := os.OpenFile(p, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fs.ErrInvalid
	}
	return io.ReadAll(io.LimitReader(f, MaxText))
}

// snippet is one line of text around byte offset at, redacted again.
func snippet(text []byte, at int) string {
	start, end := max(0, at-snippetRadius), min(len(text), at+snippetRadius)
	for start > 0 && !utf8.RuneStart(text[start]) {
		start--
	}
	for end < len(text) && !utf8.RuneStart(text[end]) {
		end++
	}
	s := strings.Join(strings.Fields(string(text[start:end])), " ")
	if start > 0 {
		s = "…" + s
	}
	if end < len(text) {
		s += "…"
	}
	return Redact(s)
}
