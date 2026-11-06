package engine

// recover.go: reading the log back from disk.
//
// Recovery algorithm in plain English:
//  1. Start at byte 0.
//  2. Read a 12-byte header. If fewer than 12 bytes are left, stop (torn header).
//  3. If the magic is not "KVR1", stop.
//  4. If plen is bigger than the bytes left in the file, stop (torn payload).
//  5. Read the payload and check the CRC. If it does not match, stop.
//  6. Apply every op of the record to the in-memory index, then move to the
//     next record.
//  7. Where we stopped is the "valid end". Everything after it is a torn tail
//     from a write that never returned success. Open() truncates the file to
//     the valid end and fsyncs.
//
// A record is applied only after ALL its bytes passed the CRC check, so a
// batch is applied fully or not at all.
// Scanning does not change the file, so a kill during the scan changes
// nothing. Truncating to the same valid end twice gives the same result, so
// a kill during the truncate is also safe: the next open just does it again.

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
)

// ScanResult describes what the scan found.
type ScanResult struct {
	Records  int    // number of valid records
	ValidEnd int64  // offset just after the last valid record
	FileSize int64  // size of the file when the scan started
	Reason   string // why the scan stopped before FileSize ("" if the file is clean)
}

// scanLog reads records from offset 0 to size and calls apply for every op
// of every valid record, in file order. valueOff is the file offset of the
// op's value bytes.
func scanLog(r io.ReaderAt, size int64, apply func(op Op, valueOff int64)) (ScanResult, error) {
	res := ScanResult{FileSize: size}
	br := bufio.NewReaderSize(io.NewSectionReader(r, 0, size), 4<<20)

	var hdr [HeaderSize]byte
	var payload []byte
	var ops []Op
	var offs []int64
	var off int64

	for off < size {
		remaining := size - off
		if remaining < HeaderSize {
			res.Reason = fmt.Sprintf("torn header at offset %d: %d of %d bytes present", off, remaining, HeaderSize)
			break
		}
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			return res, fmt.Errorf("read header at %d: %w", off, err)
		}
		if string(hdr[0:4]) != RecordMagic {
			res.Reason = fmt.Sprintf("bad magic %q at offset %d", hdr[0:4], off)
			break
		}
		plen := int64(binary.LittleEndian.Uint32(hdr[4:8]))
		if plen > MaxPayloadSize {
			res.Reason = fmt.Sprintf("impossible payload length %d at offset %d", plen, off)
			break
		}
		if plen > remaining-HeaderSize {
			res.Reason = fmt.Sprintf("torn record at offset %d: needs %d payload bytes, only %d present",
				off, plen, remaining-HeaderSize)
			break
		}
		if int64(cap(payload)) < plen {
			payload = make([]byte, plen)
		}
		payload = payload[:plen]
		if _, err := io.ReadFull(br, payload); err != nil {
			return res, fmt.Errorf("read payload at %d: %w", off, err)
		}
		if recordCRC(hdr[4:8], payload) != binary.LittleEndian.Uint32(hdr[8:12]) {
			res.Reason = fmt.Sprintf("checksum mismatch in record at offset %d", off)
			break
		}
		var err error
		ops, offs, err = decodePayload(payload, ops, offs)
		if err != nil {
			res.Reason = fmt.Sprintf("bad payload in record at offset %d: %v", off, err)
			break
		}
		// Only now, with the whole record verified, do we apply it.
		for i := range ops {
			apply(ops[i], off+offs[i])
		}
		off += HeaderSize + plen
		res.Records++
	}
	res.ValidEnd = off
	return res, nil
}
