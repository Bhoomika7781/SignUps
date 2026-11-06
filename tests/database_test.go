package tests

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"invx/internal/database"
	"invx/internal/storage"
)

func dbPath(t *testing.T) string { return filepath.Join(t.TempDir(), "test.db") }

func open(t *testing.T, path string, opts database.Options) *database.DB {
	t.Helper()
	db, err := database.Open(path, opts)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return db
}

func mustGet(t *testing.T, db *database.DB, key string) (string, bool) {
	t.Helper()
	v, ok, err := db.Get([]byte(key))
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	return string(v), ok
}

func expect(t *testing.T, db *database.DB, key, want string, present bool) {
	t.Helper()
	got, ok := mustGet(t, db, key)
	if ok != present || (present && got != want) {
		t.Fatalf("key %s: got (%q, %v), want (%q, %v)", key, got, ok, want, present)
	}
}

func mustCheck(t *testing.T, path string) {
	t.Helper()
	rep, err := database.Check(path, database.Options{})
	if err != nil || !rep.OK {
		t.Fatalf("check failed: %v %s", err, rep.Reason)
	}
}

func TestPutGetDeleteReopen(t *testing.T) {
	p := dbPath(t)
	db := open(t, p, database.Options{})
	db.Put([]byte("a"), []byte("1"))
	db.Put([]byte("a"), []byte("2"))
	db.Put([]byte("b"), []byte(""))
	db.Delete([]byte("missing")) // not an error
	db.Put([]byte("c"), []byte("3"))
	db.Delete([]byte("c"))
	db.Close()

	db = open(t, p, database.Options{})
	defer db.Close()
	expect(t, db, "a", "2", true)
	expect(t, db, "b", "", true)
	expect(t, db, "c", "", false)
	mustCheck2(t, db)
}

func mustCheck2(t *testing.T, db *database.DB) {
	t.Helper()
	p := db.Path()
	db.Close()
	mustCheck(t, p)
}

func TestLimits(t *testing.T) {
	db := open(t, dbPath(t), database.Options{})
	defer db.Close()
	if err := db.Put(bytes.Repeat([]byte("k"), 1024), bytes.Repeat([]byte("v"), 1<<20)); err != nil {
		t.Fatalf("max sizes should work: %v", err)
	}
	if db.Put(bytes.Repeat([]byte("k"), 1025), nil) == nil {
		t.Fatal("1025-byte key accepted")
	}
	if db.Put(nil, []byte("x")) == nil {
		t.Fatal("empty key accepted")
	}
	if db.Put([]byte("k"), make([]byte, 1<<20+1)) == nil {
		t.Fatal("value > 1 MB accepted")
	}
}

// B5: ops inside a batch apply in order, live and after reopen.
func TestBatchOrdering(t *testing.T) {
	p := dbPath(t)
	db := open(t, p, database.Options{})
	b := db.NewBatch()
	b.Put([]byte("x"), []byte("1"))
	b.Delete([]byte("x"))
	b.Delete([]byte("y"))
	b.Put([]byte("y"), []byte("2"))
	if err := b.Commit(); err != nil {
		t.Fatal(err)
	}
	expect(t, db, "x", "", false)
	expect(t, db, "y", "2", true)
	db.Close()
	db = open(t, p, database.Options{})
	defer db.Close()
	expect(t, db, "x", "", false)
	expect(t, db, "y", "2", true)
}

// A1/A2/A3/A7/B1/B2/B3/F4: cut the last frame at every possible length.
// The previous state must come back and the torn bytes must be removed.
func TestTornTailAtEveryByte(t *testing.T) {
	for _, valSize := range []int{10, 1 << 20} {
		dir := t.TempDir()
		base := filepath.Join(dir, "base.db")
		db := open(t, base, database.Options{})
		db.Put([]byte("keep"), []byte("old"))
		goodSize := db.FileSize()
		b := db.NewBatch()
		for i := 0; i < 10; i++ {
			b.Put([]byte(fmt.Sprintf("b%d", i)), bytes.Repeat([]byte{'z'}, valSize/10+1))
		}
		b.Put([]byte("keep"), []byte("new"))
		if err := b.Commit(); err != nil {
			t.Fatal(err)
		}
		fullSize := db.FileSize()
		db.Close()
		full, _ := os.ReadFile(base)

		step := int64(1)
		if valSize > 1000 {
			step = 4093 // 1 MB frame: sample cut points
		}
		for cut := goodSize; cut < fullSize; cut += step {
			p := filepath.Join(dir, fmt.Sprintf("cut%d.db", cut))
			os.WriteFile(p, full[:cut], 0o644)
			db := open(t, p, database.Options{})
			expect(t, db, "keep", "old", true)
			for i := 0; i < 10; i++ {
				expect(t, db, fmt.Sprintf("b%d", i), "", false)
			}
			if db.FileSize() != goodSize {
				t.Fatalf("cut at %d: torn tail not truncated (size %d, want %d)", cut, db.FileSize(), goodSize)
			}
			db.Close()
			mustCheck(t, p)
			os.Remove(p)
		}
	}
}

// B3-style: the commit marker or CRC is wrong -> whole batch ignored.
func TestBadCommitMarkerOrCRC(t *testing.T) {
	for _, flip := range []int{-1, -6, -30} { // in commit marker, in CRC, in payload
		p := dbPath(t)
		db := open(t, p, database.Options{})
		db.Put([]byte("a"), []byte("1"))
		b := db.NewBatch()
		b.Put([]byte("a"), []byte("2"))
		b.Put([]byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"), []byte("3"))
		b.Commit()
		db.Close()
		data, _ := os.ReadFile(p)
		data[len(data)+flip] ^= 0xff
		os.WriteFile(p, data, 0o644)
		db = open(t, p, database.Options{})
		expect(t, db, "a", "1", true)
		expect(t, db, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "", false)
		db.Close()
	}
}

// F4: truncate one byte by hand.
func TestTruncateOneByte(t *testing.T) {
	p := dbPath(t)
	db := open(t, p, database.Options{})
	db.Put([]byte("a"), []byte("1"))
	db.Put([]byte("b"), []byte("2"))
	db.Close()
	info, _ := os.Stat(p)
	os.Truncate(p, info.Size()-1)
	db = open(t, p, database.Options{})
	defer db.Close()
	expect(t, db, "a", "1", true)
	expect(t, db, "b", "", false)
}

// C1/C2/F2: recovery is idempotent. Opening the same torn file many times
// (as if each recovery was killed after it truncated, or before) always
// gives the same state.
func TestRecoveryIdempotent(t *testing.T) {
	p := dbPath(t)
	db := open(t, p, database.Options{})
	for i := 0; i < 100; i++ {
		db.Put([]byte(fmt.Sprintf("k%d", i)), []byte(fmt.Sprintf("v%d", i)))
	}
	db.Close()
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	f.Write([]byte("FRM1\x40\x00\x00\x00garbage")) // half-written frame
	f.Close()
	for round := 0; round < 3; round++ {
		db = open(t, p, database.Options{})
		if db.Count() != 100 {
			t.Fatalf("round %d: %d keys", round, db.Count())
		}
		db.Close()
	}
	mustCheck(t, p)
}

// A file that is shorter than its header (crash while creating it) opens as empty.
func TestPartialHeader(t *testing.T) {
	p := dbPath(t)
	os.WriteFile(p, []byte("INVX"), 0o644)
	db := open(t, p, database.Options{})
	defer db.Close()
	if db.Count() != 0 {
		t.Fatal("expected empty db")
	}
}

// Not our file: open refuses instead of overwriting it.
func TestForeignFileRejected(t *testing.T) {
	p := dbPath(t)
	os.WriteFile(p, bytes.Repeat([]byte("x"), 100), 0o644)
	if _, err := database.Open(p, database.Options{}); err == nil {
		t.Fatal("foreign file accepted")
	}
}

// C3/C5: a leftover temp file from a crashed compaction is ignored and removed.
func TestLeftoverCompactionTemp(t *testing.T) {
	p := dbPath(t)
	db := open(t, p, database.Options{})
	db.Put([]byte("a"), []byte("1"))
	db.Close()
	// A complete-looking temp file with different content: must NOT win.
	other := open(t, p+".other", database.Options{})
	other.Put([]byte("a"), []byte("WRONG"))
	other.Close()
	data, _ := os.ReadFile(p + ".other")
	os.WriteFile(p+".compact", data, 0o644)

	db = open(t, p, database.Options{})
	expect(t, db, "a", "1", true)
	if !db.Stats().RemovedTemp {
		t.Fatal("temp not reported as removed")
	}
	db.Close()
	if _, err := os.Stat(p + ".compact"); !os.IsNotExist(err) {
		t.Fatal("temp file still exists")
	}
}

// File shrinks after puts followed by deletes (manual and automatic compaction).
func TestCompactionShrinks(t *testing.T) {
	n := 1_000_000
	if testing.Short() {
		n = 50_000
	}
	p := dbPath(t)
	db := open(t, p, database.Options{})
	for i := 0; i < n; i += 10000 {
		b := db.NewBatch()
		for j := i; j < i+10000 && j < n; j++ {
			b.Put([]byte(fmt.Sprintf("key-%07d", j)), []byte("some value bytes"))
		}
		if err := b.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	peak := db.FileSize()
	for i := 0; i < n; i += 10000 {
		b := db.NewBatch()
		for j := i; j < i+10000 && j < n; j++ {
			b.Delete([]byte(fmt.Sprintf("key-%07d", j)))
		}
		if err := b.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Compact(); err != nil {
		t.Fatal(err)
	}
	if db.FileSize() != storage.FileHeaderSize || db.Count() != 0 {
		t.Fatalf("after compaction: size %d (peak %d), keys %d", db.FileSize(), peak, db.Count())
	}
	db.Put([]byte("after"), []byte("ok"))
	db.Close()
	db = open(t, p, database.Options{})
	defer db.Close()
	expect(t, db, "after", "ok", true)
	t.Logf("peak %d bytes -> %d bytes", peak, db.FileSize())
}

// E1/E2/E3/E5: disk-full errors are clean and the database stays usable.
func TestDiskFull(t *testing.T) {
	p := dbPath(t)
	opts := database.Options{MaxBytes: 64 << 10}
	db := open(t, p, opts)
	val := bytes.Repeat([]byte("v"), 1000)
	var i int
	for i = 0; ; i++ {
		err := db.Put([]byte(fmt.Sprintf("k%d", i)), val)
		if errors.Is(err, database.ErrDiskFull) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	acked := i
	if db.FileSize() > opts.MaxBytes {
		t.Fatalf("file %d > cap", db.FileSize())
	}
	// E2: a batch that does not fit is rejected as a whole.
	b := db.NewBatch()
	b.Put([]byte("batch-a"), val)
	b.Put([]byte("k0"), []byte("changed"))
	if err := b.Commit(); !errors.Is(err, database.ErrDiskFull) {
		t.Fatalf("batch: %v", err)
	}
	expect(t, db, "batch-a", "", false)
	expect(t, db, "k0", string(val), true)
	db.Close()

	// E3: reopen (simulating a kill after the cap was hit): all data intact.
	db = open(t, p, opts)
	if db.Count() != acked {
		t.Fatalf("after reopen: %d keys, want %d", db.Count(), acked)
	}
	// E5: delete half; the next write triggers compaction and succeeds.
	for j := 0; j < acked/2; j++ {
		if err := db.Delete([]byte(fmt.Sprintf("k%d", j))); err != nil && !errors.Is(err, database.ErrDiskFull) {
			t.Fatal(err)
		}
	}
	db.Compact()
	if err := db.Put([]byte("new"), val); err != nil {
		t.Fatalf("write after freeing space: %v", err)
	}
	db.Close()
	mustCheck(t, p)
}

// E4: compaction that would exceed the cap fails cleanly; old file stays valid.
func TestCompactionOverCap(t *testing.T) {
	p := dbPath(t)
	db := open(t, p, database.Options{})
	for i := 0; i < 100; i++ {
		db.Put([]byte(fmt.Sprintf("k%d", i)), bytes.Repeat([]byte("v"), 500))
	}
	db.Close()
	db = open(t, p, database.Options{MaxBytes: 1000}) // cap below current live size
	if err := db.Compact(); err == nil {
		t.Fatal("compaction over cap should fail")
	}
	if db.Count() != 100 {
		t.Fatal("data lost")
	}
	db.Put([]byte("x"), []byte("y")) // still rejected cleanly, not corrupt
	db.Close()
	mustCheck(t, p)
	db = open(t, p, database.Options{})
	defer db.Close()
	expect(t, db, "k99", string(bytes.Repeat([]byte("v"), 500)), true)
}

// D1/D2/D3/D4: concurrent writers; a reader never sees half a batch.
// Each batch writes the same value to a pair of keys; any snapshot in which
// the two keys differ would be a half-applied batch.
func TestConcurrentWritersAndReaders(t *testing.T) {
	p := dbPath(t)
	db := open(t, p, database.Options{})
	var wg sync.WaitGroup
	stop := make(chan struct{})
	var readErr error
	var mu sync.Mutex

	for w := 0; w < 3; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				v := []byte(fmt.Sprintf("w%d-%d", w, i))
				b := db.NewBatch()
				b.Put([]byte("pair-a"), v)
				b.Put([]byte("pair-b"), v)
				if err := b.Commit(); err != nil {
					t.Error(err)
					return
				}
				db.Put([]byte(fmt.Sprintf("solo-%d-%d", w, i)), v)
				db.Put([]byte("same"), v)
			}
		}(w)
	}
	for r := 0; r < 2; r++ {
		go func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				vals := map[string]string{}
				db.Dump(func(k, v []byte) error { vals[string(k)] = string(v); return nil })
				if vals["pair-a"] != vals["pair-b"] {
					mu.Lock()
					readErr = fmt.Errorf("half batch visible: %q vs %q", vals["pair-a"], vals["pair-b"])
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if db.Count() != 3*300+3 {
		t.Fatalf("got %d keys", db.Count())
	}
	same, _ := mustGet(t, db, "same")
	if len(same) == 0 || same[0] != 'w' {
		t.Fatalf("same holds %q", same)
	}
	db.Close()
	mustCheck(t, p)
}

// F3: 1,000,000 keys recover in under 2 seconds.
func TestRecoveryOneMillionKeys(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	p := dbPath(t)
	db := open(t, p, database.Options{DisableAutoCompact: true})
	for i := 0; i < 1_000_000; i += 10000 {
		b := db.NewBatch()
		for j := i; j < i+10000; j++ {
			b.Put([]byte(fmt.Sprintf("key-%07d", j)), []byte(fmt.Sprintf("value-%07d", j)))
		}
		if err := b.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	start := time.Now()
	db = open(t, p, database.Options{})
	took := time.Since(start)
	defer db.Close()
	if db.Count() != 1_000_000 {
		t.Fatalf("%d keys", db.Count())
	}
	t.Logf("recovered 1,000,000 keys in %s", took)
	if took > 2*time.Second {
		t.Fatalf("recovery took %s", took)
	}
}

// Two opens of the same file at once are refused (lock file).
func TestSecondOpenIsLocked(t *testing.T) {
	p := dbPath(t)
	db := open(t, p, database.Options{})
	defer db.Close()
	start := time.Now()
	if _, err := database.Open(p, database.Options{}); err == nil {
		t.Fatal("second open succeeded")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("lock retry too slow")
	}
}

// C4 on a filesystem where rename is not atomic: old name gone, complete temp
// file present. open() must promote the temp file, not delete it.
func TestMissingMainWithCompleteTemp(t *testing.T) {
	p := dbPath(t)
	db := open(t, p, database.Options{})
	db.Put([]byte("a"), []byte("1"))
	db.Close()
	os.Rename(p, p+".compact")
	db = open(t, p, database.Options{})
	defer db.Close()
	expect(t, db, "a", "1", true)
	if !db.Stats().PromotedTemp {
		t.Fatal("temp not promoted")
	}
}
