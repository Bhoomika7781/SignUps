// Package storage owns the on-disk format of an invx database file.
//
// File layout (all integers little-endian):
//
//	+------------------------------+  offset 0
//	| File header (16 bytes)       |
//	|   0  8  magic  "INVXDB01"    |
//	|   8  4  format version (=1)  |
//	|  12  4  CRC32C of bytes 0..11|
//	+------------------------------+  offset 16
//	| Frame 1                      |
//	| Frame 2                      |
//	| ...                          |
//	+------------------------------+  end of file
//
// Every write (a single put, a single delete, or a whole batch) is exactly ONE
// frame, written with one WriteAt call and made durable with one fsync.
//
//	Frame:
//	   0  4  frame magic "FRM1"
//	   4  4  payload length P
//	   8  4  op count N (1..10000)
//	  12  P  payload = N ops back to back
//	12+P  4  CRC32C over bytes [0, 12+P) of this frame
//	16+P  4  commit marker "CMIT"
//
//	Op (inside payload):
//	   0  1  kind (1 = put, 2 = delete)
//	   1  2  key length K (1..1024)
//	   3  4  value length V (0..1048576, always 0 for delete)
//	   7  K  key bytes
//	 7+K  V  value bytes
//
// A frame counts only if it is complete, its CRC matches and its commit
// marker is present. Anything else at the end of the file is a torn write
// from a crashed process and recovery cuts it off.
package storage

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

const (
	FileMagic      = "INVXDB01"
	FormatVersion  = 1
	FileHeaderSize = 16

	FrameMagic       = "FRM1"
	CommitMarker     = "CMIT"
	FrameHeaderSize  = 12
	FrameTrailerSize = 8
	FrameOverhead    = FrameHeaderSize + FrameTrailerSize

	OpHeaderSize = 7

	OpPut    = byte(1)
	OpDelete = byte(2)

	MaxKeySize   = 1024
	MaxValueSize = 1 << 20
	MaxBatchOps  = 10000

	// MaxPayloadSize bounds one frame so that a corrupt length field can never
	// make recovery allocate gigabytes. A batch of 10,000 x 1 MB values does
	// not fit and is rejected up front (documented in BREAKING.md).
	MaxPayloadSize = 256 << 20
)

var (
	ErrKeySize     = errors.New("key must be 1 to 1024 bytes")
	ErrValueSize   = errors.New("value must be at most 1 MB (1048576 bytes)")
	ErrBatchEmpty  = errors.New("batch has no operations")
	ErrBatchTooBig = errors.New("batch too large (max 10000 ops and 256 MB payload)")
	ErrBadHeader   = errors.New("file header is not a valid invx header")
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

// Checksum is CRC32C (Castagnoli), hardware accelerated on x86 and ARM.
func Checksum(b []byte) uint32 { return crc32.Checksum(b, crcTable) }

// Op is one put or delete.
type Op struct {
	Kind  byte
	Key   []byte
	Value []byte
}

// ValidateOp checks the data limits from the problem statement.
func ValidateOp(op Op) error {
	if len(op.Key) < 1 || len(op.Key) > MaxKeySize {
		return ErrKeySize
	}
	switch op.Kind {
	case OpPut:
		if len(op.Value) > MaxValueSize {
			return ErrValueSize
		}
	case OpDelete:
		if len(op.Value) != 0 {
			return errors.New("delete cannot carry a value")
		}
	default:
		return fmt.Errorf("unknown op kind %d", op.Kind)
	}
	return nil
}

// EncodeHeader returns the 16-byte file header.
func EncodeHeader() []byte {
	h := make([]byte, FileHeaderSize)
	copy(h[0:8], FileMagic)
	binary.LittleEndian.PutUint32(h[8:12], FormatVersion)
	binary.LittleEndian.PutUint32(h[12:16], Checksum(h[0:12]))
	return h
}

// CheckHeader validates a 16-byte file header.
func CheckHeader(h []byte) error {
	if len(h) != FileHeaderSize ||
		string(h[0:8]) != FileMagic ||
		binary.LittleEndian.Uint32(h[8:12]) != FormatVersion ||
		binary.LittleEndian.Uint32(h[12:16]) != Checksum(h[0:12]) {
		return ErrBadHeader
	}
	return nil
}

// EncodeFrame turns a list of ops into one frame. It also returns, for every
// op, the offset of its value bytes relative to the start of the frame, so the
// caller can update the in-memory index after the frame is durable.
func EncodeFrame(ops []Op) ([]byte, []int64, error) {
	if len(ops) == 0 {
		return nil, nil, ErrBatchEmpty
	}
	if len(ops) > MaxBatchOps {
		return nil, nil, ErrBatchTooBig
	}
	payload := 0
	for _, op := range ops {
		if err := ValidateOp(op); err != nil {
			return nil, nil, err
		}
		payload += OpHeaderSize + len(op.Key) + len(op.Value)
	}
	if payload > MaxPayloadSize {
		return nil, nil, ErrBatchTooBig
	}

	frame := make([]byte, FrameOverhead+payload)
	copy(frame[0:4], FrameMagic)
	binary.LittleEndian.PutUint32(frame[4:8], uint32(payload))
	binary.LittleEndian.PutUint32(frame[8:12], uint32(len(ops)))

	valueOffsets := make([]int64, len(ops))
	p := FrameHeaderSize
	for i, op := range ops {
		frame[p] = op.Kind
		binary.LittleEndian.PutUint16(frame[p+1:p+3], uint16(len(op.Key)))
		binary.LittleEndian.PutUint32(frame[p+3:p+7], uint32(len(op.Value)))
		p += OpHeaderSize
		p += copy(frame[p:], op.Key)
		valueOffsets[i] = int64(p)
		p += copy(frame[p:], op.Value)
	}

	binary.LittleEndian.PutUint32(frame[p:p+4], Checksum(frame[:p]))
	copy(frame[p+4:p+8], CommitMarker)
	return frame, valueOffsets, nil
}

// ScannedOp is one op found while scanning a frame. Key points into a buffer
// that is reused, so callers must copy it if they keep it.
type ScannedOp struct {
	Kind        byte
	Key         []byte
	ValueOffset int64 // absolute offset of the value in the file
	ValueLen    uint32
}

// parsePayload splits a CRC-verified payload into ops. It still checks every
// length, so a bug can never make it read outside the payload.
func parsePayload(payload []byte, count uint32, payloadFileOffset int64, out []ScannedOp) ([]ScannedOp, error) {
	out = out[:0]
	p := 0
	for i := uint32(0); i < count; i++ {
		if len(payload)-p < OpHeaderSize {
			return nil, errors.New("op header runs past payload")
		}
		kind := payload[p]
		klen := int(binary.LittleEndian.Uint16(payload[p+1 : p+3]))
		vlen := int(binary.LittleEndian.Uint32(payload[p+3 : p+7]))
		p += OpHeaderSize
		if (kind != OpPut && kind != OpDelete) || klen < 1 || klen > MaxKeySize ||
			vlen > MaxValueSize || (kind == OpDelete && vlen != 0) {
			return nil, errors.New("invalid op fields")
		}
		if len(payload)-p < klen+vlen {
			return nil, errors.New("op body runs past payload")
		}
		out = append(out, ScannedOp{
			Kind:        kind,
			Key:         payload[p : p+klen],
			ValueOffset: payloadFileOffset + int64(p+klen),
			ValueLen:    uint32(vlen),
		})
		p += klen + vlen
	}
	if p != len(payload) {
		return nil, errors.New("payload has trailing bytes")
	}
	return out, nil
}
