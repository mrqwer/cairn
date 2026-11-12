package doctor

import (
	"bytes"
	"strings"
	"testing"

	"github.com/mrqwer/cairn/internal/ui"
)

func TestRenderStyledGroupsAndSummarises(t *testing.T) {
	var b bytes.Buffer
	RenderStyled(&b, []Result{
		{ID: "sleep.a", Title: "Swap is active", Status: OK, Evidence: "/swap/swapfile"},
		{ID: "sleep.b", Title: "Resume matches", Status: Crit, Evidence: "expected 1, kernel has 2", Fix: "sudo cairn host repair"},
		{ID: "gpu.a", Title: "Units enabled", Status: Warn, Fix: "cairn host apply"},
		{ID: "x.a", Title: "Odd one", Status: Unknown},
	}, ui.Style{Width: 100}, "cairn doctor · host")
	out := b.String()
	for _, want := range []string{
		"cairn doctor · host",
		"SLEEP AND HIBERNATION", "GRAPHICS", "OTHER",
		"✓  Swap is active", "✗  Resume matches", "→ sudo cairn host repair", "!  Units enabled", "?  Odd one",
		"1 problem · 1 warning · 1 not checked · 1 ok",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b[") {
		t.Errorf("colour codes in a no-colour style:\n%q", out)
	}
	if i, j := strings.Index(out, "SLEEP"), strings.Index(out, "GRAPHICS"); i > j {
		t.Errorf("groups not in first-seen order")
	}
}
