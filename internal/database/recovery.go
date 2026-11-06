package database

import (
	"invx/internal/storage"
)

// RecoveryStats is reported by `kvdb open`.
type RecoveryStats struct {
	Frames       int
	Ops          int
	Keys         int
	TornBytes    int64  // bytes cut off the end of the file
	TornReason   string // why they were cut ("" if nothing was cut)
	RemovedTemp  bool   // a leftover compaction temp file was deleted
	PromotedTemp bool   // main file was missing; complete temp file was renamed into place
	FileSize     int64  // size after recovery
	DurationNano int64
}

// recover rebuilds the in-memory index from the data file.
//
// Algorithm:
//  1. Scan frames from offset 16. Each frame that is complete, has a matching
//     CRC32C and ends with "CMIT" is applied to the index as a whole.
//  2. Stop at the first frame that fails any check. Everything from there to
//     the end of the file is a torn write from a crash: it was never fsynced
//     and ACKed, because appends are serialised and each one is fsynced before
//     the next one starts.
//  3. ftruncate the file to the end of the last good frame and fsync.
//
// Recovery never writes anything except that truncate, and truncating to the
// same offset again is a no-op, so a crash at any point inside recovery just
// means the next open() does the same work again and reaches the same state.
func (db *DB) recover() error {
	res, err := storage.ScanFrames(db.log.File(), db.log.Size(), db.idx.applyScanned)
	if err != nil {
		return err
	}
	db.stats.Frames = res.Frames
	db.stats.Ops = res.Ops
	db.stats.TornBytes = res.TornBytes()
	db.stats.TornReason = res.TornReason

	if res.TornBytes() > 0 {
		if err := db.log.Truncate(res.ValidEnd); err != nil {
			return err
		}
	}
	db.stats.Keys = len(db.idx.entries)
	db.stats.FileSize = db.log.Size()
	return nil
}
