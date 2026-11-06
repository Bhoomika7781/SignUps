package database

import (
	"errors"

	"invx/internal/storage"
)

// Batch collects puts and deletes and applies them atomically with Commit.
//
// Ordering rule (scenario B5): ops are applied in the order they were added.
// If a batch puts X and later deletes X, X ends up deleted. If it deletes X
// and later puts X, X ends up with the put value. Recovery replays the ops of
// a frame in the same order, so the rule holds after a crash too.
type Batch struct {
	db        *DB
	ops       []storage.Op
	committed bool
}

// NewBatch starts an empty batch.
func (db *DB) NewBatch() *Batch { return &Batch{db: db} }

// Put adds a put. The key and value are copied.
func (b *Batch) Put(key, value []byte) error {
	op := storage.Op{Kind: storage.OpPut, Key: append([]byte(nil), key...), Value: append([]byte(nil), value...)}
	if err := storage.ValidateOp(op); err != nil {
		return err
	}
	b.ops = append(b.ops, op)
	return nil
}

// Delete adds a delete. Deleting a missing key is not an error.
func (b *Batch) Delete(key []byte) error {
	op := storage.Op{Kind: storage.OpDelete, Key: append([]byte(nil), key...)}
	if err := storage.ValidateOp(op); err != nil {
		return err
	}
	b.ops = append(b.ops, op)
	return nil
}

// Len is the number of ops in the batch.
func (b *Batch) Len() int { return len(b.ops) }

// Commit writes the whole batch as ONE frame with ONE fsync. Either the whole
// frame is valid on disk (CRC + commit marker) or recovery discards all of it.
func (b *Batch) Commit() error {
	if b.committed {
		return errors.New("batch already committed")
	}
	if err := b.db.write(b.ops); err != nil {
		return err
	}
	b.committed = true
	return nil
}
