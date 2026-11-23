package fsx_test

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/mrqwer/cairn/internal/fsx"
)

func TestWriteAtomicReplacesContent(t *testing.T) {
	r := fsx.Root{Dir: t.TempDir()}
	if err := r.WriteAtomic("/etc/x.conf", []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteAtomic("/etc/x.conf", []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := r.ReadFile("/etc/x.conf")
	if err != nil || string(got) != "b" {
		t.Fatalf("got %q, %v", got, err)
	}
	entries, err := os.ReadDir(filepath.Join(r.Dir, "etc"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("leftover temp file %s", e.Name())
		}
	}
	if !r.Exists("/etc/x.conf") {
		t.Fatal("expected Exists")
	}
}

func TestRemoveMissingIsNotError(t *testing.T) {
	r := fsx.Root{Dir: t.TempDir()}
	if err := r.Remove("/nope"); err != nil {
		t.Fatal(err)
	}
}

func TestPathStaysInsideRoot(t *testing.T) {
	r := fsx.Root{Dir: "/tmp/r"}
	if got := r.Path("/../../etc/x"); got != "/tmp/r/etc/x" {
		t.Fatalf("got %s", got)
	}
	if got := (fsx.Root{Dir: "/"}).Path("/etc/x"); got != "/etc/x" {
		t.Fatalf("got %s", got)
	}
}

func TestHeader(t *testing.T) {
	want := "# Managed by cairn host (sleep). Do not edit; run \"cairn host ...\"\n"
	if got := fsx.Header("sleep"); got != want {
		t.Fatalf("got %q", got)
	}
}

func TestSHA256(t *testing.T) {
	if got := fsx.SHA256([]byte("")); got != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatal(got)
	}
}

func TestCreateExclusive(t *testing.T) {
	r := fsx.Root{Dir: t.TempDir()}
	if err := r.CreateExclusive("/a/b/x.toml", []byte("one"), 0o640); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.ReadFile("/a/b/x.toml"); string(got) != "one" {
		t.Fatalf("content %q", got)
	}
	if fi, _ := r.Stat("/a/b/x.toml"); fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v", fi.Mode())
	}
	err := r.CreateExclusive("/a/b/x.toml", []byte("two"), 0o644)
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second create: %v", err)
	}
	if got, _ := r.ReadFile("/a/b/x.toml"); string(got) != "one" {
		t.Fatalf("overwritten: %q", got)
	}
	entries, _ := os.ReadDir(r.Path("/a/b"))
	if len(entries) != 1 {
		t.Fatalf("temp files left: %v", entries)
	}
}

func TestCreateExclusiveRace(t *testing.T) {
	r := fsx.Root{Dir: t.TempDir()}
	for i := range 50 {
		p := fmt.Sprintf("/d/%d.toml", i)
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for g := range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[g] = r.CreateExclusive(p, []byte{byte('a' + g)}, 0o644)
			}()
		}
		wg.Wait()
		ok := 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case !errors.Is(err, fs.ErrExist):
				t.Fatalf("race %d: %v", i, err)
			}
		}
		if ok != 1 {
			t.Fatalf("race %d: %d winners (%v)", i, ok, errs)
		}
	}
	entries, _ := os.ReadDir(r.Path("/d"))
	if len(entries) != 50 {
		t.Fatalf("entries %d (temp files left?)", len(entries))
	}
}

func TestLstatSeesSymlink(t *testing.T) {
	r := fsx.Root{Dir: t.TempDir()}
	if err := r.WriteAtomic("/t", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(r.Path("/t"), r.Path("/l")); err != nil {
		t.Fatal(err)
	}
	if fi, err := r.Lstat("/l"); err != nil || fi.Mode()&fs.ModeSymlink == 0 {
		t.Fatalf("Lstat: %v %v", fi, err)
	}
}

func TestCreateExclusiveOnFilesystemWithoutHardLinks(t *testing.T) {
	for _, errno := range []error{syscall.EPERM, syscall.ENOTSUP, syscall.EOPNOTSUPP, syscall.EXDEV} {
		restore := fsx.SetLink(func(string, string) error { return &os.LinkError{Op: "link", Err: errno} })
		r := fsx.Root{Dir: t.TempDir()}
		err := r.CreateExclusive("/a/f", []byte("x"), 0o644)
		restore()
		if err == nil || !strings.Contains(err.Error(), "does not support hard links") || !errors.Is(err, errno) {
			t.Fatalf("%v: err %v", errno, err)
		}
		if errors.Is(err, fs.ErrExist) {
			t.Fatalf("%v: must not look like an existing file", errno)
		}
		entries, _ := os.ReadDir(r.Path("/a"))
		if len(entries) != 0 {
			t.Fatalf("%v: left files behind: %v", errno, entries)
		}
	}
	// An existing target keeps its ErrExist identity.
	r := fsx.Root{Dir: t.TempDir()}
	if err := r.CreateExclusive("/f", []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateExclusive("/f", []byte("2"), 0o644); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("err %v", err)
	}
}
