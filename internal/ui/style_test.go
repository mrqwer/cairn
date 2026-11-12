package ui

import (
	"reflect"
	"testing"
)

func TestBytes(t *testing.T) {
	for n, want := range map[uint64]string{512: "512 B", 1536: "1.5 KiB", 6 << 30: "6.0 GiB"} {
		if got := Bytes(n); got != want {
			t.Errorf("Bytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestTruncatePadWrap(t *testing.T) {
	if got := Truncate("héllo wörld", 6); got != "héllo…" {
		t.Errorf("Truncate = %q", got)
	}
	if got := Pad("ab", 4); got != "ab  " {
		t.Errorf("Pad = %q", got)
	}
	if got, want := Wrap("one two three four", 10), []string{"one two", "three four"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Wrap = %q, want %q", got, want)
	}
}

func TestNoColorPaintsNothing(t *testing.T) {
	if got := (Style{}).Red("x"); got != "x" {
		t.Errorf("plain style painted %q", got)
	}
	if got := (Style{Color: true}).Red("x"); got != "\x1b[31mx\x1b[0m" {
		t.Errorf("colour style gave %q", got)
	}
}
