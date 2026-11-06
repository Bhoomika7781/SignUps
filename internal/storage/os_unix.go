//go:build !windows

package storage

import (
	"errors"
	"os"
	"syscall"
)

// ErrLocked means another process has the database open.
var ErrLocked = errors.New("database is locked by another process")

// LockFile takes an exclusive flock on path. The kernel releases it
// automatically when the process dies, even with kill -9.
func LockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, ErrLocked
	}
	return f, nil
}

// SyncDir fsyncs a directory so that a rename or file creation inside it is
// durable across power loss.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
