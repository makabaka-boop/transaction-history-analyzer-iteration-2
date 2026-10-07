// Command txcheck audits a database operation log (JSON) for reads-from
// relationships, conflict-graph serializability, and the recoverable /
// cascadeless / strict execution properties.
//
// Usage:
//
//	txcheck [--prefix] [log.json]     (reads stdin when no file is given)
//
// With --prefix the input is audited as a live prefix of a log still being
// appended: declared transactions may be unterminated, violations already
// observed are reported as established (properties without evidence only as
// not yet violated), and each open transaction gets a commit-now
// recoverability admission.
//
// The audit report is written to stdout as JSON. Input/validation errors
// are reported as a JSON object on stderr with exit code 1.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

func main() {
	prefix, file, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "usage: txcheck [--prefix] [log.json]   (reads stdin when no file is given)")
		os.Exit(2)
	}
	var data []byte
	if file != "" {
		data, err = os.ReadFile(file)
	} else {
		data, err = io.ReadAll(os.Stdin)
	}
	if err != nil {
		fail(err)
	}
	parse := ParseLog
	if prefix {
		parse = ParseLogPrefix
	}
	log, err := parse(data)
	if err != nil {
		fail(err)
	}
	rep := Audit(log)
	out, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		fail(err)
	}
	fmt.Println(string(out))
}

// parseArgs separates the optional --prefix flag from the optional log file
// argument; more than one file is a usage error.
func parseArgs(args []string) (prefix bool, file string, err error) {
	for _, a := range args {
		if a == "--prefix" || a == "-prefix" {
			prefix = true
			continue
		}
		if file != "" {
			return false, "", fmt.Errorf("at most one log file may be given, got %q and %q", file, a)
		}
		file = a
	}
	return prefix, file, nil
}

// fail reports err as a JSON object on stderr and exits non-zero.
func fail(err error) {
	msg, _ := json.Marshal(map[string]any{"ok": false, "error": err.Error()})
	fmt.Fprintln(os.Stderr, string(msg))
	os.Exit(1)
}
