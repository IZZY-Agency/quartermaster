// Command qm picks the model and effort for each Claude Code worker and
// reviewer before it is spawned.
package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/IZZY-Agency/quartermaster/internal/buildinfo"
)

// command is one qm subcommand.
type command struct {
	summary string
	run     func(args []string, stdout, stderr io.Writer) int
}

// commands holds every subcommand by name; tests enumerate it. Filled in
// init functions because help reads it; a file's init may run before this
// one, so the map exists from the start and every init only adds to it.
var commands = map[string]command{}

// aliases maps flag spellings to subcommands.
var aliases = map[string]string{"--version": "version", "-h": "help", "--help": "help"}

func init() {
	for name, c := range map[string]command{
		"version": {"print the version and commit", runVersion},
		"help":    {"print this text", runHelp},
		"hook":    {"run a plugin hook: qm hook <name>, input on stdin", runHook},
		"route":   {"pick the tier for a worker or a review: qm route worker|review", runRoute},
		"spawn":   {"route and launch a worker or a review: qm spawn worker|review", runSpawn},
	} {
		commands[name] = c
	}
	commands["feedback-analyze"] = command{"analyze a user message for the feedback hook (started by it)", runFeedbackAnalyze}
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches a subcommand and returns the exit code.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage())
		return 2
	}
	name := args[0]
	if a, ok := aliases[name]; ok {
		name = a
	}
	c, ok := commands[name]
	if !ok {
		_, _ = fmt.Fprintf(stderr, "qm: unknown command %q\n%s", args[0], usage())
		return 2
	}
	return c.run(args[1:], stdout, stderr)
}

func usage() string {
	names := make([]string, 0, len(commands))
	for n := range commands {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("usage: qm <command> [arguments]\n\ncommands:\n")
	for _, n := range names {
		fmt.Fprintf(&b, "  %-12s %s\n", n, commands[n].summary)
	}
	return b.String()
}

// isHelp reports whether args start with -h or --help.
func isHelp(args []string) bool {
	return len(args) > 0 && (args[0] == "-h" || args[0] == "--help")
}

const versionUsage = `usage:
  qm version
  qm --version
`

func runVersion(args []string, stdout, _ io.Writer) int {
	if isHelp(args) {
		_, _ = fmt.Fprint(stdout, versionUsage)
		return 0
	}
	_, _ = fmt.Fprintf(stdout, "qm %s (%s)\n", buildinfo.Version, buildinfo.Commit)
	return 0
}

func runHelp(_ []string, stdout, _ io.Writer) int {
	_, _ = fmt.Fprint(stdout, usage())
	return 0
}
