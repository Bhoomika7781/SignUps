package database

import (
	"encoding/binary"
	"errors"
	"fmt"

	"invx/internal/storage"
)

func (db *DB) recover() error {
	size := db.log.Size()
	var offset int64

	for offset < size {
		remaining := size - offset

		// Check whether a complete header exists.
		if remaining < storage.HeaderSize {
			return db.log.Truncate(offset)
		}

		header, err := db.log.Read(offset, storage.HeaderSize)
		if err != nil {
			return err
		}

		// Verify record magic.
		if string(header[:4]) != string(storage.Magic[:]) {
			return fmt.Errorf("invalid record magic at offset %d", offset)
		}

		kind := header[4]

		keyLen := binary.LittleEndian.Uint32(header[5:9])
		valueLen := binary.LittleEndian.Uint32(header[9:13])
		expectedCRC := binary.LittleEndian.Uint32(header[13:17])

		if keyLen == 0 || keyLen > storage.MaxKeySize {
			return errors.New("invalid key length")
		}

		if valueLen > storage.MaxValueSize {
			return errors.New("invalid value length")
		}

		if kind != storage.RecordPut && kind != storage.RecordDelete {
			return errors.New("invalid record type")
		}

		if kind == storage.RecordDelete && valueLen != 0 {
			return errors.New("delete record contains a value")
		}

		recordLength := int64(storage.HeaderSize) +
			int64(keyLen) + int64(valueLen)

		// Remove incomplete final records.
		if remaining < recordLength {
			return db.log.Truncate(offset)
		}

		body, err := db.log.Read(
			offset+storage.HeaderSize,
			int(keyLen+valueLen),
		)
		if err != nil {
			return err
		}

		// Reconstruct the record and verify its checksum.
		record, err := storage.EncodeRecord(
			kind,
			string(body[:keyLen]),
			body[keyLen:],
		)
		if err != nil {
			return err
		}

		actualCRC := binary.LittleEndian.Uint32(record[13:17])

		if actualCRC != expectedCRC {
			return fmt.Errorf("checksum mismatch at offset %d", offset)
		}

		key := string(body[:keyLen])

		if kind == storage.RecordPut {
			db.index.Set(key, valueRef{
				offset: offset + int64(storage.HeaderSize) + int64(keyLen),
				length: valueLen,
			})
		} else {
			db.index.Delete(key)
		}

		offset += recordLength
	}

	return nil
}
