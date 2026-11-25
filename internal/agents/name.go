package agents

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"unicode"
)

var fillers = []string{
	"please ", "can you ", "could you ", "would you ", "i want you to ", "i want to ",
	"i need you to ", "i need to ", "we need to ", "help me ", "let's ", "lets ",
}

var stopwords = map[string]bool{
	"a": true, "an": true, "the": true, "to": true, "of": true, "in": true, "on": true,
	"for": true, "and": true, "with": true, "my": true, "our": true, "this": true,
	"that": true, "is": true, "be": true, "it": true, "at": true, "by": true, "from": true,
}

const (
	nameWords  = 5
	purposeMax = 100
)

// SuggestName proposes a conversation name and purpose from its first
// prompt with a local heuristic (spec §9.6): the first line without polite
// filler; the purpose is its first sentence, the name its first significant
// words in lower case.
func SuggestName(prompt string) (name, purpose string) {
	line := strings.TrimSpace(prompt)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	for changed := true; changed; {
		changed = false
		for _, f := range fillers {
			if len(line) >= len(f) && strings.EqualFold(line[:len(f)], f) {
				line, changed = strings.TrimSpace(line[len(f):]), true
			}
		}
	}
	purpose = line
	for _, sep := range []string{". ", "! ", "? "} {
		if i := strings.Index(purpose, sep); i >= 0 {
			purpose = purpose[:i+1]
		}
	}
	purpose = strings.TrimSuffix(purpose, ".")
	if r := []rune(purpose); len(r) > purposeMax {
		purpose = strings.TrimSpace(string(r[:purposeMax]))
	}
	var words []string
	for _, w := range strings.FieldsFunc(strings.ToLower(purpose), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if !stopwords[w] {
			words = append(words, w)
		}
		if len(words) == nameWords {
			break
		}
	}
	return strings.Join(words, " "), purpose
}

// FirstPrompt returns the first user prompt of a JSONL transcript. It is
// tolerant: unparsable lines are skipped, a message may be {"role","content"}
// or {"type":"user","message":{...}}, content a string or a list of text
// parts, and tool wrappers ("<command-name>...") are not prompts.
func FirstPrompt(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		if inner, ok := m["message"].(map[string]any); ok {
			m = inner
		}
		if role, _ := m["role"].(string); role != "user" {
			continue
		}
		text := strings.TrimSpace(contentText(m["content"]))
		if text == "" || strings.HasPrefix(text, "<") {
			continue
		}
		return text, nil
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", errors.New("no user prompt in the transcript")
}

func contentText(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, p := range c {
			if m, ok := p.(map[string]any); ok {
				if t, ok := m["text"].(string); ok && (m["type"] == nil || m["type"] == "text") {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}
