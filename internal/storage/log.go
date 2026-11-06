package storage

import (
	"errors"
	"io"
	"os"
)

type Log struct {
	file      *os.File
	endOffset int64
	closed    bool
}

func OpenLog(path string) (*Log, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}

	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}

	return &Log{
		file:      file,
		endOffset: info.Size(),
	}, nil
}

// Append writes a record and returns its starting offset.
func (l *Log) Append(record []byte) (int64, error) {
	if l.closed {
		return 0, errors.New("log is closed")
	}

	start := l.endOffset
	written := 0

	for written < len(record) {
		n, err := l.file.WriteAt(record[written:], start+int64(written))
		written += n

		if err != nil {
			l.file.Truncate(start)
			return 0, err
		}

		if n == 0 {
			l.file.Truncate(start)
			return 0, io.ErrShortWrite
		}
	}

	if err := l.file.Sync(); err != nil {
		l.file.Truncate(start)
		l.file.Sync()
		return 0, err
	}

	l.endOffset += int64(len(record))
	return start, nil
}

// Read reads data from a particular offset.
func (l *Log) Read(offset int64, size int) ([]byte, error) {
	if l.closed {
		return nil, errors.New("log is closed")
	}

	if size < 0 || offset < 0 {
		return nil, errors.New("invalid read position or size")
	}

	data := make([]byte, size)

	_, err := l.file.ReadAt(data, offset)
	if err != nil {
		return nil, err
	}

	return data, nil
}

// Returns the current size of the log.
func (l *Log) Size() int64 {
	return l.endOffset
}

// Truncate removes data after the given offset.
func (l *Log) Truncate(offset int64) error {
	if l.closed {
		return errors.New("log is closed")
	}

	if offset < 0 || offset > l.endOffset {
		return errors.New("invalid truncate offset")
	}

	if err := l.file.Truncate(offset); err != nil {
		return err
	}

	if err := l.file.Sync(); err != nil {
		return err
	}

	l.endOffset = offset
	return nil
}

// Sync makes pending changes durable.
func (l *Log) Sync() error {
	if l.closed {
		return errors.New("log is closed")
	}

	return l.file.Sync()
}

// Close closes the log file.
func (l *Log) Close() error {
	if l.closed {
		return nil
	}

	l.closed = true
	return l.file.Close()
}
