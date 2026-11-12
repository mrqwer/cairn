package plan

import (
	"bytes"
	"strings"
	"testing"

	"github.com/mrqwer/cairn/internal/ui"
)

func TestRenderSection(t *testing.T) {
	p := Plan{Actions: []Action{
		{Kind: WriteFile, Path: "/home/u/.config/systemd/user/x.service", State: Create},
		{Kind: WriteFile, Path: "/etc/a.conf", State: Foreign},
		{Kind: WriteFile, Path: "/etc/b.conf", State: ModifiedByUser},
		{Kind: EnableUnit, Unit: "x.service", State: Create, Reason: "why"},
		{Kind: RunCommand, Cmd: []string{"systemctl", "daemon-reload"}, State: Update},
		{Kind: WriteFile, Path: "/etc/same.conf", State: Unchanged},
	}}
	var b bytes.Buffer
	RenderSection(&b, p, ui.Style{Width: 100}, "your account", "", "/home/u")
	out := b.String()
	for _, want := range []string{
		"YOUR ACCOUNT · 5 changes",
		"+ file    ~/.config/systemd/user/x.service",
		"~ file    /etc/a.conf  replaces an existing file (backed up first)",
		"! file    /etc/b.conf  changed by hand — needs --force",
		"+ enable  x.service  why",
		"↻ run     systemctl daemon-reload",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "same.conf") {
		t.Errorf("unchanged action shown:\n%s", out)
	}

	b.Reset()
	RenderSection(&b, Plan{}, ui.Style{Width: 100}, "system", "", "")
	if !strings.Contains(b.String(), "nothing to do") {
		t.Errorf("empty section: %q", b.String())
	}
}
