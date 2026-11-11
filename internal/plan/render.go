package plan

import (
	"fmt"
	"io"
	"text/tabwriter"
)

// Render prints pending actions grouped into "user-level" and "needs root",
// one line per action: state, kind, target, reason. Unchanged actions are
// summarised as a count per group.
func Render(w io.Writer, p Plan) {
	if len(p.Pending()) == 0 {
		_, _ = fmt.Fprintln(w, "nothing to do")
		return
	}
	for _, g := range []struct {
		title string
		root  bool
	}{{"user-level", false}, {"needs root", true}} {
		var pending []Action
		unchanged := 0
		for _, a := range p.Actions {
			switch {
			case a.NeedsRoot != g.root:
			case a.State == Unchanged:
				unchanged++
			default:
				pending = append(pending, a)
			}
		}
		if len(pending) == 0 {
			continue
		}
		_, _ = fmt.Fprintf(w, "%s:\n", g.title)
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		for _, a := range pending {
			reason := ""
			if a.Reason != "" {
				reason = "# " + a.Reason
			}
			_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", a.State, a.Kind, describe(a), reason)
		}
		_ = tw.Flush()
		if unchanged > 0 {
			_, _ = fmt.Fprintf(w, "  (%d unchanged)\n", unchanged)
		}
	}
}

func describe(a Action) string {
	switch a.Kind {
	case EnableUnit, DisableUnit:
		if a.UserUnit {
			return a.Unit + " (user)"
		}
		return a.Unit
	case SetGSetting:
		return a.Schema + " " + a.Key + " = " + a.Value
	default:
		return a.target()
	}
}
