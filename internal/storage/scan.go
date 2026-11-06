package storage

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
)

// ScanResult describes what a scan found.
type ScanResult struct {
	ValidEnd   int64  // offset just after the last valid frame
	FileSize   int64  // size of the file when the scan started
	Frames     int    // number of valid frames
	Ops        int    // number of ops inside valid frames
	TornReason string // why the bytes after ValidEnd were rejected ("" if none)
}

// TornBytes is how many bytes at the end of the file are not part of a valid frame.
func (r ScanResult) TornBytes() int64 { return r.FileSize - r.ValidEnd }

// ScanFrames reads every frame after the file header, in order. For each valid
// frame it calls apply with all of the frame's ops at once (so a caller can
// never see half a batch). It stops at the first frame that is incomplete,
// has a bad CRC or has no commit marker, and reports that offset in ValidEnd.
// It never modifies the file.
func ScanFrames(f *os.File, size int64, apply func(ops []ScannedOp)) (ScanResult, error) {
	res := ScanResult{ValidEnd: FileHeaderSize, FileSize: size}
	if size <= FileHeaderSize {
		return res, nil
	}

	r := bufio.NewReaderSize(io.NewSectionReader(f, FileHeaderSize, size-FileHeaderSize), 4<<20)
	header := make([]byte, FrameHeaderSize)
	trailer := make([]byte, FrameTrailerSize)
	var payload []byte
	var ops []ScannedOp
	off := int64(FileHeaderSize)

	torn := func(reason string) (ScanResult, error) {
		res.ValidEnd = off
		res.TornReason = fmt.Sprintf("%s at offset %d", reason, off)
		return res, nil
	}

	for off < size {
		remaining := size - off
		if remaining < FrameOverhead {
			return torn("partial frame header")
		}
		if _, err := io.ReadFull(r, header); err != nil {
			return res, err
		}
		if string(header[0:4]) != FrameMagic {
			return torn("bad frame magic")
		}
		plen := int64(binary.LittleEndian.Uint32(header[4:8]))
		count := binary.LittleEndian.Uint32(header[8:12])
		if plen > MaxPayloadSize || count == 0 || count > MaxBatchOps {
			return torn("impossible frame length or op count")
		}
		if remaining < FrameOverhead+plen {
			return torn("incomplete frame (torn write)")
		}
		if int64(cap(payload)) < plen {
			payload = make([]byte, plen)
		}
		payload = payload[:plen]
		if _, err := io.ReadFull(r, payload); err != nil {
			return res, err
		}
		if _, err := io.ReadFull(r, trailer); err != nil {
			return res, err
		}

		crc := Checksum(header)
		crc = crcUpdate(crc, payload)
		if binary.LittleEndian.Uint32(trailer[0:4]) != crc {
			return torn("frame CRC mismatch")
		}
		if string(trailer[4:8]) != CommitMarker {
			return torn("missing commit marker")
		}

		var err error
		ops, err = parsePayload(payload, count, off+FrameHeaderSize, ops)
		if err != nil {
			return torn("malformed payload: " + err.Error())
		}

		apply(ops)
		res.Frames++
		res.Ops += len(ops)
		off += FrameOverhead + plen
		res.ValidEnd = off
	}
	return res, nil
}

func crcUpdate(crc uint32, b []byte) uint32 { return crc32.Update(crc, crcTable, b) }

// ReadHeader reads and validates the 16-byte file header.
func ReadHeader(f *os.File) error {
	h := make([]byte, FileHeaderSize)
	if _, err := f.ReadAt(h, 0); err != nil {
		if errors.Is(err, io.EOF) {
			return ErrBadHeader
		}
		return err
	}
	return CheckHeader(h)
}
