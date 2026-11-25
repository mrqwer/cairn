package testtool_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrqwer/cairn/internal/agents/testtool"
)

func run(t *testing.T, path string, wantExit int, args ...string) string {
	t.Helper()
	out, err := exec.Command(path, args...).Output()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	if code != wantExit {
		t.Fatalf("exit %d want %d", code, wantExit)
	}
	return string(out)
}

func TestWrite(t *testing.T) {
	dir := t.TempDir()
	p := testtool.Write(t, dir, testtool.Spec{Name: "fake", Version: "1.2.3", ListJSON: `[{"id":"x"}]`, ExitCode: 3})
	if out := run(t, p, 0, "--version"); strings.TrimSpace(out) != "1.2.3" {
		t.Fatalf("version %q", out)
	}
	if out := run(t, p, 0, "agents", "--json"); out != `[{"id":"x"}]` {
		t.Fatalf("list %q", out)
	}
	run(t, p, 3, "start", "a b")
	log, _ := os.ReadFile(filepath.Join(dir, "fake.log"))
	if !strings.Contains(string(log), "  a b\n") || !strings.Contains(string(log), "ENV:") {
		t.Fatalf("log %q", log)
	}
}

func TestWriteHelp(t *testing.T) {
	dir := t.TempDir()
	p := testtool.Write(t, dir, testtool.Spec{Name: "fake", Help: "Usage: fake [--model <m>]\n  --name <n>\n"})
	if out := run(t, p, 0, "agents", "--help"); out != "Usage: fake [--model <m>]\n  --name <n>\n" {
		t.Fatalf("help %q", out)
	}
	log, _ := os.ReadFile(filepath.Join(dir, "fake.log"))
	if !strings.Contains(string(log), "  agents\n  --help\n") {
		t.Fatalf("help call must be logged: %q", log)
	}
}
