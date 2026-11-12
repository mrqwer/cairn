package plan

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/mrqwer/cairn/internal/ui"
)

var kindLabels = map[Kind]string{
	WriteFile:   "file",
	RemoveFile:  "remove",
	MakeDir:     "folder",
	EnableUnit:  "enable",
	DisableUnit: "disable",
	SetGSetting: "setting",
	RunCommand:  "run",
}

// mark returns the symbol and note for an action's kind and state.
func mark(st ui.Style, a Action) (string, string) {
	switch {
	case a.State == ModifiedByUser:
		return st.Red("!"), "changed by hand — needs --force"
	case a.Kind == RemoveFile || a.Kind == DisableUnit:
		return st.Red("-"), ""
	case a.Kind == RunCommand:
		return st.Dim("↻"), ""
	case a.State == Foreign:
		return st.Yellow("~"), "replaces an existing file (backed up first)"
	case a.State == Update:
		return st.Yellow("~"), ""
	default:
		return st.Green("+"), ""
	}
}

func styledTarget(a Action, home string) string {
	switch a.Kind {
	case EnableUnit, DisableUnit:
		return a.Unit
	case SetGSetting:
		return a.Key + " = " + a.Value
	case RunCommand:
		return strings.Join(a.Cmd, " ")
	}
	if home != "" && strings.HasPrefix(a.Path, home+"/") {
		return "~" + strings.TrimPrefix(a.Path, home)
	}
	return a.Path
}

// RenderSection prints the pending actions of p for a terminal under one
// heading: a symbol for what happens, the kind, the target and a short note.
// home shortens paths under the user's home directory to "~/…".
func RenderSection(w io.Writer, p Plan, st ui.Style, title, subtitle, home string) {
	pending := p.Pending()
	head := st.Dim(strings.ToUpper(title))
	if n := len(pending); n > 0 {
		head += st.Dim(" · " + ui.Plural(n, "change", "changes"))
	}
	if subtitle != "" {
		head += st.Dim(" · " + subtitle)
	}
	_, _ = fmt.Fprintf(w, "\n %s\n", head)
	if len(pending) == 0 {
		_, _ = fmt.Fprintf(w, "  %s  %s\n", st.Green("✓"), "nothing to do")
		return
	}
	const lead = 12 // columns before the target
	for _, a := range pending {
		sym, note := mark(st, a)
		if note == "" {
			note = a.Reason
		}
		target := ui.Truncate(styledTarget(a, home), st.Width-lead-1)
		line := fmt.Sprintf("  %s %s %s", sym, st.Dim(ui.Pad(kindLabels[a.Kind], 7)), target)
		if note != "" && lead+utf8.RuneCountInString(target)+2+utf8.RuneCountInString(note) <= st.Width {
			line += "  " + st.Dim(note)
			note = ""
		}
		_, _ = fmt.Fprintln(w, strings.TrimRight(line, " "))
		if note != "" {
			_, _ = fmt.Fprintf(w, "            %s\n", st.Dim(note))
		}
	}
}
