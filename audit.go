package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// ---------- input model ----------

// OpType is one of the four logged operation kinds.
type OpType string

const (
	OpRead   OpType = "READ"
	OpWrite  OpType = "WRITE"
	OpCommit OpType = "COMMIT"
	OpAbort  OpType = "ABORT"
)

// Op is a single logged operation. Seq is the 1-based position in the log,
// assigned by ParseLog (any seq present in the input is ignored). Value is
// only meaningful for WRITE (default 0).
type Op struct {
	Seq   int    `json:"seq"`
	Txn   string `json:"txn"`
	Type  OpType `json:"op"`
	Key   string `json:"key,omitempty"`
	Value int64  `json:"value,omitempty"`
}

// Log is the audited input: 2..8 transactions and up to 500 ordered ops.
type Log struct {
	Txns []string `json:"transactions"`
	Ops  []Op     `json:"ops"`
}

const (
	minTxns = 2
	maxTxns = 8
	maxOps  = 500
)

// ParseLog decodes and validates a complete log. Every declared transaction
// must have exactly one terminator.
func ParseLog(data []byte) (*Log, error) {
	return parseLog(data, false)
}

// ParseLogPrefix decodes and validates a still-growing log prefix. Declared
// transactions may remain unterminated, but duplicate terminators, operations
// after termination, illegal fields and unknown JSON fields are still rejected.
func ParseLogPrefix(data []byte) (*Log, error) {
	return parseLog(data, true)
}

func parseLog(data []byte, prefix bool) (*Log, error) {
	var l Log
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&l); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if len(l.Txns) < minTxns || len(l.Txns) > maxTxns {
		return nil, fmt.Errorf("transactions: need %d..%d distinct ids, got %d", minTxns, maxTxns, len(l.Txns))
	}
	declared := make(map[string]bool, len(l.Txns))
	for _, t := range l.Txns {
		if t == "" {
			return nil, fmt.Errorf("transactions: empty id")
		}
		if declared[t] {
			return nil, fmt.Errorf("transactions: duplicate id %q", t)
		}
		declared[t] = true
	}
	if !prefix && len(l.Ops) == 0 {
		return nil, fmt.Errorf("ops: log is empty")
	}
	if len(l.Ops) > maxOps {
		return nil, fmt.Errorf("ops: at most %d operations allowed, got %d", maxOps, len(l.Ops))
	}
	terminated := make(map[string]int, len(l.Txns)) // txn -> seq of its terminator
	for i := range l.Ops {
		op := &l.Ops[i]
		op.Seq = i + 1
		if !declared[op.Txn] {
			return nil, fmt.Errorf("op %d: undeclared transaction %q", op.Seq, op.Txn)
		}
		switch op.Type {
		case OpRead, OpWrite:
			if op.Key == "" {
				return nil, fmt.Errorf("op %d: %s requires a key", op.Seq, op.Type)
			}
		case OpCommit, OpAbort:
			if op.Key != "" {
				return nil, fmt.Errorf("op %d: %s must not carry a key", op.Seq, op.Type)
			}
		default:
			return nil, fmt.Errorf("op %d: unknown op %q (want READ, WRITE, COMMIT or ABORT)", op.Seq, op.Type)
		}
		if t, ok := terminated[op.Txn]; ok {
			return nil, fmt.Errorf("op %d: transaction %q already terminated at op %d", op.Seq, op.Txn, t)
		}
		if op.Type == OpCommit || op.Type == OpAbort {
			terminated[op.Txn] = op.Seq
		}
	}
	if !prefix {
		for _, t := range l.Txns {
			if _, ok := terminated[t]; !ok {
				return nil, fmt.Errorf("transaction %q never terminates: exactly one COMMIT or ABORT is required", t)
			}
		}
	}
	return &l, nil
}

// ---------- report model ----------

// Source describes where a READ obtained its value: the latest preceding
// WRITE on the key (even if that write was later aborted), or the initial
// version when no WRITE precedes the read.
type Source struct {
	Kind  string `json:"kind"` // "initial" or "write"
	Seq   int    `json:"seq,omitempty"`
	Txn   string `json:"txn,omitempty"`
	Value *int64 `json:"value,omitempty"`
}

// ReadFact records one READ and the source it observed.
type ReadFact struct {
	Seq    int    `json:"seq"`
	Txn    string `json:"txn"`
	Key    string `json:"key"`
	Value  *int64 `json:"value,omitempty"`
	Source Source `json:"source"`
}

// Conflict is one ordered pair of conflicting ops (same key, different
// transactions, at least one WRITE). Kind is "RW", "WR" or "WW".
type Conflict struct {
	Key     string `json:"key"`
	Kind    string `json:"kind"`
	FromSeq int    `json:"fromSeq"`
	ToSeq   int    `json:"toSeq"`
}

// Edge is a directed edge From -> To in the conflict graph, with every
// conflicting op pair that witnesses it.
type Edge struct {
	From      string     `json:"from"`
	To        string     `json:"to"`
	Conflicts []Conflict `json:"conflicts"`
}

// Violation pinpoints the first operation breaking a property.
type Violation struct {
	Seq    int    `json:"seq"`
	Txn    string `json:"txn"`
	Op     OpType `json:"op"`
	Key    string `json:"key,omitempty"`
	Reason string `json:"reason"`
}

const (
	propViolated       = "determined-violation"
	propNotViolatedYet = "not-violated-yet"

	serialCyclic     = "determined-cycle"
	serialAcyclicYet = "acyclic-so-far"
)

// PropResult is the verdict for one execution property. Status is reported
// only for prefixes: it distinguishes a determined violation from a property
// that has merely not been violated yet.
type PropResult struct {
	OK        bool       `json:"ok"`
	Status    string     `json:"status,omitempty"`
	Violation *Violation `json:"violation,omitempty"`
}

// SerialResult reports conflict serializability: the lexicographically
// smallest serial order (ids compared bytewise) when the conflict graph is
// acyclic, otherwise one real directed cycle.
type SerialResult struct {
	Acyclic bool     `json:"acyclic"`
	Status  string   `json:"status,omitempty"`
	Order   []string `json:"order,omitempty"`
	Cycle   []string `json:"cycle,omitempty"`
}

// CommitAdmission says whether an active transaction could commit at the
// current end of a prefix. An aborted read source remains a permanent blocker.
type CommitAdmission struct {
	Txn             string   `json:"txn"`
	CanCommitNow    bool     `json:"canCommitNow"`
	BlockingSources []string `json:"blockingSources"`
}

// Report is the full audit output. In prefix mode FinalState is omitted and
// CommittedState names the same committed values for what has been seen so
// far; the report is explicitly marked as a prefix and must not be read as a
// verdict for a complete log.
type Report struct {
	OK               bool              `json:"ok"`
	Prefix           bool              `json:"prefix,omitempty"`
	Transactions     []string          `json:"transactions"`
	NumOps           int               `json:"numOps"`
	Reads            []ReadFact        `json:"reads"`
	Edges            []Edge            `json:"edges"`
	Serializability  SerialResult      `json:"serializability"`
	Recoverable      PropResult        `json:"recoverable"`
	Cascadeless      PropResult        `json:"cascadeless"`
	Strict           PropResult        `json:"strict"`
	FinalState       map[string]int64  `json:"finalState"`
	CommittedState   map[string]int64  `json:"committedState,omitempty"`
	CommitAdmissions []CommitAdmission `json:"commitAdmissions,omitempty"`
}

// MarshalJSON keeps complete reports byte-for-byte on the old field set
// (including an empty "finalState": {}) while hiding that field in prefix
// reports.
func (r Report) MarshalJSON() ([]byte, error) {
	type reportWire Report
	if !r.Prefix {
		return json.Marshal(reportWire(r))
	}
	type prefixWire struct {
		*reportWire
		FinalState     map[string]int64 `json:"finalState,omitempty"`
		CommittedState map[string]int64 `json:"committedState"`
	}
	wire := reportWire(r)
	return json.Marshal(prefixWire{
		reportWire:     &wire,
		CommittedState: wire.CommittedState,
	})
}

// ---------- audit ----------

// auditState is the single streaming state used by both complete-log and
// prefix audits. This keeps parsing-time legality and audit-time history on
// the same operation rules.
type auditState struct {
	committed map[string]int              // txn -> commit seq
	aborted   map[string]int              // txn -> abort seq
	lastWrite map[string]int              // key -> seq of latest WRITE
	readsFrom map[string]map[string][]int // reader -> writer -> read seqs
	reads     []ReadFact
	recV      *Violation
	casV      *Violation
	strV      *Violation
}

func newAuditState(txnCount int) *auditState {
	return &auditState{
		committed: make(map[string]int, txnCount),
		aborted:   make(map[string]int, txnCount),
		lastWrite: make(map[string]int),
		readsFrom: make(map[string]map[string][]int),
	}
}

func (s *auditState) apply(l *Log, op *Op) {
	switch op.Type {
	case OpRead:
		fact := ReadFact{Seq: op.Seq, Txn: op.Txn, Key: op.Key}
		if ws, ok := s.lastWrite[op.Key]; ok {
			w := &l.Ops[ws-1]
			v := w.Value
			fact.Value = &v
			fact.Source = Source{Kind: "write", Seq: w.Seq, Txn: w.Txn, Value: &v}
			if w.Txn != op.Txn {
				if s.readsFrom[op.Txn] == nil {
					s.readsFrom[op.Txn] = map[string][]int{}
				}
				s.readsFrom[op.Txn][w.Txn] = append(s.readsFrom[op.Txn][w.Txn], op.Seq)
				if _, ok := s.committed[w.Txn]; !ok {
					// The source write is uncommitted: its transaction is
					// still active or already aborted (an abort does not
					// cleanse the write).
					if s.casV == nil {
						s.casV = &Violation{Seq: op.Seq, Txn: op.Txn, Op: op.Type, Key: op.Key,
							Reason: fmt.Sprintf("reads uncommitted write of transaction %s (op %d)", w.Txn, w.Seq)}
					}
					if s.strV == nil {
						s.strV = &Violation{Seq: op.Seq, Txn: op.Txn, Op: op.Type, Key: op.Key,
							Reason: fmt.Sprintf("reads uncommitted write of transaction %s (op %d)", w.Txn, w.Seq)}
					}
				}
			}
		} else {
			fact.Source = Source{Kind: "initial"}
		}
		s.reads = append(s.reads, fact)
	case OpWrite:
		if ws, ok := s.lastWrite[op.Key]; ok {
			w := &l.Ops[ws-1]
			if w.Txn != op.Txn {
				if _, ok := s.committed[w.Txn]; !ok && s.strV == nil {
					s.strV = &Violation{Seq: op.Seq, Txn: op.Txn, Op: op.Type, Key: op.Key,
						Reason: fmt.Sprintf("overwrites uncommitted write of transaction %s (op %d)", w.Txn, w.Seq)}
				}
			}
		}
		s.lastWrite[op.Key] = op.Seq
	case OpCommit:
		// Recoverable: every transaction this one has read from must
		// already be committed. If a source aborted or is still active,
		// committing now is the first recoverability violation.
		if s.recV == nil {
			for _, wt := range sortedReadSources(s.readsFrom[op.Txn]) {
				if _, ok := s.committed[wt]; !ok {
					s.recV = &Violation{Seq: op.Seq, Txn: op.Txn, Op: op.Type,
						Reason: fmt.Sprintf("commits before its source transaction %s (read at op %d) has committed", wt, s.readsFrom[op.Txn][wt][0])}
					break
				}
			}
		}
		s.committed[op.Txn] = op.Seq
	case OpAbort:
		// Aborting is always safe for the aborting transaction itself;
		// readers of its writes are caught at their own COMMIT.
		s.aborted[op.Txn] = op.Seq
	}
}

func sortedReadSources(m map[string][]int) []string {
	out := make([]string, 0, len(m))
	for t := range m {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

func committedState(l *Log, committed map[string]int) map[string]int64 {
	state := map[string]int64{}
	for i := range l.Ops {
		op := &l.Ops[i]
		if op.Type == OpWrite {
			if _, ok := committed[op.Txn]; ok {
				state[op.Key] = op.Value
			}
		}
	}
	return state
}

func propResult(v *Violation) PropResult {
	return PropResult{OK: v == nil, Violation: v}
}

func prefixPropResult(v *Violation) PropResult {
	if v != nil {
		return PropResult{OK: false, Status: propViolated, Violation: v}
	}
	return PropResult{OK: true, Status: propNotViolatedYet}
}

// Audit scans the log once in order and derives the full report.
//
// The properties are judged purely by log position, never by the final
// state: a write becomes visible to other transactions only when its
// transaction COMMITs, and an ABORT never erases a dirty read that already
// happened.
func Audit(l *Log) *Report {
	s := newAuditState(len(l.Txns))
	for i := range l.Ops {
		s.apply(l, &l.Ops[i])
	}

	rep := &Report{
		OK:           true,
		Transactions: l.Txns,
		NumOps:       len(l.Ops),
		Reads:        s.reads,
		FinalState:   committedState(l, s.committed),
	}
	rep.Edges = conflictEdges(l)
	rep.Serializability = classify(l.Txns, rep.Edges)
	rep.Recoverable = propResult(s.recV)
	rep.Cascadeless = propResult(s.casV)
	rep.Strict = propResult(s.strV)
	return rep
}

// AuditPrefix audits a log prefix that may contain active transactions. A
// prefix with all declared transactions already terminated has the same
// verdict as Audit; otherwise its verdict is explicitly limited to the
// operations observed so far.
func AuditPrefix(l *Log) *Report {
	if allTerminated(l) {
		return Audit(l)
	}

	s := newAuditState(len(l.Txns))
	for i := range l.Ops {
		s.apply(l, &l.Ops[i])
	}

	rep := &Report{
		OK:             s.recV == nil && s.casV == nil && s.strV == nil,
		Prefix:         true,
		Transactions:   l.Txns,
		NumOps:         len(l.Ops),
		Reads:          s.reads,
		CommittedState: committedState(l, s.committed),
	}
	rep.Edges = conflictEdges(l)
	rep.Serializability = classify(l.Txns, rep.Edges)
	if rep.Serializability.Acyclic {
		rep.Serializability.Status = serialAcyclicYet
	} else {
		rep.Serializability.Status = serialCyclic
		rep.OK = false
	}
	rep.Recoverable = prefixPropResult(s.recV)
	rep.Cascadeless = prefixPropResult(s.casV)
	rep.Strict = prefixPropResult(s.strV)

	active := make([]string, 0)
	for _, t := range l.Txns {
		if _, committed := s.committed[t]; !committed {
			if _, aborted := s.aborted[t]; !aborted {
				active = append(active, t)
			}
		}
	}
	sort.Strings(active)
	rep.CommitAdmissions = make([]CommitAdmission, 0, len(active))
	for _, t := range active {
		blockers := make([]string, 0)
		for _, source := range sortedReadSources(s.readsFrom[t]) {
			if _, ok := s.committed[source]; !ok {
				blockers = append(blockers, source)
			}
		}
		rep.CommitAdmissions = append(rep.CommitAdmissions, CommitAdmission{
			Txn:             t,
			CanCommitNow:    len(blockers) == 0,
			BlockingSources: blockers,
		})
	}
	return rep
}

func allTerminated(l *Log) bool {
	terminated := make(map[string]bool, len(l.Txns))
	for i := range l.Ops {
		op := &l.Ops[i]
		if op.Type == OpCommit || op.Type == OpAbort {
			terminated[op.Txn] = true
		}
	}
	for _, t := range l.Txns {
		if !terminated[t] {
			return false
		}
	}
	return true
}

// conflictEdges builds the directed conflict graph: for every ordered pair
// of ops (a before b in the log) on the same key by different transactions
// where at least one is a WRITE, an edge a.Txn -> b.Txn.
func conflictEdges(l *Log) []Edge {
	type pair struct{ from, to string }
	byPair := map[pair][]Conflict{}
	for i := range l.Ops {
		a := &l.Ops[i]
		if a.Type != OpRead && a.Type != OpWrite {
			continue
		}
		for j := i + 1; j < len(l.Ops); j++ {
			b := &l.Ops[j]
			if b.Type != OpRead && b.Type != OpWrite {
				continue
			}
			if a.Txn == b.Txn || a.Key != b.Key {
				continue
			}
			if a.Type == OpRead && b.Type == OpRead {
				continue
			}
			p := pair{a.Txn, b.Txn}
			byPair[p] = append(byPair[p], Conflict{
				Key:     a.Key,
				Kind:    string(a.Type[0]) + string(b.Type[0]),
				FromSeq: a.Seq,
				ToSeq:   b.Seq,
			})
		}
	}
	edges := make([]Edge, 0, len(byPair))
	for p, cs := range byPair {
		edges = append(edges, Edge{From: p.from, To: p.to, Conflicts: cs})
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}
		return edges[i].To < edges[j].To
	})
	return edges
}

// classify topologically sorts the conflict graph. When it is acyclic the
// result is the lexicographically smallest serial order (transaction ids
// compared bytewise); otherwise a real directed cycle is returned.
func classify(txns []string, edges []Edge) SerialResult {
	adj := make(map[string][]string, len(txns))
	indeg := make(map[string]int, len(txns))
	for _, t := range txns {
		indeg[t] = 0
	}
	for _, e := range edges {
		adj[e.From] = append(adj[e.From], e.To)
		indeg[e.To]++
	}
	for t := range adj {
		sort.Strings(adj[t])
	}

	// Kahn's algorithm, always emitting the byte-order-smallest available
	// id: this yields the lexicographically smallest topological order.
	remaining := make(map[string]bool, len(txns))
	for _, t := range txns {
		remaining[t] = true
	}
	order := make([]string, 0, len(txns))
	for len(remaining) > 0 {
		best := ""
		for t := range remaining {
			if indeg[t] == 0 && (best == "" || t < best) {
				best = t
			}
		}
		if best == "" {
			return SerialResult{Acyclic: false, Cycle: findCycle(txns, adj)}
		}
		order = append(order, best)
		delete(remaining, best)
		for _, m := range adj[best] {
			indeg[m]--
		}
	}
	return SerialResult{Acyclic: true, Order: order}
}

// findCycle returns one real directed cycle as a list of transaction ids
// with the first id repeated at the end; every consecutive pair (including
// last -> first) is an edge of the graph. Deterministic: transactions and
// adjacency lists are visited in byte order.
func findCycle(txns []string, adj map[string][]string) []string {
	const (
		white = iota
		gray
		black
	)
	color := make(map[string]int, len(txns))
	var stack []string
	var visit func(u string) []string
	visit = func(u string) []string {
		color[u] = gray
		stack = append(stack, u)
		for _, v := range adj[u] {
			switch color[v] {
			case gray:
				start := 0
				for stack[start] != v {
					start++
				}
				cycle := append([]string{}, stack[start:]...)
				return append(cycle, v)
			case white:
				if c := visit(v); c != nil {
					return c
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[u] = black
		return nil
	}
	ordered := append([]string{}, txns...)
	sort.Strings(ordered)
	for _, t := range ordered {
		if color[t] == white {
			if c := visit(t); c != nil {
				return c
			}
		}
	}
	return nil
}
