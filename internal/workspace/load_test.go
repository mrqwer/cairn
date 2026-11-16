package workspace_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/workspace"
	"github.com/mrqwer/cairn/internal/xdg"
)

var dirs = xdg.Dirs{Home: "/home/u", Config: "/home/u/.config/cairn"}

// setup copies the named testdata files into a temp root's projects dir.
func setup(t *testing.T, files ...string) fsx.Root {
	t.Helper()
	root := fsx.Root{Dir: t.TempDir()}
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join("testdata", "projects", f))
		if err != nil {
			t.Fatal(err)
		}
		if err := root.WriteAtomic(filepath.Join(dirs.Config, "projects", f), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestLoadSortedAndStrict(t *testing.T) {
	root := setup(t, "web.toml", "demo.toml", "bad.toml")
	ps, lerrs, err := workspace.Load(root, dirs)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 || ps[0].ID != "demo" || ps[1].ID != "web" {
		t.Fatalf("projects = %+v", ps)
	}
	if len(lerrs) != 1 {
		t.Fatalf("load errors = %v", lerrs)
	}
	msg := lerrs[0].Error()
	if !strings.Contains(msg, "bad.toml") || !strings.Contains(msg, "windows.panes.splitt") {
		t.Fatalf("error %q should name file and key", msg)
	}
}

func TestMissingDirIsEmpty(t *testing.T) {
	ps, lerrs, err := workspace.Load(fsx.Root{Dir: t.TempDir()}, dirs)
	if err != nil || len(ps) != 0 || len(lerrs) != 0 {
		t.Fatalf("got %v %v %v", ps, lerrs, err)
	}
}

func TestIDMustMatchFilename(t *testing.T) {
	ps, lerrs, err := workspace.Load(setup(t, "mismatch.toml"), dirs)
	if err != nil || len(ps) != 0 || len(lerrs) != 1 {
		t.Fatalf("got %v %v %v", ps, lerrs, err)
	}
	if !strings.Contains(lerrs[0].Error(), "mismatch.toml") || !strings.Contains(lerrs[0].Error(), "other") {
		t.Fatalf("error = %v", lerrs[0])
	}
}

func TestDefaults(t *testing.T) {
	p := workspace.Project{
		ID: "demo", Root: "/home/u/demo",
		Windows: []workspace.Window{
			{Name: "Main Shell"},
			{Key: "svc", Panes: []workspace.Pane{{}, {Role: "service", Split: "v", Cwd: "sub"}}},
		},
	}
	p.ApplyDefaults("/home/u")
	if p.Session != "demo" || p.Status != "active" {
		t.Fatalf("session/status = %q %q", p.Session, p.Status)
	}
	w0 := p.Windows[0]
	if w0.Key != "main-shell" || len(w0.Panes) != 1 {
		t.Fatalf("window 0 = %+v", w0)
	}
	if pn := w0.Panes[0]; pn.Role != "shell" || pn.Cwd != "/home/u/demo" || pn.Restart != "never" {
		t.Fatalf("pane = %+v", pn)
	}
	w1 := p.Windows[1]
	if w1.Name != "svc" {
		t.Fatalf("name = %q", w1.Name)
	}
	if pn := w1.Panes[1]; pn.Restart != "on-failure" || pn.Cwd != "/home/u/demo/sub" || pn.From != 1 || pn.Size != 50 {
		t.Fatalf("service pane = %+v", pn)
	}
	if errs := p.Validate(); len(errs) != 0 {
		t.Fatalf("validate: %v", errs)
	}
}

func TestTilde(t *testing.T) {
	ps, lerrs, err := workspace.Load(setup(t, "web.toml"), dirs)
	if err != nil || len(lerrs) != 0 || len(ps) != 1 {
		t.Fatalf("got %v %v %v", ps, lerrs, err)
	}
	if ps[0].Root != "/home/u/web" {
		t.Fatalf("root = %q", ps[0].Root)
	}
	p := workspace.Project{Root: "~/code/x"}
	p.ApplyDefaults("/home/u")
	if p.Root != "/home/u/code/x" {
		t.Fatalf("root = %q", p.Root)
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	for _, name := range workspace.TemplateNames() {
		t.Run(name, func(t *testing.T) {
			p, _ := workspace.Template(name)
			p.ID, p.Name, p.Root = "demo", "Demo", "/home/u/demo"
			data, err := workspace.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			got, err := workspace.Decode(data)
			if err != nil {
				t.Fatalf("decode: %v\n%s", err, data)
			}
			if !reflect.DeepEqual(got, p) {
				t.Fatalf("round trip mismatch\n got %+v\nwant %+v\n%s", got, p, data)
			}
			again, _ := workspace.Marshal(got)
			if string(again) != string(data) {
				t.Fatal("marshal not deterministic")
			}
		})
	}
}

func TestMarshalOmitsDefaults(t *testing.T) {
	p, _ := workspace.Template("dev")
	p.ID, p.Name, p.Root = "demo", "Demo", "/home/u/demo"
	p.Tags = []string{"a", `b"c`}
	p.ApplyDefaults("/home/u")
	data, _ := workspace.Marshal(p)
	s := string(data)
	for _, bad := range []string{"session", "status", "role = \"shell\"", "restart", "cwd", "from", "size = 50"} {
		if strings.Contains(s, bad) {
			t.Fatalf("output contains default %q:\n%s", bad, s)
		}
	}
	if !strings.Contains(s, "size = 35") || !strings.Contains(s, `tags = ["a", "b\"c"]`) {
		t.Fatalf("unexpected output:\n%s", s)
	}
	got, err := workspace.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	got.ApplyDefaults("/home/u")
	if !reflect.DeepEqual(got, p) {
		t.Fatalf("defaulted round trip mismatch\n got %+v\nwant %+v", got, p)
	}
}

func TestTemplates(t *testing.T) {
	if got := workspace.TemplateNames(); !reflect.DeepEqual(got, []string{"dev", "service", "shell"}) {
		t.Fatalf("names = %v", got)
	}
	if _, ok := workspace.Template("nope"); ok {
		t.Fatal("unexpected template")
	}
	for _, name := range workspace.TemplateNames() {
		p, ok := workspace.Template(name)
		if !ok {
			t.Fatal(name)
		}
		p.ID, p.Name, p.Root = "demo", "Demo", "/home/u/demo"
		p.ApplyDefaults("/home/u")
		if errs := p.Validate(); len(errs) != 0 {
			t.Fatalf("%s: %v", name, errs)
		}
	}
}
