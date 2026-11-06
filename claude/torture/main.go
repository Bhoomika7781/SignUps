// Command torture tries to break kvdb by killing it at random moments.
//
//	torture --iterations 500 --seed 12345
//	torture --iterations 100 --seed 999 --mode large-values
//	torture --iterations 100 --seed 7   --mode compaction
//	torture --iterations 100 --seed 42  --mode recovery-crash
//	torture --iterations 50  --seed 5   --mode large-batch
//	torture --iterations 100 --seed 3   --mode disk-full
//	torture --seed 1 --mode million      (1,000,000 keys, kill, time the recovery)
//
// Every iteration:
//  1. start `kvdb serve` as a child process
//  2. run >= 2 concurrent writers (puts, deletes, batches) plus, depending
//     on the mode, a concurrent reader and/or a compactor
//  3. record every op sent and every ACK (the oracle; see oracle.go)
//  4. after a random delay in milliseconds, SIGKILL the child (mid-operation)
//  5. (recovery-crash mode) start `kvdb open` and SIGKILL it too, 1-3 times
//  6. run `kvdb open`, `kvdb check`, `kvdb dump`
//  7. compare the dump with the oracle; stop at the first violation
//
// The seed fixes every op stream and every kill delay. The OS scheduler
// still decides which exact instruction the kill lands on.
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

type torture struct {
	kvdb     string
	dir      string
	dbPath   string
	mode     string
	cfg      modeCfg
	writers  int
	maxBytes int64
	seed     int64
	log      *oracleLog
	base     map[string]string // state verified after the previous iteration
	lastOpen time.Duration

	// totals for the summary
	acked, pending, full, errs, inflightKills, tornTails int
	recInterrupted, recCompleted                         int
	maxRecovery                                          float64
}

type iterStats struct {
	killAfter      time.Duration
	sent, acked    int
	pending, full  int
	errs           int
	recoverMs      float64
	truncated      int64
	keys           int
	fileBytes      int64
	recInterrupted int
	recCompleted   int
}

func main() {
	iterations := flag.Int("iterations", 500, "number of kill/restart cycles")
	seed := flag.Int64("seed", 0, "random seed (0 = choose one from the clock and print it)")
	mode := flag.String("mode", "mixed", "mixed | large-values | large-batch | compaction | recovery-crash | disk-full | million")
	kvdb := flag.String("kvdb", defaultBinary(), "path to the kvdb binary")
	dir := flag.String("dir", "torture-data", "working directory (wiped at start)")
	writers := flag.Int("writers", 2, "number of concurrent writers (at least 2)")
	maxBytes := flag.Int64("max-bytes", -1, "override --max-bytes passed to kvdb (-1 = mode default)")
	flag.Parse()

	if *seed == 0 {
		*seed = time.Now().UnixNano() % 1_000_000_000
	}
	if *writers < 2 {
		*writers = 2
	}
	bin, err := filepath.Abs(*kvdb)
	if err == nil {
		_, err = os.Stat(bin)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot find kvdb binary at %s. Build first: go build -o bin/ ./src/kvdb ./torture\n", *kvdb)
		os.Exit(2)
	}

	if err := os.RemoveAll(*dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	log, err := openOracleLog(filepath.Join(*dir, "oracle.log"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	defer log.flush()

	t := &torture{
		kvdb: bin, dir: *dir, dbPath: filepath.Join(*dir, "test.db"),
		mode: *mode, writers: *writers, seed: *seed, log: log,
		base: map[string]string{}, lastOpen: 30 * time.Millisecond,
	}

	fmt.Printf("torture: mode=%s seed=%d iterations=%d writers=%d kvdb=%s\n",
		*mode, *seed, *iterations, *writers, bin)

	if *mode == "million" {
		os.Exit(t.runMillion())
	}
	cfg, ok := modes[*mode]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown mode %q\n", *mode)
		os.Exit(2)
	}
	t.cfg = cfg
	t.maxBytes = cfg.maxBytes
	if *maxBytes >= 0 {
		t.maxBytes = *maxBytes
	}
	os.Exit(t.run(*iterations))
}

// defaultBinary looks for kvdb next to the torture binary itself (bin/).
func defaultBinary() string {
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
	return filepath.Join("bin", name)
}

func (t *torture) run(iterations int) int {
	master := rand.New(rand.NewSource(t.seed))
	start := time.Now()
	for it := 1; it <= iterations; it++ {
		itSeed := master.Int63()
		t.log.it = it
		st, viol, err := t.iteration(it, itSeed)
		t.log.flush()
		if err != nil {
			fmt.Printf("[%4d/%d] ERROR: %v\n", it, iterations, err)
			fmt.Printf("FAILED at iteration %d (run seed %d)\n", it, t.seed)
			return 1
		}
		status := "OK"
		if len(viol) > 0 {
			status = fmt.Sprintf("FAIL (%d violations)", len(viol))
		}
		extra := ""
		if t.cfg.recoveryKills > 0 {
			extra = fmt.Sprintf(" open-kills=%d/%d", st.recInterrupted, st.recInterrupted+st.recCompleted)
		}
		fmt.Printf("[%4d/%d] kill@%4dms sent=%-5d acked=%-5d inflight=%d full=%-3d err=%d torn=%-6d keys=%-6d file=%-9d recovery=%.1fms%s %s\n",
			it, iterations, st.killAfter.Milliseconds(), st.sent, st.acked, st.pending, st.full, st.errs,
			st.truncated, st.keys, st.fileBytes, st.recoverMs, extra, status)
		if len(viol) > 0 {
			fmt.Printf("\nINVARIANT VIOLATED in iteration %d (run with --seed %d --mode %s to reproduce the op streams)\n",
				it, t.seed, t.mode)
			for i, v := range viol {
				if i == 25 {
					fmt.Printf("  ... and %d more\n", len(viol)-25)
					break
				}
				fmt.Println("  -", v)
			}
			fmt.Printf("Full op history: %s\n", filepath.Join(t.dir, "oracle.log"))
			fmt.Printf("\nRESULT: FAIL after %d passing iterations\n", it-1)
			return 1
		}
	}
	fmt.Printf("\nRESULT: PASS %d/%d iterations, 0 violations (%.1fs)\n", iterations, iterations, time.Since(start).Seconds())
	fmt.Printf("  acked ops: %d   in-flight ops at kill: %d   kills that hit an in-flight op: %d/%d\n",
		t.acked, t.pending, t.inflightKills, iterations)
	fmt.Printf("  torn tails repaired by open: %d   rejected with FULL: %d   other errors: %d\n",
		t.tornTails, t.full, t.errs)
	if t.cfg.recoveryKills > 0 {
		fmt.Printf("  kills of `kvdb open`: %d landed during recovery, %d too late (open had finished)\n",
			t.recInterrupted, t.recCompleted)
	}
	fmt.Printf("  slowest recovery: %.1fms\n", t.maxRecovery)
	return 0
}

func (t *torture) iteration(it int, itSeed int64) (iterStats, []string, error) {
	var st iterStats
	r := rand.New(rand.NewSource(itSeed))

	s, err := startServe(t.kvdb, t.dbPath, t.maxBytes, t.log)
	if err != nil {
		return st, nil, err
	}

	if t.cfg.prefill > 0 && it == 1 {
		if err := t.prefill(s, "p.1.", t.cfg.prefill, t.cfg.prefillVal); err != nil {
			s.kill()
			return st, nil, err
		}
	}

	var wg sync.WaitGroup
	for w := 0; w < t.writers; w++ {
		g := newGen(t.cfg, rand.New(rand.NewSource(r.Int63())), fmt.Sprintf("w%d.%d.", w, it))
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !s.isDead() {
				if !s.do(&liveOp{genOp: g.next()}) {
					return
				}
			}
		}()
	}
	if t.cfg.reader {
		rr := rand.New(rand.NewSource(r.Int63()))
		wg.Add(1)
		go func() {
			defer wg.Done()
			t.readerLoop(s, rr, it)
		}()
	}
	if t.cfg.compactor {
		cr := rand.New(rand.NewSource(r.Int63()))
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 1; !s.isDead(); n++ {
				time.Sleep(time.Duration(cr.Intn(20)) * time.Millisecond)
				op := &liveOp{genOp: &genOp{id: fmt.Sprintf("c.%d.%d", it, n), kind: "compact"}}
				if !s.do(op) {
					return
				}
			}
		}()
	}

	// Kill after a random number of milliseconds, wherever the child is.
	st.killAfter = time.Duration(t.cfg.killMinMs+r.Intn(t.cfg.killMaxMs-t.cfg.killMinMs+1)) * time.Millisecond
	time.Sleep(st.killAfter)
	s.kill()
	wg.Wait()

	// Tally what happened.
	for _, o := range s.ops {
		if o.eff == nil {
			continue
		}
		st.sent++
		switch o.st {
		case stAcked:
			st.acked++
		case stPending:
			st.pending++
		case stFull:
			st.full++
		case stErr:
			st.errs++
		}
	}

	// recovery-crash: kill `kvdb open` in the middle of recovery 1-3 times.
	if t.cfg.recoveryKills > 0 {
		n := 1 + r.Intn(t.cfg.recoveryKills)
		for i := 0; i < n; i++ {
			d := time.Duration(r.Int63n(int64(t.lastOpen) + 1))
			if t.killOpenAfter(d) {
				st.recInterrupted++
			} else {
				st.recCompleted++
			}
		}
	}

	// open (recovery), check, dump.
	openStart := time.Now()
	out, err := t.kv(2*time.Minute, "open", t.dbPath)
	t.lastOpen = time.Since(openStart)
	if err != nil {
		return st, nil, fmt.Errorf("`kvdb open` failed after the kill (rule 5 violated): %v\n%s", err, out)
	}
	st.recoverMs = parseFloatField(out, "recovered_in", "ms")
	st.truncated = int64(parseFloatField(out, "truncated_bytes", ""))
	st.fileBytes = int64(parseFloatField(out, "bytes", ""))

	out, err = t.kv(2*time.Minute, "check", t.dbPath)
	if err != nil || !strings.HasPrefix(out, "OK") {
		return st, nil, fmt.Errorf("`kvdb check` did not say OK after open: %v\n%s", err, out)
	}
	dump, err := t.kv(5*time.Minute, "dump", t.dbPath)
	if err != nil {
		return st, nil, fmt.Errorf("`kvdb dump` failed: %v", err)
	}
	observed := parseDump(dump)
	st.keys = len(observed)

	viol := verify(t.base, s.ops, observed)
	viol = append(viol, s.readViol...)

	t.acked += st.acked
	t.pending += st.pending
	t.full += st.full
	t.errs += st.errs
	if st.pending > 0 {
		t.inflightKills++
	}
	if st.truncated > 0 {
		t.tornTails++
	}
	t.recInterrupted += st.recInterrupted
	t.recCompleted += st.recCompleted
	if st.recoverMs > t.maxRecovery {
		t.maxRecovery = st.recoverMs
	}
	t.log.note("kill_after=%s open: recovered_in=%.3fms truncated=%d keys=%d violations=%d",
		st.killAfter, st.recoverMs, st.truncated, st.keys, len(viol))

	if len(viol) == 0 {
		// The dump has just been proven legal, so it becomes the baseline.
		t.base = observed
	}
	return st, viol, nil
}

// readerLoop calls get concurrently with the writers and checks that every
// value it sees is complete and belongs to the key (D4: never a torn or
// half-applied value).
func (t *torture) readerLoop(s *session, r *rand.Rand, it int) {
	for n := 1; !s.isDead(); n++ {
		key := "k" + strconv.Itoa(r.Intn(t.cfg.keys))
		op := &liveOp{genOp: &genOp{id: fmt.Sprintf("r.%d.%d", it, n), kind: "get", key: key}}
		if !s.do(op) {
			return
		}
		if op.st == stAcked && op.msg != "" {
			id, k, ok := parseValue(op.msg)
			if !ok {
				s.addReadViolation(fmt.Sprintf("get %s returned a TORN or GARBAGE value: %s", key, short(op.msg)))
			} else if k != key {
				s.addReadViolation(fmt.Sprintf("get %s returned the value op %s wrote for key %s", key, id, k))
			}
		}
		time.Sleep(time.Duration(1+r.Intn(3)) * time.Millisecond)
	}
}

// killOpenAfter starts `kvdb open` and SIGKILLs it after d.
// Returns true if the kill landed while open was still running.
func (t *torture) killOpenAfter(d time.Duration) bool {
	cmd := exec.Command(t.kvdb, "open", t.dbPath)
	if err := cmd.Start(); err != nil {
		return false
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	select {
	case <-done:
		return false
	case <-time.After(d):
		cmd.Process.Kill()
		<-done
		return true
	}
}

func (t *torture) prefill(s *session, prefix string, n, valSize int) error {
	for start := 0; start < n; start += 10000 {
		end := start + 10000
		if end > n {
			end = n
		}
		id := prefix + strconv.Itoa(start/10000+1)
		op := &genOp{id: id, kind: "batch", batch: make([]bop, 0, end-start)}
		for i := start; i < end; i++ {
			k := "k" + strconv.Itoa(i)
			if t.cfg.keys > 0 {
				k = "k" + strconv.Itoa(i%t.cfg.keys)
				if i >= t.cfg.keys {
					k = "p" + strconv.Itoa(i)
				}
			}
			op.batch = append(op.batch, bop{key: k, val: makeValue(id, k, valSize)})
		}
		lo := &liveOp{genOp: op}
		if !s.do(lo) || lo.st != stAcked {
			return fmt.Errorf("prefill batch %s was not ACKed (%s %s)", id, lo.st, lo.msg)
		}
	}
	return nil
}

// runMillion: 1,000,000 keys, kill -9, then time the recovery (F3).
func (t *torture) runMillion() int {
	const n = 1_000_000
	t.cfg = modeCfg{keys: 0}
	t.log.it = 1
	s, err := startServe(t.kvdb, t.dbPath, 0, t.log)
	if err != nil {
		fmt.Println("ERROR:", err)
		return 1
	}
	fmt.Printf("writing %d keys in batches of 10,000 ...\n", n)
	w0 := time.Now()
	if err := t.prefill(s, "m.1.", n, 32); err != nil {
		fmt.Println("ERROR:", err)
		return 1
	}
	fmt.Printf("written in %.1fs, now kill -9\n", time.Since(w0).Seconds())
	s.kill()

	out, err := t.kv(5*time.Minute, "open", t.dbPath)
	if err != nil {
		fmt.Println("ERROR: open failed:", err, out)
		return 1
	}
	ms := parseFloatField(out, "recovered_in", "ms")
	fmt.Print(out)
	dump, err := t.kv(5*time.Minute, "dump", t.dbPath)
	if err != nil {
		fmt.Println("ERROR: dump failed:", err)
		return 1
	}
	observed := parseDump(dump)
	viol := verify(t.base, s.ops, observed)
	for i, v := range viol {
		if i == 10 {
			break
		}
		fmt.Println("  -", v)
	}
	fmt.Printf("keys after recovery: %d, violations: %d, recovery time: %.1fms (limit 2000ms)\n", len(observed), len(viol), ms)
	if len(viol) > 0 || ms > 2000 || len(observed) != n {
		fmt.Println("RESULT: FAIL")
		return 1
	}
	fmt.Println("RESULT: PASS")
	return 0
}

func (t *torture) kv(timeout time.Duration, args ...string) (string, error) {
	cmd := exec.Command(t.kvdb, args...)
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		return "", err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return out.String() + errb.String(), err
		}
		return out.String(), nil
	case <-time.After(timeout):
		cmd.Process.Kill()
		<-done
		return out.String(), fmt.Errorf("timed out after %s", timeout)
	}
}

func parseDump(s string) map[string]string {
	m := make(map[string]string)
	for len(s) > 0 {
		line := s
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			line, s = s[:i], s[i+1:]
		} else {
			s = ""
		}
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		k, v, _ := strings.Cut(line, "\t")
		m[k] = v
	}
	return m
}

var fieldRe = map[string]*regexp.Regexp{}

func parseFloatField(out, name, unit string) float64 {
	re, ok := fieldRe[name]
	if !ok {
		re = regexp.MustCompile(`\b` + name + `=([0-9.]+)` + unit)
		fieldRe[name] = re
	}
	m := re.FindStringSubmatch(out)
	if m == nil {
		return 0
	}
	f, _ := strconv.ParseFloat(m[1], 64)
	return f
}
