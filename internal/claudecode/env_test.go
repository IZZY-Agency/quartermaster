package claudecode

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func TestParseEnvLine(t *testing.T) {
	line := " PATH=/bin AGTERM_SESSION_ID=a-2 TMUX_PANE=%3"
	if got, want := ParseEnvLine(line), (TermEnv{AgtermSession: "a-2", TmuxPane: "%3"}); got != want {
		t.Errorf("ParseEnvLine() = %+v, want %+v", got, want)
	}
}

func TestParseEnvLineNeedsTokenStart(t *testing.T) {
	for _, line := range []string{"XAGTERM_SESSION_ID=no", "claude XTMUX_PANE=%1", ""} {
		if got := ParseEnvLine(line); got != (TermEnv{}) {
			t.Errorf("ParseEnvLine(%q) = %+v, want empty", line, got)
		}
	}
}

func TestParseEnviron(t *testing.T) {
	b := []byte("PATH=/bin\x00AGTERM_SESSION_ID=s-9\x00NOTAGTERM_SESSION_ID=bad\x00TMUX_PANE=%7\x00")
	if got, want := ParseEnviron(b), (TermEnv{AgtermSession: "s-9", TmuxPane: "%7"}); got != want {
		t.Errorf("ParseEnviron() = %+v, want %+v", got, want)
	}
}

func TestTermEnvOfBySystem(t *testing.T) {
	oldOS, oldEnv, oldPS, oldArgs := goos, readEnviron, psEnv, psArgs
	t.Cleanup(func() { goos, readEnviron, psEnv, psArgs = oldOS, oldEnv, oldPS, oldArgs })
	psArgs = func(context.Context, int) (string, error) { return "claude -n w\n", nil }
	readEnviron = func(pid int) ([]byte, error) {
		if pid != 42 {
			return nil, errors.New("wrong pid")
		}
		return []byte("AGTERM_SESSION_ID=lin\x00"), nil
	}
	psEnv = func(_ context.Context, pid int) (string, error) {
		if pid != 42 {
			return "", errors.New("wrong pid")
		}
		return "claude -n w TMUX_PANE=%2 AGTERM_SESSION_ID=mac", nil
	}
	goos = "linux"
	if got, err := TermEnvOf(42); err != nil || got.AgtermSession != "lin" {
		t.Errorf("linux: %+v, %v, want lin", got, err)
	}
	goos = "darwin"
	if got, err := TermEnvOf(42); err != nil || got != (TermEnv{AgtermSession: "mac", TmuxPane: "%2"}) {
		t.Errorf("darwin: %+v, %v", got, err)
	}
	if _, err := TermEnvOf(7); err == nil {
		t.Error("an unreadable environment: want an error")
	}
}

// A value that is not shaped like an id is not a terminal id: TMUX_PANE
// goes to tmux -t, where any other text is a target of its own.
func TestParseEnvShape(t *testing.T) {
	long := strings.Repeat("a", 65)
	for _, v := range []string{"-x", "a;b", "a b", "../x", "a/b", "a*", long, "é"} {
		if got := ParseEnviron([]byte("AGTERM_SESSION_ID=" + v + "\x00")); got.AgtermSession != "" {
			t.Errorf("AGTERM_SESSION_ID=%q read as %q, want none", v, got.AgtermSession)
		}
	}
	for _, v := range []string{"", "%", "3", "%3x", "main:0.1", "%-1", "%3;kill-server", "=work"} {
		if got := ParseEnviron([]byte("TMUX_PANE=" + v + "\x00")); got.TmuxPane != "" {
			t.Errorf("TMUX_PANE=%q read as %q, want none", v, got.TmuxPane)
		}
		if got := ParseEnvLine("TMUX_PANE=" + v); got.TmuxPane != "" {
			t.Errorf("line TMUX_PANE=%q read as %q, want none", v, got.TmuxPane)
		}
	}
	uuid := "0A1B2C3D-4E5F-6A7B-8C9D-0E1F2A3B4C5D"
	if got, want := ParseEnvLine("AGTERM_SESSION_ID="+uuid+" TMUX_PANE=%12"), (TermEnv{AgtermSession: uuid, TmuxPane: "%12"}); got != want {
		t.Errorf("ParseEnvLine() = %+v, want %+v", got, want)
	}
}

// Two different ids in one environment line: one of them is text inside
// another variable's value, and the line cannot say which, so none is read.
func TestParseEnvLineAmbiguous(t *testing.T) {
	got := ParseEnvLine("AGTERM_SESSION_ID=real NOTE=see AGTERM_SESSION_ID=other TMUX_PANE=%1 TMUX_PANE=%1")
	if want := (TermEnv{TmuxPane: "%1"}); got != want {
		t.Errorf("ParseEnvLine() = %+v, want %+v", got, want)
	}
}

// On macOS the command line comes first in ps eww's line: its words are not
// environment, also when ps shows no environment at all.
func TestTermEnvOfSkipsCommandLine(t *testing.T) {
	oldOS, oldEnv, oldArgs := goos, psEnv, psArgs
	t.Cleanup(func() { goos, psEnv, psArgs = oldOS, oldEnv, oldArgs })
	goos = "darwin"
	cmd := "claude explain AGTERM_SESSION_ID=arg TMUX_PANE=%9"
	psArgs = func(context.Context, int) (string, error) { return cmd + "\n", nil }
	for _, tc := range []struct {
		line string
		want TermEnv
	}{
		{cmd + "\n", TermEnv{}},
		{cmd + " PATH=/bin\n", TermEnv{}},
		{cmd + " AGTERM_SESSION_ID=own PATH=/bin\n", TermEnv{AgtermSession: "own"}},
	} {
		psEnv = func(context.Context, int) (string, error) { return tc.line, nil }
		if got, err := TermEnvOf(1); err != nil || got != tc.want {
			t.Errorf("TermEnvOf with %q = %+v, %v, want %+v", tc.line, got, err, tc.want)
		}
	}
	// The two ps runs do not agree: nothing tells the environment apart.
	for _, line := range []string{"other AGTERM_SESSION_ID=x\n", cmd + "extra AGTERM_SESSION_ID=x\n"} {
		psEnv = func(context.Context, int) (string, error) { return line, nil }
		if got, err := TermEnvOf(1); err == nil {
			t.Errorf("TermEnvOf with %q = %+v, want an error", line, got)
		}
	}
	psArgs = func(context.Context, int) (string, error) { return "\n", nil }
	if got, err := TermEnvOf(1); err == nil {
		t.Errorf("TermEnvOf with no command line = %+v, want an error", got)
	}
}

// The real ps: a process whose arguments name a terminal id, and whose
// environment has none, is read as having none.
func TestTermEnvOfRealProcessArguments(t *testing.T) {
	cmd := exec.Command("sh", "-c", "read x", "AGTERM_SESSION_ID=from-argv", "TMUX_PANE=%99")
	cmd.Env = []string{"PATH=/bin:/usr/bin", "TMUX_PANE=%7"}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Skip("sh not started:", err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Wait() })
	got, err := TermEnvOf(cmd.Process.Pid)
	if err != nil {
		t.Skip("environment not readable here:", err)
	}
	if got.AgtermSession != "" || got.TmuxPane != "" && got.TmuxPane != "%7" {
		t.Errorf("TermEnvOf = %+v, want no id from the arguments", got)
	}
}
