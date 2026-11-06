package storage

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ErrBroken means a failed write could not be rolled back, so the file may end
// in a partial frame. The database refuses further writes until it is reopened
// (reopening runs recovery, which cuts the partial frame off).
var ErrBroken = errors.New("log is in an unknown state after a failed write; reopen the database")

// Log is the single data file. It only ever grows by whole frames at the end,
// except for Truncate during recovery or rollback.
type Log struct {
	file *os.File
	path string
	size int64
}

// OpenLog opens (or creates) the data file and makes sure it has a valid
// header. It does NOT scan frames; that is recovery's job.
func OpenLog(path string) (*Log, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	l := &Log{file: f, path: path, size: info.Size()}

	needInit := false
	switch {
	case l.size < FileHeaderSize:
		// Brand new file, or we crashed while writing the very first header.
		// No frame can exist yet, so nothing that was ACKed is lost.
		needInit = true
	case l.size == FileHeaderSize && ReadHeader(f) != nil:
		// Same situation: header bytes landed but were not all correct.
		needInit = true
	default:
		if err := ReadHeader(f); err != nil {
			f.Close()
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}

	if needInit {
		if err := l.writeHeader(); err != nil {
			f.Close()
			return nil, err
		}
	}
	return l, nil
}

func (l *Log) writeHeader() error {
	if err := l.file.Truncate(0); err != nil {
		return err
	}
	if _, err := l.file.WriteAt(EncodeHeader(), 0); err != nil {
		return err
	}
	if err := l.file.Sync(); err != nil {
		return err
	}
	l.size = FileHeaderSize
	// The file itself may be new: fsync the directory so its entry survives
	// power loss (process kills do not need this, but it is cheap).
	return SyncDir(filepath.Dir(l.path))
}

// File exposes the underlying file for scanning.
func (l *Log) File() *os.File { return l.file }

// Size is the logical end of the log (offset where the next frame goes).
func (l *Log) Size() int64 { return l.size }

// Append writes one frame at the end of the file and fsyncs it.
// Only when this returns nil is the frame durable.
//
// Order: (1) pwrite frame at offset size, (2) fsync, (3) advance size.
// If (1) or (2) fails we truncate back to the old size so the file still ends
// on a frame boundary.
func (l *Log) Append(frame []byte) (int64, error) {
	start := l.size
	n, err := l.file.WriteAt(frame, start)
	if err == nil && n != len(frame) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = l.file.Sync()
	}
	if err != nil {
		if terr := l.file.Truncate(start); terr != nil {
			return 0, fmt.Errorf("%w (write error: %v, truncate error: %v)", ErrBroken, err, terr)
		}
		if serr := l.file.Sync(); serr != nil {
			return 0, fmt.Errorf("%w (write error: %v, sync error: %v)", ErrBroken, err, serr)
		}
		return 0, err
	}
	l.size = start + int64(len(frame))
	return start, nil
}

// ReadAt reads n bytes at offset.
func (l *Log) ReadAt(offset int64, n int) ([]byte, error) {
	buf := make([]byte, n)
	if n == 0 {
		return buf, nil
	}
	if _, err := l.file.ReadAt(buf, offset); err != nil {
		return nil, err
	}
	return buf, nil
}

// Truncate cuts the file at offset and fsyncs. Used by recovery to drop a torn
// tail. Truncating to the same offset twice is harmless, which is what makes
// recovery safe to crash and repeat.
func (l *Log) Truncate(offset int64) error {
	if err := l.file.Truncate(offset); err != nil {
		return err
	}
	if err := l.file.Sync(); err != nil {
		return err
	}
	l.size = offset
	return nil
}

// Close fsyncs and closes the file.
func (l *Log) Close() error {
	serr := l.file.Sync()
	cerr := l.file.Close()
	if serr != nil {
		return serr
	}
	return cerr
}
