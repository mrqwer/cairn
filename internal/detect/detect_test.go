package detect

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mrqwer/cairn/internal/execx"
	"github.com/mrqwer/cairn/internal/fsx"
)

const fixture = "../../testdata/detect/arch-nvidia"

func runner() *execx.FakeRunner {
	return &execx.FakeRunner{Responses: map[string]execx.Response{
		"systemctl --version": {Out: []byte("systemd 262 (262.1-1-arch)\n+PAM\n")},
		"uname -r":            {Out: []byte("7.2.11-arch1-1\n")},
	}}
}

func getenv(k string) string {
	if k == "XDG_CURRENT_DESKTOP" {
		return "GNOME"
	}
	return ""
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDetectArchNvidia(t *testing.T) {
	s, err := Detect(context.Background(), fsx.Root{Dir: fixture}, runner(), getenv)
	if err != nil {
		t.Fatal(err)
	}
	if s.Distro != "arch" || s.SystemdVersion != 262 || s.PkgManager != "pacman" ||
		s.GPU != "nvidia" || s.BootLoader != "systemd-boot" || s.Desktop != "gnome" ||
		s.KernelRunning != "7.2.11-arch1-1" || !s.KernelModulesPresent {
		t.Errorf("unexpected system: %+v", s)
	}
	if len(s.Swaps) != 3 {
		t.Fatalf("swaps = %d", len(s.Swaps))
	}
	if sw := s.Swaps[2]; sw.Path != "/hibernate-swap/swapfile" || sw.Type != "file" ||
		sw.SizeKiB != 25165820 || sw.UsedKiB != 0 || sw.Priority != -1 {
		t.Errorf("swap[2] = %+v", sw)
	}
	if s.Resume != (Resume{Device: "259:3", Offset: 61112320}) {
		t.Errorf("resume = %+v", s.Resume)
	}
	if !s.MemSleepDeep || s.InitramfsResume != Yes || !s.UPowerDropIns {
		t.Errorf("flags: %+v", s)
	}
	if s.NvidiaParams["TemporaryFilePath"] != `"/var/tmp"` || s.NvidiaParams["PreserveVideoMemoryAllocations"] != "1" {
		t.Errorf("nvidia params = %v", s.NvidiaParams)
	}
}

func TestKernelModulesMissing(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, fixture, dir)
	if err := os.RemoveAll(filepath.Join(dir, "usr/lib/modules/7.2.11-arch1-1")); err != nil {
		t.Fatal(err)
	}
	s, err := Detect(context.Background(), fsx.Root{Dir: dir}, runner(), getenv)
	if err != nil {
		t.Fatal(err)
	}
	if s.KernelModulesPresent {
		t.Error("KernelModulesPresent should be false")
	}
}

func TestDetectEmptyRoot(t *testing.T) {
	s, err := Detect(context.Background(), fsx.Root{Dir: t.TempDir()}, &execx.FakeRunner{}, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if s.InitramfsResume != Unknown || s.BootLoader != "unknown" || len(s.Swaps) != 0 || s.GPU != "" {
		t.Errorf("unexpected: %+v", s)
	}
}

func TestDetectMalformedSwaps(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, fixture, dir)
	if err := os.WriteFile(filepath.Join(dir, "proc/swaps"), []byte("Filename Type Size Used Priority\n/x file abc 0 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Detect(context.Background(), fsx.Root{Dir: dir}, runner(), getenv)
	if err == nil || !contains(err.Error(), "proc/swaps") {
		t.Fatalf("err = %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestHibernateSupported(t *testing.T) {
	for name, c := range map[string]struct {
		state, lockdown string
		want            Tri
		mode            string
	}{
		"fixture":         {"freeze mem disk\n", "[none] integrity confidentiality\n", Yes, "none"},
		"integrity":       {"freeze mem disk\n", "none [integrity] confidentiality\n", No, "integrity"},
		"confidentiality": {"freeze mem\n", "none integrity [confidentiality]\n", No, "confidentiality"},
		"no disk":         {"freeze mem\n", "", No, ""},
		"no lockdown":     {"freeze mem disk\n", "", Yes, ""},
		"unreadable":      {"", "", Unknown, ""},
	} {
		dir := t.TempDir()
		copyTree(t, fixture, dir)
		for p, v := range map[string]string{"sys/power/state": c.state, "sys/kernel/security/lockdown": c.lockdown} {
			_ = os.Remove(filepath.Join(dir, p))
			if v != "" {
				if err := os.WriteFile(filepath.Join(dir, p), []byte(v), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
		s, err := Detect(context.Background(), fsx.Root{Dir: dir}, runner(), getenv)
		if err != nil {
			t.Fatal(err)
		}
		if s.HibernateSupported != c.want || s.Lockdown != c.mode {
			t.Errorf("%s: supported=%v lockdown=%q", name, s.HibernateSupported, s.Lockdown)
		}
	}
}
