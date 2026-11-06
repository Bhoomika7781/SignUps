package main

// session.go: one running `kvdb serve` child process.

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// clock orders events seen by the torture tool (see oracle.go).
var clock int64

func tick() int64 { return atomic.AddInt64(&clock, 1) }

type session struct {
	cmd     *exec.Cmd
	stdin   *os.File
	inMu    sync.Mutex
	stderr  bytes.Buffer
	mu      sync.Mutex
	waiting map[string]*liveOp
	ops     []*liveOp
	dead    chan struct{} // closed when the child's stdout hits EOF
	ready   chan string
	log     *oracleLog

	readViol []string // problems seen by the concurrent reader / reply parser
}

func startServe(kvdb, dbPath string, maxBytes int64, log *oracleLog) (*session, error) {
	s := &session{
		waiting: map[string]*liveOp{},
		dead:    make(chan struct{}),
		ready:   make(chan string, 1),
		log:     log,
	}
	args := []string{"serve", dbPath}
	if maxBytes > 0 {
		args = append(args, "--max-bytes", fmt.Sprint(maxBytes))
	}
	// We use our own os.Pipe pairs (not cmd.StdoutPipe) so that we, not
	// cmd.Wait, decide when the read side is closed: no ACK that the child
	// managed to print before dying is ever thrown away.
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	s.cmd = exec.Command(kvdb, args...)
	s.cmd.Stdin = inR
	s.cmd.Stdout = outW
	s.cmd.Stderr = &s.stderr
	if err := s.cmd.Start(); err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return nil, err
	}
	inR.Close()
	outW.Close()
	s.stdin = inW
	go s.readLoop(outR)

	select {
	case line := <-s.ready:
		if !strings.HasPrefix(line, "READY") {
			s.kill()
			return nil, fmt.Errorf("serve said %q instead of READY", line)
		}
		return s, nil
	case <-s.dead:
		s.cmd.Wait()
		return nil, fmt.Errorf("serve exited before READY: %s", strings.TrimSpace(s.stderr.String()))
	case <-time.After(2 * time.Minute):
		s.kill()
		return nil, fmt.Errorf("serve did not print READY within 2 minutes")
	}
}

func (s *session) readLoop(r *os.File) {
	defer close(s.dead)
	defer r.Close()
	br := bufio.NewReaderSize(r, 1<<20)
	first := true
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			// EOF. A partial line without '\n' is ignored: an ACK that was
			// not fully printed was never a promise.
			return
		}
		t := tick()
		line = strings.TrimRight(line, "\r\n")
		if first {
			first = false
			s.ready <- line
			continue
		}
		typ, rest, _ := strings.Cut(line, " ")
		id, payload, _ := strings.Cut(rest, " ")
		s.mu.Lock()
		op := s.waiting[id]
		delete(s.waiting, id)
		if op == nil {
			s.readViol = append(s.readViol, fmt.Sprintf("serve sent a reply for unknown op: %s", short(line)))
		}
		s.mu.Unlock()
		if op == nil {
			continue
		}
		op.ack = t
		op.reply <- typ + " " + payload
	}
}

// do sends one op and waits for its answer. It returns false when the child
// is dead (the op is then left PENDING unless its answer already arrived).
func (s *session) do(op *liveOp) bool {
	op.reply = make(chan string, 1)
	op.eff = op.effects()
	s.mu.Lock()
	s.waiting[op.id] = op
	s.ops = append(s.ops, op)
	s.mu.Unlock()

	wire := op.wire()
	op.send = tick()
	s.log.sent(op)
	s.inMu.Lock()
	_, err := io.WriteString(s.stdin, wire)
	s.inMu.Unlock()
	if err != nil {
		// The child died while we were writing. Bytes may or may not have
		// reached it. PENDING is the only honest status.
		select {
		case r := <-op.reply:
			s.classify(op, r)
		default:
			op.st = stPending
		}
		s.log.result(op)
		return false
	}
	select {
	case r := <-op.reply:
		s.classify(op, r)
		s.log.result(op)
		return true
	case <-s.dead:
		select {
		case r := <-op.reply:
			s.classify(op, r)
		default:
			op.st = stPending
		}
		s.log.result(op)
		return false
	}
}

func (s *session) classify(op *liveOp, reply string) {
	typ, payload, _ := strings.Cut(reply, " ")
	switch typ {
	case "ACK", "NOTFOUND":
		op.st = stAcked
	case "VAL":
		op.st = stAcked
		op.msg = payload
	case "ERR":
		op.msg = payload
		if strings.HasPrefix(payload, "FULL") {
			op.st = stFull
		} else {
			op.st = stErr
		}
	default:
		op.st = stErr
		op.msg = reply
	}
}

func (s *session) isDead() bool {
	select {
	case <-s.dead:
		return true
	default:
		return false
	}
}

// kill sends SIGKILL (TerminateProcess on Windows). No graceful shutdown.
func (s *session) kill() {
	s.cmd.Process.Kill()
	<-s.dead
	s.cmd.Wait()
	s.stdin.Close()
}

func (s *session) addReadViolation(msg string) {
	s.mu.Lock()
	s.readViol = append(s.readViol, msg)
	s.mu.Unlock()
}

// ---------------------------------------------------------------------------
// oracle log: an append-only text file owned by the torture tool with every
// op sent and every answer received. The in-memory copy is what verify()
// uses; the file is there so a human can audit a failure.

type oracleLog struct {
	mu sync.Mutex
	w  *bufio.Writer
	f  *os.File
	it int
}

func openOracleLog(path string) (*oracleLog, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return &oracleLog{f: f, w: bufio.NewWriterSize(f, 1<<20)}, nil
}

func (l *oracleLog) sent(op *liveOp) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch op.kind {
	case "put":
		fmt.Fprintf(l.w, "it=%d t=%d SEND %s put %s len=%d\n", l.it, op.send, op.id, op.key, len(op.val))
	case "del", "get":
		fmt.Fprintf(l.w, "it=%d t=%d SEND %s %s %s\n", l.it, op.send, op.id, op.kind, op.key)
	case "batch":
		fmt.Fprintf(l.w, "it=%d t=%d SEND %s batch ops=%d keys=%d\n", l.it, op.send, op.id, len(op.batch), len(op.eff))
	default:
		fmt.Fprintf(l.w, "it=%d t=%d SEND %s %s\n", l.it, op.send, op.id, op.kind)
	}
}

func (l *oracleLog) result(op *liveOp) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.w, "it=%d t=%d %s %s\n", l.it, op.ack, op.st, op.id)
}

func (l *oracleLog) note(format string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.w, "it=%d NOTE "+format+"\n", append([]any{l.it}, a...)...)
}

func (l *oracleLog) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.w.Flush()
}
