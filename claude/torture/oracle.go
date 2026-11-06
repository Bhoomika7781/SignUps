package main

// oracle.go: deciding whether what the database shows after a crash is legal.
//
// The oracle never asks the database what it promised. It only uses what the
// torture tool itself saw: what it sent, and which ACKs came back, plus a
// global event clock:
//   op.send = clock tick taken just BEFORE the op was written to stdin
//   op.ack  = clock tick taken just AFTER the ACK line was read
//
// If a.ack < b.send then a was durable and applied before b was even sent,
// so b must have been applied after a ("real-time order").
//
// Rules checked for every key (Section 4 of the problem statement):
//   - The value must be a valid self-checking value written for THIS key
//     (no torn values, no garbage, no value from another key).
//   - The value must come from an op that may legally be the last write:
//       * the baseline from the previous iteration, unless an ACKed op on
//         this key exists (it would have replaced it);
//       * an ACKed op, unless another ACKed op on this key was sent after
//         it was ACKed (then it was overwritten - a lost update if visible);
//       * a pending op (sent, no answer before the kill): may or may not be
//         applied;
//       * never an op answered with ERR FULL (those must leave no trace).
//   - "Absent" is legal only if the baseline was absent and is still allowed,
//     or a legal delete exists.
// Plus batch atomicity for pending batches: if ANY key shows the batch's
// value, every other key of the batch must show the batch's effect, or a
// value from an op that may have come after the batch.

import (
	"fmt"
	"sort"
	"strings"
)

type status int

const (
	stPending status = iota // no answer before the kill: may or may not be applied
	stAcked                 // ACK received: must be durable
	stFull                  // ERR FULL: must not be applied
	stErr                   // other ERR: treated as "may or may not be applied"
)

func (s status) String() string {
	return [...]string{"PENDING", "ACKED", "ERR-FULL", "ERR"}[s]
}

type liveOp struct {
	*genOp
	eff   map[string]effect
	send  int64
	ack   int64
	st    status
	msg   string // payload of the reply (value for VAL, error text for ERR)
	reply chan string
}

func (o *liveOp) describe() string {
	return fmt.Sprintf("%s(%s, %d keys, send=%d ack=%d, %s)", o.id, o.kind, len(o.eff), o.send, o.ack, o.st)
}

// verify compares the observed dump with what the oracle allows.
// base is the state verified at the end of the previous iteration.
func verify(base map[string]string, ops []*liveOp, observed map[string]string) []string {
	byKey := map[string][]*liveOp{}
	byID := map[string]*liveOp{}
	for _, o := range ops {
		if o.eff == nil {
			continue
		}
		byID[o.id] = o
		for k := range o.eff {
			byKey[k] = append(byKey[k], o)
		}
	}
	keys := map[string]struct{}{}
	for k := range base {
		keys[k] = struct{}{}
	}
	for k := range byKey {
		keys[k] = struct{}{}
	}
	for k := range observed {
		keys[k] = struct{}{}
	}

	var viol []string
	bad := func(format string, a ...any) { viol = append(viol, fmt.Sprintf(format, a...)) }

	for k := range keys {
		touching := byKey[k]
		baseVal, baseHas := base[k]
		baseAllowed := true
		for _, o := range touching {
			if o.st == stAcked {
				baseAllowed = false
				break
			}
		}
		allowed := func(o *liveOp) bool {
			switch o.st {
			case stFull:
				return false
			case stPending, stErr:
				return true
			}
			for _, o2 := range touching {
				if o2 != o && o2.st == stAcked && o2.send > o.ack {
					return false
				}
			}
			return true
		}
		expectList := func() string {
			var s []string
			if baseAllowed {
				if baseHas {
					s = append(s, "baseline value "+short(baseVal))
				} else {
					s = append(s, "baseline ABSENT")
				}
			}
			for _, o := range touching {
				if allowed(o) {
					e := o.eff[k]
					if e.del {
						s = append(s, "ABSENT from "+o.describe())
					} else {
						s = append(s, "value of "+o.describe())
					}
				}
			}
			if len(s) == 0 {
				return "(nothing)"
			}
			return strings.Join(s, "; ")
		}

		obsVal, obsHas := observed[k]
		if !obsHas {
			ok := !baseHas && baseAllowed
			for _, o := range touching {
				if o.eff[k].del && allowed(o) {
					ok = true
				}
			}
			if !ok {
				bad("key %s is MISSING. Allowed: %s", k, expectList())
			}
			continue
		}

		if baseHas && obsVal == baseVal {
			if !baseAllowed {
				var acked []string
				for _, o := range touching {
					if o.st == stAcked {
						acked = append(acked, o.describe())
					}
				}
				bad("key %s still has its OLD value %s, but these ACKed ops changed it: %s",
					k, short(obsVal), strings.Join(acked, ", "))
			}
			continue
		}

		opID, vk, ok := parseValue(obsVal)
		if !ok {
			bad("key %s holds a TORN or GARBAGE value (len %d): %s", k, len(obsVal), short(obsVal))
			continue
		}
		if vk != k {
			bad("key %s holds a value that op %s wrote for a DIFFERENT key %s", k, opID, vk)
			continue
		}
		o := byID[opID]
		if o == nil || o.eff[k].del || o.eff[k].val != obsVal {
			bad("key %s holds value from op %s, which is not a write to this key in this iteration "+
				"(stale or resurrected). Allowed: %s", k, opID, expectList())
			continue
		}
		if o.st == stFull {
			bad("key %s holds value from op %s, which was REJECTED with ERR FULL", k, o.describe())
			continue
		}
		if !allowed(o) {
			var later []string
			for _, o2 := range touching {
				if o2 != o && o2.st == stAcked && o2.send > o.ack {
					later = append(later, o2.describe())
				}
			}
			bad("key %s holds value from %s, but it was overwritten by later ACKed op(s) %s (LOST UPDATE)",
				k, o.describe(), strings.Join(later, ", "))
		}
	}

	// Batch atomicity for batches whose fate is unknown.
	for _, b := range ops {
		if b.kind != "batch" || (b.st != stPending && b.st != stErr) {
			continue
		}
		applied := false
		var witness string
		for k, e := range b.eff {
			if !e.del && observed[k] == e.val {
				applied, witness = true, k
				break
			}
		}
		if !applied {
			continue
		}
		for k, e := range b.eff {
			v, has := observed[k]
			if has && !e.del && v == e.val {
				continue
			}
			if !has && e.del {
				continue
			}
			// Something else is visible here. That is fine only if it was
			// written by an op that may have run AFTER the batch.
			explained := false
			if has {
				if id, _, ok := parseValue(v); ok {
					if o2 := byID[id]; o2 != nil && o2 != b && mayFollow(o2, b) {
						explained = true
					}
				}
			} else {
				for _, o2 := range byKey[k] {
					if o2 != b && o2.eff[k].del && mayFollow(o2, b) {
						explained = true
					}
				}
			}
			if !explained {
				bad("PARTIAL BATCH %s: key %s shows the batch was applied, but key %s shows %s instead of the batch's effect",
					b.describe(), witness, k, showObs(v, has))
			}
		}
	}

	sort.Strings(viol)
	return viol
}

// mayFollow: could o2 have been applied after batch b?
func mayFollow(o2, b *liveOp) bool {
	if o2.st == stFull {
		return false
	}
	if o2.st == stAcked && o2.ack < b.send {
		return false // o2 finished before b even started
	}
	return true
}

func showObs(v string, has bool) string {
	if !has {
		return "ABSENT"
	}
	return short(v)
}

func short(v string) string {
	if len(v) > 60 {
		return fmt.Sprintf("%q...(len %d)", v[:60], len(v))
	}
	return fmt.Sprintf("%q", v)
}
