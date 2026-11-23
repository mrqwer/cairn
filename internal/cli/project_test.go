package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/history"
	"github.com/mrqwer/cairn/internal/workspace"
	"github.com/mrqwer/cairn/internal/xdg"
)

func testDirs(t *testing.T) xdg.Dirs {
	t.Helper()
	d, err := xdg.Resolve(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func projectPath(t *testing.T, id string) string {
	return filepath.Join(testDirs(t).Config, "projects", id+".toml")
}

func histEvents(t *testing.T) []history.Event {
	t.Helper()
	d := testDirs(t)
	evs, err := history.Store{Root: fsx.Root{Dir: "/"}, Path: filepath.Join(d.State, "history.jsonl")}.List(0)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func TestProjectAddWritesValidFileAndRecordsHistory(t *testing.T) {
	dir, _ := wsTestEnv(t)
	out := mustRun(t, "", "project", "add", "demo", "--root", dir, "--template", "dev", "--name", "Demo app")
	if !strings.Contains(out, "demo") {
		t.Fatalf("add output:\n%s", out)
	}
	ps, lerrs, err := workspace.Load(fsx.Root{Dir: "/"}, testDirs(t))
	if err != nil || len(lerrs) != 0 || len(ps) != 1 {
		t.Fatalf("load: %v %v %v", ps, lerrs, err)
	}
	if p := ps[0]; p.ID != "demo" || p.Name != "Demo app" || p.Root != dir || len(p.Windows) != 2 {
		t.Fatalf("project: %+v", p)
	}
	data, _ := os.ReadFile(projectPath(t, "demo"))
	evs := histEvents(t)
	if len(evs) != 1 || evs[0].Kind != "project.add" || len(evs[0].Files) != 1 ||
		evs[0].Files[0].Path != projectPath(t, "demo") || string(evs[0].Files[0].After) != string(data) ||
		evs[0].Files[0].BeforeExists || !evs[0].Files[0].AfterExists {
		t.Fatalf("history: %+v", evs)
	}

	out, err = runHost(t, "", "project", "add", "demo", "--root", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate add: %v\n%s", err, out)
	}
	if again, _ := os.ReadFile(projectPath(t, "demo")); string(again) != string(data) || len(histEvents(t)) != 1 {
		t.Fatal("duplicate add changed the file or history")
	}
}

func TestProjectAddDefaultsRootToCwdAndRejectsBadInput(t *testing.T) {
	dir, _ := wsTestEnv(t)
	wd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "", "project", "add", "web")
	ps, _, _ := workspace.Load(fsx.Root{Dir: "/"}, testDirs(t))
	if len(ps) != 1 || ps[0].Root != dir || ps[0].Name != "web" {
		t.Fatalf("projects: %+v", ps)
	}
	for _, bad := range [][]string{
		{"project", "add", "../evil"},
		{"project", "add", "a/b"},
		{"project", "add", "Upper"},
		{"project", "add", "x", "--root", "~other/dir"},
		{"project", "add", "y", "--template", "nope"},
	} {
		out, err := runHost(t, "", bad...)
		if err == nil {
			t.Fatalf("%v accepted:\n%s", bad, out)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(testDirs(t).Config, "projects"))
	if len(entries) != 1 {
		t.Fatalf("files written: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(testDirs(t).Config, "evil.toml")); err == nil {
		t.Fatal("path escaped the projects directory")
	}
}

func TestProjectEditInvalidRestoresAndValidRecords(t *testing.T) {
	dir, _ := wsTestEnv(t)
	addProject(t, "demo", dir)
	path := projectPath(t, "demo")
	before, _ := os.ReadFile(path)
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "myeditor --wait")

	var argv []string
	write := func(content string) {
		runEditor = func(a []string) error {
			argv = a
			return os.WriteFile(a[len(a)-1], []byte(content), 0o644)
		}
	}

	write(strings.Replace(string(before), `root =`, "status = \"bogus\"\nroot =", 1))
	out, err := runHost(t, "", "project", "edit", "demo")
	if exitCode(err) != 1 || !strings.Contains(out, path) || !strings.Contains(out, "status") {
		t.Fatalf("invalid edit: %v\n%s", err, out)
	}
	if strings.Join(argv, " ") != "myeditor --wait "+path {
		t.Fatalf("editor argv = %v", argv)
	}
	if got, _ := os.ReadFile(path); string(got) != string(before) {
		t.Fatalf("not restored:\n%s", got)
	}

	write("id = [")
	if out, err := runHost(t, "", "project", "edit", "demo"); exitCode(err) != 1 || !strings.Contains(out, path) {
		t.Fatalf("broken toml: %v\n%s", err, out)
	}
	if got, _ := os.ReadFile(path); string(got) != string(before) {
		t.Fatalf("not restored:\n%s", got)
	}
	if len(histEvents(t)) != 1 {
		t.Fatal("invalid edits recorded")
	}

	after := strings.Replace(string(before), `root =`, "tags = [\"x\"]\nroot =", 1)
	write(after)
	mustRun(t, "", "project", "edit", "demo")
	evs := histEvents(t)
	if len(evs) != 2 || evs[0].Kind != "project.edit" || string(evs[0].Files[0].Before) != string(before) ||
		string(evs[0].Files[0].After) != after {
		t.Fatalf("history: %+v", evs)
	}

	t.Setenv("VISUAL", "vis")
	write(after)
	mustRun(t, "", "project", "edit", "demo")
	if argv[0] != "vis" {
		t.Fatalf("VISUAL not preferred: %v", argv)
	}
	if len(histEvents(t)) != 2 {
		t.Fatal("unchanged edit recorded")
	}
}

func TestProjectArchiveRmUndoChain(t *testing.T) {
	dir, fd := wsTestEnv(t)
	addProject(t, "demo", dir)
	path := projectPath(t, "demo")
	added, _ := os.ReadFile(path)

	out, err := runHost(t, "", "project", "rm", "demo", "--yes")
	if err == nil || !strings.Contains(err.Error(), "cairn project archive demo") {
		t.Fatalf("rm active: %v\n%s", err, out)
	}
	mustRun(t, "", "project", "archive", "demo")
	data, _ := os.ReadFile(path)
	p, err := workspace.Decode(data)
	if err != nil || p.Status != workspace.StatusArchived {
		t.Fatalf("archived file: %v\n%s", err, data)
	}
	if len(fd.calls) != 0 {
		t.Fatalf("archive touched tmux: %v", fd.calls)
	}
	if out := mustRun(t, "", "project", "ls"); strings.Contains(out, "demo") {
		t.Fatalf("archived project listed without --all:\n%s", out)
	}
	if out := mustRun(t, "", "project", "ls", "--all"); !strings.Contains(out, "archived") {
		t.Fatalf("ls --all:\n%s", out)
	}

	mustRun(t, "", "project", "rm", "demo", "--yes")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file still there: %v", err)
	}

	out = mustRun(t, "", "undo")
	if !strings.Contains(out, "Run: cairn apply") {
		t.Fatalf("undo output:\n%s", out)
	}
	if got, _ := os.ReadFile(path); string(got) != string(data) {
		t.Fatalf("undo of rm:\n%s", got)
	}
	// Next undo skips the undo event and the undone rm: it undoes the archive.
	mustRun(t, "", "undo")
	if got, _ := os.ReadFile(path); string(got) != string(added) {
		t.Fatalf("undo of archive:\n%s", got)
	}
	out = mustRun(t, "", "history")
	for _, want := range []string{"project.add", "project.archive", "project.rm", "undo"} {
		if !strings.Contains(out, want) {
			t.Fatalf("history lacks %q:\n%s", want, out)
		}
	}
}

func TestProjectResume(t *testing.T) {
	dir, _ := wsTestEnv(t)
	addProject(t, "demo", dir)
	mustRun(t, "", "project", "archive", "demo")
	mustRun(t, "", "project", "resume", "demo")
	data, _ := os.ReadFile(projectPath(t, "demo"))
	if p, err := workspace.Decode(data); err != nil || (p.Status != "" && p.Status != workspace.StatusActive) {
		t.Fatalf("resumed: %v\n%s", err, data)
	}
	if out := mustRun(t, "", "project", "resume", "demo"); !strings.Contains(out, "already") {
		t.Fatalf("second resume:\n%s", out)
	}
}

func TestUndoConflictSuggestsRestore(t *testing.T) {
	dir, _ := wsTestEnv(t)
	addProject(t, "demo", dir)
	path := projectPath(t, "demo")
	if err := os.WriteFile(path, []byte("# changed by hand\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runHost(t, "", "undo")
	if exitCode(err) != 1 || !strings.Contains(out, "cairn history restore") {
		t.Fatalf("undo conflict: %v\n%s", err, out)
	}
	if got, _ := os.ReadFile(path); string(got) != "# changed by hand\n" {
		t.Fatal("conflicting undo wrote the file")
	}

	id := histEvents(t)[0].ID
	out = mustRun(t, "", "history", "restore", id)
	if !strings.Contains(out, "Run: cairn apply") {
		t.Fatalf("restore:\n%s", out)
	}
	if got, _ := os.ReadFile(path); !strings.Contains(string(got), `id = "demo"`) {
		t.Fatalf("restored:\n%s", got)
	}
}

func TestUndoAlreadyUndoneAndNothingToUndo(t *testing.T) {
	dir, _ := wsTestEnv(t)
	if out := mustRun(t, "", "undo"); !strings.Contains(out, "nothing to undo") {
		t.Fatalf("empty undo:\n%s", out)
	}
	addProject(t, "demo", dir)
	id := histEvents(t)[0].ID
	mustRun(t, "", "undo", id)
	out, err := runHost(t, "", "undo", id)
	if err != nil || !strings.Contains(out, "already undone") {
		t.Fatalf("second undo: %v\n%s", err, out)
	}
}

func TestHistoryShowAndLimit(t *testing.T) {
	dir, _ := wsTestEnv(t)
	addProject(t, "api", dir)
	addProject(t, "web", dir)
	out := mustRun(t, "", "history", "-n", "1")
	if !strings.Contains(out, "web") || strings.Contains(out, "api") {
		t.Fatalf("history -n 1:\n%s", out)
	}
	id := histEvents(t)[1].ID // api
	out = mustRun(t, "", "history", "show", id)
	if !strings.Contains(out, `+ id = "api"`) || !strings.Contains(out, projectPath(t, "api")) {
		t.Fatalf("show:\n%s", out)
	}
}

func TestProjectCommandsRefuseSymlinkedFile(t *testing.T) {
	dir, _ := wsTestEnv(t)
	addProject(t, "demo", dir)
	path := projectPath(t, "demo")
	target := filepath.Join(dir, "elsewhere.toml")
	if err := os.Rename(path, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(target)
	for _, args := range [][]string{{"project", "edit", "demo"}, {"project", "archive", "demo"}, {"project", "resume", "demo"}} {
		out, err := runHost(t, "", args...)
		if err == nil || !strings.Contains(err.Error(), path+" is a symlink; edit the target directly") {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	if fi, err := os.Lstat(path); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink replaced: %v", err)
	}
	if got, _ := os.ReadFile(target); string(got) != string(before) {
		t.Fatal("target changed")
	}
}

func TestUndoAddRemovesFileAndRestoreBringsItBack(t *testing.T) {
	dir, _ := wsTestEnv(t)
	addProject(t, "demo", dir)
	path := projectPath(t, "demo")
	data, _ := os.ReadFile(path)
	addID := histEvents(t)[0].ID
	mustRun(t, "", "undo")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("undo of add left the file: %v", err)
	}
	mustRun(t, "", "history", "restore", addID)
	if got, _ := os.ReadFile(path); string(got) != string(data) {
		t.Fatalf("restore of add:\n%s", got)
	}
}
