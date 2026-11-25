package index

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
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
	Truncated                  bool
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

func (claudeParser) Parse(r io.Reader) (Transcript, error) { return parseClaude(r, nil) }

// ParseHead is Parse that stops after the line during which limit bytes of
// r were consumed, once the session id, cwd and start time are known:
// enough to import a conversation without reading all of it.
func (claudeParser) ParseHead(r io.Reader, limit int64) (Transcript, error) {
	cr := &countingReader{r: r}
	return parseClaude(cr, func(tr *Transcript) bool {
		return cr.n >= limit && tr.NativeID != "" && tr.Cwd != "" && !tr.StartedAt.IsZero()
	})
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// toolWrappers start user "messages" that tools inject (slash commands,
// local command output, reminders); they are not conversation text.
var toolWrappers = []string{"<command-", "<local-command-", "<system-reminder", "<user-prompt-submit-hook",
	"<bash-input", "<bash-stdout", "<bash-stderr", "<task-notification"}

func isToolWrapper(text string) bool {
	for _, w := range toolWrappers {
		if strings.HasPrefix(text, w) {
			return true
		}
	}
	return false
}

// Metadata of a line too long to decode, read from its head and tail.
var (
	headType = regexp.MustCompile(`"type":"(user|assistant)"`)
	headSID  = regexp.MustCompile(`"sessionId":"([^"\\]*)"`)
	headCwd  = regexp.MustCompile(`"cwd":("(?:[^"\\]|\\.)*")`)
	headTime = regexp.MustCompile(`"timestamp":"([^"\\]*)"`)
)

func (tr *Transcript) noteTime(at time.Time) {
	if at.IsZero() {
		return
	}
	if tr.StartedAt.IsZero() || at.Before(tr.StartedAt) {
		tr.StartedAt = at
	}
	if at.After(tr.EndedAt) {
		tr.EndedAt = at
	}
}

func (tr *Transcript) noteIDs(sid, cwd string) {
	if tr.NativeID == "" {
		tr.NativeID = sid
	}
	if tr.Cwd == "" {
		tr.Cwd = cwd
	}
}

// parseClaude reads r line by line; stop, when set, ends the read early.
// Base64 image data is dropped while streaming, so a pasted screenshot
// does not make its line too long; a line still over MaxLine is skipped
// but its head and tail give the session id, cwd and time.
func parseClaude(r io.Reader, stop func(*Transcript) bool) (Transcript, error) {
	var tr Transcript
	valid, text := 0, 0
	err := eachLine(newImageDropper(r), MaxLine, func(b []byte, tooLong bool, edge []byte) bool {
		if tooLong {
			tr.Skipped++
			if headType.Match(edge) {
				valid++
				var sid, cwd string
				if m := headSID.FindSubmatch(edge); m != nil {
					sid = string(m[1])
				}
				if m := headCwd.FindSubmatch(edge); m != nil {
					_ = json.Unmarshal(m[1], &cwd)
				}
				tr.noteIDs(sid, cwd)
				if m := headTime.FindSubmatch(edge); m != nil {
					at, _ := time.Parse(time.RFC3339Nano, string(m[1]))
					tr.noteTime(at)
				}
			}
			return stop == nil || !stop(&tr)
		}
		b = bytes.TrimSpace(b)
		if len(b) == 0 {
			return true
		}
		var l claudeLine
		if json.Unmarshal(b, &l) != nil || l.Type == "" {
			tr.Skipped++
			return true
		}
		valid++
		if l.Type != "user" && l.Type != "assistant" {
			return true
		}
		tr.noteIDs(l.SessionID, l.Cwd)
		at, _ := time.Parse(time.RFC3339Nano, l.Timestamp)
		tr.noteTime(at)
		if !l.IsMeta && len(l.Message) > 0 {
			var m claudeMessage
			if json.Unmarshal(l.Message, &m) == nil {
				t := strings.TrimSpace(claudeText(m.Content))
				role := m.Role
				if role == "" {
					role = l.Type
				}
				if t != "" && !isToolWrapper(t) {
					if tr.FirstPrompt == "" && role == "user" {
						tr.FirstPrompt = t
					}
					// Keep at most MaxText of text in memory; the times
					// still cover the whole transcript.
					if text+len(t) > MaxText {
						tr.Truncated = true
					} else {
						text += len(t)
						tr.Messages = append(tr.Messages, Message{Role: role, Text: t, At: at})
					}
				}
			}
		}
		return stop == nil || !stop(&tr)
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

// edgeSize is how much of the head and of the tail of an over-long line
// eachLine keeps for metadata.
const edgeSize = 64 << 10

// eachLine calls fn with each line of r until fn returns false. A line
// longer than max is passed as tooLong with no content, and never held in
// memory beyond max bytes; edge then holds its first and last edgeSize
// bytes. An invalid UTF-8 sequence is replaced. The final line needs no
// newline.
func eachLine(r io.Reader, max int, fn func(line []byte, tooLong bool, edge []byte) bool) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var buf, head, tail []byte
	tooLong := false
	for {
		chunk, err := br.ReadSlice('\n')
		if !tooLong {
			n := len(buf) + len(chunk)
			switch {
			case n > max:
				tooLong = true
				head = append(head[:0], buf[:min(len(buf), edgeSize)]...)
				buf = buf[:0]
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
		if tooLong {
			tail = append(tail, chunk...)
			if len(tail) > 2*edgeSize {
				tail = append(tail[:0], tail[len(tail)-edgeSize:]...)
			}
		}
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case err == nil || errors.Is(err, io.EOF):
			more := true
			if tooLong {
				if len(tail) > edgeSize {
					tail = tail[len(tail)-edgeSize:]
				}
				more = fn(nil, true, append(append(head, '\n'), tail...))
			} else if len(buf) > 0 {
				if !utf8.Valid(buf) {
					buf = bytes.ToValidUTF8(buf, []byte("\uFFFD"))
				}
				more = fn(buf, false, nil)
			}
			buf, head, tail, tooLong = buf[:0], head[:0], tail[:0], false
			if err != nil || !more {
				return nil
			}
		default:
			return err
		}
	}
}

// imageMinRun is how long a base64 run after "data": must be before it is
// treated as image data and dropped.
const imageMinRun = 256

// imageDropper passes a JSONL stream through, emptying long base64 values
// of "data" members (pasted images). A literal "data": inside a JSON
// string is always escaped (\"data\":), so only real members match.
type imageDropper struct {
	r     *bufio.Reader
	out   []byte
	pend  []byte
	k     int  // bytes of `"data":` matched
	state int  // 0 scanning, 1 after `"data":`, 2 inside the value
	drop  bool // the current value is being dropped
	err   error
}

const dataKey = `"data":`

func newImageDropper(r io.Reader) *imageDropper {
	return &imageDropper{r: bufio.NewReaderSize(r, 64<<10)}
}

func (d *imageDropper) Read(p []byte) (int, error) {
	for len(d.out) < len(p) && d.err == nil {
		b, err := d.r.ReadByte()
		if err != nil {
			d.err = err
			if d.state == 2 && !d.drop {
				d.out = append(d.out, d.pend...)
			}
			break
		}
		d.feed(b)
	}
	if len(d.out) == 0 {
		return 0, d.err
	}
	n := copy(p, d.out)
	d.out = append(d.out[:0], d.out[n:]...)
	return n, nil
}

func base64Byte(b byte) bool {
	return b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '+' || b == '/' || b == '='
}

func (d *imageDropper) feed(b byte) {
	switch d.state {
	case 2:
		if base64Byte(b) {
			if d.drop {
				return
			}
			d.pend = append(d.pend, b)
			if len(d.pend) > imageMinRun {
				d.drop, d.pend = true, d.pend[:0]
			}
			return
		}
		if !d.drop {
			d.out = append(d.out, d.pend...)
		}
		d.pend, d.drop, d.state = d.pend[:0], false, 0
	case 1:
		d.out = append(d.out, b)
		switch b {
		case ' ':
		case '"':
			d.state = 2
		default:
			d.state = 0
		}
		return
	}
	d.out = append(d.out, b)
	switch b {
	case dataKey[d.k]:
		d.k++
		if d.k == len(dataKey) {
			d.state, d.k = 1, 0
		}
	case '"':
		d.k = 1
	default:
		d.k = 0
	}
}
