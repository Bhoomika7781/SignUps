package database

import (
	"errors"

	"invx/internal/storage"
)

type batchOperation struct {
	kind  byte
	key   string
	value []byte
}

type Batch struct {
	db         *DB
	operations []batchOperation
	committed  bool
}

// Create a new batch.
func (db *DB) NewBatch() *Batch {
	return &Batch{
		db: db,
	}
}

// Add a PUT operation.
func (b *Batch) Put(key string, value []byte) error {
	if b.committed {
		return errors.New("batch already committed")
	}

	if _, err := storage.EncodeRecord(storage.RecordPut, key, value); err != nil {
		return err
	}

	valueCopy := append([]byte(nil), value...)

	b.operations = append(b.operations, batchOperation{
		kind:  storage.RecordPut,
		key:   key,
		value: valueCopy,
	})

	return nil
}

// Add a DELETE operation.
func (b *Batch) Delete(key string) error {
	if b.committed {
		return errors.New("batch already committed")
	}

	if _, err := storage.EncodeRecord(storage.RecordDelete, key, nil); err != nil {
		return err
	}

	b.operations = append(b.operations, batchOperation{
		kind: storage.RecordDelete,
		key:  key,
	})

	return nil
}

// Execute all operations in the batch.
func (b *Batch) Commit() error {
	if b.committed {
		return errors.New("batch already committed")
	}

	if b.db == nil {
		return errors.New("database is nil")
	}

	db := b.db

	db.mu.Lock()
	defer db.mu.Unlock()

	if db.closed || db.failed {
		return errors.New("database is not writable")
	}

	// Validate and encode all records before writing.
	records := make([][]byte, len(b.operations))

	for i, op := range b.operations {
		record, err := storage.EncodeRecord(op.kind, op.key, op.value)
		if err != nil {
			return err
		}
		records[i] = record
	}

	// Append records and update the index.
	for i, op := range b.operations {
		offset, err := db.log.Append(records[i])
		if err != nil {
			db.failed = true
			return err
		}

		if op.kind == storage.RecordPut {
			db.index.Set(op.key, valueRef{
				offset: offset + int64(storage.HeaderSize) + int64(len(op.key)),
				length: uint32(len(op.value)),
			})
		} else {
			db.index.Delete(op.key)
		}
	}

	b.committed = true
	return nil
}
