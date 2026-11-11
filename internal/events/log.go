// Package events implements cairn's append-only JSONL event log.
package events

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mrqwer/cairn/internal/fsx"
)

// Event kinds.
const (
	KindSleepPre            = "sleep.pre"
	KindSleepPost           = "sleep.post"
	KindSleepCheckFailed    = "sleep.check.failed"
	KindSleepFallback       = "sleep.fallback"
	KindBootLostDuringSleep = "boot.lost-during-sleep"
	KindRebootRequired      = "kernel.reboot-required"
)

const bootIDPath = "/proc/sys/kernel/random/boot_id"

// Event is one log record.
type Event struct {
	TS     time.Time      `json:"ts"`
	Boot   string         `json:"boot"`
	Source string         `json:"source"`
	Kind   string         `json:"kind"`
	Data   map[string]any `json:"data,omitempty"`
}

// Log is an append-only JSONL file at Path under Root.
type Log struct {
	Root fsx.Root
	Path string
}

// Append writes e as one JSON line and fsyncs.
func (l Log) Append(e Event) error {
	b, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	b = append(b, '\n')
	if err := l.Root.MkdirAll(filepath.Dir(l.Path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(l.Root.Path(l.Path), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// ReadFrom returns events starting at byte offset and the offset to resume
// from. Unparsable complete lines are skipped; an unterminated trailing line
// is left unread so a later completed write is picked up.
func (l Log) ReadFrom(offset int64) (evs []Event, next int64, err error) {
	data, err := l.Root.ReadFile(l.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, offset, err
	}
	if offset < 0 || offset > int64(len(data)) {
		offset = 0
	}
	pos := offset
	rest := data[offset:]
	for {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			break
		}
		line := rest[:i]
		rest = rest[i+1:]
		pos += int64(i) + 1
		var e Event
		if len(bytes.TrimSpace(line)) == 0 || json.Unmarshal(line, &e) != nil {
			continue
		}
		evs = append(evs, e)
	}
	return evs, pos, nil
}

// CurrentBoot returns the kernel boot id.
func CurrentBoot(root fsx.Root) (string, error) {
	b, err := root.ReadFile(bootIDPath)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
