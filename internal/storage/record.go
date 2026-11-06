package storage

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
)

const (
	HeaderSize   = 17
	MaxKeySize   = 1024
	MaxValueSize = 1 << 20

	RecordPut    = byte(1)
	RecordDelete = byte(2)
)

var Magic = [4]byte{'K', 'V', 'D', '1'}

type Record struct {
	Kind  byte
	Key   string
	Value []byte
}

func EncodeRecord(kind byte, key string, value []byte) ([]byte, error) {
	if len(key) == 0 || len(key) > MaxKeySize {
		return nil, errors.New("key must contain 1 to 1024 bytes")
	}

	if len(value) > MaxValueSize {
		return nil, errors.New("value exceeds 1 MB")
	}

	if kind != RecordPut && kind != RecordDelete {
		return nil, errors.New("invalid record type")
	}

	if kind == RecordDelete && len(value) != 0 {
		return nil, errors.New("delete record cannot contain a value")
	}

	record := make([]byte, HeaderSize+len(key)+len(value))

	copy(record[0:4], Magic[:])
	record[4] = kind

	binary.LittleEndian.PutUint32(record[5:9], uint32(len(key)))
	binary.LittleEndian.PutUint32(record[9:13], uint32(len(value)))

	copy(record[HeaderSize:], key)
	copy(record[HeaderSize+len(key):], value)

	checksum := crc32.ChecksumIEEE(record[4:13])
	checksum = crc32.Update(checksum, crc32.IEEETable, record[HeaderSize:])

	binary.LittleEndian.PutUint32(record[13:17], checksum)

	return record, nil
}
