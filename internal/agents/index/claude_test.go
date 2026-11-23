package index_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mrqwer/cairn/internal/agents/index"
)

func claudeParser(t *testing.T) index.Parser {
	t.Helper()
	p, err := index.ParserFor(index.Parsers(), "claude-jsonl@1")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "claude-jsonl@1" {
		t.Fatalf("name %q", p.Name())
	}
	return p
}

func TestParseClaudeFixture(t *testing.T) {
	f, err := os.Open("testdata/claude/basic.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	tr, err := claudeParser(t).Parse(f)
	if err != nil {
		t.Fatal(err)
	}
	if tr.NativeID != "5f0c2a9e-1b3d-4c5e-8f70-9a1b2c3d4e5f" || tr.Cwd != "/home/u/projects/demo" {
		t.Fatalf("ids: %+v", tr)
	}
	if tr.FirstPrompt != "Fix the flaky login test in the auth module" {
		t.Fatalf("first prompt %q", tr.FirstPrompt)
	}
	if want := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC); !tr.StartedAt.Equal(want) {
		t.Fatalf("started %v", tr.StartedAt)
	}
	if want := time.Date(2026, 10, 1, 9, 10, 0, 0, time.UTC); !tr.EndedAt.Equal(want) {
		t.Fatalf("ended %v", tr.EndedAt)
	}
	var texts []string
	for _, m := range tr.Messages {
		texts = append(texts, m.Role+": "+m.Text)
	}
	want := []string{
		"user: Fix the flaky login test in the auth module",
		"assistant: I will look at the retry logic.",
		"assistant: The retry window was too short; fixed.",
	}
	if strings.Join(texts, "\n") != strings.Join(want, "\n") {
		t.Fatalf("messages:\n%s", strings.Join(texts, "\n"))
	}
	if tr.Skipped != 0 {
		t.Fatalf("skipped %d", tr.Skipped)
	}
}

// repeatReader yields n bytes of b without holding them in memory.
type repeatReader struct {
	b byte
	n int
}

func (r *repeatReader) Read(p []byte) (int, error) {
	if r.n == 0 {
		return 0, io.EOF
	}
	k := min(len(p), r.n)
	for i := range p[:k] {
		p[i] = r.b
	}
	r.n -= k
	return k, nil
}

// garbageInput is a transcript with a truncated line, an unknown type,
// binary junk and one line of huge bytes, streamed without holding it.
func garbageInput(huge int) io.Reader {
	good := `{"type":"user","sessionId":"s1","cwd":"/w","timestamp":"2026-10-01T09:00:00Z","message":{"role":"user","content":"hello there"}}` + "\n"
	return io.MultiReader(
		strings.NewReader(good),
		strings.NewReader(`{"type":"user","message":{"role":"user","content":"trunc`+"\n"), // truncated line
		strings.NewReader(`{"type":"future-thing","x":1}`+"\n"),                            // unknown type
		bytes.NewReader([]byte{0, 1, 2, 0xff, 0xfe, '\n'}),                                 // binary junk
		strings.NewReader(`{"type":"user","message":{"role":"user","content":"`),
		&repeatReader{b: 'a', n: huge}, // one huge line
		strings.NewReader("\"}}\n"),
		strings.NewReader(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"still parsed"}]}}`), // no final newline
	)
}

func parseAlloc(t *testing.T, huge int) (index.Transcript, uint64) {
	t.Helper()
	p := claudeParser(t)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	tr, err := p.Parse(garbageInput(huge))
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	return tr, after.TotalAlloc - before.TotalAlloc
}

func TestParseTolerantGarbage(t *testing.T) {
	tr, alloc10 := parseAlloc(t, 10<<20) // a 10 MB single line
	if len(tr.Messages) != 2 || tr.Messages[0].Text != "hello there" || tr.Messages[1].Text != "still parsed" {
		t.Fatalf("messages %+v", tr.Messages)
	}
	// truncated, junk and the over-long line; an unknown type is not a fault.
	if tr.Skipped != 3 {
		t.Fatalf("skipped %d, want 3", tr.Skipped)
	}
	// Memory is bounded by the line cap, not the line: a line four times
	// longer costs no more.
	_, alloc40 := parseAlloc(t, 40<<20)
	if alloc10 > 3*index.MaxLine || alloc40 > alloc10+(1<<20) {
		t.Fatalf("allocated %d bytes for 10 MB and %d for 40 MB: not bounded by MaxLine %d", alloc10, alloc40, index.MaxLine)
	}
}

func TestParseAllGarbageFails(t *testing.T) {
	_, err := claudeParser(t).Parse(strings.NewReader("not json\n\x00\x01\n"))
	if !errors.Is(err, index.ErrNoTranscript) {
		t.Fatalf("err %v", err)
	}
	_, err = claudeParser(t).Parse(strings.NewReader(""))
	if !errors.Is(err, index.ErrNoTranscript) {
		t.Fatalf("empty: err %v", err)
	}
}

func TestParserVersionUnknown(t *testing.T) {
	for _, name := range []string{"claude-jsonl@2", "codex-jsonl@1", "", "claude-jsonl"} {
		if _, err := index.ParserFor(index.Parsers(), name); !errors.Is(err, index.ErrUnsupportedParser) {
			t.Errorf("%q: err %v", name, err)
		}
	}
}
