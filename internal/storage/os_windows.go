//go:build windows

package storage

import (
	"errors"
	"os"
	"syscall"
)

// ErrLocked means another process has the database open.
var ErrLocked = errors.New("database is locked by another process")

// LockFile opens path with share mode 0, so no other process can open it.
// Windows closes the handle (and so releases the lock) when the process is
// terminated, including by TerminateProcess / Task Manager.
func LockFile(path string) (*os.File, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		0, // no sharing: this is the lock
		nil,
		syscall.OPEN_ALWAYS,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0)
	if err != nil {
		return nil, ErrLocked
	}
	return os.NewFile(uintptr(h), path), nil
}

// SyncDir is a no-op on Windows: Go's standard library cannot open a directory
// handle for FlushFileBuffers. NTFS journals metadata, and the hackathon only
// requires surviving process kills, which do not need it (see BREAKING.md).
func SyncDir(dir string) error { return nil }
