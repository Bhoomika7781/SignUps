package database

import "invx/internal/storage"

// valueRef says where a key's current value lives in the data file.
// The index is a plain map protected by DB.mu; it has no lock of its own.
type valueRef struct {
	offset int64  // absolute file offset of the value bytes
	length uint32 // value length
	keyLen uint16 // kept so we can track live bytes for compaction
}

// liveSize is how many payload bytes this key would take in a compacted file.
func (r valueRef) liveSize() int64 {
	return int64(storage.OpHeaderSize) + int64(r.keyLen) + int64(r.length)
}

type index struct {
	entries   map[string]valueRef
	liveBytes int64
}

func newIndex(sizeHint int) *index {
	return &index{entries: make(map[string]valueRef, sizeHint)}
}

func (ix *index) set(key string, ref valueRef) {
	if old, ok := ix.entries[key]; ok {
		ix.liveBytes -= old.liveSize()
	}
	ix.entries[key] = ref
	ix.liveBytes += ref.liveSize()
}

func (ix *index) del(key string) {
	if old, ok := ix.entries[key]; ok {
		ix.liveBytes -= old.liveSize()
		delete(ix.entries, key)
	}
}

// applyScanned applies all ops of one frame in order. Later ops in the same
// frame win, so "put X; del X" leaves X deleted and "del X; put X" leaves X set.
func (ix *index) applyScanned(ops []storage.ScannedOp) {
	for _, op := range ops {
		if op.Kind == storage.OpPut {
			ix.set(string(op.Key), valueRef{offset: op.ValueOffset, length: op.ValueLen, keyLen: uint16(len(op.Key))})
		} else {
			ix.del(string(op.Key))
		}
	}
}
