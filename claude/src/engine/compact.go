package engine

// compact.go: crash-safe compaction.
//
// Ordering (the rename is the commit point):
//  1. create  <path>.compact  (O_TRUNC, so an old leftover is wiped)
//  2. write every live key into it as normal records (same format)
//  3. File.Sync() the temp file          -> new data is durable
//  4. close the temp file, close the old file (Windows cannot rename over an open file)
//  5. os.Rename(<path>.compact, <path>)   -> atomic swap, old file is gone
//  6. fsync the directory                -> the rename itself is durable (Linux/macOS)
//  7. reopen <path> and switch the index to the new offsets
//
// Kill before step 5: the old file is untouched and still authoritative; the
// next Open deletes the temp file. (C3)
// Kill during step 5: rename is atomic, so <path> is either the complete old
// file or the complete (already fsynced) new file. (C4)
// Kill after step 5: <path> is the new file, no temp file is left. (C5)

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// compactChunkBytes caps how much payload goes into one record during compaction.
const compactChunkBytes = 8 << 20

// Compact rewrites the data file so it only holds live keys.
func (db *DB) Compact() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.writableLocked(); err != nil {
		return err
	}
	return db.compactLocked(nil)
}

// compactLocked does the rewrite. Keys in drop are left out of the new file;
// that is how a delete-only batch works when the file is full.
// Caller holds db.mu exclusively, so no reader or writer runs meanwhile.
func (db *DB) compactLocked(drop map[string]struct{}) error {
	tmpPath := db.path + TempSuffix
	tf, err := os.OpenFile(tmpPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("compaction: create temp: %w", err)
	}
	abort := func(e error) error {
		tf.Close()
		os.Remove(tmpPath) // the old file was never touched
		return fmt.Errorf("compaction: %w", e)
	}

	w := bufio.NewWriterSize(tf, 4<<20)
	newIndex := make(map[string]entry, len(db.index))
	var newSize, newLive int64
	var ops []Op
	var keys []string
	pending := CountSize

	flush := func() error {
		if len(ops) == 0 {
			return nil
		}
		rec, valueOffsets, err := encodeRecord(ops)
		if err != nil {
			return err
		}
		if _, err := w.Write(rec); err != nil {
			return err
		}
		for i := range ops {
			newIndex[keys[i]] = entry{off: newSize + valueOffsets[i], vlen: uint32(len(ops[i].Value))}
			newLive += opCost(len(keys[i]), len(ops[i].Value))
		}
		newSize += int64(len(rec))
		ops, keys, pending = ops[:0], keys[:0], CountSize
		return nil
	}

	for k, e := range db.index {
		if _, skip := drop[k]; skip {
			continue
		}
		val := make([]byte, e.vlen)
		if e.vlen > 0 {
			if _, err := db.f.ReadAt(val, e.off); err != nil {
				return abort(fmt.Errorf("read value: %w", err))
			}
		}
		cost := OpHeaderSize + len(k) + len(val)
		if len(ops) == MaxBatchOps || (len(ops) > 0 && pending+cost > compactChunkBytes) {
			if err := flush(); err != nil {
				return abort(err)
			}
		}
		ops = append(ops, PutOp([]byte(k), val))
		keys = append(keys, k)
		pending += cost
	}
	if err := flush(); err != nil {
		return abort(err)
	}
	if err := w.Flush(); err != nil {
		return abort(err) // e.g. a real disk-full (ENOSPC): old file is untouched
	}
	// Step 3: the new file must be durable BEFORE it can replace the old one.
	if err := tf.Sync(); err != nil {
		return abort(err)
	}
	if err := tf.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("compaction: close temp: %w", err)
	}

	// Step 4: close the old file so Windows lets us replace it.
	if err := db.f.Close(); err != nil {
		os.Remove(tmpPath)
		db.broken = fmt.Errorf("compaction: close old file: %w", err)
		return db.broken
	}

	// Step 5: the commit point.
	if err := os.Rename(tmpPath, db.path); err != nil {
		os.Remove(tmpPath)
		// The old file is still there and complete; reopen it.
		f, err2 := os.OpenFile(db.path, os.O_RDWR, 0)
		if err2 != nil {
			db.broken = fmt.Errorf("compaction: rename failed (%v) and reopen failed: %w", err, err2)
			return db.broken
		}
		db.f = f
		return fmt.Errorf("compaction: rename: %w", err)
	}

	// Step 6: make the rename durable. Only matters for power loss, not for
	// a killed process, so an error here does not undo anything.
	_ = syncDir(filepath.Dir(db.path))

	// Step 7: switch over to the new file.
	f, err := os.OpenFile(db.path, os.O_RDWR, 0)
	if err != nil {
		db.broken = fmt.Errorf("compaction: reopen new file: %w", err)
		return db.broken
	}
	db.f = f
	db.index = newIndex
	db.size = newSize
	db.live = newLive
	db.dead = 0
	return nil
}

// compactionWouldMakeRoomLocked estimates whether compacting would free
// enough space for a record of `need` bytes, so we never compact for nothing.
func (db *DB) compactionWouldMakeRoomLocked(need int64) bool {
	if db.dead == 0 && db.size <= db.live+HeaderSize+CountSize {
		return false
	}
	records := int64(len(db.index))/MaxBatchOps + db.live/compactChunkBytes + 2
	estimate := db.live + records*(HeaderSize+CountSize)
	return estimate < db.size && estimate+need <= db.opts.MaxBytes
}

// maybeAutoCompactLocked compacts when the file is big and mostly garbage,
// so the file shrinks after many deletes without anyone asking.
func (db *DB) maybeAutoCompactLocked() {
	min := db.opts.AutoCompactMinBytes
	if min < 0 || db.size < min || db.dead <= db.live {
		return
	}
	_ = db.compactLocked(nil) // on failure the old file stays authoritative
}

// syncDir fsyncs a directory so a create or rename inside it is durable.
// Windows has no way to fsync a directory (opening one for FlushFileBuffers
// fails); NTFS journals metadata itself, so we skip it there. This is listed
// as a known weakness in BREAKING.md.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
