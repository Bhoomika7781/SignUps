package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

	"invx/internal/database"
)

// serve protocol (one command per line on stdin):
//
//	[id] put <key> <value...>     -> ACK <id>
//	[id] del <key>                -> ACK <id>
//	[id] get <key>                -> VALUE <id> <value>   or   NOTFOUND <id>
//	[id] batch <n>                -> the next n lines are "put k v" / "del k"; then ACK <id>
//	[id] batch                    -> lines until a line "end"; then ACK <id>
//	[id] compact                  -> ACK <id>
//	[id] count                    -> COUNT <id> <n>
//	quit                          -> close the database and exit
//
// Any failure prints ERR <id> <message> and the operation had no effect.
//
// The id is optional. If the first word is not a command it is the id; if it
// is missing, ids are assigned 1, 2, 3, ... An id may carry a channel prefix,
// "<channel>.<n>" (e.g. "w1.17"). Commands on the same channel run in order,
// one at a time; different channels run concurrently on separate goroutines.
// That is how the torture tool drives two concurrent writers.
//
// ACK is printed only after the database call returned nil, which happens
// only after the frame was fsynced. Every ACK is therefore a durable promise.

type job struct {
	id  string
	run func() string // returns the response line
}

type server struct {
	db      *database.DB
	outMu   sync.Mutex
	out     *bufio.Writer
	workers map[string]chan job
	wg      sync.WaitGroup
	autoID  int
}

func serve(path string, opts database.Options) int {
	db, err := database.Open(path, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		return 1
	}
	s := &server{db: db, out: bufio.NewWriter(os.Stdout), workers: map[string]chan job{}}
	st := db.Stats()
	s.emit(fmt.Sprintf("READY keys=%d recovery_ms=%.2f", st.Keys, float64(st.DurationNano)/1e6))

	in := bufio.NewReaderSize(os.Stdin, 1<<20)
	for {
		line, err := readLine(in)
		if err != nil {
			break // EOF: shut down cleanly
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.TrimSpace(line) == "quit" {
			break
		}
		s.dispatch(line, in)
	}

	for _, ch := range s.workers {
		close(ch)
	}
	s.wg.Wait()
	if err := db.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		return 1
	}
	return 0
}

func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil && (err != io.EOF || line == "") {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

var commands = map[string]bool{"put": true, "del": true, "delete": true, "get": true, "batch": true, "compact": true, "count": true}

func (s *server) dispatch(line string, in *bufio.Reader) {
	id, rest := "", line
	first, after, _ := strings.Cut(line, " ")
	if !commands[strings.ToLower(first)] {
		id, rest = first, after
	} else {
		s.autoID++
		id = strconv.Itoa(s.autoID)
	}
	cmd, args, _ := strings.Cut(rest, " ")
	cmd = strings.ToLower(cmd)

	var run func() string
	switch cmd {
	case "put":
		key, value, _ := strings.Cut(args, " ")
		run = func() string { return ackOrErr(id, s.db.Put([]byte(key), []byte(value))) }
	case "del", "delete":
		key := args
		run = func() string { return ackOrErr(id, s.db.Delete([]byte(key))) }
	case "get":
		key := args
		run = func() string {
			v, ok, err := s.db.Get([]byte(key))
			if err != nil {
				return "ERR " + id + " " + oneLine(err)
			}
			if !ok {
				return "NOTFOUND " + id
			}
			return "VALUE " + id + " " + string(v)
		}
	case "count":
		run = func() string { return fmt.Sprintf("COUNT %s %d", id, s.db.Count()) }
	case "compact":
		run = func() string { return ackOrErr(id, s.db.Compact()) }
	case "batch":
		// The batch body is read here, on the reader goroutine, so its lines
		// never get mixed up with other commands.
		b, err := s.readBatch(strings.TrimSpace(args), in)
		if err != nil {
			run = func() string { return "ERR " + id + " " + oneLine(err) }
		} else {
			run = func() string { return ackOrErr(id, b.Commit()) }
		}
	default:
		run = func() string { return "ERR " + id + " unknown command " + strconv.Quote(cmd) }
	}

	channel := ""
	if c, _, ok := strings.Cut(id, "."); ok {
		channel = c
	}
	s.worker(channel) <- job{id: id, run: run}
}

func (s *server) readBatch(arg string, in *bufio.Reader) (*database.Batch, error) {
	b := s.db.NewBatch()
	n := -1
	if arg != "" {
		v, err := strconv.Atoi(arg)
		if err != nil || v < 0 {
			return nil, fmt.Errorf("batch count %q is not a number", arg)
		}
		n = v
	}
	var firstErr error
	for i := 0; n < 0 || i < n; i++ {
		line, err := readLine(in)
		if err != nil {
			return nil, errors.New("input ended inside batch")
		}
		if n < 0 && (strings.TrimSpace(line) == "end" || strings.TrimSpace(line) == "commit") {
			break
		}
		// Keep reading the whole body even after an error so the stream stays in sync.
		if err := addBatchLine(b, line); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return b, nil
}

func (s *server) worker(channel string) chan job {
	ch, ok := s.workers[channel]
	if ok {
		return ch
	}
	ch = make(chan job, 16)
	s.workers[channel] = ch
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for j := range ch {
			s.emit(j.run())
		}
	}()
	return ch
}

func (s *server) emit(line string) {
	s.outMu.Lock()
	s.out.WriteString(line)
	s.out.WriteByte('\n')
	s.out.Flush()
	s.outMu.Unlock()
}

func ackOrErr(id string, err error) string {
	if err != nil {
		return "ERR " + id + " " + oneLine(err)
	}
	return "ACK " + id
}

func oneLine(err error) string { return strings.ReplaceAll(err.Error(), "\n", " ") }
