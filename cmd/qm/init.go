package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/IZZY-Agency/quartermaster/internal/setup"
)

func init() {
	commands["init"] = command{"set up qm: config, status line, plugin commands: qm init [--yes] [--undo] [--marketplace PATH|OWNER/REPO]", runInit}
}

func runInit(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("init", flag.ContinueOnError)
	fl.SetOutput(stderr)
	yes := fl.Bool("yes", false, "answer yes to every question (the TypeSafe key prompt is skipped)")
	undo := fl.Bool("undo", false, "put back what qm init changed; --yes keeps edits made since init")
	market := fl.String("marketplace", "", "the marketplace the printed /plugin command adds: a local checkout's folder or owner/repo")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	s := setup.Streams{In: os.Stdin, Out: stdout}
	o := setup.Options{Yes: *yes, Marketplace: *market}
	run := setup.Init
	if *undo {
		run = setup.Undo
	}
	if err := run(context.Background(), s, o); err != nil {
		_, _ = fmt.Fprintf(stderr, "qm init: %v\n", err)
		return 1
	}
	return 0
}
