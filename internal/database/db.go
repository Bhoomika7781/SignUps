// Package database is the invx key-value engine: an append-only log of
// checksummed frames plus an in-memory hash index.
package database

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"invx/internal/storage"
)

var (
	// ErrDiskFull is returned when a write would push the file past MaxBytes.
	// Nothing is written when this is returned.
	ErrDiskFull = errors.New("disk full: write would exceed max-bytes limit")
	ErrClosed   = errors.New("database is closed")
)

// Options configure Open.
type Options struct {
	// MaxBytes caps the data file size. 0 means no limit.
	MaxBytes int64
	// DisableAutoCompact turns off automatic compaction (used by tests).
	DisableAutoCompact bool
}

// DB is one open database. All methods are safe for concurrent use.
//
// Locking: one sync.RWMutex (mu). Put, Delete, Batch, Compact and Close take
// it exclusively for the whole write including fsync. Get, Count, Keys and
// Dump take it shared. Because the index is only changed while mu is held
// exclusively, and a whole batch is applied to the index inside one critical
// section, a reader sees the state before a batch or after it, never between.
type DB struct {
	mu     sync.RWMutex
	path   string
	opts   Options
	log    *storage.Log
	lock   *os.File
	idx    *index
	closed bool
	broken error // set when the file may end in a partial frame
	stats  RecoveryStats
}

// Open opens or creates the database at path and runs crash recovery.
func Open(path string, opts Options) (*DB, error) {
	start := time.Now()

	lock, err := acquireLock(path + ".lock")
	if err != nil {
		return nil, err
	}

	db := &DB{path: path, opts: opts, lock: lock, idx: newIndex(0)}

	if err := db.resolveCompactionTemp(); err != nil {
		lock.Close()
		return nil, err
	}

	db.log, err = storage.OpenLog(path)
	if err != nil {
		lock.Close()
		return nil, err
	}
	// Pre-size the map from the file size (about 40 bytes per small key) so
	// recovery of 1M keys does not spend its time re-hashing a growing map.
	db.idx = newIndex(int(min64(db.log.Size()/40, 4_000_000)))
	if err := db.recover(); err != nil {
		db.log.Close()
		lock.Close()
		return nil, fmt.Errorf("recovery failed: %w", err)
	}
	db.stats.DurationNano = time.Since(start).Nanoseconds()
	return db, nil
}

// resolveCompactionTemp deals with path.compact left by a crashed compaction.
//
//   - path exists: the rename never happened (or happened and the temp name
//     is gone, in which case there is nothing here). path is authoritative,
//     the temp file is garbage -> delete it. (C3, C5)
//   - path is missing but path.compact exists: only possible if a non-atomic
//     rename (not POSIX, possibly Windows) removed the old name before adding
//     the new one. The temp file was fsynced before the rename started, so if
//     it scans cleanly it is the complete compacted database -> promote it.
func (db *DB) resolveCompactionTemp() error {
	tmp := tempPath(db.path)
	if _, err := os.Stat(tmp); os.IsNotExist(err) {
		return nil
	}
	if _, err := os.Stat(db.path); os.IsNotExist(err) && tempIsComplete(tmp) {
		if err := os.Rename(tmp, db.path); err != nil {
			return err
		}
		db.stats.PromotedTemp = true
		return storage.SyncDir(filepath.Dir(db.path))
	}
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return err
	}
	db.stats.RemovedTemp = true
	return nil
}

func tempIsComplete(tmp string) bool {
	f, err := os.Open(tmp)
	if err != nil {
		return false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || storage.ReadHeader(f) != nil {
		return false
	}
	res, err := storage.ScanFrames(f, info.Size(), func([]storage.ScannedOp) {})
	return err == nil && res.TornBytes() == 0
}

// acquireLock retries for a few seconds: on Windows the OS can take a moment
// to release the lock handle of a process that was just terminated.
func acquireLock(path string) (*os.File, error) {
	deadline := time.Now().Add(3 * time.Second)
	for {
		f, err := storage.LockFile(path)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, storage.ErrLocked) || time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func tempPath(path string) string { return path + ".compact" }

// Stats returns what recovery found when the database was opened.
func (db *DB) Stats() RecoveryStats { return db.stats }

// Path returns the data file path.
func (db *DB) Path() string { return db.path }

// Put stores value under key. When it returns nil the value is on disk.
func (db *DB) Put(key, value []byte) error {
	return db.write([]storage.Op{{Kind: storage.OpPut, Key: key, Value: value}})
}

// Delete removes key. Deleting a missing key is not an error.
func (db *DB) Delete(key []byte) error {
	return db.write([]storage.Op{{Kind: storage.OpDelete, Key: key}})
}

// write is the single write path for put, delete and batch.
//
//  1. Encode all ops into ONE frame (validation happens here, before any I/O).
//  2. Take the write lock.
//  3. Disk-full guard: if the frame would push the file past MaxBytes, try a
//     compaction; if it still does not fit, return ErrDiskFull having written
//     nothing.
//  4. log.Append: pwrite the frame, fsync. (storage/log.go)
//  5. Only after fsync succeeded, update the in-memory index.
//  6. Return nil -> caller (serve) prints ACK.
func (db *DB) write(ops []storage.Op) error {
	frame, valueOffsets, err := storage.EncodeFrame(ops)
	if err != nil {
		return err
	}

	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return ErrClosed
	}
	if db.broken != nil {
		return db.broken
	}

	// Single delete of a key that is not there: nothing to do. The index only
	// ever reflects durable state, so "absent in index" means absent on disk.
	if len(ops) == 1 && ops[0].Kind == storage.OpDelete {
		if _, ok := db.idx.entries[string(ops[0].Key)]; !ok {
			return nil
		}
	}

	if !db.fits(len(frame)) {
		if db.opts.DisableAutoCompact || !db.compactionWouldHelp(len(frame)) {
			return ErrDiskFull
		}
		if err := db.compactLocked(); err != nil {
			if db.broken != nil {
				return db.broken
			}
			return fmt.Errorf("%w (compaction to free space failed: %v)", ErrDiskFull, err)
		}
		if !db.fits(len(frame)) {
			return ErrDiskFull
		}
	}

	start, err := db.log.Append(frame)
	if err != nil {
		if errors.Is(err, storage.ErrBroken) {
			db.broken = err
		}
		return err
	}

	// The frame is durable. Publish it.
	for i, op := range ops {
		if op.Kind == storage.OpPut {
			db.idx.set(string(op.Key), valueRef{
				offset: start + valueOffsets[i],
				length: uint32(len(op.Value)),
				keyLen: uint16(len(op.Key)),
			})
		} else {
			db.idx.del(string(op.Key))
		}
	}

	db.maybeAutoCompact()
	return nil
}

func (db *DB) fits(frameLen int) bool {
	return db.opts.MaxBytes <= 0 || db.log.Size()+int64(frameLen) <= db.opts.MaxBytes
}

// Get returns the current value. ok is false if the key does not exist.
func (db *DB) Get(key []byte) (value []byte, ok bool, err error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return nil, false, ErrClosed
	}
	ref, ok := db.idx.entries[string(key)]
	if !ok {
		return nil, false, nil
	}
	v, err := db.log.ReadAt(ref.offset, int(ref.length))
	if err != nil {
		return nil, false, err
	}
	return v, true, nil
}

// Count returns the number of live keys.
func (db *DB) Count() int {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return len(db.idx.entries)
}

// FileSize returns the current size of the data file.
func (db *DB) FileSize() int64 {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.log.Size()
}

// Dump calls fn for every key in sorted order, under one read lock, so it
// sees a single consistent state.
func (db *DB) Dump(fn func(key, value []byte) error) error {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return ErrClosed
	}
	keys := make([]string, 0, len(db.idx.entries))
	for k := range db.idx.entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		ref := db.idx.entries[k]
		v, err := db.log.ReadAt(ref.offset, int(ref.length))
		if err != nil {
			return err
		}
		if err := fn([]byte(k), v); err != nil {
			return err
		}
	}
	return nil
}

// Close fsyncs and closes the file. A crash during Close is the same as a
// crash at any other moment: every ACKed frame was already fsynced.
func (db *DB) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return nil
	}
	db.closed = true
	var err error
	if db.log != nil {
		err = db.log.Close()
	}
	db.lock.Close()
	return err
}
