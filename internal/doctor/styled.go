package doctor

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/mrqwer/cairn/internal/ui"
)

// groupTitles maps a check ID prefix (the part before the first dot) to the
// section heading it is shown under.
var groupTitles = map[string]string{
	"host":  "Policy",
	"sleep": "Sleep and hibernation",
	"gpu":   "Graphics",
}

func groupOf(id string) string {
	prefix, _, _ := strings.Cut(id, ".")
	if t, ok := groupTitles[prefix]; ok {
		return t
	}
	return "Other"
}

func symbol(st ui.Style, s Status) string {
	switch s {
	case OK:
		return st.Green("✓")
	case Warn:
		return st.Yellow("!")
	case Crit:
		return st.Red("✗")
	default:
		return st.Dim("?")
	}
}

// RenderStyled prints results for a terminal: grouped by area, one aligned
// line per check, the fix on its own line, and a summary at the end.
func RenderStyled(w io.Writer, rs []Result, st ui.Style, heading string) {
	titleW := 0
	for _, r := range rs {
		titleW = max(titleW, utf8.RuneCountInString(r.Title))
	}
	titleW = min(titleW, 56, st.Width/2)
	evidenceW := st.Width - titleW - 9

	_, _ = fmt.Fprintf(w, "\n %s\n", st.Bold(heading))
	var order []string
	groups := map[string][]Result{}
	for _, r := range rs {
		g := groupOf(r.ID)
		if _, ok := groups[g]; !ok {
			order = append(order, g)
		}
		groups[g] = append(groups[g], r)
	}
	counts := map[Status]int{}
	for _, g := range order {
		_, _ = fmt.Fprintf(w, "\n %s\n", st.Dim(strings.ToUpper(g)))
		for _, r := range groups[g] {
			counts[r.Status]++
			title := ui.Pad(ui.Truncate(r.Title, titleW), titleW)
			if r.Status == Crit {
				title = st.Bold(title)
			}
			lines := ui.Wrap(r.Evidence, evidenceW)
			first := ""
			if len(lines) > 0 {
				first = lines[0]
			}
			_, _ = fmt.Fprintln(w, strings.TrimRight(fmt.Sprintf("  %s  %s   %s", symbol(st, r.Status), title, st.Dim(first)), " "))
			for _, l := range lines[min(1, len(lines)):] {
				_, _ = fmt.Fprintf(w, "     %s   %s\n", strings.Repeat(" ", titleW), st.Dim(l))
			}
			if r.Status != OK && r.Fix != "" {
				for i, l := range ui.Wrap(r.Fix, st.Width-10) {
					arrow := "→"
					if i > 0 {
						arrow = " "
					}
					_, _ = fmt.Fprintf(w, "     %s %s\n", st.Cyan(arrow), st.Cyan(l))
				}
			}
		}
	}

	var parts []string
	if n := counts[Crit]; n > 0 {
		parts = append(parts, st.Red(ui.Plural(n, "problem", "problems")))
	}
	if n := counts[Warn]; n > 0 {
		parts = append(parts, st.Yellow(ui.Plural(n, "warning", "warnings")))
	}
	if n := counts[Unknown]; n > 0 {
		parts = append(parts, st.Dim(ui.Plural(n, "not checked", "not checked")))
	}
	parts = append(parts, st.Green(fmt.Sprintf("%d ok", counts[OK])))
	_, _ = fmt.Fprintf(w, "\n %s\n\n", strings.Join(parts, st.Dim(" · ")))
}
