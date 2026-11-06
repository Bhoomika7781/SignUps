package engine

// record.go: how one record is laid out on disk.
//
// The data file is nothing but records, one after another. There is no file
// header. Every write (put, delete or batch) becomes exactly ONE record.
//
// Record layout (all integers are little-endian):
//
//	offset  size   field
//	0       4      magic   = the ASCII bytes "KVR1"
//	4       4      plen    = payload length in bytes (uint32)
//	8       4      crc     = CRC-32C (Castagnoli) over the 4 plen bytes followed by the payload
//	12      plen   payload
//
// Payload layout:
//
//	0       4      count   = number of operations in this record (1..10000)
//	then `count` times:
//	        1      type    = 1 put, 2 delete
//	        4      klen    = key length (1..1024)
//	        4      vlen    = value length (0..1048576, always 0 for delete)
//	        klen   key bytes
//	        vlen   value bytes
//
// Why this gives atomicity: the whole batch is covered by ONE checksum.
// If the process dies while the record is being written, the record is
// either incomplete (plen says more bytes than the file has) or its CRC does
// not match. Either way recovery ignores the WHOLE record, so a batch is
// never half visible.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

const (
	RecordMagic  = "KVR1"
	HeaderSize   = 12 // magic + plen + crc
	CountSize    = 4  // op count at the start of the payload
	OpHeaderSize = 9  // type + klen + vlen

	OpPut    byte = 1
	OpDelete byte = 2

	MaxKeySize     = 1024
	MaxValueSize   = 1 << 20 // 1 MB
	MaxBatchOps    = 10000
	MaxPayloadSize = 64 << 20 // 64 MB: largest record we will write or accept
)

// ErrInvalid is returned for operations that break the data limits.
var ErrInvalid = errors.New("invalid operation")

var crcTable = crc32.MakeTable(crc32.Castagnoli)

// Op is one put or delete inside a batch.
type Op struct {
	Type  byte
	Key   []byte
	Value []byte
}

// PutOp and DeleteOp are small helpers for building batches.
func PutOp(key, value []byte) Op { return Op{Type: OpPut, Key: key, Value: value} }
func DeleteOp(key []byte) Op     { return Op{Type: OpDelete, Key: key} }

// validateOps checks the limits from the problem statement before anything
// touches the disk.
func validateOps(ops []Op) error {
	if len(ops) == 0 {
		return fmt.Errorf("%w: empty batch", ErrInvalid)
	}
	if len(ops) > MaxBatchOps {
		return fmt.Errorf("%w: batch has %d ops, limit is %d", ErrInvalid, len(ops), MaxBatchOps)
	}
	for i, op := range ops {
		if len(op.Key) < 1 || len(op.Key) > MaxKeySize {
			return fmt.Errorf("%w: op %d: key length %d, must be 1..%d", ErrInvalid, i, len(op.Key), MaxKeySize)
		}
		switch op.Type {
		case OpPut:
			if len(op.Value) > MaxValueSize {
				return fmt.Errorf("%w: op %d: value length %d, limit is %d", ErrInvalid, i, len(op.Value), MaxValueSize)
			}
		case OpDelete:
		default:
			return fmt.Errorf("%w: op %d: unknown type %d", ErrInvalid, i, op.Type)
		}
	}
	return nil
}

func valueLen(op Op) int {
	if op.Type == OpPut {
		return len(op.Value)
	}
	return 0
}

// encodeRecord builds the full record bytes for a batch.
// It also returns, for each op, where its value starts, counted from the
// first byte of the record. The caller adds the record's file offset to get
// the value's file offset for the in-memory index.
func encodeRecord(ops []Op) ([]byte, []int64, error) {
	plen := CountSize
	for _, op := range ops {
		plen += OpHeaderSize + len(op.Key) + valueLen(op)
	}
	if plen > MaxPayloadSize {
		return nil, nil, fmt.Errorf("%w: batch is %d bytes, limit is %d", ErrInvalid, plen, MaxPayloadSize)
	}

	buf := make([]byte, HeaderSize+plen)
	copy(buf[0:4], RecordMagic)
	binary.LittleEndian.PutUint32(buf[4:8], uint32(plen))
	binary.LittleEndian.PutUint32(buf[HeaderSize:HeaderSize+4], uint32(len(ops)))

	valueOffsets := make([]int64, len(ops))
	pos := HeaderSize + CountSize
	for i, op := range ops {
		vlen := valueLen(op)
		buf[pos] = op.Type
		binary.LittleEndian.PutUint32(buf[pos+1:pos+5], uint32(len(op.Key)))
		binary.LittleEndian.PutUint32(buf[pos+5:pos+9], uint32(vlen))
		pos += OpHeaderSize
		pos += copy(buf[pos:], op.Key)
		valueOffsets[i] = int64(pos)
		if vlen > 0 {
			pos += copy(buf[pos:], op.Value)
		}
	}

	// The CRC is computed last, over the length field and the finished payload.
	binary.LittleEndian.PutUint32(buf[8:12], recordCRC(buf[4:8], buf[HeaderSize:]))
	return buf, valueOffsets, nil
}

// recordCRC covers the length field as well as the payload, so a damaged
// length field is caught too.
func recordCRC(lenField, payload []byte) uint32 {
	c := crc32.Update(0, crcTable, lenField)
	return crc32.Update(c, crcTable, payload)
}

// decodePayload parses a payload whose CRC already matched. It still checks
// every length, so a bad record can never make us read out of bounds.
// The returned ops point into `payload` (no copying). valueOffsets are
// counted from the first byte of the record (so they include HeaderSize).
// ops and offs are reused buffers to avoid allocations during recovery.
func decodePayload(payload []byte, ops []Op, offs []int64) ([]Op, []int64, error) {
	ops, offs = ops[:0], offs[:0]
	if len(payload) < CountSize {
		return ops, offs, errors.New("payload shorter than op count")
	}
	count := int(binary.LittleEndian.Uint32(payload[0:4]))
	if count < 1 || count > MaxBatchOps {
		return ops, offs, fmt.Errorf("bad op count %d", count)
	}
	pos := CountSize
	for i := 0; i < count; i++ {
		if len(payload)-pos < OpHeaderSize {
			return ops, offs, fmt.Errorf("op %d: header runs past payload", i)
		}
		typ := payload[pos]
		klen := int(binary.LittleEndian.Uint32(payload[pos+1 : pos+5]))
		vlen := int(binary.LittleEndian.Uint32(payload[pos+5 : pos+9]))
		pos += OpHeaderSize
		if typ != OpPut && typ != OpDelete {
			return ops, offs, fmt.Errorf("op %d: bad type %d", i, typ)
		}
		if klen < 1 || klen > MaxKeySize || vlen > MaxValueSize || (typ == OpDelete && vlen != 0) {
			return ops, offs, fmt.Errorf("op %d: bad lengths klen=%d vlen=%d", i, klen, vlen)
		}
		if len(payload)-pos < klen+vlen {
			return ops, offs, fmt.Errorf("op %d: data runs past payload", i)
		}
		key := payload[pos : pos+klen]
		pos += klen
		offs = append(offs, int64(HeaderSize+pos))
		val := payload[pos : pos+vlen]
		pos += vlen
		ops = append(ops, Op{Type: typ, Key: key, Value: val})
	}
	if pos != len(payload) {
		return ops, offs, fmt.Errorf("%d trailing bytes after last op", len(payload)-pos)
	}
	return ops, offs, nil
}
