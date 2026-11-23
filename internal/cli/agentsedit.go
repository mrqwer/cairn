package cli

import (
	"bytes"
	"strconv"
	"strings"

	"github.com/mrqwer/cairn/internal/workspace"
)

// Text edits of a project file for agent pane bindings. Like withStatus,
// the file is edited as text (comments and layout kept) and the result is
// accepted only when it decodes to exactly the intended project; otherwise
// the caller falls back to workspace.Marshal and says so.

// tomlBlock is one table of the file: the header line and the end of its
// content (exclusive), not counting trailing blank and comment lines, which
// belong to whatever follows.
type tomlBlock struct {
	header     string
	start, end int
}

func scanBlocks(lines []string) []tomlBlock {
	var bs []tomlBlock
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "[") {
			bs = append(bs, tomlBlock{header: strings.TrimSpace(l), start: i, end: i + 1})
			continue
		}
		if len(bs) == 0 {
			continue
		}
		if t := strings.TrimSpace(l); t != "" && !strings.HasPrefix(t, "#") {
			bs[len(bs)-1].end = i + 1
		}
	}
	return bs
}

// windowText is one [[windows]] table and its [[windows.panes]] tables.
type windowText struct {
	head  tomlBlock
	panes []tomlBlock
}

// windowsOf groups the blocks by window; ok is false for a layout this
// editor does not handle (another kind of table inside a window).
func windowsOf(bs []tomlBlock) ([]windowText, bool) {
	var ws []windowText
	for _, b := range bs {
		switch {
		case b.header == "[[windows]]":
			ws = append(ws, windowText{head: b})
		case b.header == "[[windows.panes]]":
			if len(ws) == 0 {
				return nil, false
			}
			ws[len(ws)-1].panes = append(ws[len(ws)-1].panes, b)
		case strings.HasPrefix(b.header, "[windows") || strings.HasPrefix(b.header, "[[windows"):
			return nil, false
		}
	}
	return ws, true
}

func (w windowText) end() int {
	if len(w.panes) > 0 {
		return w.panes[len(w.panes)-1].end
	}
	return w.head.end
}

func paneBlock(pn workspace.Pane) []string {
	out := []string{"", "[[windows.panes]]", "role = " + strconv.Quote(pn.Role)}
	if pn.Split != "" {
		out = append(out, "split = "+strconv.Quote(pn.Split))
	}
	return append(out, "agent = "+strconv.Quote(pn.Agent))
}

func insertLines(lines []string, at int, add []string) []string {
	out := append([]string(nil), lines[:at]...)
	out = append(out, add...)
	return append(out, lines[at:]...)
}

// cutBlock removes lines [start, end) and one blank line before them.
func cutBlock(lines []string, start, end int) []string {
	if start > 0 && strings.TrimSpace(lines[start-1]) == "" {
		start--
	}
	return append(append([]string(nil), lines[:start]...), lines[end:]...)
}

// sameProject compares two raw projects by their canonical rendering.
func sameProject(a, b workspace.Project) bool {
	x, err1 := workspace.Marshal(a)
	y, err2 := workspace.Marshal(b)
	return err1 == nil && err2 == nil && bytes.Equal(x, y)
}

// accept returns text if it decodes to want.
func accept(text string, want workspace.Project) ([]byte, bool) {
	got, err := workspace.Decode([]byte(text))
	if err != nil || !sameProject(got, want) {
		return nil, false
	}
	return []byte(text), true
}

// addPaneText renders data with the agent pane that AddAgentPane appended to
// window wi of want (want is the edited project).
func addPaneText(data []byte, want workspace.Project, wi int) ([]byte, bool) {
	lines := strings.Split(string(data), "\n")
	ws, ok := windowsOf(scanBlocks(lines))
	if !ok {
		return nil, false
	}
	w := want.Windows[wi]
	pn := w.Panes[len(w.Panes)-1]
	switch {
	case wi < len(ws):
		lines = insertLines(lines, ws[wi].end(), paneBlock(pn))
	case wi == len(ws):
		// A new window: appended at the end of the file.
		for len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		lines = append(lines, "", "[[windows]]", "key = "+strconv.Quote(w.Key), "name = "+strconv.Quote(w.Name))
		lines = append(lines, paneBlock(pn)...)
		lines = append(lines, "")
	default:
		return nil, false
	}
	return accept(strings.Join(lines, "\n"), want)
}

// removePaneText removes pane pos (1-based) of window wi from data; when
// windowGone the (now empty) window table goes too. Later panes of the
// window get their split/from/size lines set to want's values.
func removePaneText(data []byte, want workspace.Project, wi, pos int, windowGone bool) ([]byte, bool) {
	lines := strings.Split(string(data), "\n")
	ws, ok := windowsOf(scanBlocks(lines))
	if !ok || wi >= len(ws) || pos < 1 || pos > len(ws[wi].panes) {
		return nil, false
	}
	w := ws[wi]
	if windowGone {
		if len(w.panes) != 1 {
			return nil, false
		}
		return accept(strings.Join(cutBlock(lines, w.head.start, w.end()), "\n"), want)
	}
	// Rewrite the later panes first (their line numbers stay valid), then
	// cut the removed block.
	var target workspace.Window
	for _, tw := range want.Windows {
		if tw.Key == keyOfWindow(lines, w) {
			target = tw
		}
	}
	for j := len(w.panes) - 1; j >= pos; j-- {
		np := j - 1 // index of this pane after the removal
		if np >= len(target.Panes) {
			return nil, false
		}
		lines = syncPaneKeys(lines, w.panes[j], target.Panes[np])
	}
	b := w.panes[pos-1]
	return accept(strings.Join(cutBlock(lines, b.start, b.end), "\n"), want)
}

// keyOfWindow reads the key = "..." line of a window table.
func keyOfWindow(lines []string, w windowText) string {
	for i := w.head.start + 1; i < w.head.end; i++ {
		k, v, ok := strings.Cut(lines[i], "=")
		if ok && strings.TrimSpace(k) == "key" {
			if s, err := strconv.Unquote(strings.TrimSpace(v)); err == nil {
				return s
			}
		}
	}
	return ""
}

var splitKeys = []string{"split", "from", "size"}

// syncPaneKeys makes the split/from/size lines of pane block b say what pn
// says (a zero value removes the line). Lines are replaced in place or
// blanked-and-dropped so the block's line range stays valid for callers
// working from the end.
func syncPaneKeys(lines []string, b tomlBlock, pn workspace.Pane) []string {
	want := map[string]string{}
	if pn.Split != "" {
		want["split"] = "split = " + strconv.Quote(pn.Split)
	}
	if pn.From != 0 {
		want["from"] = "from = " + strconv.Itoa(pn.From)
	}
	if pn.Size != 0 {
		want["size"] = "size = " + strconv.Itoa(pn.Size)
	}
	out := append([]string(nil), lines[:b.start+1]...)
	seen := map[string]bool{}
	for i := b.start + 1; i < b.end; i++ {
		k, _, ok := strings.Cut(lines[i], "=")
		key := strings.TrimSpace(k)
		isSplit := false
		for _, s := range splitKeys {
			isSplit = isSplit || (ok && key == s)
		}
		if !isSplit {
			out = append(out, lines[i])
			continue
		}
		seen[key] = true
		if v, keep := want[key]; keep {
			out = append(out, v)
		}
	}
	for _, s := range splitKeys {
		if v, ok := want[s]; ok && !seen[s] {
			out = append(out, v)
		}
	}
	return append(out, lines[b.end:]...)
}
