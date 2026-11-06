package engine

// db.go: the database object, open/close, and the write path.
//
// Design: append-only log file + in-memory hash index.
//   - The index maps key -> (file offset of the value, value length).
//     Values stay on disk; Get reads them with ReadAt.
//   - Every write appends ONE record, fsyncs, and only then updates the index
//     and returns success.
//   - One RWMutex: writers take it exclusively, Get takes it shared. So a Get
//     sees the index either before or after a batch, never in between.
//
// Write ordering for Put / Delete / Batch (see Batch below):
//   1. validate + encode the record          (no lock, nothing on disk)
//   2. lock
//   3. max_bytes check                       (fail -> nothing written)
//   4. WriteAt(record, end of file)          (write syscall)
//   5. File.Sync()                           (fsync syscall)
//   6. update in-memory index
//   7. unlock, return nil  -> the caller prints ACK
// A kill before step 5 finishes may leave a torn or complete-but-unacked
// record; recovery handles both. A kill after step 5 leaves a complete,
// durable record that recovery will replay.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// TempSuffix is added to the data file name for the compaction temp file.
const TempSuffix = ".compact"

// DefaultAutoCompactMinBytes: auto-compaction only runs once the file is at
// least this big and more than half of it is garbage.
const DefaultAutoCompactMinBytes = 8 << 20

var (
	ErrFull     = errors.New("database full: max_bytes limit reached")
	ErrClosed   = errors.New("database is closed")
	ErrReadOnly = errors.New("database opened read-only")
)

// Options control how the database is opened.
type Options struct {
	// MaxBytes is the largest the data file may grow (0 = no limit).
	// A write that would go past it fails with ErrFull and writes nothing.
	MaxBytes int64
	// ReadOnly opens without repairing anything (used by get, dump, check).
	ReadOnly bool
	// AutoCompactMinBytes: 0 = default, negative = never auto-compact.
	AutoCompactMinBytes int64
}

// RecoveryInfo reports what Open found.
type RecoveryInfo struct {
	Keys           int
	Records        int
	FileSize       int64 // size before repair
	ValidEnd       int64
	TruncatedBytes int64 // torn tail removed by this open
	Reason         string
	RemovedTemp    bool // a leftover compaction temp file was deleted
	Duration       time.Duration
}

type entry struct {
	off  int64  // file offset of the value bytes
	vlen uint32 // value length
}

// DB is an open database.
type DB struct {
	mu    sync.RWMutex
	path  string
	opts  Options
	f     *os.File
	index map[string]entry
	size  int64 // end of the last valid record = where the next record goes
	live  int64 // bytes of ops that are still live (op header + key + value)
	dead  int64 // bytes of ops that are overwritten, deleted, or tombstones

	closed bool
	broken error // set when the file can no longer be trusted for writes
	info   RecoveryInfo
}

// Open opens or creates the database at path and recovers from any crash.
func Open(path string, opts Options) (*DB, error) {
	start := time.Now()
	if opts.AutoCompactMinBytes == 0 {
		opts.AutoCompactMinBytes = DefaultAutoCompactMinBytes
	}
	db := &DB{path: path, opts: opts, index: make(map[string]entry)}

	// Step 1: a leftover temp file means a compaction was killed BEFORE its
	// rename. The rename is the commit point, so the main file is still the
	// truth and the temp file is garbage. (C3, C5)
	if !opts.ReadOnly {
		err := os.Remove(path + TempSuffix)
		if err == nil {
			db.info.RemovedTemp = true
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("remove leftover temp file: %w", err)
		}
	}

	// Step 2: open (or create) the data file.
	var f *os.File
	var err error
	if opts.ReadOnly {
		f, err = os.Open(path)
	} else {
		_, statErr := os.Stat(path)
		f, err = os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
		if err == nil && errors.Is(statErr, fs.ErrNotExist) {
			// New file: make its directory entry durable too.
			if err := syncDir(filepath.Dir(path)); err != nil {
				f.Close()
				return nil, fmt.Errorf("fsync directory: %w", err)
			}
		}
	}
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}

	// Step 3: scan the log and rebuild the index.
	res, err := scanLog(f, st.Size(), db.applyOp)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("recovery: %w", err)
	}

	// Step 4: cut off the torn tail, then fsync so the cut is durable.
	if res.ValidEnd < res.FileSize && !opts.ReadOnly {
		if err := f.Truncate(res.ValidEnd); err != nil {
			f.Close()
			return nil, fmt.Errorf("truncate torn tail: %w", err)
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return nil, fmt.Errorf("fsync after truncate: %w", err)
		}
		db.info.TruncatedBytes = res.FileSize - res.ValidEnd
	}

	db.f = f
	db.size = res.ValidEnd
	db.info.Keys = len(db.index)
	db.info.Records = res.Records
	db.info.FileSize = res.FileSize
	db.info.ValidEnd = res.ValidEnd
	db.info.Reason = res.Reason
	db.info.Duration = time.Since(start)
	return db, nil
}

// applyOp updates the in-memory index for one op. Used by recovery and by
// the write path, so both build exactly the same state.
func (db *DB) applyOp(op Op, valueOff int64) {
	old, exists := db.index[string(op.Key)]
	if exists {
		cost := opCost(len(op.Key), int(old.vlen))
		db.live -= cost
		db.dead += cost
	}
	switch op.Type {
	case OpPut:
		db.index[string(op.Key)] = entry{off: valueOff, vlen: uint32(len(op.Value))}
		db.live += opCost(len(op.Key), len(op.Value))
	case OpDelete:
		if exists {
			delete(db.index, string(op.Key))
		}
		db.dead += opCost(len(op.Key), 0) // the tombstone itself is garbage after compaction
	}
}

func opCost(klen, vlen int) int64 { return int64(OpHeaderSize + klen + vlen) }

// Info returns what recovery found when the database was opened.
func (db *DB) Info() RecoveryInfo { return db.info }

// Put stores value under key. A put is a batch of one.
func (db *DB) Put(key, value []byte) error { return db.Batch([]Op{PutOp(key, value)}) }

// Delete removes key. Deleting a missing key is not an error.
func (db *DB) Delete(key []byte) error { return db.Batch([]Op{DeleteOp(key)}) }

// Batch applies all ops atomically. Ops run in order, so for the same key
// the LAST op in the batch wins (put x then del x => x is absent).
// When Batch returns nil, the whole batch is on disk (fsync returned).
func (db *DB) Batch(ops []Op) error {
	if err := validateOps(ops); err != nil {
		return err
	}
	rec, valueOffsets, err := encodeRecord(ops)
	if err != nil {
		return err
	}

	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.writableLocked(); err != nil {
		return err
	}

	// Disk-full guard: check BEFORE writing a single byte.
	if db.opts.MaxBytes > 0 && db.size+int64(len(rec)) > db.opts.MaxBytes {
		if isDeleteOnly(ops) {
			// Deletes must always be possible, or a full database could never
			// be emptied. Instead of appending tombstones we rewrite the file
			// without these keys (a compaction that also applies the deletes).
			// The rename inside compaction is the commit point.
			return db.compactLocked(keySet(ops))
		}
		if db.compactionWouldMakeRoomLocked(int64(len(rec))) {
			if err := db.compactLocked(nil); err != nil && db.broken != nil {
				return db.broken
			}
		}
		if db.size+int64(len(rec)) > db.opts.MaxBytes {
			return fmt.Errorf("%w (file %d bytes + record %d bytes > limit %d)",
				ErrFull, db.size, len(rec), db.opts.MaxBytes)
		}
	}

	off := db.size
	// (4) write. We write at an explicit offset, always the end of the last
	// valid record, so a failed earlier write can never leave garbage in the
	// middle of the log.
	if _, err := db.f.WriteAt(rec, off); err != nil {
		db.undoWriteLocked(off)
		return fmt.Errorf("write: %w", err)
	}
	// (5) fsync. After this returns, the record is durable.
	if err := db.f.Sync(); err != nil {
		// After a failed fsync we cannot know what reached the disk, and the
		// OS may have dropped the dirty pages. Refuse further writes; a
		// restart re-reads the file and trusts only what passes the CRC.
		db.undoWriteLocked(off)
		db.broken = fmt.Errorf("fsync failed, restart required: %w", err)
		return fmt.Errorf("fsync: %w", err)
	}
	// (6) update the index. Readers hold RLock, so they cannot see this half done.
	for i, op := range ops {
		db.applyOp(op, off+valueOffsets[i])
	}
	db.size = off + int64(len(rec))

	// Optional housekeeping. The batch is already durable, so a compaction
	// failure here must not turn the batch into an error.
	db.maybeAutoCompactLocked()
	return nil
}

// undoWriteLocked removes a partially written record so the next append
// starts at a clean record boundary.
func (db *DB) undoWriteLocked(off int64) {
	if err := db.f.Truncate(off); err != nil {
		db.broken = fmt.Errorf("could not undo failed write, restart required: %w", err)
	}
}

func (db *DB) writableLocked() error {
	if db.closed {
		return ErrClosed
	}
	if db.opts.ReadOnly {
		return ErrReadOnly
	}
	if db.broken != nil {
		return db.broken
	}
	return nil
}

// Get returns the value for key. found is false if the key does not exist.
func (db *DB) Get(key []byte) (value []byte, found bool, err error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return nil, false, ErrClosed
	}
	e, ok := db.index[string(key)]
	if !ok {
		return nil, false, nil
	}
	buf := make([]byte, e.vlen)
	if e.vlen > 0 {
		if _, err := db.f.ReadAt(buf, e.off); err != nil {
			return nil, false, fmt.Errorf("read value: %w", err)
		}
	}
	return buf, true, nil
}

// Len returns the number of live keys.
func (db *DB) Len() int {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return len(db.index)
}

// Size returns the current length of the data file in bytes.
func (db *DB) Size() int64 {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.size
}

// Stats returns the bytes of live operations and of garbage (overwritten or
// deleted values and tombstones) that a compaction would reclaim.
func (db *DB) Stats() (live, dead int64) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.live, db.dead
}

// MaxBytes returns the configured size limit (0 = none).
func (db *DB) MaxBytes() int64 { return db.opts.MaxBytes }

// Keys returns all live keys, sorted.
func (db *DB) Keys() []string {
	db.mu.RLock()
	keys := make([]string, 0, len(db.index))
	for k := range db.index {
		keys = append(keys, k)
	}
	db.mu.RUnlock()
	sort.Strings(keys)
	return keys
}

// Close closes the file. Every successful write was already fsynced, so
// Close has nothing to flush; the extra Sync is only belt and braces.
// A kill during Close is the same as a kill at any other time (C6).
func (db *DB) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return nil
	}
	db.closed = true
	var syncErr error
	if !db.opts.ReadOnly && db.broken == nil {
		syncErr = db.f.Sync()
	}
	closeErr := db.f.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func isDeleteOnly(ops []Op) bool {
	for _, op := range ops {
		if op.Type != OpDelete {
			return false
		}
	}
	return true
}

func keySet(ops []Op) map[string]struct{} {
	s := make(map[string]struct{}, len(ops))
	for _, op := range ops {
		s[string(op.Key)] = struct{}{}
	}
	return s
}