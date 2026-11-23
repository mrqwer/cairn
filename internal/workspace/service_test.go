package workspace

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrqwer/cairn/internal/execx"
	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/inventory"
	"github.com/mrqwer/cairn/internal/plan"
)

var update = flag.Bool("update", false, "rewrite golden files")

const (
	testSelf    = "/usr/local/bin/cairn"
	testUnitDir = "/home/u/.config/systemd/user"
)

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", "service", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != string(got) {
		t.Errorf("%s mismatch\n--- want\n%s\n--- got\n%s", name, want, got)
	}
}

func fileContent(t *testing.T, acts []plan.Action, base string) []byte {
	t.Helper()
	for _, a := range acts {
		if a.Kind == plan.WriteFile && filepath.Base(a.Path) == base {
			return a.Content
		}
	}
	t.Fatalf("no WriteFile for %s", base)
	return nil
}

func TestServiceGolden(t *testing.T) {
	for _, tc := range []struct{ name, socket string }{{"default", ""}, {"socket", "work"}} {
		acts := ServiceActions(testSelf, tc.socket, testUnitDir)
		golden(t, "cairn-tmux."+tc.name+".service.golden", fileContent(t, acts, "cairn-tmux.service"))
		golden(t, "cairn-up."+tc.name+".service.golden", fileContent(t, acts, "cairn-up.service"))
	}
}

func TestServiceActionsShape(t *testing.T) {
	acts := ServiceActions(testSelf, "", testUnitDir)
	var enables []string
	reload := -1
	for i, a := range acts {
		if err := plan.CheckUserAction(a); err != nil {
			t.Errorf("action %d rejected: %v", i, err)
		}
		if a.NeedsRoot || a.Policy == "" && a.Kind != plan.RunCommand {
			t.Errorf("action %d: bad NeedsRoot/Policy: %+v", i, a)
		}
		switch a.Kind {
		case plan.WriteFile:
			if a.Class != inventory.DropIn || a.Policy != "workspace" || !strings.HasPrefix(string(a.Content), fsx.Header("workspace")) {
				t.Errorf("bad file action %+v", a)
			}
		case plan.EnableUnit:
			enables = append(enables, a.Unit)
			if !a.UserUnit || a.Class != inventory.UnitEnable || i < reload {
				t.Errorf("bad enable %+v", a)
			}
		case plan.RunCommand:
			reload = i
		}
	}
	if len(enables) != 2 || enables[0] != "cairn-tmux.service" || enables[1] != "cairn-up.service" || reload < 0 {
		t.Errorf("enables %v reload %d", enables, reload)
	}
	def := string(fileContent(t, acts, "cairn-tmux.service"))
	if strings.Contains(def, "-L") || strings.Contains(def, "ExecStop") {
		t.Error("default socket must not pass -L nor kill the default server")
	}
	sock := string(fileContent(t, ServiceActions(testSelf, "work", testUnitDir), "cairn-tmux.service"))
	if !strings.Contains(sock, "ExecStop=/usr/bin/env tmux -L work kill-server") {
		t.Error("explicit socket keeps kill-server")
	}
}

func TestServiceTmuxUnitStartIsIdempotentAndLoadsUserConfig(t *testing.T) {
	for _, sock := range []string{"", "work"} {
		unit := string(fileContent(t, ServiceActions(testSelf, sock, testUnitDir), "cairn-tmux.service"))
		if strings.Contains(unit, "-f ") || strings.Contains(unit, "/dev/null new-session") {
			t.Errorf("socket %q: the user's tmux.conf must load, no -f:\n%s", sock, unit)
		}
		l := "tmux"
		if sock != "" {
			l = "tmux -L " + sock
		}
		want := "ExecStart=/bin/sh -c '/usr/bin/env " + l + " has-session -t =cairn-base 2>/dev/null || exec /usr/bin/env " + l + " new-session -d -s cairn-base'\n"
		if !strings.Contains(unit, want) {
			t.Errorf("socket %q: want line %q in\n%s", sock, want, unit)
		}
		if !strings.Contains(unit, "Type=oneshot\nRemainAfterExit=yes\n") {
			t.Errorf("socket %q: a start that may exit with nothing to fork needs oneshot+RemainAfterExit:\n%s", sock, unit)
		}
		for _, line := range strings.Split(unit, "\n") {
			if strings.HasPrefix(line, "ExecStart") && strings.ContainsAny(line, "$%;`\"") {
				t.Errorf("systemd or shell metacharacter in %q", line)
			}
		}
	}
}

func TestServiceRejectsUnsafeInput(t *testing.T) {
	for _, self := range []string{"/opt/my cairn/cairn", "/opt/c%h/cairn", "/o/c\"x", "/o/$x", "/o/a;b", "/o/`x`", "/o/a'b", "/o/a\\b", "relative/cairn", ""} {
		if CheckServiceInput(self, "") == nil || ServiceActions(self, "", testUnitDir) != nil {
			t.Errorf("self %q accepted", self)
		}
	}
	for _, sock := range []string{"a b", "a%b", "a/b", "$x", "a;b", ".", "..", "-x", ".hidden"} {
		if CheckServiceInput(testSelf, sock) == nil || ServiceActions(testSelf, sock, testUnitDir) != nil {
			t.Errorf("socket %q accepted", sock)
		}
	}
}

func TestParseUnitState(t *testing.T) {
	for in, want := range map[string]string{
		"enabled\n": "enabled", "disabled\n": "disabled", "not-found\n": "not-found",
		"active\n": "active", "inactive\n": "inactive", "failed\n": "failed",
		"activating\n": "activating", "": "unknown", "Failed to connect to bus\n": "unknown",
		"static\n": "static", "masked\n": "masked",
	} {
		if got := ParseUnitState([]byte(in)); got != want {
			t.Errorf("ParseUnitState(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestQueryUnits(t *testing.T) {
	dir := t.TempDir()
	root := fsx.Root{Dir: dir}
	if err := root.WriteAtomic(testUnitDir+"/cairn-tmux.service", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	exit1 := errors.New("exit status 1")
	r := &execx.FakeRunner{Responses: map[string]execx.Response{
		"systemctl --user is-enabled cairn-tmux.service": {Out: []byte("enabled\n")},
		"systemctl --user is-active cairn-tmux.service":  {Out: []byte("failed\n"), Err: exit1},
		"systemctl --user is-enabled cairn-up.service":   {Out: []byte("not-found\n"), Err: exit1},
		"systemctl --user is-active cairn-up.service":    {Out: []byte("inactive\n"), Err: exit1},
	}}
	got := QueryUnits(context.Background(), r, root, testUnitDir)
	want := []UnitStatus{
		{Unit: "cairn-tmux.service", File: true, Enabled: "enabled", Active: "failed"},
		{Unit: "cairn-up.service", File: false, Enabled: "not-found", Active: "inactive"},
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("got %+v want %+v", got, want)
	}
}

func TestServiceSocketErrorWording(t *testing.T) {
	err := CheckServiceInput(testSelf, "a b")
	if err == nil || strings.Contains(err.Error(), "binary path") || !strings.Contains(err.Error(), "socket name") {
		t.Errorf("err = %v", err)
	}
}

func TestRecordedSocket(t *testing.T) {
	for _, sock := range []string{"", "work"} {
		acts := ServiceActions(testSelf, sock, testUnitDir)
		got := RecordedSocket(fileContent(t, acts, "cairn-tmux.service"))
		if got != sock {
			t.Errorf("RecordedSocket(%q unit) = %q", sock, got)
		}
	}
}
