// Package fsx provides filesystem access rooted at a configurable directory,
// so the same code can manage the real system ("/") or a temp dir in tests.
package fsx

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// Root anchors absolute paths under Dir. Dir "/" is the real system.
type Root struct{ Dir string }

// Path joins Dir with the absolute path p, never escaping Dir.
func (r Root) Path(p string) string {
	return filepath.Join(r.Dir, filepath.Clean("/"+p))
}

func (r Root) ReadFile(p string) ([]byte, error) { return os.ReadFile(r.Path(p)) }

func (r Root) Stat(p string) (fs.FileInfo, error) { return os.Stat(r.Path(p)) }

func (r Root) MkdirAll(p string, perm fs.FileMode) error { return os.MkdirAll(r.Path(p), perm) }

// Exists reports whether p exists.
func (r Root) Exists(p string) bool {
	_, err := r.Stat(p)
	return err == nil
}

// Remove deletes p; a missing file is not an error.
func (r Root) Remove(p string) error {
	if err := os.Remove(r.Path(p)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// WriteAtomic writes data to p via a temp file in the same directory,
// fsync, rename, and fsync of the directory.
func (r Root) WriteAtomic(p string, data []byte, perm fs.FileMode) error {
	full := r.Path(p)
	dir := filepath.Dir(full)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(full)+".tmp*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	cleanup := func(err error) error {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return cleanup(err)
	}
	if err := tmp.Chmod(perm); err != nil {
		return cleanup(err)
	}
	if err := tmp.Sync(); err != nil {
		return cleanup(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, full); err != nil {
		_ = os.Remove(name)
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// Header returns the marker line placed at the top of files managed by cairn.
func Header(policy string) string {
	return "# Managed by cairn host (" + policy + "). Do not edit; run \"cairn host ...\"\n"
}

// SHA256 returns the hex digest of data.
func SHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
