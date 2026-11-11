package fsx_test

import (
	"os"
	"path/filepath"
	"strings"
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
