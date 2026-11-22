package claudecode

import (
	"context"
	"errors"
	"testing"
)

func TestParseEnvLineLastWins(t *testing.T) {
	line := "claude -n w AGTERM_SESSION_ID=x1 PATH=/bin AGTERM_SESSION_ID=a-2 TMUX_PANE=%3"
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
	oldOS, oldEnv, oldPS := goos, readEnviron, psEnv
	t.Cleanup(func() { goos, readEnviron, psEnv = oldOS, oldEnv, oldPS })
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
