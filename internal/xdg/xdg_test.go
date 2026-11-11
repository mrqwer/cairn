package xdg

import "testing"

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDefaults(t *testing.T) {
	d, err := Resolve(env(map[string]string{"HOME": "/h", "XDG_RUNTIME_DIR": "/run/user/1"}))
	if err != nil {
		t.Fatal(err)
	}
	want := Dirs{
		Home:    "/h",
		Config:  "/h/.config/cairn",
		State:   "/h/.local/state/cairn",
		Data:    "/h/.local/share/cairn",
		Cache:   "/h/.cache/cairn",
		Runtime: "/run/user/1/cairn",
	}
	if d != want {
		t.Fatalf("got %+v want %+v", d, want)
	}
}

func TestRuntimeUnsetErrors(t *testing.T) {
	if _, err := Resolve(env(map[string]string{"HOME": "/h"})); err == nil {
		t.Fatal("expected error when XDG_RUNTIME_DIR unset")
	}
}

func TestAbsoluteXDGOverrides(t *testing.T) {
	d, err := Resolve(env(map[string]string{
		"HOME": "/h", "XDG_RUNTIME_DIR": "/r",
		"XDG_CONFIG_HOME": "/c", "XDG_STATE_HOME": "/s",
		"XDG_DATA_HOME": "/d", "XDG_CACHE_HOME": "/k",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if d.Config != "/c/cairn" || d.State != "/s/cairn" || d.Data != "/d/cairn" || d.Cache != "/k/cairn" {
		t.Fatalf("got %+v", d)
	}
}

func TestRelativeXDGIgnored(t *testing.T) {
	d, err := Resolve(env(map[string]string{
		"HOME": "/h", "XDG_RUNTIME_DIR": "/r",
		"XDG_CONFIG_HOME": "rel/path", "XDG_CACHE_HOME": "rel",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if d.Config != "/h/.config/cairn" || d.Cache != "/h/.cache/cairn" {
		t.Fatalf("got %+v", d)
	}
}

func TestRelativeRuntimeErrors(t *testing.T) {
	if _, err := Resolve(env(map[string]string{"HOME": "/h", "XDG_RUNTIME_DIR": "rel"})); err == nil {
		t.Fatal("expected error for relative XDG_RUNTIME_DIR")
	}
}

func TestMissingHomeErrors(t *testing.T) {
	if _, err := Resolve(env(map[string]string{"XDG_RUNTIME_DIR": "/r"})); err == nil {
		t.Fatal("expected error when HOME unset")
	}
}
