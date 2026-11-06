package main

import (
	"bufio"
	"fmt"
	"hash/crc32"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// The oracle is the torture tool's own record of what the database promised.
// It never reads the database to decide what is "expected": it only learns
// from the ops it generated and the ACK / ERR lines it received. Every ACK is
// also appended to oracle.log on disk.
//
// Key spaces:
//
//	p-<n>   preload keys, written before the kill clock starts
//	w<i>-<n> private keys of writer i (only writer i touches them, batches too)
//	s-<n>   shared keys; both writers do single puts/deletes on them
//
// Each writer waits for the response to one op before sending the next, so a
// writer has at most one op in flight when the kill lands.

const (
	statusPending = iota // sent, no response: may or may not be applied
	statusAcked          // ACK received: MUST be applied
	statusFailed         // ERR received: must NOT be applied
)

type mut struct {
	key string
	del bool
	val string
}

type opRec struct {
	id      string
	writer  int
	kind    string // put, del, batch
	muts    []mut
	sendSeq int64
	ackSeq  int64
	status  int
	errMsg  string
}

type oracle struct {
	mu   sync.Mutex
	seq  atomic.Int64
	base map[string]string // verified state after the previous iteration
	ops  []*opRec          // ops of the current iteration
	log  *bufio.Writer
	file *os.File
	iter int
}

func newOracle(path string) (*oracle, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return &oracle{base: map[string]string{}, log: bufio.NewWriterSize(f, 1<<20), file: f}, nil
}

func (o *oracle) startIteration(it int) {
	o.mu.Lock()
	o.iter = it
	o.ops = nil
	o.mu.Unlock()
}

func (o *oracle) sent(op *opRec) {
	op.sendSeq = o.seq.Add(1)
	op.ackSeq = math.MaxInt64
	o.mu.Lock()
	o.ops = append(o.ops, op)
	o.mu.Unlock()
}

func (o *oracle) acked(op *opRec) {
	op.ackSeq = o.seq.Add(1)
	o.mu.Lock()
	op.status = statusAcked
	fmt.Fprintf(o.log, "it=%d ACK %s %s\n", o.iter, op.id, describe(op))
	o.mu.Unlock()
}

func (o *oracle) failed(op *opRec, msg string) {
	o.mu.Lock()
	op.status = statusFailed
	op.errMsg = msg
	fmt.Fprintf(o.log, "it=%d ERR %s %s :: %s\n", o.iter, op.id, describe(op), msg)
	o.mu.Unlock()
}

// preloaded records keys that were ACKed before the kill clock started.
func (o *oracle) preloaded(id string, keys, vals []string) {
	o.mu.Lock()
	for i := range keys {
		o.base[keys[i]] = vals[i]
	}
	fmt.Fprintf(o.log, "it=%d ACK %s preload %d keys %s..%s\n", o.iter, id, len(keys), keys[0], keys[len(keys)-1])
	o.mu.Unlock()
}

func (o *oracle) flush() { o.log.Flush() }

func describe(op *opRec) string {
	var b strings.Builder
	b.WriteString(op.kind)
	for i, m := range op.muts {
		if i == 8 {
			fmt.Fprintf(&b, " ...(+%d)", len(op.muts)-8)
			break
		}
		if m.del {
			fmt.Fprintf(&b, " del:%s", m.key)
		} else {
			fmt.Fprintf(&b, " put:%s=%s", m.key, short(m.val))
		}
	}
	return b.String()
}

// short prints a value compactly: long values become len+crc.
func short(v string) string {
	if len(v) <= 40 {
		return v
	}
	return fmt.Sprintf("%s..(len=%d crc=%08x)", v[:24], len(v), crc32.ChecksumIEEE([]byte(v)))
}

func keyOwner(k string) (kind string, writer int) {
	switch {
	case strings.HasPrefix(k, "p-"):
		return "preload", -1
	case strings.HasPrefix(k, "s-"):
		return "shared", -1
	case strings.HasPrefix(k, "w"):
		var w int
		if _, err := fmt.Sscanf(k, "w%d-", &w); err == nil {
			return "private", w
		}
	}
	return "unknown", -1
}

type state struct {
	present bool
	val     string
}

func (s state) String() string {
	if !s.present {
		return "<absent>"
	}
	return short(s.val)
}

func lookup(m map[string]string, k string) state {
	v, ok := m[k]
	return state{ok, v}
}

// verify compares the dump against the oracle. It returns human-readable
// failure lines (empty slice = pass).
func (o *oracle) verify(observed map[string]string, writers int) []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	var fails []string

	// 1. No unknown keys (values from nowhere / keys from a different space).
	for k := range observed {
		if kind, _ := keyOwner(k); kind == "unknown" {
			fails = append(fails, fmt.Sprintf("unknown key %q appeared with value %s", k, short(observed[k])))
		}
	}

	// 2. Preload keys never change after they are written.
	for k, v := range o.base {
		if kind, _ := keyOwner(k); kind == "preload" {
			if got := lookup(observed, k); got != (state{true, v}) {
				fails = append(fails, fmt.Sprintf("preload key %s: expected %s, found %s", k, short(v), got))
			}
		}
	}
	for k := range observed {
		if kind, _ := keyOwner(k); kind == "preload" {
			if _, ok := o.base[k]; !ok {
				fails = append(fails, fmt.Sprintf("preload key %s present but never ACKed", k))
			}
		}
	}

	// 3. Private keys, per writer: exactly "all ACKed ops applied" or
	// "all ACKed ops + the whole in-flight op applied". This is what checks
	// batch atomicity: half a batch matches neither.
	for w := 0; w < writers; w++ {
		fails = append(fails, o.verifyWriter(w, observed)...)
	}

	// 4. Shared keys: the value must come from some op S (or the previous
	// state), and no op that was ACKed must have been sent after S was ACKed
	// (it would have had to overwrite S).
	fails = append(fails, o.verifyShared(observed)...)
	return fails
}

func (o *oracle) verifyWriter(w int, observed map[string]string) []string {
	prefix := fmt.Sprintf("w%d-", w)
	expA := map[string]string{}
	for k, v := range o.base {
		if strings.HasPrefix(k, prefix) {
			expA[k] = v
		}
	}
	var mine []*opRec
	for _, op := range o.ops {
		if op.writer == w {
			mine = append(mine, op)
		}
	}
	sort.Slice(mine, func(i, j int) bool { return mine[i].sendSeq < mine[j].sendSeq })

	var pending []*opRec
	lastAck := map[string]string{} // key -> id of last acked op that touched it
	for _, op := range mine {
		switch op.status {
		case statusAcked:
			for _, m := range op.muts {
				if strings.HasPrefix(m.key, prefix) {
					apply(expA, m)
					lastAck[m.key] = op.id
				}
			}
		case statusPending:
			pending = append(pending, op)
		}
	}
	if len(pending) > 1 {
		return []string{fmt.Sprintf("torture tool bug: writer %d has %d ops in flight", w, len(pending))}
	}

	keys := map[string]bool{}
	for k := range expA {
		keys[k] = true
	}
	for k := range observed {
		if strings.HasPrefix(k, prefix) {
			keys[k] = true
		}
	}
	var expB map[string]string
	var inflight *opRec
	if len(pending) == 1 {
		inflight = pending[0]
		expB = map[string]string{}
		for k, v := range expA {
			expB[k] = v
		}
		for _, m := range inflight.muts {
			if strings.HasPrefix(m.key, prefix) {
				apply(expB, m)
				keys[m.key] = true
			}
		}
	}

	diff := func(exp map[string]string) []string {
		var d []string
		for k := range keys {
			if lookup(observed, k) != lookup(exp, k) {
				d = append(d, k)
			}
		}
		sort.Strings(d)
		return d
	}
	dA := diff(expA)
	if len(dA) == 0 {
		return nil
	}
	if expB != nil && len(diff(expB)) == 0 {
		return nil
	}

	var out []string
	head := fmt.Sprintf("writer %d: state matches neither 'all ACKed ops applied' nor 'ACKed ops + in-flight op applied'", w)
	if inflight != nil {
		head += fmt.Sprintf("; in-flight op %s = %s", inflight.id, describe(inflight))
	}
	out = append(out, head)
	show := func(title string, ks []string, exp map[string]string) {
		out = append(out, "  "+title)
		for i, k := range ks {
			if i == 8 {
				out = append(out, fmt.Sprintf("    ... and %d more keys", len(ks)-8))
				break
			}
			out = append(out, fmt.Sprintf("    key %s: found %s, expected %s (last ACKed op on key: %s)", k, lookup(observed, k), lookup(exp, k), orNone(lastAck[k])))
		}
	}
	show("compared with 'all ACKed ops applied':", dA, expA)
	if expB != nil {
		dB := diff(expB)
		show(fmt.Sprintf("compared with 'ACKed ops + in-flight %s applied':", inflight.id), dB, expB)
		if inflight.kind == "batch" {
			out = append(out, fmt.Sprintf("  => batch %s is PARTIALLY visible: %d of its keys show it, %d do not", inflight.id, len(dA), len(dB)))
		}
	}
	return out
}

func (o *oracle) verifyShared(observed map[string]string) []string {
	type source struct {
		id      string
		st      state
		ackSeq  int64
		sendSeq int64
		status  int
	}
	byKey := map[string][]source{}
	keys := map[string]bool{}
	for k := range o.base {
		if kind, _ := keyOwner(k); kind == "shared" {
			keys[k] = true
		}
	}
	for k := range observed {
		if kind, _ := keyOwner(k); kind == "shared" {
			keys[k] = true
		}
	}
	for _, op := range o.ops {
		if op.status == statusFailed {
			continue
		}
		for _, m := range op.muts {
			if kind, _ := keyOwner(m.key); kind == "shared" {
				keys[m.key] = true
				byKey[m.key] = append(byKey[m.key], source{op.id, state{!m.del, m.val}, op.ackSeq, op.sendSeq, op.status})
			}
		}
	}

	var fails []string
	var sorted []string
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		got := lookup(observed, k)
		cands := append([]source{{id: "previous-state", st: lookup(o.base, k), ackSeq: -1, sendSeq: -1, status: statusAcked}}, byKey[k]...)
		ok := false
		for _, s := range cands {
			if s.st != got {
				continue
			}
			overwritten := false
			for _, q := range byKey[k] {
				if q.id != s.id && q.status == statusAcked && q.sendSeq > s.ackSeq {
					overwritten = true
					break
				}
			}
			if !overwritten {
				ok = true
				break
			}
		}
		if !ok {
			line := fmt.Sprintf("shared key %s: found %s; candidates:", k, got)
			for _, s := range cands {
				st := map[int]string{statusPending: "in-flight", statusAcked: "ACKed", statusFailed: "ERR"}[s.status]
				line += fmt.Sprintf(" [%s %s %s sent=%d ack=%d]", s.id, st, s.st, s.sendSeq, s.ackSeq)
			}
			fails = append(fails, line)
		}
	}
	return fails
}

func apply(m map[string]string, mu mut) {
	if mu.del {
		delete(m, mu.key)
	} else {
		m[mu.key] = mu.val
	}
}

func orNone(s string) string {
	if s == "" {
		return "none this iteration"
	}
	return s
}

// commit makes the verified observed state the base for the next iteration.
func (o *oracle) commit(observed map[string]string) {
	o.mu.Lock()
	o.base = observed
	o.mu.Unlock()
}
