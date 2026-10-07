// Command txcheck audits a database operation log (JSON) for reads-from
// relationships, conflict-graph serializability, and the recoverable /
// cascadeless / strict execution properties.
//
// Usage:
//
//	txcheck [--prefix] [log.json]     (reads stdin when no file is given)
//
// The audit report is written to stdout as JSON. Input/validation errors
// are reported as a JSON object on stderr with exit code 1.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("txcheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	prefix := fs.Bool("prefix", false, "audit an incomplete log prefix")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: txcheck [--prefix] [log.json]   (reads stdin when no file is given)")
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}

	var (
		data []byte
		err  error
	)
	if fs.NArg() == 1 {
		data, err = os.ReadFile(fs.Arg(0))
	} else {
		data, err = io.ReadAll(stdin)
	}
	if err != nil {
		return fail(stderr, err)
	}

	var l *Log
	if *prefix {
		l, err = ParseLogPrefix(data)
	} else {
		l, err = ParseLog(data)
	}
	if err != nil {
		return fail(stderr, err)
	}

	var rep *Report
	if *prefix {
		rep = AuditPrefix(l)
	} else {
		rep = Audit(l)
	}
	out, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return fail(stderr, err)
	}
	if _, err := fmt.Fprintln(stdout, string(out)); err != nil {
		return fail(stderr, err)
	}
	return 0
}

// fail reports err as a JSON object on stderr and returns the conventional
// input/runtime failure exit code.
func fail(stderr io.Writer, err error) int {
	msg, _ := json.Marshal(map[string]any{"ok": false, "error": err.Error()})
	fmt.Fprintln(stderr, string(msg))
	return 1
}
