package database

import "sync"

type valueRef struct {
	offset int64
	length uint32
}

type Index struct {
	mu      sync.RWMutex
	entries map[string]valueRef
}

func NewIndex() *Index {
	return &Index{
		entries: make(map[string]valueRef),
	}
}

// Add or update a key in the index.
func (idx *Index) Set(key string, ref valueRef) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	idx.entries[key] = ref
}

// Find a key in the index.
func (idx *Index) Get(key string) (valueRef, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	ref, found := idx.entries[key]
	return ref, found
}

// Remove a key from the index.
func (idx *Index) Delete(key string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	delete(idx.entries, key)
}

// Return the number of stored keys.
func (idx *Index) Len() int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	return len(idx.entries)
}

// Return all stored keys.
func (idx *Index) Keys() []string {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	keys := make([]string, 0, len(idx.entries))

	for key := range idx.entries {
		keys = append(keys, key)
	}

	return keys
}
