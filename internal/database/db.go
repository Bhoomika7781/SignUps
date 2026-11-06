package database

import (
	"errors"
	"sync"

	"invx/internal/storage"
)

type DB struct {
	mu     sync.RWMutex
	log    *storage.Log
	index  *Index
	closed bool
	failed bool
}

// Open opens an existing database or creates a new one.
func Open(path string) (*DB, error) {
	log, err := storage.OpenLog(path)
	if err != nil {
		return nil, err
	}

	db := &DB{
		log:   log,
		index: NewIndex(),
	}

	if err := db.recover(); err != nil {
		log.Close()
		return nil, err
	}

	return db, nil
}

// Put stores or updates a value.
func (db *DB) Put(key string, value []byte) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	if db.closed || db.failed {
		return errors.New("database is not writable")
	}

	record, err := storage.EncodeRecord(storage.RecordPut, key, value)
	if err != nil {
		return err
	}

	offset, err := db.log.Append(record)
	if err != nil {
		db.failed = true
		return err
	}

	valueOffset := offset + int64(storage.HeaderSize) + int64(len(key))

	db.index.Set(key, valueRef{
		offset: valueOffset,
		length: uint32(len(value)),
	})

	return nil
}

// Get retrieves a value using its key.
func (db *DB) Get(key string) ([]byte, bool, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	if db.closed {
		return nil, false, errors.New("database is closed")
	}

	ref, found := db.index.Get(key)
	if !found {
		return nil, false, nil
	}

	value, err := db.log.Read(ref.offset, int(ref.length))
	if err != nil {
		return nil, false, err
	}

	return value, true, nil
}

// Delete removes a key from the database.
func (db *DB) Delete(key string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	if db.closed || db.failed {
		return errors.New("database is not writable")
	}

	record, err := storage.EncodeRecord(storage.RecordDelete, key, nil)
	if err != nil {
		return err
	}

	_, err = db.log.Append(record)
	if err != nil {
		db.failed = true
		return err
	}

	db.index.Delete(key)

	return nil
}

// Close flushes and closes the database.
func (db *DB) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()

	if db.closed {
		return nil
	}

	db.closed = true
	return db.log.Close()
}

// Return all currently stored keys.
func (db *DB) Keys() []string {
	db.mu.RLock()
	defer db.mu.RUnlock()

	if db.closed {
		return nil
	}

	return db.index.Keys()
}

// Return the current database log size.
func (db *DB) LogSize() int64 {
	db.mu.RLock()
	defer db.mu.RUnlock()

	return db.log.Size()
}

// Read bytes from the database log.
func (db *DB) ReadLog(offset int64, size int) ([]byte, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	if db.closed {
		return nil, errors.New("database is closed")
	}

	return db.log.Read(offset, size)
}
