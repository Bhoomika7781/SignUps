// Command kvdb is the command-line wrapper around the invx engine.
//
//	kvdb open   <path> [--max-bytes N]
//	kvdb put    <path> <key> <value>
//	kvdb get    <path> <key>
//	kvdb del    <path> <key>
//	kvdb batch  <path> <file>
//	kvdb dump   <path>
//	kvdb check  <path>
//	kvdb serve  <path>
//	kvdb compact <path>
//	kvdb fill   <path> <count>     (helper: write <count> keys in 10,000-op batches)
//
// Every command accepts --max-bytes N anywhere on the command line.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"invx/internal/database"
)

const usage = `usage:
  kvdb open    <path> [--max-bytes N]   open/create, run recovery, print key count and recovery time
  kvdb put     <path> <key> <value>
  kvdb get     <path> <key>              prints the value, or NOTFOUND
  kvdb del     <path> <key>
  kvdb batch   <path> <file>             file: one op per line, "put k v" or "del k"
  kvdb dump    <path>                    every key<TAB>value, sorted by key
  kvdb check   <path>                    verify file integrity, prints OK or a reason
  kvdb serve   <path>                    read ops from stdin, print ACK <id> after each durable op
  kvdb compact <path>                    rewrite the file keeping only live keys
  kvdb fill    <path> <count>            write <count> keys key-0000000.. in batches of 10,000
options:
  --max-bytes N   fail writes cleanly once the data file would exceed N bytes
`

func main() {
	args, opts, err := parseArgs(os.Args[1:])
	if err != nil {
		fatal(err)
	}
	if len(args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, path, rest := args[0], args[1], args[2:]

	switch cmd {
	case "open":
		cmdOpen(path, opts)
	case "put":
		need(rest, 1, "put <path> <key> <value>")
		value := ""
		if len(rest) > 1 {
			value = strings.Join(rest[1:], " ")
		}
		withDB(path, opts, func(db *database.DB) error { return db.Put([]byte(rest[0]), []byte(value)) })
		fmt.Println("OK")
	case "get":
		need(rest, 1, "get <path> <key>")
		withDB(path, opts, func(db *database.DB) error {
			v, ok, err := db.Get([]byte(rest[0]))
			if err != nil {
				return err
			}
			if !ok {
				fmt.Println("NOTFOUND")
				return nil
			}
			os.Stdout.Write(v)
			fmt.Println()
			return nil
		})
	case "del", "delete":
		need(rest, 1, "del <path> <key>")
		withDB(path, opts, func(db *database.DB) error { return db.Delete([]byte(rest[0])) })
		fmt.Println("OK")
	case "batch":
		need(rest, 1, "batch <path> <file>")
		cmdBatch(path, rest[0], opts)
	case "dump":
		withDB(path, opts, func(db *database.DB) error {
			w := bufio.NewWriterSize(os.Stdout, 1<<20)
			defer w.Flush()
			return db.Dump(func(k, v []byte) error {
				w.Write(k)
				w.WriteByte('\t')
				w.Write(v)
				return w.WriteByte('\n')
			})
		})
	case "check":
		rep, err := database.Check(path, opts)
		if err != nil {
			fatal(err)
		}
		if !rep.OK {
			fmt.Printf("CORRUPT: %s\n", rep.Reason)
			os.Exit(1)
		}
		fmt.Printf("OK keys=%d frames=%d bytes=%d\n", rep.Keys, rep.Frames, rep.FileSize)
	case "serve":
		os.Exit(serve(path, opts))
	case "compact":
		withDB(path, opts, func(db *database.DB) error {
			before := db.FileSize()
			if err := db.Compact(); err != nil {
				return err
			}
			fmt.Printf("OK bytes_before=%d bytes_after=%d keys=%d\n", before, db.FileSize(), db.Count())
			return nil
		})
	case "fill":
		need(rest, 1, "fill <path> <count>")
		n, err := strconv.Atoi(rest[0])
		if err != nil {
			fatal(err)
		}
		cmdFill(path, n, opts)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

// parseArgs pulls --max-bytes out from anywhere in the argument list.
func parseArgs(in []string) ([]string, database.Options, error) {
	var opts database.Options
	var out []string
	for i := 0; i < len(in); i++ {
		a := in[i]
		switch {
		case a == "--max-bytes" || a == "-max-bytes":
			if i+1 >= len(in) {
				return nil, opts, errors.New("--max-bytes needs a value")
			}
			i++
			a = "--max-bytes=" + in[i]
			fallthrough
		case strings.HasPrefix(a, "--max-bytes="):
			n, err := parseSize(strings.TrimPrefix(a, "--max-bytes="))
			if err != nil {
				return nil, opts, err
			}
			opts.MaxBytes = n
		default:
			out = append(out, a)
		}
	}
	return out, opts, nil
}

// parseSize accepts 50000000, 50_000_000, 50MB, 512KB, 1GB.
func parseSize(s string) (int64, error) {
	s = strings.ToUpper(strings.ReplaceAll(s, "_", ""))
	mult := int64(1)
	for suffix, m := range map[string]int64{"KB": 1 << 10, "MB": 1 << 20, "GB": 1 << 30} {
		if strings.HasSuffix(s, suffix) {
			mult, s = m, strings.TrimSuffix(s, suffix)
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid --max-bytes value %q", s)
	}
	return n * mult, nil
}

func cmdOpen(path string, opts database.Options) {
	db, err := database.Open(path, opts)
	if err != nil {
		fatal(err)
	}
	st := db.Stats()
	if err := db.Close(); err != nil {
		fatal(err)
	}
	fmt.Printf("OK keys=%d recovery_ms=%.2f frames=%d bytes=%d", st.Keys, float64(st.DurationNano)/1e6, st.Frames, st.FileSize)
	if st.TornBytes > 0 {
		fmt.Printf(" truncated_bytes=%d (%s)", st.TornBytes, st.TornReason)
	}
	if st.RemovedTemp {
		fmt.Print(" removed_compaction_temp=1")
	}
	if st.PromotedTemp {
		fmt.Print(" promoted_compaction_temp=1")
	}
	fmt.Println()
}

func cmdBatch(path, file string, opts database.Options) {
	data, err := os.ReadFile(file)
	if err != nil {
		fatal(err)
	}
	withDB(path, opts, func(db *database.DB) error {
		b := db.NewBatch()
		for n, line := range strings.Split(string(data), "\n") {
			line = strings.TrimRight(line, "\r")
			if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			if err := addBatchLine(b, line); err != nil {
				return fmt.Errorf("%s line %d: %w", file, n+1, err)
			}
		}
		if err := b.Commit(); err != nil {
			return err
		}
		fmt.Printf("OK ops=%d\n", b.Len())
		return nil
	})
}

// addBatchLine parses "put <key> <value...>" or "del <key>".
func addBatchLine(b *database.Batch, line string) error {
	parts := strings.SplitN(line, " ", 3)
	switch strings.ToLower(parts[0]) {
	case "put":
		if len(parts) < 2 {
			return errors.New("put needs a key")
		}
		value := ""
		if len(parts) == 3 {
			value = parts[2]
		}
		return b.Put([]byte(parts[1]), []byte(value))
	case "del", "delete":
		if len(parts) != 2 {
			return errors.New("del takes exactly one key")
		}
		return b.Delete([]byte(parts[1]))
	default:
		return fmt.Errorf("unknown batch op %q", parts[0])
	}
}

func cmdFill(path string, n int, opts database.Options) {
	withDB(path, opts, func(db *database.DB) error {
		start := time.Now()
		for i := 0; i < n; {
			b := db.NewBatch()
			for j := 0; j < 10000 && i < n; j, i = j+1, i+1 {
				b.Put([]byte(fmt.Sprintf("key-%07d", i)), []byte(fmt.Sprintf("value-%07d", i)))
			}
			if err := b.Commit(); err != nil {
				return err
			}
		}
		fmt.Printf("OK wrote=%d keys=%d bytes=%d took=%s\n", n, db.Count(), db.FileSize(), time.Since(start).Round(time.Millisecond))
		return nil
	})
}

func withDB(path string, opts database.Options, fn func(*database.DB) error) {
	db, err := database.Open(path, opts)
	if err != nil {
		fatal(err)
	}
	err = fn(db)
	cerr := db.Close()
	if err != nil {
		fatal(err)
	}
	if cerr != nil {
		fatal(cerr)
	}
}

func need(rest []string, n int, form string) {
	if len(rest) < n {
		fatal(fmt.Errorf("usage: kvdb %s", form))
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "ERROR:", err)
	os.Exit(1)
}
