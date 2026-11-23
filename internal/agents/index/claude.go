package index

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	// ErrUnsupportedParser is returned for a history.parser cairn does not
	// know (another tool's format or a future version); only that tool's
	// conversations are skipped.
	ErrUnsupportedParser = errors.New("unsupported transcript parser")
	// ErrNoTranscript is returned when not one line of the input is a
	// transcript line: the file is not a transcript of this format.
	ErrNoTranscript = errors.New("no transcript lines")
)

// MaxLine bounds one transcript line; a longer line is skipped without
// being held in memory.
const MaxLine = 4 << 20

// Message is one user or assistant text turn.
type Message struct {
	Role string
	Text string
	At   time.Time
}

// Transcript is the plain text of one conversation. Skipped counts lines
// that could not be read (malformed, truncated or over MaxLine); unknown
// line types are not counted, they are ignored by design.
type Transcript struct {
	NativeID, Cwd, FirstPrompt string
	Messages                   []Message
	StartedAt, EndedAt         time.Time
	Skipped                    int
}

// Parser reads one transcript format at one version ("claude-jsonl@1").
// Parse is tolerant: it fails only when the input holds no line of its
// format at all, or on a read error.
type Parser interface {
	Name() string
	Parse(r io.Reader) (Transcript, error)
}

// Parsers returns every parser cairn ships, by name.
func Parsers() map[string]Parser {
	return map[string]Parser{claudeJSONL1: claudeParser{}}
}

// ParserFor returns the parser called name, or ErrUnsupportedParser.
func ParserFor(ps map[string]Parser, name string) (Parser, error) {
	if p, ok := ps[name]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("%w %q", ErrUnsupportedParser, name)
}

const claudeJSONL1 = "claude-jsonl@1"

// claudeParser reads Claude Code session files: one JSON object per line,
// {"type":"user"|"assistant", "sessionId", "cwd", "timestamp",
// "message":{"role","content"}} where content is a string or a list of
// parts. Only text parts are kept: thinking, tool calls and tool results
// (file contents, command output) never reach the index.
type claudeParser struct{}

func (claudeParser) Name() string { return claudeJSONL1 }

type claudeLine struct {
	Type      string          `json:"type"`
	IsMeta    bool            `json:"isMeta"`
	SessionID string          `json:"sessionId"`
	Cwd       string          `json:"cwd"`
	Timestamp string          `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
}

type claudeMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func (claudeParser) Parse(r io.Reader) (Transcript, error) {
	var tr Transcript
	valid := 0
	err := eachLine(r, MaxLine, func(b []byte, tooLong bool) {
		b = bytes.TrimSpace(b)
		if tooLong {
			tr.Skipped++
			return
		}
		if len(b) == 0 {
			return
		}
		var l claudeLine
		if json.Unmarshal(b, &l) != nil || l.Type == "" {
			tr.Skipped++
			return
		}
		valid++
		if l.Type != "user" && l.Type != "assistant" {
			return
		}
		if tr.NativeID == "" {
			tr.NativeID = l.SessionID
		}
		if tr.Cwd == "" {
			tr.Cwd = l.Cwd
		}
		at, _ := time.Parse(time.RFC3339Nano, l.Timestamp)
		if !at.IsZero() {
			if tr.StartedAt.IsZero() || at.Before(tr.StartedAt) {
				tr.StartedAt = at
			}
			if at.After(tr.EndedAt) {
				tr.EndedAt = at
			}
		}
		if l.IsMeta || len(l.Message) == 0 {
			return
		}
		var m claudeMessage
		if json.Unmarshal(l.Message, &m) != nil {
			return
		}
		text := strings.TrimSpace(claudeText(m.Content))
		// Tool wrappers ("<command-name>", "<local-command-stdout>") are
		// not conversation text.
		if text == "" || strings.HasPrefix(text, "<") {
			return
		}
		role := m.Role
		if role == "" {
			role = l.Type
		}
		tr.Messages = append(tr.Messages, Message{Role: role, Text: text, At: at})
		if tr.FirstPrompt == "" && role == "user" {
			tr.FirstPrompt = text
		}
	})
	if err != nil {
		return tr, err
	}
	if valid == 0 {
		return tr, ErrNoTranscript
	}
	return tr, nil
}

// claudeText is the text of a content value: a string, or the text parts
// of a list.
func claudeText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var out []string
	for _, p := range parts {
		if p.Type == "text" && p.Text != "" {
			out = append(out, p.Text)
		}
	}
	return strings.Join(out, "\n")
}

// eachLine calls fn with each line of r. A line longer than max is passed
// as tooLong with no content and never held in memory beyond max bytes. An
// invalid UTF-8 sequence is replaced. The final line needs no newline.
func eachLine(r io.Reader, max int, fn func(line []byte, tooLong bool)) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var buf []byte
	tooLong := false
	for {
		chunk, err := br.ReadSlice('\n')
		if !tooLong {
			n := len(buf) + len(chunk)
			switch {
			case n > max:
				tooLong, buf = true, buf[:0]
			case n > cap(buf):
				// Double (not append's slower growth) so a line near max
				// costs about 2*max of allocation in all.
				nb := make([]byte, len(buf), min(max, 2*n))
				copy(nb, buf)
				buf = append(nb, chunk...)
			default:
				buf = append(buf, chunk...)
			}
		}
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case err == nil || errors.Is(err, io.EOF):
			if tooLong {
				fn(nil, true)
			} else if len(buf) > 0 {
				if !utf8.Valid(buf) {
					buf = bytes.ToValidUTF8(buf, []byte("�"))
				}
				fn(buf, false)
			}
			buf, tooLong = buf[:0], false
			if err != nil {
				return nil
			}
		default:
			return err
		}
	}
}
