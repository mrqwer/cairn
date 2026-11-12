package inventory

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/mrqwer/cairn/internal/fsx"
)

func TestRoundTrip(t *testing.T) {
	root := fsx.Root{Dir: t.TempDir()}
	inv := &Inventory{}
	inv.Upsert(Entry{Path: "/etc/b.conf", Policy: "sleep", Class: DropIn, SHA256: "bb"})
	inv.Upsert(Entry{Path: "/etc/a.conf", Policy: "sleep", Class: Mask, Upstream: "/usr/lib/a", UpstreamSHA256: "aa", Backup: "/var/b", Previous: "x"})
	if err := inv.Save(root, "/var/inv.json"); err != nil {
		t.Fatal(err)
	}
	got, err := Load(root, "/var/inv.json")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Entries, inv.Entries) {
		t.Fatalf("got %+v want %+v", got.Entries, inv.Entries)
	}
	if got.Entries[0].Path != "/etc/a.conf" {
		t.Fatalf("not sorted: %+v", got.Entries)
	}
	st, _ := os.Stat(root.Path("/var/inv.json"))
	if st.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v", st.Mode())
	}
}

func TestUpsertReplacesByPath(t *testing.T) {
	inv := &Inventory{}
	inv.Upsert(Entry{Path: "/a", SHA256: "1"})
	inv.Upsert(Entry{Path: "/a", SHA256: "2"})
	if len(inv.Entries) != 1 {
		t.Fatalf("len %d", len(inv.Entries))
	}
	e, ok := inv.Get("/a")
	if !ok || e.SHA256 != "2" {
		t.Fatalf("%+v %v", e, ok)
	}
	inv.Delete("/a")
	if _, ok := inv.Get("/a"); ok {
		t.Fatal("not deleted")
	}
}

func TestLoadMissingIsEmpty(t *testing.T) {
	inv, err := Load(fsx.Root{Dir: t.TempDir()}, "/nope.json")
	if err != nil || inv == nil || len(inv.Entries) != 0 {
		t.Fatalf("%+v %v", inv, err)
	}
}

func TestLoadMalformedErrors(t *testing.T) {
	root := fsx.Root{Dir: t.TempDir()}
	if err := root.WriteAtomic("/bad.json", []byte("{nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(root, "/bad.json")
	if err == nil || !strings.Contains(err.Error(), "/bad.json") {
		t.Fatalf("err %v", err)
	}
}
