package database

import (
	"fmt"
	"os"

	"invx/internal/storage"
)

// CheckReport is printed by `kvdb check`.
type CheckReport struct {
	OK       bool
	Reason   string
	Keys     int
	Frames   int
	FileSize int64
}

// Check opens the database (which runs recovery, exactly like any other
// command) and then independently verifies the file:
//   - the header is valid,
//   - every byte after the header belongs to a complete frame with a correct
//     CRC32C and commit marker (no torn tail is left),
//   - an index rebuilt from scratch matches the live index entry by entry,
//   - every value the index points to lies inside the file,
//   - no compaction temp file is left behind.
func Check(path string, opts Options) (CheckReport, error) {
	if _, err := os.Stat(path); err != nil {
		return CheckReport{Reason: err.Error()}, nil
	}
	opts.DisableAutoCompact = true
	db, err := Open(path, opts)
	if err != nil {
		return CheckReport{Reason: "open failed: " + err.Error()}, nil
	}
	defer db.Close()

	db.mu.RLock()
	defer db.mu.RUnlock()

	rep := CheckReport{Keys: len(db.idx.entries), FileSize: db.log.Size()}
	bad := func(format string, a ...any) (CheckReport, error) {
		rep.Reason = fmt.Sprintf(format, a...)
		return rep, nil
	}

	if err := storage.ReadHeader(db.log.File()); err != nil {
		return bad("header: %v", err)
	}
	info, err := db.log.File().Stat()
	if err != nil {
		return rep, err
	}
	if info.Size() != db.log.Size() {
		return bad("file size %d does not match log size %d", info.Size(), db.log.Size())
	}

	fresh := newIndex(len(db.idx.entries))
	res, err := storage.ScanFrames(db.log.File(), info.Size(), fresh.applyScanned)
	if err != nil {
		return rep, err
	}
	rep.Frames = res.Frames
	if res.TornBytes() > 0 {
		return bad("%d invalid bytes at end of file: %s", res.TornBytes(), res.TornReason)
	}
	if len(fresh.entries) != len(db.idx.entries) {
		return bad("rescan found %d keys, index has %d", len(fresh.entries), len(db.idx.entries))
	}
	for k, ref := range db.idx.entries {
		if fresh.entries[k] != ref {
			return bad("index entry for key %q does not match file", k)
		}
		if ref.offset+int64(ref.length) > info.Size() {
			return bad("value of key %q points past end of file", k)
		}
	}
	if _, err := os.Stat(tempPath(path)); err == nil {
		return bad("leftover compaction temp file %s", tempPath(path))
	}
	rep.OK = true
	return rep, nil
}
