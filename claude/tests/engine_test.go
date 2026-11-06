package tests

// Run with:  go test ./tests/ -v

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"kvdb/src/engine"
)

func open(t *testing.T, path string, opts engine.Options) *engine.DB {
	t.Helper()
	db, err := engine.Open(path, opts)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return db
}

func mustGet(t *testing.T, db *engine.DB, key string) (string, bool) {
	t.Helper()
	v, ok, err := db.Get([]byte(key))
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	return string(v), ok
}

func TestPutGetDeleteReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	db := open(t, path, engine.Options{})
	if err := db.Put([]byte("a"), []byte("1")); err != nil {
		t.Fatal(err)
	}
	if err := db.Put([]byte("b"), []byte("")); err != nil { // empty value is legal
		t.Fatal(err)
	}
	if err := db.Put([]byte("a"), []byte("2")); err != nil {
		t.Fatal(err)
	}
	if err := db.Delete([]byte("missing")); err != nil { // not an error
		t.Fatal(err)
	}
	db.Close()

	db = open(t, path, engine.Options{})
	defer db.Close()
	if v, ok := mustGet(t, db, "a"); !ok || v != "2" {
		t.Fatalf("a = %q %v", v, ok)
	}
	if v, ok := mustGet(t, db, "b"); !ok || v != "" {
		t.Fatalf("b = %q %v", v, ok)
	}
	db.Delete([]byte("a"))
	if _, ok := mustGet(t, db, "a"); ok {
		t.Fatal("a should be gone")
	}
}

// TestTornTailAtEveryByte simulates a kill at every possible byte of the
// last record (A2, B1, B2, F4): the file is cut at each length, then opened.
func TestTornTailAtEveryByte(t *testing.T) {
	dir := t.TempDir()
	full := filepath.Join(dir, "full")
	db := open(t, full, engine.Options{})
	db.Put([]byte("a"), []byte("first"))
	db.Batch([]engine.Op{engine.PutOp([]byte("b"), []byte("bee")), engine.PutOp([]byte("c"), []byte("sea"))})
	before := db.Size()
	// last record: a batch of 3 with a 3 KB value
	db.Batch([]engine.Op{
		engine.PutOp([]byte("d"), bytes.Repeat([]byte("x"), 3000)),
		engine.DeleteOp([]byte("a")),
		engine.PutOp([]byte("e"), []byte("eee")),
	})
	after := db.Size()
	db.Close()
	data, _ := os.ReadFile(full)

	for cut := before; cut < after; cut++ {
		p := filepath.Join(dir, fmt.Sprintf("cut%d", cut))
		os.WriteFile(p, data[:cut], 0o644)
		db := open(t, p, engine.Options{})
		if db.Size() != before {
			t.Fatalf("cut %d: size after recovery %d, want %d", cut, db.Size(), before)
		}
		for k, want := range map[string]string{"a": "first", "b": "bee", "c": "sea"} {
			if v, ok := mustGet(t, db, k); !ok || v != want {
				t.Fatalf("cut %d: %s = %q %v, want %q", cut, k, v, ok, want)
			}
		}
		for _, k := range []string{"d", "e"} {
			if _, ok := mustGet(t, db, k); ok {
				t.Fatalf("cut %d: %s visible from a torn batch", cut, k)
			}
		}
		db.Close()
		// open again: recovery must be idempotent (F2)
		db = open(t, p, engine.Options{})
		if db.Size() != before || db.Info().TruncatedBytes != 0 {
			t.Fatalf("cut %d: second open not idempotent", cut)
		}
		db.Close()
		if rep, err := engine.Check(p); err != nil || !rep.OK {
			t.Fatalf("cut %d: check after open: %+v %v", cut, rep, err)
		}
	}
}

// TestGarbageTail: random bytes after the last record are ignored.
func TestGarbageTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	db := open(t, path, engine.Options{})
	db.Put([]byte("k"), []byte("v"))
	db.Close()
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.Write([]byte("KVR1\xff\xff\x00\x00garbage-garbage-garbage"))
	f.Close()
	db = open(t, path, engine.Options{})
	defer db.Close()
	if v, ok := mustGet(t, db, "k"); !ok || v != "v" {
		t.Fatalf("k = %q %v", v, ok)
	}
}

// TestBatchOrdering documents B5: inside a batch the last op on a key wins.
func TestBatchOrdering(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	db := open(t, path, engine.Options{})
	db.Batch([]engine.Op{engine.PutOp([]byte("x"), []byte("1")), engine.DeleteOp([]byte("x"))})
	db.Batch([]engine.Op{engine.DeleteOp([]byte("y")), engine.PutOp([]byte("y"), []byte("2"))})
	db.Close()
	db = open(t, path, engine.Options{})
	defer db.Close()
	if _, ok := mustGet(t, db, "x"); ok {
		t.Fatal("put x; del x should leave x absent")
	}
	if v, ok := mustGet(t, db, "y"); !ok || v != "2" {
		t.Fatal("del y; put y should leave y = 2")
	}
}

func TestLimits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	db := open(t, path, engine.Options{})
	defer db.Close()
	bad := []error{
		db.Put([]byte(""), []byte("v")),
		db.Put(bytes.Repeat([]byte("k"), 1025), []byte("v")),
		db.Put([]byte("k"), make([]byte, (1<<20)+1)),
		db.Batch(make([]engine.Op, 10001)),
		db.Batch(nil),
	}
	for i, err := range bad {
		if !errors.Is(err, engine.ErrInvalid) {
			t.Errorf("case %d: want ErrInvalid, got %v", i, err)
		}
	}
	if err := db.Put(bytes.Repeat([]byte("k"), 1024), make([]byte, 1<<20)); err != nil {
		t.Fatalf("1024-byte key with 1 MB value must work: %v", err)
	}
	ops := make([]engine.Op, 10000)
	for i := range ops {
		ops[i] = engine.PutOp([]byte(fmt.Sprintf("b%d", i)), []byte("v"))
	}
	if err := db.Batch(ops); err != nil {
		t.Fatalf("10,000-op batch must work: %v", err)
	}
}

// TestMaxBytes covers E1, E2, E3, E5.
func TestMaxBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	const limit = 20000
	db := open(t, path, engine.Options{MaxBytes: limit})
	val := bytes.Repeat([]byte("v"), 900)
	n := 0
	var err error
	for ; n < 1000; n++ {
		if err = db.Put([]byte(fmt.Sprintf("k%d", n)), val); err != nil {
			break
		}
	}
	if !errors.Is(err, engine.ErrFull) {
		t.Fatalf("want ErrFull, got %v", err)
	}
	if db.Size() > limit {
		t.Fatalf("file %d bytes is over the limit", db.Size())
	}
	sizeAtFull := db.Size()
	// E2: a batch that does not fit is rejected as a whole
	err = db.Batch([]engine.Op{engine.PutOp([]byte("x1"), val), engine.PutOp([]byte("x2"), val)})
	if !errors.Is(err, engine.ErrFull) || db.Size() != sizeAtFull {
		t.Fatalf("batch over the limit: err=%v size=%d", err, db.Size())
	}
	db.Close()

	// E3: reopen, all acknowledged data intact, rejected batch absent
	db = open(t, path, engine.Options{MaxBytes: limit})
	for i := 0; i < n; i++ {
		if v, ok := mustGet(t, db, fmt.Sprintf("k%d", i)); !ok || v != string(val) {
			t.Fatalf("k%d lost after full", i)
		}
	}
	if _, ok := mustGet(t, db, "x1"); ok {
		t.Fatal("rejected batch is visible")
	}
	// E5: deletes always work, and afterwards writes succeed again
	for i := 0; i < n/2; i++ {
		if err := db.Delete([]byte(fmt.Sprintf("k%d", i))); err != nil {
			t.Fatalf("delete while full: %v", err)
		}
	}
	if err := db.Put([]byte("again"), val); err != nil {
		t.Fatalf("put after deletes: %v", err)
	}
	db.Close()
	if rep, err := engine.Check(path); err != nil || !rep.OK {
		t.Fatalf("check: %+v %v", rep, err)
	}
}

// TestCompactionShrinks: many puts then deletes, compaction shrinks the file.
func TestCompactionShrinks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	db := open(t, path, engine.Options{AutoCompactMinBytes: -1})
	ops := make([]engine.Op, 0, 10000)
	for i := 0; i < 10000; i++ {
		ops = append(ops, engine.PutOp([]byte(fmt.Sprintf("key%05d", i)), []byte("some value")))
	}
	db.Batch(ops)
	ops = ops[:0]
	for i := 0; i < 9990; i++ {
		ops = append(ops, engine.DeleteOp([]byte(fmt.Sprintf("key%05d", i))))
	}
	db.Batch(ops)
	big := db.Size()
	if err := db.Compact(); err != nil {
		t.Fatal(err)
	}
	if db.Size() >= big/50 {
		t.Fatalf("file did not shrink: %d -> %d", big, db.Size())
	}
	db.Close()
	db = open(t, path, engine.Options{})
	defer db.Close()
	if db.Len() != 10 {
		t.Fatalf("want 10 keys after compaction, got %d", db.Len())
	}
	if v, ok := mustGet(t, db, "key09995"); !ok || v != "some value" {
		t.Fatal("live key lost by compaction")
	}
}

// TestLeftoverTempFile: C3 and C5. A half-written temp file from a killed
// compaction is deleted on open and the main file is still the truth.
func TestLeftoverTempFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	db := open(t, path, engine.Options{})
	db.Put([]byte("k"), []byte("v"))
	db.Close()
	os.WriteFile(path+engine.TempSuffix, []byte("KVR1 half written compaction"), 0o644)
	db = open(t, path, engine.Options{})
	defer db.Close()
	if !db.Info().RemovedTemp {
		t.Fatal("temp file not removed")
	}
	if _, err := os.Stat(path + engine.TempSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("temp file still exists")
	}
	if v, ok := mustGet(t, db, "k"); !ok || v != "v" {
		t.Fatal("data lost")
	}
}

// TestConcurrentWriters: D1, D2 without a crash.
func TestConcurrentWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	db := open(t, path, engine.Options{})
	var wg sync.WaitGroup
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				db.Put([]byte(fmt.Sprintf("w%d-%d", w, i)), []byte("x"))
				db.Put([]byte("shared"), []byte(fmt.Sprintf("writer%d", w)))
			}
		}(w)
	}
	wg.Wait()
	db.Close()
	db = open(t, path, engine.Options{})
	defer db.Close()
	if db.Len() != 201 {
		t.Fatalf("want 201 keys, got %d", db.Len())
	}
	if v, _ := mustGet(t, db, "shared"); v != "writer0" && v != "writer1" {
		t.Fatalf("shared = %q", v)
	}
}
