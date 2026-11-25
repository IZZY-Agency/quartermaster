package main

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/config"
	"github.com/IZZY-Agency/quartermaster/internal/up"
)

// fallbackSince is the first Claude Code that can be given qm's default model
// and fallback: the "fable" name came with 2.1.170, and --fallback-model is
// used in interactive sessions since 2.1.166. An earlier one ignores the
// fallback there, or does not know the flag and does not start.
var fallbackSince = [3]int{2, 1, 170}

// claudeVersionWait bounds the reading of claude's version.
const claudeVersionWait = 2 * time.Second

// upClaudeVersion is what claude --version prints.
var upClaudeVersion = func() (string, error) {
	return readClaudeVersion(context.Background(), claudeVersionWait)
}

func readClaudeVersion(ctx context.Context, wait time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	cmd := exec.CommandContext(ctx, "claude", "--version")
	// A process it left behind may hold its output open.
	cmd.WaitDelay = 100 * time.Millisecond
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return "", fmt.Errorf("claude --version did not answer in %s", wait)
	}
	return string(out), err
}

var claudeVersionRE = regexp.MustCompile(`^\s*(\d+)\.(\d+)\.(\d+)\b`)

// upModelCheck says why claude cannot be given the config's default model and
// fallback ("" when it can).
func upModelCheck() string {
	const without = "the coordinator starts without the model and the fallback of the config"
	out, err := upClaudeVersion()
	if err != nil {
		return fmt.Sprintf("Claude Code's version was not read (%v): %s", err, without)
	}
	m := claudeVersionRE.FindStringSubmatch(out)
	if m == nil {
		return fmt.Sprintf("Claude Code's version was not read (claude --version printed %.80q): %s", strings.TrimSpace(out), without)
	}
	for i, least := range fallbackSince {
		n, err := strconv.Atoi(m[i+1])
		if err != nil || n > least {
			break // a number too large to read is newer
		}
		if n < least {
			return fmt.Sprintf("Claude Code %s is older than %d.%d.%d: %s", strings.TrimSpace(m[0]), fallbackSince[0], fallbackSince[1], fallbackSince[2], without)
		}
	}
	return ""
}

// withModel gives d the config's model and fallback for a coordinator that
// starts from scratch. Claude's version is read once, and only when one is to
// be used.
func withModel(d up.Deps, cfg config.Config) up.Deps {
	var err error
	if d.Model.Model, err = cfg.CoordinatorModel(); err != nil {
		d.Model.ModelNote = err.Error()
	}
	if d.Model.Fallback, err = cfg.CoordinatorFallback(); err != nil {
		d.Model.FallbackNote = err.Error()
	}
	d.ModelCheck = sync.OnceValue(upModelCheck)
	return d
}

// printModel prints what coordinator the choice c starts with (would: in a
// dry run), and its note.
func printModel(w io.Writer, c up.ModelChoice, would bool) {
	printModelNote(w, c)
	verb := "starts"
	if would {
		verb = "would start"
	}
	if c.Text != "" {
		printLine(w, "the coordinator %s with %s", verb, c.Text)
	}
}

func printModelNote(w io.Writer, c up.ModelChoice) {
	if c.Note != "" {
		printLine(w, "note: %s", c.Note)
	}
}

// modelSuffix ends a start line with the model and fallback started with.
func modelSuffix(c up.ModelChoice) string {
	if c.Text == "" {
		return ""
	}
	return ", " + c.Text
}
