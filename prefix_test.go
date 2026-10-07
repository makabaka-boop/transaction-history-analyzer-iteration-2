package main

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func parsePrefixJSON(t *testing.T, in string) *Log {
	t.Helper()
	l, err := ParseLogPrefix([]byte(in))
	if err != nil {
		t.Fatalf("ParseLogPrefix: %v", err)
	}
	return l
}

func prefixJSON(t *testing.T, l *Log) *Log {
	t.Helper()
	data, err := json.Marshal(l)
	if err != nil {
		t.Fatalf("marshal prefix: %v", err)
	}
	p, err := ParseLogPrefix(data)
	if err != nil {
		t.Fatalf("ParseLogPrefix: %v", err)
	}
	return p
}

func admissionFor(rep *Report, txn string) CommitAdmission {
	for _, a := range rep.CommitAdmissions {
		if a.Txn == txn {
			return a
		}
	}
	return CommitAdmission{Txn: txn, BlockingSources: nil}
}

func marshalJSON(t *testing.T, v any) map[string]any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal JSON: %v", err)
	}
	return out
}

func marshalString(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	return string(data)
}

func TestPrefixDirtyReadAbortedSourceBlocksCommit(t *testing.T) {
	in := `{
	  "transactions": ["T1", "T2"],
	  "ops": [
	    {"txn": "T1", "op": "WRITE", "key": "x", "value": 1},
	    {"txn": "T2", "op": "READ",  "key": "x"},
	    {"txn": "T1", "op": "ABORT"},
	    {"txn": "T2", "op": "COMMIT"}
	  ]
	}`
	var full Log
	if err := json.Unmarshal([]byte(in), &full); err != nil {
		t.Fatal(err)
	}

	one := AuditPrefix(prefixJSON(t, &Log{Txns: full.Txns, Ops: full.Ops[:1]}))
	if !one.Prefix || one.NumOps != 1 {
		t.Fatalf("first prefix = %+v, want marked prefix with one op", one)
	}
	if one.OK != true {
		t.Fatalf("OK before any violation = false, want true")
	}
	if one.Serializability.Status != serialAcyclicYet || !one.Serializability.Acyclic {
		t.Fatalf("serial = %+v, want acyclic-so-far", one.Serializability)
	}
	for _, name := range []string{"recoverable", "cascadeless", "strict"} {
		p := propByName(one, name)
		if p.Status != propNotViolatedYet || !p.OK || p.Violation != nil {
			t.Fatalf("%s = %+v, want not-violated-yet", name, p)
		}
	}
	if got := admissionFor(one, "T1"); !got.CanCommitNow || len(got.BlockingSources) != 0 {
		t.Fatalf("T1 admission = %+v, want commit now with no blockers", got)
	}
	if got := admissionFor(one, "T2"); !got.CanCommitNow || len(got.BlockingSources) != 0 {
		t.Fatalf("T2 admission = %+v, want commit now with no blockers", got)
	}
	if len(one.CommittedState) != 0 {
		t.Fatalf("committedState = %v, want empty", one.CommittedState)
	}
	if one.FinalState != nil {
		t.Fatalf("prefix report must not use finalState: %+v", one.FinalState)
	}

	two := AuditPrefix(prefixJSON(t, &Log{Txns: full.Txns, Ops: full.Ops[:2]}))
	if src := two.Reads[0].Source; src.Kind != "write" || src.Txn != "T1" || src.Seq != 1 {
		t.Fatalf("source = %+v, want T1 write at 1", src)
	}
	if len(two.Edges) != 1 || two.Edges[0].From != "T1" || two.Edges[0].To != "T2" {
		t.Fatalf("edges = %+v, want T1->T2", two.Edges)
	}
	if two.OK || violSeq(two.Cascadeless) != 2 || violSeq(two.Strict) != 2 {
		t.Fatalf("prefix after dirty read: ok=%v cas=%+v strict=%+v", two.OK, two.Cascadeless, two.Strict)
	}
	if two.Cascadeless.Status != propViolated || two.Strict.Status != propViolated {
		t.Fatalf("dirty-read statuses = %q %q, want determined", two.Cascadeless.Status, two.Strict.Status)
	}
	if two.Recoverable.Status != propNotViolatedYet || violSeq(two.Recoverable) != 0 {
		t.Fatalf("recoverable = %+v, want not violated yet", two.Recoverable)
	}
	got := admissionFor(two, "T2")
	if got.CanCommitNow || !reflect.DeepEqual(got.BlockingSources, []string{"T1"}) {
		t.Fatalf("T2 admission = %+v, want blocked by active T1", got)
	}

	three := AuditPrefix(prefixJSON(t, &Log{Txns: full.Txns, Ops: full.Ops[:3]}))
	got = admissionFor(three, "T2")
	if got.CanCommitNow || !reflect.DeepEqual(got.BlockingSources, []string{"T1"}) {
		t.Fatalf("T2 admission after T1 abort = %+v, want aborted T1 still blocking", got)
	}
	if violSeq(three.Cascadeless) != 2 || violSeq(three.Strict) != 2 {
		t.Fatalf("dirty-read evidence disappeared after abort: %+v %+v", three.Cascadeless, three.Strict)
	}

	four := AuditPrefix(prefixJSON(t, &full))
	want := Audit(parsePrefixJSON(t, in))
	if !reflect.DeepEqual(four, want) {
		t.Fatalf("complete prefix report differs from complete report:\ngot  %+v\nwant %+v", four, want)
	}
	fullDoc := marshalJSON(t, want)
	if _, ok := fullDoc["finalState"]; !ok {
		t.Fatalf("complete report must preserve finalState JSON field: %s", marshalString(t, want))
	}
	if _, ok := fullDoc["prefix"]; ok {
		t.Fatalf("complete report must not include prefix field")
	}
	if violSeq(four.Recoverable) != 4 || violSeq(four.Cascadeless) != 2 || violSeq(four.Strict) != 2 {
		t.Fatalf("final evidence = rec %d cas %d strict %d", violSeq(four.Recoverable), violSeq(four.Cascadeless), violSeq(four.Strict))
	}
	if len(four.FinalState) != 0 || four.CommittedState != nil || four.Prefix {
		t.Fatalf("complete report leaked prefix fields: %+v", four)
	}
}

func propByName(rep *Report, name string) PropResult {
	switch name {
	case "recoverable":
		return rep.Recoverable
	case "cascadeless":
		return rep.Cascadeless
	default:
		return rep.Strict
	}
}

func TestPrefixDeterminedCyclePersists(t *testing.T) {
	in := `{
	  "transactions": ["T1", "T2"],
	  "ops": [
	    {"txn": "T1", "op": "WRITE", "key": "x", "value": 1},
	    {"txn": "T2", "op": "READ",  "key": "x"},
	    {"txn": "T2", "op": "WRITE", "key": "y", "value": 2},
	    {"txn": "T1", "op": "READ",  "key": "y"},
	    {"txn": "T1", "op": "COMMIT"},
	    {"txn": "T2", "op": "COMMIT"}
	  ]
	}`
	var full Log
	if err := json.Unmarshal([]byte(in), &full); err != nil {
		t.Fatal(err)
	}
	partial := AuditPrefix(prefixJSON(t, &Log{Txns: full.Txns, Ops: full.Ops[:4]}))
	if partial.Serializability.Acyclic || partial.Serializability.Status != serialCyclic {
		t.Fatalf("serial = %+v, want determined cycle", partial.Serializability)
	}
	if partial.OK {
		t.Fatalf("OK = true with determined cycle")
	}
	edgeSet := map[[2]string]bool{}
	for _, e := range partial.Edges {
		edgeSet[[2]string{e.From, e.To}] = true
	}
	checkRealCycle(t, partial.Serializability.Cycle, edgeSet)

	final := AuditPrefix(prefixJSON(t, &full))
	if final.Serializability.Acyclic {
		t.Fatalf("cycle disappeared after appending terminators: %+v", final.Serializability)
	}
	checkRealCycle(t, final.Serializability.Cycle, edgeSet)
}

func TestPrefixCommittedStateAndAdmissions(t *testing.T) {
	rep := AuditPrefix(parsePrefixJSON(t, `{
	  "transactions": ["T1", "T2"],
	  "ops": [
	    {"txn": "T1", "op": "WRITE", "key": "x", "value": 1},
	    {"txn": "T1", "op": "COMMIT"},
	    {"txn": "T2", "op": "WRITE", "key": "x", "value": 2},
	    {"txn": "T2", "op": "READ",  "key": "z"}
	  ]
	}`))
	if got := rep.CommittedState["x"]; got != 1 {
		t.Fatalf("committedState[x] = %d, want committed value 1 only", got)
	}
	if _, ok := rep.CommittedState["z"]; ok {
		t.Fatalf("reads must not appear in committedState: %+v", rep.CommittedState)
	}
	if len(rep.CommitAdmissions) != 1 || rep.CommitAdmissions[0].Txn != "T2" {
		t.Fatalf("admissions = %+v, want only active T2", rep.CommitAdmissions)
	}
	if !rep.CommitAdmissions[0].CanCommitNow {
		t.Fatalf("T2 has no read dependency, admission = %+v", rep.CommitAdmissions[0])
	}
}

func TestPrefixValidationSharesIllegalOperationRules(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"unknown top field", `{"transactions":["T1","T2"],"ops":[],"bogus":1}`, "unknown field"},
		{"unknown op field", `{"transactions":["T1","T2"],"ops":[{"txn":"T1","op":"READ","key":"x","bogus":1}]}`, "unknown field"},
		{"undeclared", `{"transactions":["T1","T2"],"ops":[{"txn":"T9","op":"READ","key":"x"}]}`, "undeclared"},
		{"read no key", `{"transactions":["T1","T2"],"ops":[{"txn":"T1","op":"READ"}]}`, "requires a key"},
		{"commit key", `{"transactions":["T1","T2"],"ops":[{"txn":"T1","op":"COMMIT","key":"x"}]}`, "must not carry"},
		{"unknown op", `{"transactions":["T1","T2"],"ops":[{"txn":"T1","op":"DELETE","key":"x"}]}`, "unknown op"},
		{"after terminate", `{"transactions":["T1","T2"],"ops":[{"txn":"T1","op":"COMMIT"},{"txn":"T1","op":"ABORT"}]}`, "already terminated"},
		{"too many ops", tooManyPrefixOps(), "at most 500"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseLogPrefix([]byte(tc.in)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want substring %q", err, tc.want)
			}
		})
	}

	if _, err := ParseLogPrefix([]byte(`{"transactions":["T1","T2"],"ops":[]}`)); err != nil {
		t.Fatalf("empty prefix should be allowed: %v", err)
	}
	if _, err := ParseLog([]byte(`{"transactions":["T1","T2"],"ops":[]}`)); err == nil || !strings.Contains(err.Error(), "log is empty") {
		t.Fatalf("complete parser error = %v, want empty log", err)
	}
}

func tooManyPrefixOps() string {
	var b strings.Builder
	b.WriteString(`{"transactions":["T1","T2"],"ops":[`)
	for i := 0; i < 501; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"txn":"T1","op":"READ","key":"x"}`)
	}
	b.WriteString(`]}`)
	return b.String()
}

func TestCLIPrefixAndErrorPaths(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--prefix"}, strings.NewReader(`{"transactions":["T1","T2"],"ops":[{"txn":"T1","op":"COMMIT"}]}`), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d stderr=%s", code, stderr.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["prefix"] != true {
		t.Fatalf("stdout = %s, want prefix marker", stdout.String())
	}
	if _, ok := doc["finalState"]; ok {
		t.Fatalf("prefix stdout must not contain finalState: %s", stdout.String())
	}
	if _, ok := doc["committedState"]; !ok {
		t.Fatalf("prefix stdout must contain committedState: %s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = run([]string{"--prefix"}, strings.NewReader(`{`), &stdout, &stderr)
	if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "invalid JSON") {
		t.Fatalf("invalid input: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = run([]string{"a", "b"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("extra args exit = %d, want 2", code)
	}
}

func TestCrossCheckRandomPrefixes(t *testing.T) {
	for seed := int64(10001); seed <= 10120; seed++ {
		l := genLog(rand.New(rand.NewSource(seed)))
		var sawRec, sawCas, sawStr, sawCycle bool

		for k := 0; k <= len(l.Ops); k++ {
			p := prefixJSON(t, &Log{Txns: l.Txns, Ops: l.Ops[:k]})
			rep := AuditPrefix(p)

			if want := refReads(p); !reflect.DeepEqual(rep.Reads, want) {
				t.Fatalf("seed %d k %d reads mismatch:\ngot  %+v\nwant %+v", seed, k, rep.Reads, want)
			}
			wantEdges := refEdges(p)
			if !reflect.DeepEqual(rep.Edges, wantEdges) {
				t.Fatalf("seed %d k %d edges mismatch:\ngot  %+v\nwant %+v", seed, k, rep.Edges, wantEdges)
			}
			edgeSet := map[[2]string]bool{}
			for _, e := range wantEdges {
				edgeSet[[2]string{e.From, e.To}] = true
			}

			wantOrder, acyclic := refSerialOrder(p)
			if acyclic {
				if !rep.Serializability.Acyclic {
					t.Fatalf("seed %d k %d cycle %v, reference order %v", seed, k, rep.Serializability.Cycle, wantOrder)
				}
				if !reflect.DeepEqual(rep.Serializability.Order, wantOrder) {
					t.Fatalf("seed %d k %d order got %v want %v", seed, k, rep.Serializability.Order, wantOrder)
				}
			} else {
				if rep.Serializability.Acyclic {
					t.Fatalf("seed %d k %d reports order %v, reference found cycle", seed, k, rep.Serializability.Order)
				}
				checkRealCycle(t, rep.Serializability.Cycle, edgeSet)
			}

			wantRec, wantCas, wantStr := refFirstViolations(p)
			wantAdmissions, wantActive := refAdmissions(p)
			if got := violSeq(rep.Recoverable); got != wantRec {
				t.Fatalf("seed %d k %d recoverable seq got %d want %d", seed, k, got, wantRec)
			}
			if got := violSeq(rep.Cascadeless); got != wantCas {
				t.Fatalf("seed %d k %d cascadeless seq got %d want %d", seed, k, got, wantCas)
			}
			if got := violSeq(rep.Strict); got != wantStr {
				t.Fatalf("seed %d k %d strict seq got %d want %d", seed, k, got, wantStr)
			}
			if !reflect.DeepEqual(rep.CommitAdmissions, wantAdmissions) {
				t.Fatalf("seed %d k %d admissions got %+v want %+v", seed, k, rep.CommitAdmissions, wantAdmissions)
			}
			wantOK := !wantActive || (wantRec == 0 && wantCas == 0 && wantStr == 0 && acyclic)
			if rep.OK != wantOK {
				t.Fatalf("seed %d k %d ok got %v want %v", seed, k, rep.OK, wantOK)
			}

			if wantActive {
				checkPrefixProp(t, "recoverable", seed, k, rep.Recoverable, wantRec)
				checkPrefixProp(t, "cascadeless", seed, k, rep.Cascadeless, wantCas)
				checkPrefixProp(t, "strict", seed, k, rep.Strict, wantStr)
				if !rep.Prefix {
					t.Fatalf("seed %d k %d missing prefix marker", seed, k)
				}
				wantSerialStatus := serialAcyclicYet
				if !acyclic {
					wantSerialStatus = serialCyclic
				}
				if rep.Serializability.Status != wantSerialStatus {
					t.Fatalf("seed %d k %d serial status = %q, want %q", seed, k, rep.Serializability.Status, wantSerialStatus)
				}
				if rep.FinalState != nil {
					t.Fatalf("seed %d k %d prefix leaked finalState", seed, k)
				}
				if wantState := refCommittedState(p); !reflect.DeepEqual(rep.CommittedState, wantState) {
					t.Fatalf("seed %d k %d committed state got %v want %v", seed, k, rep.CommittedState, wantState)
				}
			} else {
				full := Audit(p)
				if !reflect.DeepEqual(rep, full) {
					t.Fatalf("seed %d k %d complete prefix differs from Audit:\ngot  %+v\nwant %+v", seed, k, rep, full)
				}
				if wantState := refCommittedState(p); !reflect.DeepEqual(rep.FinalState, wantState) {
					t.Fatalf("seed %d k %d final state got %v want %v", seed, k, rep.FinalState, wantState)
				}
			}

			// Determined evidence cannot be removed by merely appending more
			// operations; it can remain a prefix violation or become the final
			// violation, but its first seq is stable.
			if wantRec != 0 {
				if sawRec && violSeq(rep.Recoverable) != wantRec {
					t.Fatalf("seed %d recoverable evidence changed", seed)
				}
				sawRec = true
			}
			if wantCas != 0 {
				if sawCas && violSeq(rep.Cascadeless) != wantCas {
					t.Fatalf("seed %d cascadeless evidence changed", seed)
				}
				sawCas = true
			}
			if wantStr != 0 {
				if sawStr && violSeq(rep.Strict) != wantStr {
					t.Fatalf("seed %d strict evidence changed", seed)
				}
				sawStr = true
			}
			if !acyclic {
				if sawCycle && rep.Serializability.Acyclic {
					t.Fatalf("seed %d cycle disappeared after appending", seed)
				}
				sawCycle = true
			}
		}
	}
}

func checkPrefixProp(t *testing.T, name string, seed int64, k int, got PropResult, wantSeq int) {
	t.Helper()
	if violSeq(got) != wantSeq {
		t.Fatalf("seed %d k %d %s seq got %d want %d", seed, k, name, violSeq(got), wantSeq)
	}
	if wantSeq == 0 {
		if got.Status != propNotViolatedYet || !got.OK {
			t.Fatalf("seed %d k %d %s = %+v, want not-violated-yet", seed, k, name, got)
		}
	} else {
		if got.Status != propViolated || got.OK {
			t.Fatalf("seed %d k %d %s = %+v, want determined-violation", seed, k, name, got)
		}
	}
}

func refAdmissions(l *Log) ([]CommitAdmission, bool) {
	commit := refCommitSeq(l)
	aborted := map[string]bool{}
	for _, op := range l.Ops {
		if op.Type == OpAbort {
			aborted[op.Txn] = true
		}
	}
	blockersByTxn := map[string]map[string]bool{}
	for _, op := range l.Ops {
		if op.Type != OpRead {
			continue
		}
		if ws, ok := refSourceOf(l, op.Seq); ok {
			writer := l.Ops[ws-1].Txn
			if writer != op.Txn {
				if _, committed := commit[writer]; !committed {
					if blockersByTxn[op.Txn] == nil {
						blockersByTxn[op.Txn] = map[string]bool{}
					}
					blockersByTxn[op.Txn][writer] = true
				}
			}
		}
	}

	var active []string
	for _, txn := range l.Txns {
		if _, isCommitted := commit[txn]; !isCommitted && !aborted[txn] {
			active = append(active, txn)
		}
	}
	sort.Strings(active)
	if len(active) == 0 {
		return nil, false
	}
	out := make([]CommitAdmission, 0, len(active))
	for _, txn := range active {
		var blockers []string
		for b := range blockersByTxn[txn] {
			blockers = append(blockers, b)
		}
		sort.Strings(blockers)
		if blockers == nil {
			blockers = []string{}
		}
		out = append(out, CommitAdmission{Txn: txn, CanCommitNow: len(blockers) == 0, BlockingSources: blockers})
	}
	return out, len(active) > 0
}

func refCommittedState(l *Log) map[string]int64 {
	commit := refCommitSeq(l)
	state := map[string]int64{}
	for _, op := range l.Ops {
		if op.Type == OpWrite {
			if _, ok := commit[op.Txn]; ok {
				state[op.Key] = op.Value
			}
		}
	}
	return state
}
