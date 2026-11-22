package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/config"
	"github.com/IZZY-Agency/quartermaster/internal/declog"
	"github.com/IZZY-Agency/quartermaster/internal/hooks"
	"github.com/IZZY-Agency/quartermaster/internal/route"
)

// runFeedbackAnalyze is qm feedback-analyze [--memory FILE] PROMPT_FILE,
// started in the background by the feedback hook. Errors go to the decision
// log as kind "error" Records (nobody reads its output); exit 2 on bad
// arguments, 1 when the analysis fails.
func runFeedbackAnalyze(args []string, _, stderr io.Writer) int {
	fs := flag.NewFlagSet("qm feedback-analyze", flag.ContinueOnError)
	fs.SetOutput(stderr)
	memory := fs.String("memory", "", "the project's memory index (MEMORY.md)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 {
		_, _ = fmt.Fprintln(stderr, "usage: qm feedback-analyze [--memory FILE] PROMPT_FILE")
		return 2
	}
	cfg, err := config.Load()
	if err != nil {
		// Load returns the defaults with its error; the analysis runs on them.
		logFeedbackError(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), hooks.FeedbackTimeout)
	defer cancel()
	if err := hooks.AnalyzeFeedback(ctx, fs.Arg(0), *memory, cfg); err != nil {
		logFeedbackError(err)
		_, _ = fmt.Fprintf(stderr, "qm feedback-analyze: %v\n", err)
		return 1
	}
	return 0
}

func logFeedbackError(err error) {
	_ = declog.Append(route.Record{Time: time.Now().UTC(), Kind: "error", Rule: "feedback", Detail: err.Error()})
}
