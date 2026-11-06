// Command torture tries to break kvdb.
//
// Each iteration: start `kvdb serve` as a child, drive it with concurrent
// writers (puts, deletes, batches), SIGKILL it after a random number of
// milliseconds, then run `kvdb open`, `kvdb check` and `kvdb dump` and compare
// the dump with the oracle (the tool's own record of every ACK).
//
//	torture --iterations 500 --seed 12345
//	torture --iterations 100 --seed 999 --mode large-values
//	torture --iterations 100 --seed 7   --mode compaction
//	torture --iterations 100 --seed 42  --mode recovery-crash
//	torture --iterations 100 --seed 5   --mode large-batch
//	torture --iterations 100 --seed 3   --mode disk-full
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type mode struct {
	name         string
	privKeys     int     // private keys per writer
	sharedKeys   int     // keys both writers touch with single ops
	valMin       int     // value size range
	valMax       int     //
	batchProb    float64 // chance an op is a batch
	batchMin     int     //
	batchMax     int     //
	delProb      float64 // chance a single op / batch entry is a delete
	sharedProb   float64 // chance a single op hits a shared key
	killMinMs    int     // kill delay range after READY
	killMaxMs    int     //
	preloadKeys  int     // keys written before the first kill (makes recovery/compaction slow enough to hit)
	preloadVal   int     //
	compactor    bool    // a third client keeps sending "compact"
	recoveryKill bool    // also kill `kvdb open` while it recovers
	maxBytes     int64   // --max-bytes for the child (disk-full mode)
	bigBatchW0   bool    // writer 0 only sends 10,000-op batches
}

var modes = map[string]mode{
	"default": {name: "default", privKeys: 200, sharedKeys: 40, valMin: 1, valMax: 300,
		batchProb: 0.3, batchMin: 2, batchMax: 40, delProb: 0.2, sharedProb: 0.3, killMinMs: 5, killMaxMs: 400},
	"large-values": {name: "large-values", privKeys: 6, sharedKeys: 3, valMin: 256 << 10, valMax: 1 << 20,
		batchProb: 0.25, batchMin: 2, batchMax: 4, delProb: 0.15, sharedProb: 0.3, killMinMs: 5, killMaxMs: 800},
	"large-batch": {name: "large-batch", privKeys: 12000, sharedKeys: 40, valMin: 1, valMax: 40,
		batchProb: 0.2, batchMin: 2, batchMax: 20, delProb: 0.2, sharedProb: 0.3, killMinMs: 5, killMaxMs: 1200, bigBatchW0: true},
	"recovery-crash": {name: "recovery-crash", privKeys: 200, sharedKeys: 40, valMin: 1, valMax: 4096,
		batchProb: 0.3, batchMin: 2, batchMax: 40, delProb: 0.2, sharedProb: 0.3, killMinMs: 5, killMaxMs: 600,
		preloadKeys: 200000, preloadVal: 24, recoveryKill: true},
	"compaction": {name: "compaction", privKeys: 200, sharedKeys: 40, valMin: 1, valMax: 2000,
		batchProb: 0.3, batchMin: 2, batchMax: 40, delProb: 0.3, sharedProb: 0.3, killMinMs: 20, killMaxMs: 1000,
		preloadKeys: 30000, preloadVal: 1000, compactor: true},
	"disk-full": {name: "disk-full", privKeys: 100, sharedKeys: 20, valMin: 2000, valMax: 12000,
		batchProb: 0.3, batchMin: 2, batchMax: 30, delProb: 0.3, sharedProb: 0.3, killMinMs: 20, killMaxMs: 600,
		maxBytes: 1 << 20},
}

type stats struct {
	acked, failed, diskFull, ackAfterFull, inflight atomic.Int64
	compactAcked                                    atomic.Int64
	compactInFlightAtKill                           atomic.Bool
	sawDiskFull                                     atomic.Bool
}

func main() {
	iterations := flag.Int("iterations", 500, "number of kill/restart cycles")
	seed := flag.Int64("seed", 0, "random seed (required for reproducible runs; 0 = pick one and print it)")
	modeName := flag.String("mode", "default", "default | large-values | large-batch | recovery-crash | compaction | disk-full")
	kvdbFlag := flag.String("kvdb", "", "path to the kvdb binary (default: next to this binary)")
	dir := flag.String("dir", "torture-data", "working directory for the database and oracle log")
	writers := flag.Int("writers", 2, "number of concurrent writers (at least 2)")
	maxBytes := flag.Int64("max-bytes", 0, "override --max-bytes passed to kvdb (disk-full mode default: 1 MiB)")
	flag.Parse()

	m, ok := modes[*modeName]
	if !ok {
		var names []string
		for n := range modes {
			names = append(names, n)
		}
		sort.Strings(names)
		die("unknown mode %q (modes: %s)", *modeName, strings.Join(names, ", "))
	}
	if *maxBytes > 0 {
		m.maxBytes = *maxBytes
	}
	if *writers < 2 {
		*writers = 2
	}
	if *seed == 0 {
		*seed = time.Now().UnixNano() % 1_000_000_000
	}
	runSeed = *seed
	kvdb := findKvdb(*kvdbFlag)

	if err := os.MkdirAll(*dir, 0o755); err != nil {
		die("%v", err)
	}
	db := filepath.Join(*dir, "torture.db")
	for _, p := range []string{db, db + ".lock", db + ".compact"} {
		os.Remove(p)
	}
	or, err := newOracle(filepath.Join(*dir, "oracle.log"))
	if err != nil {
		die("%v", err)
	}

	fmt.Printf("torture: mode=%s seed=%d iterations=%d writers=%d kvdb=%s db=%s\n", m.name, *seed, *iterations, *writers, kvdb, db)
	fmt.Printf("reproduce with: torture --iterations %d --seed %d --mode %s\n", *iterations, *seed, m.name)

	st := &stats{}
	lastOpenMs := 50.0
	passed := 0
	start := time.Now()

	for it := 1; it <= *iterations; it++ {
		r := rand.New(rand.NewSource(*seed*1_000_003 + int64(it)))
		or.startIteration(it)
		st.compactInFlightAtKill.Store(false)
		st.compactAcked.Store(0)
		iterAcked := st.acked.Load()

		c, err := startServe(kvdb, db, m.maxBytes)
		if err != nil {
			die("iteration %d: cannot start serve: %v", it, err)
		}
		if _, err := c.waitReady(60 * time.Second); err != nil {
			fail(or, it, *seed, m, []string{"serve failed to start (open() failed after the previous kill): " + err.Error()})
		}

		if it == 1 && m.preloadKeys > 0 {
			if err := preload(c, or, m); err != nil {
				die("preload failed: %v", err)
			}
		}

		// Start the writers, let them run for a random number of ms, then kill.
		killAfter := time.Duration(m.killMinMs+r.Intn(m.killMaxMs-m.killMinMs+1)) * time.Millisecond
		var wg sync.WaitGroup
		for w := 0; w < *writers; w++ {
			wg.Add(1)
			wr := rand.New(rand.NewSource(*seed*7_919 + int64(it)*104_729 + int64(w)))
			go func(w int) { defer wg.Done(); runWriter(c, or, st, m, w, it, wr) }(w)
		}
		if m.compactor {
			wg.Add(1)
			cr := rand.New(rand.NewSource(*seed*31 + int64(it)))
			go func() { defer wg.Done(); runCompactor(c, st, it, cr) }()
		}
		time.Sleep(killAfter)
		c.kill() // SIGKILL / TerminateProcess, mid-operation
		wg.Wait()

		// Recovery-crash mode: kill `kvdb open` while it is recovering, 1-3 times.
		recKills, recLanded := 0, 0
		if m.recoveryKill {
			n := 1 + r.Intn(3)
			for j := 0; j < n; j++ {
				d := time.Duration(r.Float64()*lastOpenMs*float64(time.Millisecond)) + time.Millisecond
				if killDuringOpen(kvdb, db, d) {
					recLanded++
				}
				recKills++
			}
		}

		// Restart: open, check, dump.
		t0 := time.Now()
		openOut, err := run(kvdb, "open", db)
		if err != nil {
			fail(or, it, *seed, m, []string{"kvdb open failed: " + err.Error() + " " + openOut})
		}
		lastOpenMs = float64(time.Since(t0).Microseconds()) / 1000
		if _, err := os.Stat(db + ".compact"); err == nil {
			fail(or, it, *seed, m, []string{"compaction temp file still present after open"})
		}
		checkOut, err := run(kvdb, "check", db)
		if err != nil || !strings.HasPrefix(checkOut, "OK") {
			fail(or, it, *seed, m, []string{"kvdb check did not say OK: " + strings.TrimSpace(checkOut)})
		}
		observed, err := dump(kvdb, db)
		if err != nil {
			fail(or, it, *seed, m, []string{"kvdb dump failed: " + err.Error()})
		}

		fails := or.verify(observed, *writers)
		if m.maxBytes > 0 {
			if info, err := os.Stat(db); err == nil && info.Size() > m.maxBytes {
				fails = append(fails, fmt.Sprintf("file size %d exceeds --max-bytes %d", info.Size(), m.maxBytes))
			}
		}
		or.flush()

		size := int64(0)
		if info, err := os.Stat(db); err == nil {
			size = info.Size()
		}
		extra := ""
		if m.compactor {
			extra += fmt.Sprintf(" compactions=%d kill_during_compaction=%v", st.compactAcked.Load(), st.compactInFlightAtKill.Load())
		}
		if m.recoveryKill {
			extra += fmt.Sprintf(" open_kills=%d landed_in_recovery=%d", recKills, recLanded)
		}
		if m.maxBytes > 0 {
			extra += fmt.Sprintf(" disk_full_errs=%d acks_after_full=%d", st.diskFull.Load(), st.ackAfterFull.Load())
		}
		status := "OK"
		if len(fails) > 0 {
			status = "FAIL"
		}
		fmt.Printf("[%4d/%d] kill@%4dms acked=%-5d inflight=%d %s keys=%d size=%s%s  %s\n",
			it, *iterations, killAfter.Milliseconds(), st.acked.Load()-iterAcked, st.inflight.Swap(0),
			strings.TrimPrefix(firstField(openOut, "recovery_ms="), " "), len(observed), human(size), extra, status)
		if len(fails) > 0 {
			fail(or, it, *seed, m, fails)
		}
		or.commit(observed)
		passed++
	}

	fmt.Printf("\nRESULT: %d/%d iterations passed, 0 failed (mode=%s seed=%d, %s)\n", passed, *iterations, m.name, *seed, time.Since(start).Round(time.Second))
	fmt.Printf("ops ACKed=%d ERR=%d (disk-full=%d, ACKs after a disk-full error=%d)\n", st.acked.Load(), st.failed.Load(), st.diskFull.Load(), st.ackAfterFull.Load())
	if m.maxBytes > 0 && st.diskFull.Load() == 0 {
		fmt.Println("WARNING: disk-full mode never hit the size limit; try a smaller --max-bytes")
	}
}

// runWriter sends ops one at a time until the child dies.
func runWriter(c *child, or *oracle, st *stats, m mode, w, it int, r *rand.Rand) {
	for n := 0; ; n++ {
		op, text := genOp(m, w, it, n, r)
		or.sent(op)
		ch, err := c.send(op.id, text)
		if err != nil { // already dead before the op was sent: drop it
			or.mu.Lock()
			op.status = statusFailed
			op.errMsg = "not sent"
			or.mu.Unlock()
			return
		}
		resp, ok := <-ch
		if !ok {
			st.inflight.Add(1) // killed while this op was in flight
			return
		}
		if strings.HasPrefix(resp, "ACK ") {
			or.acked(op)
			st.acked.Add(1)
			if st.sawDiskFull.Load() {
				st.ackAfterFull.Add(1)
			}
			continue
		}
		msg := strings.SplitN(resp, " ", 3)[2:]
		text = strings.Join(msg, "")
		or.failed(op, text)
		st.failed.Add(1)
		if m.maxBytes > 0 && strings.Contains(text, "disk full") {
			st.diskFull.Add(1)
			st.sawDiskFull.Store(true)
			continue
		}
		// Any other error is a failure: the database must not reject valid ops.
		fail(or, it, 0, m, []string{fmt.Sprintf("unexpected error for op %s (%s): %s", op.id, describe(op), text)})
	}
}

func runCompactor(c *child, st *stats, it int, r *rand.Rand) {
	for n := 0; ; n++ {
		time.Sleep(time.Duration(r.Intn(30)) * time.Millisecond)
		id := fmt.Sprintf("c.%d-%d", it, n)
		ch, err := c.send(id, id+" compact\n")
		if err != nil {
			return
		}
		resp, ok := <-ch
		if !ok {
			st.compactInFlightAtKill.Store(true)
			return
		}
		if strings.HasPrefix(resp, "ACK") {
			st.compactAcked.Add(1)
		}
	}
}

// genOp builds one random op and its protocol text. Values embed the op id so
// every value ever written is unique and traceable to the op that wrote it.
func genOp(m mode, w, it, n int, r *rand.Rand) (*opRec, string) {
	id := fmt.Sprintf("w%d.%d-%d", w, it, n)
	op := &opRec{id: id, writer: w}
	priv := func() string { return fmt.Sprintf("w%d-%d", w, r.Intn(m.privKeys)) }

	var b strings.Builder
	if m.bigBatchW0 && w == 0 {
		// 10,000-op batch over this writer's private keys.
		op.kind = "batch"
		fmt.Fprintf(&b, "%s batch %d\n", id, 10000)
		for i := 0; i < 10000; i++ {
			addBatchOp(&b, op, priv(), m, r, i)
		}
		return op, b.String()
	}
	if r.Float64() < m.batchProb {
		op.kind = "batch"
		size := m.batchMin + r.Intn(m.batchMax-m.batchMin+1)
		fmt.Fprintf(&b, "%s batch %d\n", id, size)
		for i := 0; i < size; i++ {
			addBatchOp(&b, op, priv(), m, r, i)
		}
		return op, b.String()
	}
	key := priv()
	if r.Float64() < m.sharedProb {
		key = fmt.Sprintf("s-%d", r.Intn(m.sharedKeys))
	}
	if r.Float64() < m.delProb {
		op.kind = "del"
		op.muts = []mut{{key: key, del: true}}
		return op, fmt.Sprintf("%s del %s\n", id, key)
	}
	op.kind = "put"
	v := makeValue(id, 0, m.valMin+r.Intn(m.valMax-m.valMin+1), r)
	op.muts = []mut{{key: key, val: v}}
	return op, fmt.Sprintf("%s put %s %s\n", id, key, v)
}

func addBatchOp(b *strings.Builder, op *opRec, key string, m mode, r *rand.Rand, i int) {
	if r.Float64() < m.delProb {
		op.muts = append(op.muts, mut{key: key, del: true})
		fmt.Fprintf(b, "del %s\n", key)
		return
	}
	v := makeValue(op.id, i, m.valMin+r.Intn(m.valMax-m.valMin+1), r)
	op.muts = append(op.muts, mut{key: key, val: v})
	fmt.Fprintf(b, "put %s %s\n", key, v)
}

const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// makeValue returns "<opid>#<i>~<filler>" of exactly size bytes (or just the
// prefix if size is smaller). No spaces, tabs or newlines.
func makeValue(id string, i, size int, r *rand.Rand) string {
	prefix := fmt.Sprintf("%s#%d~", id, i)
	if size <= len(prefix) {
		return prefix
	}
	chunk := make([]byte, 61)
	for j := range chunk {
		chunk[j] = alphabet[r.Intn(len(alphabet))]
	}
	var b strings.Builder
	b.Grow(size)
	b.WriteString(prefix)
	for b.Len() < size {
		need := size - b.Len()
		if need > len(chunk) {
			need = len(chunk)
		}
		b.Write(chunk[:need])
	}
	return b.String()
}

// preload writes m.preloadKeys keys in 10,000-op batches before the first kill.
func preload(c *child, or *oracle, m mode) error {
	r := rand.New(rand.NewSource(1))
	for start, n := 0, 0; start < m.preloadKeys; start, n = start+10000, n+1 {
		end := start + 10000
		if end > m.preloadKeys {
			end = m.preloadKeys
		}
		id := fmt.Sprintf("p.%d", n)
		var b strings.Builder
		fmt.Fprintf(&b, "%s batch %d\n", id, end-start)
		var keys, vals []string
		for i := start; i < end; i++ {
			k := fmt.Sprintf("p-%d", i)
			v := makeValue(id, i, m.preloadVal, r)
			keys, vals = append(keys, k), append(vals, v)
			fmt.Fprintf(&b, "put %s %s\n", k, v)
		}
		ch, err := c.send(id, b.String())
		if err != nil {
			return err
		}
		resp, ok := <-ch
		if !ok || !strings.HasPrefix(resp, "ACK") {
			return fmt.Errorf("preload batch %s: %q", id, resp)
		}
		or.preloaded(id, keys, vals)
	}
	return nil
}

// killDuringOpen starts `kvdb open` and kills it after d. Returns true if the
// kill landed before open finished on its own.
func killDuringOpen(kvdb, db string, d time.Duration) bool {
	cmd := exec.Command(kvdb, "open", db)
	if err := cmd.Start(); err != nil {
		die("cannot start kvdb open: %v", err)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	select {
	case <-done:
		return false
	case <-time.After(d):
		_ = cmd.Process.Kill()
		<-done
		return true
	}
}

func run(kvdb string, args ...string) (string, error) {
	var out, errb bytes.Buffer
	cmd := exec.Command(kvdb, args...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		return out.String() + errb.String(), err
	}
	return out.String(), nil
}

func dump(kvdb, db string) (map[string]string, error) {
	cmd := exec.Command(kvdb, "dump", db)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	m := map[string]string{}
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	var prev string
	sorted := true
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "\t")
		if !ok {
			return nil, fmt.Errorf("malformed dump line %q", short(sc.Text()))
		}
		if k < prev {
			sorted = false
		}
		prev = k
		m[k] = v
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("%v: %s", err, errb.String())
	}
	if !sorted {
		return nil, fmt.Errorf("dump output is not sorted by key")
	}
	return m, sc.Err()
}

var runSeed int64

func fail(or *oracle, it int, seed int64, m mode, lines []string) {
	if seed == 0 {
		seed = runSeed
	}
	or.flush()
	fmt.Printf("\nFAILURE in iteration %d (mode=%s):\n", it, m.name)
	for _, l := range lines {
		fmt.Println("  " + l)
	}
	fmt.Println("\nThe oracle log (every ACK/ERR) is in torture-data/oracle.log; the database file was left in place for inspection.")
	if seed != 0 {
		fmt.Printf("reproduce with: torture --seed %d --mode %s --iterations %d\n", seed, m.name, it)
	}
	os.Exit(1)
}

func findKvdb(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	name := "kvdb"
	if runtime.GOOS == "windows" {
		name = "kvdb.exe"
	}
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if abs, err := filepath.Abs(name); err == nil {
		if _, err := os.Stat(abs); err == nil {
			return abs
		}
	}
	die("cannot find %s next to the torture binary or in the current directory; build with: go build -o . ./cmd/...  (or pass --kvdb)", name)
	return ""
}

func firstField(s, prefix string) string {
	for _, f := range strings.Fields(s) {
		if strings.HasPrefix(f, prefix) {
			return "recovery=" + strings.TrimPrefix(f, prefix) + "ms"
		}
	}
	return ""
}

func human(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%dB", n)
}

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "torture: "+format+"\n", a...)
	os.Exit(2)
}
