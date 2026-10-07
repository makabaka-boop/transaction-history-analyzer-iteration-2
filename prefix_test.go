package main

// Tests for the optional live-prefix mode: prefix parsing relaxes only the
// termination rule; prefix reports label properties established vs not yet
// violated instead of final booleans; every open transaction gets a
// commit-now recoverability admission. The randomized truncation test cuts
// complete logs op by op into prefixes and cross-checks blocking sets,
// evidence persistence and full-termination agreement against the
// independent reference interpreter in reference_test.go.

import (
	"encoding/json"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func auditPrefixJSON(t *testing.T, in string) *Report {
	t.Helper()
	l, err := ParseLogPrefix([]byte(in))
	if err != nil {
		t.Fatalf("ParseLogPrefix: %v", err)
	}
	return Audit(l)
}

// Prefix mode relaxes only the termination rule: an unterminated declared
// transaction is fine, everything else is validated exactly as before.
func TestPrefixParseRelaxesTerminationOnly(t *testing.T) {
	in := `{"transactions":["T1","T2"],"ops":[{"txn":"T1","op":"READ","key":"x"}]}`
	if _, err := ParseLog([]byte(in)); err == nil || !strings.Contains(err.Error(), "never terminates") {
		t.Fatalf("ParseLog: err = %v, want 'never terminates'", err)
	}
	rep := auditPrefixJSON(t, in)
	if rep.Mode != ModePrefix {
		t.Fatalf("mode = %q, want %q", rep.Mode, ModePrefix)
	}
	// Both transactions are open; neither read from another transaction.
	for _, ot := range rep.OpenTransactions {
		if !ot.CommitNow.Admissible || len(ot.CommitNow.BlockedBy) != 0 {
			t.Fatalf("commitNow[%s] = %+v, want admissible with no blockers", ot.Txn, ot.CommitNow)
		}
	}
	if len(rep.OpenTransactions) != 2 {
		t.Fatalf("openTransactions = %+v, want T1 and T2", rep.OpenTransactions)
	}
}

// Duplicate termination, operations after a terminator and malformed fields
// are still rejected in prefix mode.
func TestPrefixParseStillRejects(t *testing.T) {
	cases := []struct {
		name, in, wantErr string
	}{
		{"bad json", `{`, "invalid JSON"},
		{"unknown top-level field", `{"transactions":["T1","T2"],"ops":[{"txn":"T1","op":"COMMIT"}],"extra":1}`, "invalid JSON"},
		{"unknown op field", `{"transactions":["T1","T2"],"ops":[{"txn":"T1","op":"READ","key":"x","foo":1}]}`, "invalid JSON"},
		{"one transaction", `{"transactions":["T1"],"ops":[{"txn":"T1","op":"COMMIT"}]}`, "need 2..8"},
		{"duplicate id", `{"transactions":["T1","T1"],"ops":[{"txn":"T1","op":"COMMIT"}]}`, "duplicate"},
		{"empty ops", `{"transactions":["T1","T2"],"ops":[]}`, "log is empty"},
		{"undeclared txn", `{"transactions":["T1","T2"],"ops":[{"txn":"T9","op":"READ","key":"x"}]}`, "undeclared transaction"},
		{"read without key", `{"transactions":["T1","T2"],"ops":[{"txn":"T1","op":"READ"}]}`, "requires a key"},
		{"commit with key", `{"transactions":["T1","T2"],"ops":[{"txn":"T1","op":"COMMIT","key":"x"}]}`, "must not carry a key"},
		{"unknown op", `{"transactions":["T1","T2"],"ops":[{"txn":"T1","op":"DELETE","key":"x"}]}`, "unknown op"},
		{"duplicate termination", `{"transactions":["T1","T2"],"ops":[
			{"txn":"T1","op":"COMMIT"},
			{"txn":"T1","op":"ABORT"}]}`, "already terminated"},
		{"op after termination", `{"transactions":["T1","T2"],"ops":[
			{"txn":"T1","op":"ABORT"},
			{"txn":"T1","op":"READ","key":"x"}]}`, "already terminated"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseLogPrefix([]byte(tc.in)); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

// An aborted source must still block a commit-now, exactly as it would break
// recoverability at a real COMMIT.
func TestPrefixAbortedSourceBlocks(t *testing.T) {
	rep := auditPrefixJSON(t, `{
	  "transactions": ["T1", "T2", "T3"],
	  "ops": [
	    {"txn": "T1", "op": "WRITE", "key": "x", "value": 9},
	    {"txn": "T1", "op": "ABORT"},
	    {"txn": "T2", "op": "READ",  "key": "x"},
	    {"txn": "T3", "op": "READ",  "key": "z"}
	  ]
	}`)
	if len(rep.OpenTransactions) != 2 {
		t.Fatalf("openTransactions = %+v, want T2 and T3 (T1 aborted)", rep.OpenTransactions)
	}
	ot := rep.OpenTransactions[0]
	if ot.Txn != "T2" || ot.CommitNow.Admissible ||
		!reflect.DeepEqual(ot.CommitNow.BlockedBy, []string{"T1"}) {
		t.Fatalf("commitNow[T2] = %+v, want blocked by [T1] (aborted source)", ot)
	}
	ot = rep.OpenTransactions[1]
	if ot.Txn != "T3" || !ot.CommitNow.Admissible || len(ot.CommitNow.BlockedBy) != 0 {
		t.Fatalf("commitNow[T3] = %+v, want admissible (read the initial version)", ot)
	}
}

// Active and committed sources: only the uncommitted ones block; blockers
// are sorted by transaction id regardless of read order.
func TestPrefixCommitNowAdmission(t *testing.T) {
	rep := auditPrefixJSON(t, `{
	  "transactions": ["b", "a", "c"],
	  "ops": [
	    {"txn": "b", "op": "WRITE", "key": "x", "value": 1},
	    {"txn": "a", "op": "WRITE", "key": "y", "value": 2},
	    {"txn": "c", "op": "READ",  "key": "x"},
	    {"txn": "c", "op": "READ",  "key": "y"}
	  ]
	}`)
	want := []OpenTransaction{
		{Txn: "a", CommitNow: CommitAdmission{Admissible: true, BlockedBy: []string{}}},
		{Txn: "b", CommitNow: CommitAdmission{Admissible: true, BlockedBy: []string{}}},
		{Txn: "c", CommitNow: CommitAdmission{Admissible: false, BlockedBy: []string{"a", "b"}}},
	}
	if !reflect.DeepEqual(rep.OpenTransactions, want) {
		t.Fatalf("openTransactions = %+v, want %+v", rep.OpenTransactions, want)
	}

	// Once the source commits, the admission clears.
	rep = auditPrefixJSON(t, `{
	  "transactions": ["T1", "T2"],
	  "ops": [
	    {"txn": "T1", "op": "WRITE", "key": "x", "value": 1},
	    {"txn": "T1", "op": "COMMIT"},
	    {"txn": "T2", "op": "READ",  "key": "x"}
	  ]
	}`)
	if len(rep.OpenTransactions) != 1 || rep.OpenTransactions[0].Txn != "T2" ||
		!rep.OpenTransactions[0].CommitNow.Admissible {
		t.Fatalf("openTransactions = %+v, want T2 admissible (source committed)", rep.OpenTransactions)
	}
}

// Established evidence persists as the log grows: the dirty read at seq 2 is
// established from the 2-op prefix onward, the unrecoverable commit at seq 4
// from the 4-op prefix onward, and the fully terminated prefix agrees with
// the complete-log report.
func TestPrefixEstablishedEvidencePersists(t *testing.T) {
	ops := []string{
		`{"txn":"T1","op":"WRITE","key":"x","value":1}`,
		`{"txn":"T2","op":"READ","key":"x"}`,
		`{"txn":"T2","op":"WRITE","key":"x","value":2}`,
		`{"txn":"T2","op":"COMMIT"}`,
		`{"txn":"T1","op":"ABORT"}`,
	}
	prefix := func(k int) *Report {
		return auditPrefixJSON(t, `{"transactions":["T1","T2"],"ops":[`+strings.Join(ops[:k], ",")+`]}`)
	}

	rep := prefix(2)
	if rep.Cascadeless.Status != StatusEstablished || violSeq(rep.Cascadeless) != 2 {
		t.Fatalf("cascadeless = %+v, want established at seq 2", rep.Cascadeless)
	}
	if rep.Strict.Status != StatusEstablished || violSeq(rep.Strict) != 2 {
		t.Fatalf("strict = %+v, want established at seq 2", rep.Strict)
	}
	if rep.Recoverable.Status != StatusNotYetViolated {
		t.Fatalf("recoverable = %+v, want not yet violated (T2 has not committed)", rep.Recoverable)
	}

	rep = prefix(4)
	if rep.Recoverable.Status != StatusEstablished || violSeq(rep.Recoverable) != 4 {
		t.Fatalf("recoverable = %+v, want established at seq 4", rep.Recoverable)
	}
	if violSeq(rep.Cascadeless) != 2 || violSeq(rep.Strict) != 2 {
		t.Fatalf("earlier evidence changed: cascadeless %+v strict %+v", rep.Cascadeless, rep.Strict)
	}

	rep = prefix(5)
	full := auditJSON(t, `{"transactions":["T1","T2"],"ops":[`+strings.Join(ops, ",")+`]}`)
	if len(rep.OpenTransactions) != 0 {
		t.Fatalf("openTransactions = %+v, want none (all terminated)", rep.OpenTransactions)
	}
	for name, got := range map[string]*Violation{
		"recoverable": rep.Recoverable.Violation,
		"cascadeless": rep.Cascadeless.Violation,
		"strict":      rep.Strict.Violation,
	} {
		var want *Violation
		switch name {
		case "recoverable":
			want = full.Recoverable.Violation
		case "cascadeless":
			want = full.Cascadeless.Violation
		case "strict":
			want = full.Strict.Violation
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s violation = %+v, want the full report's %+v", name, got, want)
		}
	}
}

// The prefix report must carry statuses and prefix-scoped fields, never the
// final-verdict shape of a complete log.
func TestPrefixReportJSONShape(t *testing.T) {
	rep := auditPrefixJSON(t, `{
	  "transactions": ["T1", "T2"],
	  "ops": [
	    {"txn": "T1", "op": "WRITE", "key": "x", "value": 1},
	    {"txn": "T2", "op": "READ",  "key": "x"}
	  ]
	}`)
	data, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["mode"] != ModePrefix {
		t.Fatalf("mode = %v, want %q", m["mode"], ModePrefix)
	}
	if _, ok := m["finalState"]; ok {
		t.Fatalf("prefix report must not carry finalState: %s", data)
	}
	if _, ok := m["committedAsOfPrefix"]; !ok {
		t.Fatalf("prefix report must carry committedAsOfPrefix: %s", data)
	}
	if _, ok := m["openTransactions"]; !ok {
		t.Fatalf("prefix report must carry openTransactions: %s", data)
	}
	for _, name := range []string{"recoverable", "cascadeless", "strict"} {
		p, ok := m[name].(map[string]any)
		if !ok {
			t.Fatalf("%s missing or not an object: %s", name, data)
		}
		if _, ok := p["ok"]; ok {
			t.Fatalf("%s must not carry a final ok in prefix mode: %s", name, data)
		}
		if p["status"] == "" {
			t.Fatalf("%s must carry a status in prefix mode: %s", name, data)
		}
	}
	if got := m["cascadeless"].(map[string]any)["status"]; got != StatusEstablished {
		t.Fatalf("cascadeless status = %v, want established (dirty read happened)", got)
	}
	if got := m["recoverable"].(map[string]any)["status"]; got != StatusNotYetViolated {
		t.Fatalf("recoverable status = %v, want not yet violated", got)
	}
	ser, ok := m["serializability"].(map[string]any)
	if !ok || ser["status"] != StatusNotYetViolated {
		t.Fatalf("serializability = %v, want status notYetViolated", ser)
	}
	if _, ok := ser["acyclic"]; ok {
		t.Fatalf("serializability must not carry a final acyclic in prefix mode: %s", data)
	}
}

// Without --prefix the report fields are exactly the ones the auditor has
// always emitted — pinned byte for byte.
func TestFullReportGoldenJSON(t *testing.T) {
	rep := auditJSON(t, `{"transactions":["T1","T2"],"ops":[
	  {"txn":"T1","op":"WRITE","key":"x","value":1},
	  {"txn":"T1","op":"COMMIT"},
	  {"txn":"T2","op":"READ","key":"x"},
	  {"txn":"T2","op":"COMMIT"}]}`)
	got, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"ok":true,"transactions":["T1","T2"],"numOps":4,` +
		`"reads":[{"seq":3,"txn":"T2","key":"x","value":1,"source":{"kind":"write","seq":1,"txn":"T1","value":1}}],` +
		`"edges":[{"from":"T1","to":"T2","conflicts":[{"key":"x","kind":"WR","fromSeq":1,"toSeq":3}]}],` +
		`"serializability":{"acyclic":true,"order":["T1","T2"]},` +
		`"recoverable":{"ok":true},"cascadeless":{"ok":true},"strict":{"ok":true},` +
		`"finalState":{"x":1}}`
	if string(got) != want {
		t.Fatalf("full report JSON changed:\n got %s\nwant %s", got, want)
	}
}

// The empty committed state of a complete log still serializes as
// "finalState": {} — and prefix mode never emits it under that name.
func TestEmptyStateFieldNames(t *testing.T) {
	in := `{"transactions":["T1","T2"],"ops":[
	  {"txn":"T1","op":"WRITE","key":"x","value":1},
	  {"txn":"T1","op":"ABORT"},
	  {"txn":"T2","op":"ABORT"}]}`
	got, err := json.Marshal(auditJSON(t, in))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(got), `"finalState":{}`) {
		t.Fatalf("full report lost the empty finalState field: %s", got)
	}
	got, err = json.Marshal(auditPrefixJSON(t, in))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(got), `"finalState"`) || !strings.Contains(string(got), `"committedAsOfPrefix":{}`) {
		t.Fatalf("prefix report must rename the state field: %s", got)
	}
	if !strings.Contains(string(got), `"openTransactions":[]`) {
		t.Fatalf("prefix report with all terminated must carry an empty openTransactions: %s", got)
	}
}

// The prefix report for a live prefix, pinned byte for byte.
func TestPrefixReportGoldenJSON(t *testing.T) {
	rep := auditPrefixJSON(t, `{"transactions":["T1","T2"],"ops":[
	  {"txn":"T1","op":"WRITE","key":"x","value":1},
	  {"txn":"T2","op":"READ","key":"x"}]}`)
	got, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"ok":true,"mode":"prefix","transactions":["T1","T2"],"numOps":2,` +
		`"reads":[{"seq":2,"txn":"T2","key":"x","value":1,"source":{"kind":"write","seq":1,"txn":"T1","value":1}}],` +
		`"edges":[{"from":"T1","to":"T2","conflicts":[{"key":"x","kind":"WR","fromSeq":1,"toSeq":2}]}],` +
		`"serializability":{"status":"notYetViolated","order":["T1","T2"]},` +
		`"recoverable":{"status":"notYetViolated"},` +
		`"cascadeless":{"status":"established","violation":{"seq":2,"txn":"T2","op":"READ","key":"x","reason":"reads uncommitted write of transaction T1 (op 1)"}},` +
		`"strict":{"status":"established","violation":{"seq":2,"txn":"T2","op":"READ","key":"x","reason":"reads uncommitted write of transaction T1 (op 1)"}},` +
		`"committedAsOfPrefix":{},` +
		`"openTransactions":[{"txn":"T1","commitNow":{"admissible":true,"blockedBy":[]}},` +
		`{"txn":"T2","commitNow":{"admissible":false,"blockedBy":["T1"]}}]}`
	if string(got) != want {
		t.Fatalf("prefix report JSON:\n got %s\nwant %s", got, want)
	}
}

func TestParseArgs(t *testing.T) {
	cases := []struct {
		args       []string
		wantPrefix bool
		wantFile   string
		wantErr    bool
	}{
		{nil, false, "", false},
		{[]string{"log.json"}, false, "log.json", false},
		{[]string{"--prefix"}, true, "", false},
		{[]string{"--prefix", "log.json"}, true, "log.json", false},
		{[]string{"log.json", "--prefix"}, true, "log.json", false},
		{[]string{"-prefix", "log.json"}, true, "log.json", false},
		{[]string{"a.json", "b.json"}, false, "", true},
		{[]string{"--prefix", "a.json", "b.json"}, false, "", true},
	}
	for _, tc := range cases {
		prefix, file, err := parseArgs(tc.args)
		if (err != nil) != tc.wantErr {
			t.Fatalf("parseArgs(%v): err = %v, wantErr %v", tc.args, err, tc.wantErr)
		}
		if err == nil && (prefix != tc.wantPrefix || file != tc.wantFile) {
			t.Fatalf("parseArgs(%v) = (%v, %q), want (%v, %q)",
				tc.args, prefix, file, tc.wantPrefix, tc.wantFile)
		}
	}
}

// prefixOf re-parses the first k operations of l in prefix mode, exercising
// the same JSON path the CLI uses.
func prefixOf(t *testing.T, l *Log, k int) *Log {
	t.Helper()
	data, err := json.Marshal(Log{Txns: l.Txns, Ops: l.Ops[:k]})
	if err != nil {
		t.Fatalf("marshal prefix: %v", err)
	}
	pl, err := ParseLogPrefix(data)
	if err != nil {
		t.Fatalf("ParseLogPrefix(%d ops): %v", k, err)
	}
	return pl
}

// refBlocking recomputes, for every unterminated transaction, the sorted set
// of source transactions that would block a COMMIT issued right now: every
// transaction it has read from that has not committed (an aborted or still
// active source blocks). Written independently of the audit core: plain
// scans, no shared helpers.
func refBlocking(l *Log) map[string][]string {
	committed := map[string]bool{}
	terminated := map[string]bool{}
	for _, op := range l.Ops {
		switch op.Type {
		case OpCommit:
			committed[op.Txn] = true
			terminated[op.Txn] = true
		case OpAbort:
			terminated[op.Txn] = true
		}
	}
	out := map[string][]string{}
	for _, t := range l.Txns {
		if terminated[t] {
			continue
		}
		blocked := []string{}
		seen := map[string]bool{}
		for _, op := range l.Ops {
			if op.Txn != t || op.Type != OpRead {
				continue
			}
			for j := op.Seq - 1; j >= 1; j-- {
				w := l.Ops[j-1]
				if w.Type == OpWrite && w.Key == op.Key {
					if w.Txn != t && !committed[w.Txn] && !seen[w.Txn] {
						seen[w.Txn] = true
						blocked = append(blocked, w.Txn)
					}
					break
				}
			}
		}
		sort.Strings(blocked)
		out[t] = blocked
	}
	return out
}

// refCommittedState recomputes the committed values as of the prefix: writes
// of transactions committed inside the prefix, applied in log order.
func refCommittedState(l *Log) map[string]int64 {
	committed := map[string]bool{}
	for _, op := range l.Ops {
		if op.Type == OpCommit {
			committed[op.Txn] = true
		}
	}
	st := map[string]int64{}
	for _, op := range l.Ops {
		if op.Type == OpWrite && committed[op.Txn] {
			st[op.Key] = op.Value
		}
	}
	return st
}

// Cut every generated complete log op by op into prefixes. For each prefix
// cross-check reads, edges, serializability, first violations, blocking sets
// and the committed state against the independent reference interpreter;
// verify that established evidence never disappears as the log grows; and
// verify that the fully terminated prefix agrees with the old complete
// report.
func TestPrefixTruncationCrossCheck(t *testing.T) {
	for seed := int64(1); seed <= 1500; seed++ {
		full := genLog(rand.New(rand.NewSource(seed)))
		n := len(full.Ops)
		fullRep := Audit(full)

		var estRec, estCas, estStr *Violation
		cyclic := false

		for k := 1; k <= n; k++ {
			pl := prefixOf(t, full, k)
			rep := Audit(pl)
			ctx := func() {
				data, _ := json.Marshal(Log{Txns: full.Txns, Ops: full.Ops[:k]})
				t.Logf("seed %d prefix %d: %s", seed, k, data)
			}

			if rep.Mode != ModePrefix {
				ctx()
				t.Fatalf("prefix report missing mode %q", ModePrefix)
			}

			// 1. reads, edges, serializability and first violations, as for
			//    complete logs
			if want := refReads(pl); !reflect.DeepEqual(rep.Reads, want) {
				ctx()
				t.Fatalf("reads mismatch:\n got %+v\nwant %+v", rep.Reads, want)
			}
			refE := refEdges(pl)
			if !reflect.DeepEqual(rep.Edges, refE) {
				ctx()
				t.Fatalf("edges mismatch:\n got %+v\nwant %+v", rep.Edges, refE)
			}
			edgeSet := map[[2]string]bool{}
			for _, e := range refE {
				edgeSet[[2]string{e.From, e.To}] = true
			}
			wantOrder, acyclic := refSerialOrder(pl)
			if acyclic {
				if cyclic {
					ctx()
					t.Fatalf("established cycle disappeared at prefix %d", k)
				}
				if !rep.Serializability.Acyclic || rep.Serializability.Status != StatusNotYetViolated {
					ctx()
					t.Fatalf("serializability = %+v, want acyclic not-yet-violated", rep.Serializability)
				}
				if !reflect.DeepEqual(rep.Serializability.Order, wantOrder) {
					ctx()
					t.Fatalf("order mismatch: got %v want %v", rep.Serializability.Order, wantOrder)
				}
			} else {
				if rep.Serializability.Acyclic || rep.Serializability.Status != StatusEstablished {
					ctx()
					t.Fatalf("serializability = %+v, want established cycle", rep.Serializability)
				}
				checkRealCycle(t, rep.Serializability.Cycle, edgeSet)
				cyclic = true
			}

			wantRec, wantCas, wantStr := refFirstViolations(pl)
			checkProp := func(name string, p PropResult, wantSeq int, est **Violation) {
				if got := violSeq(p); got != wantSeq {
					ctx()
					t.Fatalf("%s first violation: got %d want %d", name, got, wantSeq)
				}
				wantStatus := StatusNotYetViolated
				if wantSeq != 0 {
					wantStatus = StatusEstablished
				}
				if p.Status != wantStatus {
					ctx()
					t.Fatalf("%s status = %q, want %q", name, p.Status, wantStatus)
				}
				if *est != nil && !reflect.DeepEqual(*est, p.Violation) {
					ctx()
					t.Fatalf("%s established violation %+v changed to %+v", name, *est, p.Violation)
				}
				if p.Violation != nil {
					*est = p.Violation
				}
			}
			checkProp("recoverable", rep.Recoverable, wantRec, &estRec)
			checkProp("cascadeless", rep.Cascadeless, wantCas, &estCas)
			checkProp("strict", rep.Strict, wantStr, &estStr)

			// 2. commit-now admissions vs the independent interpreter
			wantBlock := refBlocking(pl)
			if len(rep.OpenTransactions) != len(wantBlock) {
				ctx()
				t.Fatalf("openTransactions = %+v, want keys of %+v", rep.OpenTransactions, wantBlock)
			}
			prev := ""
			for i, ot := range rep.OpenTransactions {
				if i > 0 && ot.Txn <= prev {
					ctx()
					t.Fatalf("openTransactions not sorted by txn id: %+v", rep.OpenTransactions)
				}
				prev = ot.Txn
				want, ok := wantBlock[ot.Txn]
				if !ok {
					ctx()
					t.Fatalf("open transaction %q is terminated or undeclared", ot.Txn)
				}
				if !reflect.DeepEqual(ot.CommitNow.BlockedBy, want) {
					ctx()
					t.Fatalf("blockedBy[%s] = %v, want %v", ot.Txn, ot.CommitNow.BlockedBy, want)
				}
				if ot.CommitNow.Admissible != (len(want) == 0) {
					ctx()
					t.Fatalf("admissible[%s] = %v, want %v", ot.Txn, ot.CommitNow.Admissible, len(want) == 0)
				}
			}

			// 3. committed values as of the prefix
			if want := refCommittedState(pl); !reflect.DeepEqual(rep.FinalState, want) {
				ctx()
				t.Fatalf("committedAsOfPrefix = %v, want %v", rep.FinalState, want)
			}

			// 4. fully terminated: the prefix report agrees with the old
			//    complete report
			if k == n {
				if len(rep.OpenTransactions) != 0 {
					ctx()
					t.Fatalf("all terminated but openTransactions = %+v", rep.OpenTransactions)
				}
				if !reflect.DeepEqual(rep.Reads, fullRep.Reads) ||
					!reflect.DeepEqual(rep.Edges, fullRep.Edges) ||
					rep.Serializability.Acyclic != fullRep.Serializability.Acyclic ||
					!reflect.DeepEqual(rep.Serializability.Order, fullRep.Serializability.Order) ||
					!reflect.DeepEqual(rep.Serializability.Cycle, fullRep.Serializability.Cycle) ||
					!reflect.DeepEqual(rep.Recoverable.Violation, fullRep.Recoverable.Violation) ||
					!reflect.DeepEqual(rep.Cascadeless.Violation, fullRep.Cascadeless.Violation) ||
					!reflect.DeepEqual(rep.Strict.Violation, fullRep.Strict.Violation) ||
					!reflect.DeepEqual(rep.FinalState, fullRep.FinalState) {
					ctx()
					t.Fatalf("fully terminated prefix disagrees with the complete report")
				}
			}
		}
	}
}
