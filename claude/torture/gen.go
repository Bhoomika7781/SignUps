package main

// gen.go: random operations and self-checking values.
//
// Every value the torture tool writes looks like
//
//	<opid>|<key>|<total length>|<filler>
//
// where filler is generated deterministically from the first three fields.
// So from any value read back we can tell:
//   - which operation wrote it (opid),
//   - which key it was written to (catches "value from a different key"),
//   - whether it is complete and untouched (regenerate it and compare;
//     catches torn and garbage values).

import (
	"fmt"
	"hash/fnv"
	"math/rand"
	"strconv"
	"strings"
)

const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

func makeValue(opID, key string, size int) string {
	head := ""
	for {
		head = opID + "|" + key + "|" + strconv.Itoa(size) + "|"
		if len(head) <= size {
			break
		}
		size = len(head)
	}
	b := make([]byte, size)
	copy(b, head)
	h := fnv.New64a()
	h.Write([]byte(head))
	x := h.Sum64()
	for i := len(head); i < size; i++ {
		x = x*6364136223846793005 + 1442695040888963407
		b[i] = alphabet[(x>>33)%uint64(len(alphabet))]
	}
	return string(b)
}

// parseValue checks a value read back from the database.
// ok is false if the value is torn, garbage, or otherwise not exactly what
// makeValue produced.
func parseValue(v string) (opID, key string, ok bool) {
	parts := strings.SplitN(v, "|", 4)
	if len(parts) != 4 {
		return "", "", false
	}
	size, err := strconv.Atoi(parts[2])
	if err != nil || size != len(v) {
		return "", "", false
	}
	if makeValue(parts[0], parts[1], size) != v {
		return "", "", false
	}
	return parts[0], parts[1], true
}

type bop struct {
	del      bool
	key, val string
}

// genOp is one operation the torture tool sends to `kvdb serve`.
type genOp struct {
	id    string
	kind  string // put, del, batch, get, compact
	key   string
	val   string
	batch []bop
}

// wire returns the exact text sent on stdin.
func (g *genOp) wire() string {
	var b strings.Builder
	switch g.kind {
	case "put":
		b.Grow(len(g.id) + len(g.key) + len(g.val) + 8)
		fmt.Fprintf(&b, "put %s %s %s\n", g.id, g.key, g.val)
	case "del":
		fmt.Fprintf(&b, "del %s %s\n", g.id, g.key)
	case "get":
		fmt.Fprintf(&b, "get %s %s\n", g.id, g.key)
	case "compact":
		fmt.Fprintf(&b, "compact %s\n", g.id)
	case "batch":
		size := 32
		for _, o := range g.batch {
			size += len(o.key) + len(o.val) + 6
		}
		b.Grow(size)
		fmt.Fprintf(&b, "batch %s %d\n", g.id, len(g.batch))
		for _, o := range g.batch {
			if o.del {
				b.WriteString("del " + o.key + "\n")
			} else {
				b.WriteString("put " + o.key + " " + o.val + "\n")
			}
		}
	}
	return b.String()
}

// effect is what an op does to one key: either puts val or deletes.
type effect struct {
	del bool
	val string
}

// effects returns the FINAL effect of the op on each key it touches.
// Inside a batch, ops are applied in order, so the last op on a key wins
// (this is the B5 ordering rule documented in the README).
func (g *genOp) effects() map[string]effect {
	switch g.kind {
	case "put":
		return map[string]effect{g.key: {val: g.val}}
	case "del":
		return map[string]effect{g.key: {del: true}}
	case "batch":
		m := make(map[string]effect, len(g.batch))
		for _, o := range g.batch {
			m[o.key] = effect{del: o.del, val: o.val}
		}
		return m
	}
	return nil
}

// modeCfg describes the workload of one torture mode.
type modeCfg struct {
	keys          int     // keyspace size: k0 .. k<keys-1>
	pPut, pDel    float64 // probability of a single put / single delete; the rest are batches
	batchMin      int
	batchMax      int
	fullBatchProb float64 // probability that a batch has exactly batchMax ops
	batchDelPct   int     // % of ops inside a batch that are deletes
	valMin        int
	valMax        int
	bigProb       float64 // probability that a value is large
	bigMin        int     // large values are bigMin..1MB (half of them exactly 1MB)
	killMinMs     int
	killMaxMs     int
	maxBytes      int64
	compactor     bool // send explicit compactions concurrently with writers
	reader        bool // run a concurrent reader that validates every get
	recoveryKills int  // kill `kvdb open` up to this many times per iteration
	prefill       int  // keys to write before the first iteration
	prefillVal    int
}

var modes = map[string]modeCfg{
	"mixed": {keys: 300, pPut: 0.50, pDel: 0.15, batchMin: 2, batchMax: 40, batchDelPct: 20,
		valMin: 1, valMax: 300, bigProb: 0.01, bigMin: 64 << 10,
		killMinMs: 1, killMaxMs: 400, reader: true},
	"large-values": {keys: 40, pPut: 0.60, pDel: 0.10, batchMin: 2, batchMax: 4, batchDelPct: 15,
		valMin: 1, valMax: 2000, bigProb: 0.8, bigMin: 256 << 10,
		killMinMs: 5, killMaxMs: 1500, reader: true},
	"large-batch": {keys: 30000, pPut: 0.10, pDel: 0.05, batchMin: 5000, batchMax: 10000, fullBatchProb: 0.5,
		batchDelPct: 10, valMin: 1, valMax: 64, killMinMs: 20, killMaxMs: 1500},
	"compaction": {keys: 60, pPut: 0.55, pDel: 0.25, batchMin: 2, batchMax: 20, batchDelPct: 30,
		valMin: 100, valMax: 4000, killMinMs: 1, killMaxMs: 600, compactor: true, reader: true},
	"recovery-crash": {keys: 5000, pPut: 0.40, pDel: 0.10, batchMin: 10, batchMax: 200, batchDelPct: 10,
		valMin: 50, valMax: 2000, killMinMs: 20, killMaxMs: 1500, recoveryKills: 3,
		prefill: 30000, prefillVal: 300},
	"disk-full": {keys: 100, pPut: 0.50, pDel: 0.20, batchMin: 2, batchMax: 30, batchDelPct: 20,
		valMin: 200, valMax: 6000, killMinMs: 5, killMaxMs: 500, maxBytes: 256 << 10, reader: true},
}

// gen produces the op stream of one writer. Its randomness comes only from
// its own *rand.Rand, so the stream is fixed by the seed.
type gen struct {
	cfg    modeCfg
	r      *rand.Rand
	prefix string
	seq    int
}

func newGen(cfg modeCfg, r *rand.Rand, prefix string) *gen {
	return &gen{cfg: cfg, r: r, prefix: prefix}
}

func (g *gen) nextID() string {
	g.seq++
	return g.prefix + strconv.Itoa(g.seq)
}

func (g *gen) key() string { return "k" + strconv.Itoa(g.r.Intn(g.cfg.keys)) }

func (g *gen) valSize() int {
	c := g.cfg
	if c.bigProb > 0 && g.r.Float64() < c.bigProb {
		if g.r.Intn(2) == 0 {
			return 1 << 20
		}
		return c.bigMin + g.r.Intn((1<<20)-c.bigMin+1)
	}
	return c.valMin + g.r.Intn(c.valMax-c.valMin+1)
}

func (g *gen) next() *genOp {
	c := g.cfg
	id := g.nextID()
	x := g.r.Float64()
	switch {
	case x < c.pPut:
		k := g.key()
		return &genOp{id: id, kind: "put", key: k, val: makeValue(id, k, g.valSize())}
	case x < c.pPut+c.pDel:
		return &genOp{id: id, kind: "del", key: g.key()}
	}
	n := c.batchMin + g.r.Intn(c.batchMax-c.batchMin+1)
	if c.fullBatchProb > 0 && g.r.Float64() < c.fullBatchProb {
		n = c.batchMax
	}
	op := &genOp{id: id, kind: "batch", batch: make([]bop, n)}
	for i := range op.batch {
		k := g.key()
		if g.r.Intn(100) < c.batchDelPct {
			op.batch[i] = bop{del: true, key: k}
		} else {
			op.batch[i] = bop{key: k, val: makeValue(id, k, g.valSize())}
		}
	}
	return op
}
