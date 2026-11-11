package sleep

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// OffsetFunc returns the resume_offset of a swap file.
type OffsetFunc func(path string) (uint64, error)

const (
	fsIocFiemap     = 0xC020660B
	fiemapHeaderLen = 32
	fiemapExtentLen = 56
)

// FirstPhysicalOffset returns the physical offset of the first extent of
// path, in pages: the value the kernel's resume_offset expects.
func FirstPhysicalOffset(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()

	// struct fiemap header (32 bytes) followed by one fiemap_extent (56 bytes).
	buf := make([]byte, fiemapHeaderLen+fiemapExtentLen)
	binary.LittleEndian.PutUint64(buf[8:], math.MaxUint64) // fm_length: whole file
	binary.LittleEndian.PutUint32(buf[16:], 1)             // fm_flags: FIEMAP_FLAG_SYNC
	binary.LittleEndian.PutUint32(buf[24:], 1)             // fm_extent_count
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), fsIocFiemap, uintptr(unsafe.Pointer(&buf[0])))
	if errno != 0 {
		return 0, fmt.Errorf("FIEMAP %s: %w", path, errno)
	}
	if binary.LittleEndian.Uint32(buf[20:]) == 0 {
		return 0, errors.New("file has no extents")
	}
	physical := binary.LittleEndian.Uint64(buf[fiemapHeaderLen+8:])
	return physical / uint64(os.Getpagesize()), nil
}

// DeviceOf returns "MAJ:MIN" of the device holding the filesystem of path.
func DeviceOf(path string) (string, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return "", fmt.Errorf("stat %s: %w", path, err)
	}
	return fmt.Sprintf("%d:%d", unix.Major(st.Dev), unix.Minor(st.Dev)), nil
}
