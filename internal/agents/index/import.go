package index

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/sys/unix"

	"github.com/mrqwer/cairn/internal/agents"
	"github.com/mrqwer/cairn/internal/agents/adapter"
	"github.com/mrqwer/cairn/internal/agents/conv"
	"github.com/mrqwer/cairn/internal/fsx"
)

// importHead bounds how much of a transcript import reads: the session id,
// cwd and start time are on the first lines.
const importHead = 1 << 20

// ImportOptions tune Import. Home expands "~/" in locators; Tool limits the
// import to one tool; DryRun records nothing; ProjectOf maps a transcript's
// working directory to a project id ("" for none).
type ImportOptions struct {
	Home      string
	Tool      string
	DryRun    bool
	ProjectOf func(cwd string) string
}

// ImportReport lists the records created (or, on a dry run, that would be),
// how many transcripts were already recorded, and what was skipped.
type ImportReport struct {
	Created  []conv.Conversation `json:"created"`
	Existing int                 `json:"existing"`
	Skipped  []Skip              `json:"skipped"`
}

// Import adopts the existing conversations of every adapter with a
// history.locator and a known parser: the locator is globbed and a record
// is created for each native id not yet recorded for that tool (removed
// conversations included), unnamed, in state stopped (so opening it
// resumes rather than starts), under the project whose root holds its
// working directory. It writes conversation records only, never desired
// state (R2), and is idempotent; concurrent imports cannot duplicate a
// record. Unusable file names, symlinks, unreadable or foreign files and
// unknown parsers are reported in Skipped, never fatal.
func Import(ctx context.Context, root fsx.Root, adapters []adapter.Adapter, store conv.Store, opt ImportOptions) (ImportReport, error) {
	rep := ImportReport{Created: []conv.Conversation{}, Skipped: []Skip{}}
	parsers := Parsers()
	var cands []conv.Conversation
	for _, a := range adapters {
		if opt.Tool != "" && a.Name != opt.Tool {
			continue
		}
		if a.History.Locator == "" {
			continue // the tool keeps no transcripts cairn can find
		}
		p, err := ParserFor(parsers, a.History.Parser)
		if err != nil {
			rep.Skipped = append(rep.Skipped, Skip{Path: "tool " + a.Name, Reason: err.Error()})
			continue
		}
		if !strings.Contains(filepath.Base(a.History.Locator), "{native_id}") {
			rep.Skipped = append(rep.Skipped, Skip{Path: "tool " + a.Name, Reason: "history.locator must name {native_id} in its file name"})
			continue
		}
		pat, err := agents.LocatorGlob(a, opt.Home)
		if err != nil {
			rep.Skipped = append(rep.Skipped, Skip{Path: "tool " + a.Name, Reason: err.Error()})
			continue
		}
		matches, err := filepath.Glob(root.Path(pat))
		if err != nil {
			rep.Skipped = append(rep.Skipped, Skip{Path: "tool " + a.Name, Reason: err.Error()})
			continue
		}
		sort.Strings(matches)
		known, err := store.NativeIDs(a.Name)
		if err != nil {
			return rep, err
		}
		for _, path := range matches {
			if err := ctx.Err(); err != nil {
				return rep, err
			}
			id := agents.NativeIDFromPath(a, path)
			if !agents.ValidNativeID(id) {
				rep.Skipped = append(rep.Skipped, Skip{Path: path, Reason: "file name holds no usable native id"})
				continue
			}
			if known[id] {
				rep.Existing++
				continue
			}
			c, err := importOne(a, p, path, id, opt)
			if err != nil {
				rep.Skipped = append(rep.Skipped, Skip{Path: path, Reason: err.Error()})
				continue
			}
			known[id] = true
			cands = append(cands, c)
		}
	}
	if opt.DryRun {
		rep.Created = append(rep.Created, cands...)
		return rep, nil
	}
	added, err := store.InsertNew(cands)
	if err != nil {
		return rep, err
	}
	rep.Created = append(rep.Created, added...)
	// Recorded by a concurrent import in the meantime.
	rep.Existing += len(cands) - len(added)
	return rep, nil
}

// importOne builds the record of one transcript from its head.
func importOne(a adapter.Adapter, p Parser, path, nativeID string, opt ImportOptions) (conv.Conversation, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ELOOP) {
		return conv.Conversation{}, errors.New("not a regular file (symlink)")
	}
	if err != nil {
		return conv.Conversation{}, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return conv.Conversation{}, err
	}
	if !fi.Mode().IsRegular() {
		return conv.Conversation{}, errors.New("not a regular file")
	}
	tr, err := p.Parse(io.LimitReader(f, importHead))
	if err != nil {
		return conv.Conversation{}, fmt.Errorf("%s: %w", p.Name(), err)
	}
	started, last := tr.StartedAt, tr.EndedAt
	if started.IsZero() {
		started = fi.ModTime()
	}
	if fi.Size() > importHead || last.IsZero() {
		last = fi.ModTime() // the end of a long transcript was not read
	}
	cwd := cleanCwd(tr.Cwd)
	project := ""
	if cwd != "" && opt.ProjectOf != nil {
		project = opt.ProjectOf(cwd)
	}
	return conv.Conversation{
		ID:               conv.NewID(started, nil),
		Tool:             a.Name,
		NativeID:         nativeID,
		Project:          project,
		Cwd:              cwd,
		State:            conv.StateStopped,
		StateSource:      conv.SourceNative,
		TranscriptRef:    path,
		TranscriptParser: p.Name(),
		CreatedAt:        started.UTC(),
		LastActivityAt:   last.UTC(),
	}, nil
}

// cleanCwd keeps a transcript's working directory only when it is a clean
// absolute path without control characters: it is untrusted file content
// and later becomes a pane's directory.
func cleanCwd(s string) string {
	if s == "" || !filepath.IsAbs(s) || len(s) > 4096 || filepath.Clean(s) != s {
		return ""
	}
	if strings.IndexFunc(s, unicode.IsControl) >= 0 {
		return ""
	}
	return s
}
