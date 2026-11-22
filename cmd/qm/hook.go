package main

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/config"
	"github.com/IZZY-Agency/quartermaster/internal/declog"
	"github.com/IZZY-Agency/quartermaster/internal/hooks"
	"github.com/IZZY-Agency/quartermaster/internal/route"
)

// hookStdin is where qm hook reads the hook input; tests replace it.
var hookStdin io.Reader = os.Stdin

const hookUsage = `usage:
  qm hook NAME

Runs the plugin hook NAME with the hook input on stdin. Started by the plugins'
hooks; it always exits 0.
`

// runHook runs one plugin hook: qm hook <name>, the hook input on stdin. It
// prints the hook output, if any, and always exits 0: a hook never blocks
// work. Errors go to the decision log as kind "error" Records.
func runHook(args []string, stdout, _ io.Writer) int {
	if isHelp(args) {
		_, _ = fmt.Fprint(stdout, hookUsage)
		return 0
	}
	name := ""
	if len(args) > 0 {
		name = args[0]
	}
	cfg, err := config.Load()
	if err != nil {
		// Load returns the defaults with its error; the hook runs on them.
		_ = declog.Append(route.Record{Time: time.Now().UTC(), Kind: "error", Rule: name, Detail: err.Error()})
	}
	if out := hooks.Handle(name, hookStdin, cfg); len(out) > 0 {
		_, _ = fmt.Fprintln(stdout, string(out))
	}
	return 0
}
