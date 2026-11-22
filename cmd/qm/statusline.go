package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/limits"
	"github.com/IZZY-Agency/quartermaster/internal/paths"
)

func init() {
	commands["statusline"] = command{"record Claude limits from the status line: qm statusline [-- <cmd>...]", runStatusline}
}

// statuslineInput is the part of Claude Code's status-line JSON that qm reads.
// Every field decodes leniently: a malformed one is treated as absent and
// never stops the others being recorded.
type statuslineInput struct {
	RateLimits struct {
		FiveHour statuslineLimit `json:"five_hour"`
		SevenDay statuslineLimit `json:"seven_day"`
	} `json:"rate_limits"`
}

// UnmarshalJSON drops a malformed rate_limits value instead of failing.
func (in *statuslineInput) UnmarshalJSON(b []byte) error {
	var raw struct {
		RateLimits json.RawMessage `json:"rate_limits"`
	}
	if json.Unmarshal(b, &raw) != nil {
		return nil
	}
	_ = json.Unmarshal(raw.RateLimits, &in.RateLimits)
	return nil
}

type statuslineLimit struct {
	UsedPercentage *float64
	ResetsAt       *time.Time
}

// UnmarshalJSON reads used_percentage and resets_at (Unix seconds)
// independently; a field that is null or not a number stays nil.
func (l *statuslineLimit) UnmarshalJSON(b []byte) error {
	var raw struct {
		Used   json.RawMessage `json:"used_percentage"`
		Resets json.RawMessage `json:"resets_at"`
	}
	if json.Unmarshal(b, &raw) != nil {
		return nil
	}
	l.UsedPercentage = number(raw.Used)
	if secs := number(raw.Resets); secs != nil {
		t := time.Unix(int64(*secs), 0).UTC()
		l.ResetsAt = &t
	}
	return nil
}

// number decodes a JSON number; null, absent or any other type gives nil.
func number(b json.RawMessage) *float64 {
	var f *float64
	if json.Unmarshal(b, &f) != nil {
		return nil
	}
	return f
}

const statuslineUsage = `usage:
  qm statusline [-- COMMAND [ARGS...]]

Reads Claude Code's status-line JSON on stdin, records the Claude limits, then
runs COMMAND (one argument is a shell command line) with the same stdin and
prints its output.
`

// runStatusline is the qm statusline command, reading the process's stdin.
func runStatusline(args []string, stdout, stderr io.Writer) int {
	if isHelp(args) {
		_, _ = fmt.Fprint(stdout, statuslineUsage)
		return 0
	}
	return statusline(os.Stdin, args, stdout, stderr)
}

// statusline reads the status-line JSON from stdin, records the Claude
// limits, then runs the wrapped command (if any) with the same stdin and
// passes its stdout through. Recording never changes the status line. One
// argument is a shell command line; several are a command and its arguments,
// passed to it unchanged.
func statusline(stdin io.Reader, args []string, stdout, stderr io.Writer) int {
	input, _ := io.ReadAll(stdin)
	recordClaudeLimits(input)

	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		return 0
	}
	var cmd *exec.Cmd
	if len(args) == 1 {
		cmd = exec.Command("/bin/sh", "-c", args[0])
	} else {
		cmd = exec.Command("/bin/sh", append([]string{"-c", `exec "$@"`, "sh"}, args...)...)
	}
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		return 1
	}
	return 0
}

// recordClaudeLimits writes limits/claude.json; every failure is ignored.
// Input with no usage figure (an API-key session, an older client) writes
// nothing, so it does not wipe a reading another session recorded.
func recordClaudeLimits(input []byte) {
	var in statuslineInput
	if json.Unmarshal(input, &in) != nil {
		return
	}
	if in.RateLimits.FiveHour.UsedPercentage == nil && in.RateLimits.SevenDay.UsedPercentage == nil {
		return
	}
	dir, err := paths.StateDir()
	if err != nil {
		return
	}
	_ = limits.WriteClaudeFile(filepath.Join(dir, "limits", "claude.json"), limits.ClaudeFile{
		FiveHourUsed:     in.RateLimits.FiveHour.UsedPercentage,
		SevenDayUsed:     in.RateLimits.SevenDay.UsedPercentage,
		FiveHourResetsAt: in.RateLimits.FiveHour.ResetsAt,
		SevenDayResetsAt: in.RateLimits.SevenDay.ResetsAt,
		UpdatedAt:        time.Now().UTC(),
	})
}
