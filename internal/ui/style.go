// Package ui holds the terminal presentation shared by cairn's commands:
// colour, symbols and layout helpers. Styled output is used only on a
// terminal; pipes and scripts get the plain renderers.
package ui

import (
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// Style describes how to draw on the current output.
type Style struct {
	Color bool
	Width int // terminal columns
}

const (
	defaultWidth = 100
	maxWidth     = 120
)

// Detect returns the style for f: styled when f is a terminal, with colour
// unless NO_COLOR is set or TERM is "dumb".
func Detect(f *os.File, getenv func(string) string) (Style, bool) {
	st, err := f.Stat()
	if err != nil || st.Mode()&os.ModeCharDevice == 0 {
		return Style{}, false
	}
	s := Style{Color: getenv("NO_COLOR") == "" && getenv("TERM") != "dumb", Width: defaultWidth}
	if ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ); err == nil && ws.Col > 0 {
		s.Width = min(int(ws.Col), maxWidth)
	}
	return s, true
}

func (s Style) paint(code, t string) string {
	if !s.Color || t == "" {
		return t
	}
	return "\x1b[" + code + "m" + t + "\x1b[0m"
}

// Bold, Dim and the colours wrap t in SGR codes when colour is on.
func (s Style) Bold(t string) string   { return s.paint("1", t) }
func (s Style) Dim(t string) string    { return s.paint("2", t) }
func (s Style) Green(t string) string  { return s.paint("32", t) }
func (s Style) Yellow(t string) string { return s.paint("33", t) }
func (s Style) Red(t string) string    { return s.paint("31", t) }
func (s Style) Blue(t string) string   { return s.paint("34", t) }
func (s Style) Cyan(t string) string   { return s.paint("36", t) }

// Pad right-pads t with spaces to n display columns.
func Pad(t string, n int) string {
	if w := utf8.RuneCountInString(t); w < n {
		return t + strings.Repeat(" ", n-w)
	}
	return t
}

// Truncate shortens t to at most n display columns, ending in "…".
func Truncate(t string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(t) <= n {
		return t
	}
	r := []rune(t)
	return string(r[:n-1]) + "…"
}

// Wrap breaks t into lines of at most n columns at spaces.
func Wrap(t string, n int) []string {
	if n < 10 {
		n = 10
	}
	var lines []string
	line := ""
	for _, word := range strings.Fields(t) {
		switch {
		case line == "":
			line = word
		case utf8.RuneCountInString(line)+1+utf8.RuneCountInString(word) <= n:
			line += " " + word
		default:
			lines = append(lines, line)
			line = word
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}

// Bytes formats a size in binary units with one decimal ("6.0 GiB").
func Bytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatUint(n, 10) + " B"
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return strconv.FormatFloat(float64(n)/float64(div), 'f', 1, 64) + " " + string("KMGTPE"[exp]) + "iB"
}

// Plural returns "1 thing" or "n things".
func Plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}
