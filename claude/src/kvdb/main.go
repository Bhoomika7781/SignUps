// Command kvdb is the command-line wrapper around the engine.
//
//	kvdb open    <path> [--max-bytes N]   open/create, run recovery, print key count and recovery time
//	kvdb put     <path> <key> <value>     store a value
//	kvdb get     <path> <key>             print the value, or NOTFOUND
//	kvdb del     <path> <key>             delete a key
//	kvdb batch   <path> <file>            file has one op per line: "put k v" or "del k"
//	kvdb dump    <path>                   print every key and value (key<TAB>value), sorted by key
//	kvdb check   <path>                   verify file integrity, print OK or the reason
//	kvdb serve   <path> [--max-bytes N]   long-running mode, reads ops from stdin, prints ACK after each durable op
//	kvdb compact <path>                   rewrite the file with only live keys
//
// --max-bytes N can be given to any command that writes.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"kvdb/src/engine"
)

const usageText = `usage:
  kvdb open    <path> [--max-bytes N]
  kvdb put     <path> <key> <value> [--max-bytes N]
  kvdb get     <path> <key>
  kvdb del     <path> <key> [--max-bytes N]
  kvdb batch   <path> <file> [--max-bytes N]
  kvdb dump    <path>
  kvdb check   <path>
  kvdb serve   <path> [--max-bytes N]
  kvdb compact <path>
`

func main() {
	args, maxBytes, err := extractMaxBytes(os.Args[1:])
	if err != nil {
		fail("%v", err)
	}
	if len(args) < 2 {
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}
	cmd, path, rest := args[0], args[1], args[2:]
	opts := engine.Options{MaxBytes: maxBytes}

	switch cmd {
	case "open":
		cmdOpen(path, opts)
	case "put":
		need(rest, 1, 2)
		value := ""
		if len(rest) == 2 {
			value = rest[1]
		}
		withDB(path, opts, func(db *engine.DB) error { return db.Put([]byte(rest[0]), []byte(value)) })
		fmt.Println("OK")
	case "get":
		need(rest, 1, 1)
		cmdGet(path, rest[0])
	case "del":
		need(rest, 1, 1)
		withDB(path, opts, func(db *engine.DB) error { return db.Delete([]byte(rest[0])) })
		fmt.Println("OK")
	case "batch":
		need(rest, 1, 1)
		ops := readBatchFile(rest[0])
		withDB(path, opts, func(db *engine.DB) error { return db.Batch(ops) })
		fmt.Printf("OK %d ops\n", len(ops))
	case "dump":
		cmdDump(path)
	case "check":
		cmdCheck(path)
	case "compact":
		var before, after int64
		withDB(path, opts, func(db *engine.DB) error {
			before = db.Size()
			err := db.Compact()
			after = db.Size()
			return err
		})
		fmt.Printf("OK compacted %d -> %d bytes\n", before, after)
	case "serve":
		serve(path, opts)
	default:
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "ERROR: "+format+"\n", a...)
	os.Exit(1)
}

func need(rest []string, min, max int) {
	if len(rest) < min || len(rest) > max {
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}
}

// extractMaxBytes removes "--max-bytes N" or "--max-bytes=N" from the arguments.
func extractMaxBytes(in []string) ([]string, int64, error) {
	var out []string
	var max int64
	for i := 0; i < len(in); i++ {
		a := in[i]
		val := ""
		switch {
		case a == "--max-bytes" || a == "-max-bytes":
			if i+1 >= len(in) {
				return nil, 0, errors.New("--max-bytes needs a number")
			}
			val = in[i+1]
			i++
		case strings.HasPrefix(a, "--max-bytes="):
			val = strings.TrimPrefix(a, "--max-bytes=")
		default:
			out = append(out, a)
			continue
		}
		n, err := strconv.ParseInt(strings.ReplaceAll(val, "_", ""), 10, 64)
		if err != nil || n < 0 {
			return nil, 0, fmt.Errorf("bad --max-bytes value %q", val)
		}
		max = n
	}
	return out, max, nil
}

// withDB opens the database (with recovery), runs fn, and closes it.
func withDB(path string, opts engine.Options, fn func(*engine.DB) error) {
	db, err := engine.Open(path, opts)
	if err != nil {
		fail("open: %v", err)
	}
	if err := fn(db); err != nil {
		db.Close()
		if errors.Is(err, engine.ErrFull) {
			fail("FULL: %v", err)
		}
		fail("%v", err)
	}
	if err := db.Close(); err != nil {
		fail("close: %v", err)
	}
}

func cmdOpen(path string, opts engine.Options) {
	db, err := engine.Open(path, opts)
	if err != nil {
		fail("open: %v", err)
	}
	info := db.Info()
	fmt.Printf("OK keys=%d records=%d bytes=%d recovered_in=%.3fms truncated_bytes=%d removed_temp=%v\n",
		info.Keys, info.Records, info.ValidEnd, float64(info.Duration.Microseconds())/1000.0,
		info.TruncatedBytes, info.RemovedTemp)
	if info.TruncatedBytes > 0 {
		fmt.Printf("repaired torn tail: %s\n", info.Reason)
	}
	if err := db.Close(); err != nil {
		fail("close: %v", err)
	}
}

// Read-only commands (get, dump) never repair the file, so running them
// while another process writes cannot cut off that process's in-flight record.
func cmdGet(path, key string) {
	db, err := engine.Open(path, engine.Options{ReadOnly: true})
	if err != nil {
		fail("open: %v", err)
	}
	defer db.Close()
	v, ok, err := db.Get([]byte(key))
	if err != nil {
		fail("get: %v", err)
	}
	if !ok {
		fmt.Println("NOTFOUND")
		return
	}
	os.Stdout.Write(v)
	fmt.Println()
}

func cmdDump(path string) {
	db, err := engine.Open(path, engine.Options{ReadOnly: true})
	if err != nil {
		fail("open: %v", err)
	}
	defer db.Close()
	out := bufio.NewWriterSize(os.Stdout, 1<<20)
	for _, k := range db.Keys() {
		v, ok, err := db.Get([]byte(k))
		if err != nil {
			fail("get %q: %v", k, err)
		}
		if !ok {
			continue
		}
		out.WriteString(k)
		out.WriteByte('\t')
		out.Write(v)
		out.WriteByte('\n')
	}
	if err := out.Flush(); err != nil {
		fail("write: %v", err)
	}
}

func cmdCheck(path string) {
	rep, err := engine.Check(path)
	if err != nil {
		fail("check: %v", err)
	}
	if !rep.OK {
		fmt.Printf("CORRUPT: %s\n", rep.Problem)
		os.Exit(1)
	}
	fmt.Printf("OK records=%d keys=%d bytes=%d", rep.Records, rep.Keys, rep.FileSize)
	if rep.TempFile {
		fmt.Printf(" (leftover %s file present, next open deletes it)", engine.TempSuffix)
	}
	fmt.Println()
}

// parseOpLine parses "put <key> <value>" or "del <key>". The value is the
// rest of the line, so it may contain spaces (but not newlines).
func parseOpLine(line string) (engine.Op, error) {
	f := strings.SplitN(strings.TrimSpace(line), " ", 3)
	switch {
	case f[0] == "put" && len(f) >= 2:
		v := ""
		if len(f) == 3 {
			v = f[2]
		}
		return engine.PutOp([]byte(f[1]), []byte(v)), nil
	case f[0] == "del" && len(f) == 2:
		return engine.DeleteOp([]byte(f[1])), nil
	}
	return engine.Op{}, fmt.Errorf("bad op line %q (want \"put k v\" or \"del k\")", line)
}

func readBatchFile(name string) []engine.Op {
	f, err := os.Open(name)
	if err != nil {
		fail("%v", err)
	}
	defer f.Close()
	var ops []engine.Op
	r := bufio.NewReaderSize(f, 1<<20)
	for n := 1; ; n++ {
		line, err := r.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "#") {
			op, perr := parseOpLine(line)
			if perr != nil {
				fail("line %d: %v", n, perr)
			}
			ops = append(ops, op)
		}
		if err != nil {
			break
		}
	}
	return ops
}

// ---------------------------------------------------------------------------
// serve mode
//
// Protocol (one command per line, fields separated by single spaces):
//
//	put     <id> <key> <value...>      -> ACK <id>
//	del     <id> <key>                 -> ACK <id>
//	get     <id> <key>                 -> VAL <id> <value>  |  NOTFOUND <id>
//	batch   <id> <n>                   -> then n lines "put k v" / "del k", then ACK <id>
//	batch   <id> put a 1 ; del b ; ... -> one-line batch (handy when typing by hand)
//	compact <id>                       -> ACK <id>
//	quit
//
// Errors: ERR <id> FULL <msg> (nothing written), ERR <id> INVALID <msg>,
// ERR <id> IO <msg>.
//
// ACK is printed only after the engine call returned nil, i.e. after fsync.
//
// Concurrency: the part of <id> before the first '.' is a "lane". Each lane
// has its own goroutine, so ids "w0.1" and "w1.1" run at the same time
// against the engine (that is how the torture tool gets two concurrent
// writers). Ops in one lane run in order. Ids without a dot share one lane.
// ---------------------------------------------------------------------------

func serve(path string, opts engine.Options) {
	db, err := engine.Open(path, opts)
	if err != nil {
		fail("open: %v", err)
	}

	out := bufio.NewWriterSize(os.Stdout, 1<<16)
	var outMu sync.Mutex
	reply := func(parts ...string) {
		outMu.Lock()
		out.WriteString(strings.Join(parts, " "))
		out.WriteByte('\n')
		out.Flush()
		outMu.Unlock()
	}
	result := func(id string, err error) {
		switch {
		case err == nil:
			reply("ACK", id)
		case errors.Is(err, engine.ErrFull):
			reply("ERR", id, "FULL", oneLine(err))
		case errors.Is(err, engine.ErrInvalid):
			reply("ERR", id, "INVALID", oneLine(err))
		default:
			reply("ERR", id, "IO", oneLine(err))
		}
	}

	info := db.Info()
	reply("READY", fmt.Sprintf("keys=%d recovered_in=%.3fms", info.Keys,
		float64(info.Duration.Microseconds())/1000.0))

	lanes := map[string]chan func(){}
	var wg sync.WaitGroup
	submit := func(id string, job func()) {
		lane := id
		if i := strings.IndexByte(id, '.'); i >= 0 {
			lane = id[:i]
		} else {
			lane = ""
		}
		ch, ok := lanes[lane]
		if !ok {
			ch = make(chan func(), 16)
			lanes[lane] = ch
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := range ch {
					j()
				}
			}()
		}
		ch <- job
	}

	in := bufio.NewReaderSize(os.Stdin, 1<<20)
	readLine := func() (string, bool) {
		line, err := in.ReadString('\n')
		if err != nil && line == "" {
			return "", false
		}
		return strings.TrimRight(line, "\r\n"), true
	}

loop:
	for {
		line, ok := readLine()
		if !ok {
			break
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.SplitN(line, " ", 4)
		cmd := f[0]
		if cmd == "quit" || cmd == "exit" {
			break loop
		}
		if len(f) < 2 {
			reply("ERR", "-", "INVALID", "missing op id")
			continue
		}
		id := f[1]
		switch cmd {
		case "put":
			if len(f) < 3 {
				reply("ERR", id, "INVALID", "usage: put <id> <key> <value>")
				continue
			}
			key, val := []byte(f[2]), []byte("")
			if len(f) == 4 {
				val = []byte(f[3])
			}
			submit(id, func() { result(id, db.Put(key, val)) })
		case "del":
			if len(f) != 3 {
				reply("ERR", id, "INVALID", "usage: del <id> <key>")
				continue
			}
			key := []byte(f[2])
			submit(id, func() { result(id, db.Delete(key)) })
		case "get":
			if len(f) != 3 {
				reply("ERR", id, "INVALID", "usage: get <id> <key>")
				continue
			}
			key := []byte(f[2])
			submit(id, func() {
				v, found, err := db.Get(key)
				switch {
				case err != nil:
					result(id, err)
				case !found:
					reply("NOTFOUND", id)
				default:
					reply("VAL", id, string(v))
				}
			})
		case "batch":
			ops, perr := readServeBatch(f, line, readLine)
			if perr != nil {
				reply("ERR", id, "INVALID", oneLine(perr))
				continue
			}
			submit(id, func() { result(id, db.Batch(ops)) })
		case "compact":
			submit(id, func() { result(id, db.Compact()) })
		default:
			reply("ERR", id, "INVALID", "unknown command "+cmd)
		}
	}

	for _, ch := range lanes {
		close(ch)
	}
	wg.Wait()
	if err := db.Close(); err != nil {
		fail("close: %v", err)
	}
}

// readServeBatch handles both "batch <id> <n>" + n lines, and the one-line
// form "batch <id> put a 1 ; del b".
func readServeBatch(f []string, line string, readLine func() (string, bool)) ([]engine.Op, error) {
	if len(f) < 3 {
		return nil, errors.New("usage: batch <id> <n>  or  batch <id> put k v ; del k")
	}
	if n, err := strconv.Atoi(f[2]); err == nil && len(f) == 3 {
		if n < 1 || n > engine.MaxBatchOps {
			// still consume the lines so the stream stays in sync
			for i := 0; i < n && i < 1_000_000; i++ {
				if _, ok := readLine(); !ok {
					break
				}
			}
			return nil, fmt.Errorf("batch size %d, must be 1..%d", n, engine.MaxBatchOps)
		}
		ops := make([]engine.Op, 0, n)
		var firstErr error
		for i := 0; i < n; i++ {
			l, ok := readLine()
			if !ok {
				return nil, errors.New("stdin closed in the middle of a batch")
			}
			op, err := parseOpLine(l)
			if err != nil && firstErr == nil {
				firstErr = err
			}
			ops = append(ops, op)
		}
		if firstErr != nil {
			return nil, firstErr
		}
		return ops, nil
	}
	// one-line form
	body := strings.SplitN(line, " ", 3)[2]
	var ops []engine.Op
	for _, part := range strings.Split(body, ";") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		op, err := parseOpLine(part)
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	return ops, nil
}

func oneLine(err error) string {
	return strings.ReplaceAll(err.Error(), "\n", " ")
}