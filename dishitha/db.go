package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"sync"
)

const (
	headerSize   = 17
	maxKeySize   = 1024
	maxValueSize = 1 << 20

	recordPut    = byte(1)
	recordDelete = byte(2)
)

var magic = [4]byte{'K', 'V', 'D', '1'}

type valueRef struct {
	offset int64
	length uint32
}

type DB struct {
	mu        sync.RWMutex
	file      *os.File
	index     map[string]valueRef
	endOffset int64
	closed    bool
	failed    bool
}

// OpenDB opens the database and rebuilds its index.
func OpenDB(path string) (*DB, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}

	db := &DB{
		file:  f,
		index: make(map[string]valueRef),
	}

	if err := db.recover(); err != nil {
		f.Close()
		return nil, err
	}

	return db, nil
}

// Create a record with a checksum.
func makeRecord(kind byte, key string, value []byte) []byte {
	record := make([]byte, headerSize+len(key)+len(value))

	copy(record[0:4], magic[:])
	record[4] = kind

	binary.LittleEndian.PutUint32(record[5:9], uint32(len(key)))
	binary.LittleEndian.PutUint32(record[9:13], uint32(len(value)))

	copy(record[headerSize:], key)
	copy(record[headerSize+len(key):], value)

	checksum := crc32.ChecksumIEEE(record[4:13])
	checksum = crc32.Update(checksum, crc32.IEEETable, record[headerSize:])

	binary.LittleEndian.PutUint32(record[13:17], checksum)

	return record
}

// Rebuild the index by scanning the database file.
func (db *DB) recover() error {
	info, err := db.file.Stat()
	if err != nil {
		return err
	}

	size := info.Size()
	var offset int64

	for offset < size {
		remaining := size - offset

		// An incomplete header means a torn final record.
		if remaining < headerSize {
			if err := db.file.Truncate(offset); err != nil {
				return err
			}
			break
		}

		header := make([]byte, headerSize)
		if _, err := db.file.ReadAt(header, offset); err != nil {
			return err
		}

		if string(header[:4]) != string(magic[:]) {
			return fmt.Errorf("invalid record magic at offset %d", offset)
		}

		kind := header[4]
		keyLen := binary.LittleEndian.Uint32(header[5:9])
		valueLen := binary.LittleEndian.Uint32(header[9:13])
		expectedCRC := binary.LittleEndian.Uint32(header[13:17])

		if keyLen == 0 || keyLen > maxKeySize ||
			valueLen > maxValueSize ||
			(kind != recordPut && kind != recordDelete) ||
			(kind == recordDelete && valueLen != 0) {
			return fmt.Errorf("invalid record fields at offset %d", offset)
		}

		recordLen := int64(headerSize) + int64(keyLen) + int64(valueLen)

		// Incomplete record body: discard the unfinished tail.
		if remaining < recordLen {
			if err := db.file.Truncate(offset); err != nil {
				return err
			}
			break
		}

		payload := make([]byte, int(keyLen)+int(valueLen))
		if _, err := db.file.ReadAt(payload, offset+headerSize); err != nil {
			return err
		}

		checksum := crc32.ChecksumIEEE(header[4:13])
		checksum = crc32.Update(checksum, crc32.IEEETable, payload)

		if checksum != expectedCRC {
			return fmt.Errorf("checksum mismatch at offset %d", offset)
		}

		key := string(payload[:keyLen])

		if kind == recordPut {
			db.index[key] = valueRef{
				offset: offset + headerSize + int64(keyLen),
				length: valueLen,
			}
		} else {
			delete(db.index, key)
		}

		offset += recordLen
	}

	db.endOffset = offset

	// Make any tail repair durable before reporting successful open.
	return db.file.Sync()
}

// Append a complete record and make it durable.
func (db *DB) appendRecord(kind byte, key string, value []byte) error {
	record := makeRecord(kind, key, value)
	start := db.endOffset

	written := 0
	for written < len(record) {
		n, err := db.file.WriteAt(record[written:], start+int64(written))
		written += n

		if err != nil {
			if rollbackErr := db.file.Truncate(start); rollbackErr != nil {
				db.failed = true
			}
			return err
		}

		if n == 0 {
			db.failed = true
			return io.ErrShortWrite
		}
	}

	// Do not acknowledge the operation before this succeeds.
	if err := db.file.Sync(); err != nil {
		db.failed = true
		return err
	}

	db.endOffset += int64(len(record))
	return nil
}

// Store or overwrite a key.
func (db *DB) Put(key string, value []byte) error {
	if len(key) == 0 || len(key) > maxKeySize {
		return errors.New("key must contain 1 to 1024 bytes")
	}
	if len(value) > maxValueSize {
		return errors.New("value exceeds 1 MB")
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	if db.closed || db.failed {
		return errors.New("database is not writable")
	}

	if err := db.appendRecord(recordPut, key, value); err != nil {
		return err
	}

	db.index[key] = valueRef{
		offset: db.endOffset - int64(headerSize+len(key)+len(value)) +
			int64(headerSize+len(key)),
		length: uint32(len(value)),
	}

	return nil
}

// Retrieve a value.
func (db *DB) Get(key string) ([]byte, bool, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	if db.closed {
		return nil, false, errors.New("database is closed")
	}

	ref, ok := db.index[key]
	if !ok {
		return nil, false, nil
	}

	value := make([]byte, int(ref.length))
	if len(value) > 0 {
		_, err := db.file.ReadAt(value, ref.offset)
		if err != nil {
			return nil, false, err
		}
	}

	return value, true, nil
}

// Delete a key.
func (db *DB) Delete(key string) error {
	if len(key) == 0 || len(key) > maxKeySize {
		return errors.New("key must contain 1 to 1024 bytes")
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	if db.closed || db.failed {
		return errors.New("database is not writable")
	}

	if err := db.appendRecord(recordDelete, key, nil); err != nil {
		return err
	}

	delete(db.index, key)
	return nil
}

// Flush and close the database.
func (db *DB) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()

	if db.closed {
		return nil
	}

	db.closed = true

	if err := db.file.Sync(); err != nil {
		db.file.Close()
		return err
	}

	return db.file.Close()
}
