package cli

import (
	"io"
	"os"

	"github.com/mrqwer/cairn/internal/ui"
)

// styleFor returns the terminal style when w is the process's stdout or
// stderr and that stream is a terminal; otherwise the plain renderers apply.
func styleFor(w io.Writer) (ui.Style, bool) {
	f, ok := w.(*os.File)
	if !ok || (f != os.Stdout && f != os.Stderr) {
		return ui.Style{}, false
	}
	return ui.Detect(f, os.Getenv)
}

// homeDir is the user's home directory, or "" when unknown.
func homeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h
}
