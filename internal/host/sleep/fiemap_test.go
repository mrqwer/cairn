package sleep

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"golang.org/x/sys/unix"
)

func TestFirstPhysicalOffsetOnRealFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "swap")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(make([]byte, 1<<20)); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	off, err := FirstPhysicalOffset(p)
	if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOTTY) {
		t.Skipf("filesystem has no FIEMAP: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if off == 0 {
		t.Fatal("offset is 0")
	}
}

func TestDeviceOf(t *testing.T) {
	dev, err := DeviceOf(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^\d+:\d+$`).MatchString(dev) {
		t.Fatalf("got %q", dev)
	}
}
