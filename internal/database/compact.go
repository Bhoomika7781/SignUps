package database

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"

	"invx/internal/storage"
)

const (
	compactFrameOps     = 1000
	compactFramePayload = 8 << 20
	autoCompactMinSize  = 4 << 20 // never auto-compact files smaller than 4 MB
)

// Compact rewrites the file so that it holds only live keys.
func (db *DB) Compact() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return ErrClosed
	}
	if db.broken != nil {
		return db.broken
	}
	return db.compactLocked()
}

// estimatedCompactedSize is an upper bound on the size after compaction.
func (db *DB) estimatedCompactedSize() int64 {
	n := int64(len(db.idx.entries))
	frames := n/compactFrameOps + db.idx.liveBytes/compactFramePayload + 1
	return storage.FileHeaderSize + db.idx.liveBytes + frames*storage.FrameOverhead
}

func (db *DB) compactionWouldHelp(frameLen int) bool {
	est := db.estimatedCompactedSize()
	return est < db.log.Size() && est+int64(frameLen) <= db.opts.MaxBytes
}

// maybeAutoCompact runs after a successful write. It compacts when more than
// half of the file is dead data. Errors are ignored here because the write
// that triggered it already succeeded; a failed compaction leaves the old
// file authoritative (or sets db.broken if the file could not be reopened).
func (db *DB) maybeAutoCompact() {
	if db.opts.DisableAutoCompact {
		return
	}
	size := db.log.Size()
	if size >= autoCompactMinSize && size > 2*db.estimatedCompactedSize() {
		_ = db.compactLocked()
	}
}

// compactLocked must be called with db.mu held exclusively.
//
// Crash-safe sequence:
//  1. Create path.compact, write header + all live keys as frames (buffered).
//  2. fsync path.compact, close it.
//     -- crash before here: path is untouched and authoritative; the next
//     open() deletes path.compact.
//  3. Close the old file handle (Windows cannot rename over an open file).
//  4. rename(path.compact, path). On POSIX this atomically replaces the old
//     file: any crash leaves either the complete old or the complete new file
//     under the name path.
//  5. fsync the directory (POSIX) so the rename survives power loss too.
//  6. Reopen path and swap in the new index.
func (db *DB) compactLocked() error {
	tmp := tempPath(db.path)
	_ = os.Remove(tmp)

	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	fail := func(e error) error {
		f.Close()
		os.Remove(tmp)
		return e
	}

	w := bufio.NewWriterSize(f, 4<<20)
	if _, err := w.Write(storage.EncodeHeader()); err != nil {
		return fail(err)
	}

	newIdx := newIndex(len(db.idx.entries))
	off := int64(storage.FileHeaderSize)
	var ops []storage.Op
	var keys []string
	payload := 0

	flush := func() error {
		if len(ops) == 0 {
			return nil
		}
		frame, valueOffsets, err := storage.EncodeFrame(ops)
		if err != nil {
			return err
		}
		if db.opts.MaxBytes > 0 && off+int64(len(frame)) > db.opts.MaxBytes {
			return ErrDiskFull // E4: compaction gives up, old file untouched
		}
		if _, err := w.Write(frame); err != nil {
			return err
		}
		for i, op := range ops {
			newIdx.set(keys[i], valueRef{offset: off + valueOffsets[i], length: uint32(len(op.Value)), keyLen: uint16(len(op.Key))})
		}
		off += int64(len(frame))
		ops, keys, payload = ops[:0], keys[:0], 0
		return nil
	}

	for key, ref := range db.idx.entries {
		v, err := db.log.ReadAt(ref.offset, int(ref.length))
		if err != nil {
			return fail(err)
		}
		ops = append(ops, storage.Op{Kind: storage.OpPut, Key: []byte(key), Value: v})
		keys = append(keys, key)
		payload += storage.OpHeaderSize + len(key) + len(v)
		if len(ops) >= compactFrameOps || payload >= compactFramePayload {
			if err := flush(); err != nil {
				return fail(err)
			}
		}
	}
	if err := flush(); err != nil {
		return fail(err)
	}
	if err := w.Flush(); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}

	// Point of no return starts here.
	if err := db.log.Close(); err != nil {
		os.Remove(tmp)
		return db.reopenAfterFailure(err)
	}
	if err := os.Rename(tmp, db.path); err != nil {
		os.Remove(tmp)
		return db.reopenAfterFailure(err)
	}
	if err := storage.SyncDir(filepath.Dir(db.path)); err != nil {
		// The rename already happened; only power-loss durability is affected.
		_ = err
	}

	l, err := storage.OpenLog(db.path)
	if err != nil {
		db.broken = fmt.Errorf("reopen after compaction failed: %w", err)
		return db.broken
	}
	if l.Size() != off {
		l.Close()
		db.broken = fmt.Errorf("compacted file has size %d, expected %d", l.Size(), off)
		return db.broken
	}
	db.log = l
	db.idx = newIdx
	return nil
}

// reopenAfterFailure reopens the old file after a failed close/rename. The old
// file is unchanged, so the existing index is still correct.
func (db *DB) reopenAfterFailure(cause error) error {
	l, err := storage.OpenLog(db.path)
	if err != nil {
		db.broken = fmt.Errorf("compaction failed (%v) and reopen failed: %w", cause, err)
		return db.broken
	}
	db.log = l
	return fmt.Errorf("compaction failed, old file kept: %w", cause)
}
