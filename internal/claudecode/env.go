package claudecode

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// TermEnv is the terminal a process runs in, as its environment names it:
// the agterm session id and the tmux pane id. Either is "" when absent.
type TermEnv struct {
	AgtermSession, TmuxPane string
}

const (
	agtermKey = "AGTERM_SESSION_ID="
	tmuxKey   = "TMUX_PANE="
)

// goos is the operating system TermEnvOf reads the environment for; tests
// replace it.
var goos = runtime.GOOS

// readEnviron reads /proc/PID/environ; tests replace it.
var readEnviron = func(pid int) ([]byte, error) {
	return os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
}

// psEnv returns ps's command column with the environment appended (macOS's
// ps eww); tests replace it.
var psEnv = func(pid int) (string, error) {
	out, err := exec.Command("ps", "eww", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// TermEnvOf reads the terminal session of the process pid from its
// environment: on Linux the NUL-separated /proc/PID/environ, elsewhere
// ps eww -o command= -p PID.
func TermEnvOf(pid int) (TermEnv, error) {
	if goos == "linux" {
		b, err := readEnviron(pid)
		if err != nil {
			return TermEnv{}, fmt.Errorf("environment of %d: %w", pid, err)
		}
		return ParseEnviron(b), nil
	}
	line, err := psEnv(pid)
	if err != nil {
		return TermEnv{}, fmt.Errorf("environment of %d: %w", pid, err)
	}
	return ParseEnvLine(line), nil
}

// ParseEnvLine reads ps eww's line (the command line, then the
// environment, all separated by spaces): the last token starting
// AGTERM_SESSION_ID= and the last starting TMUX_PANE=. The last wins since
// the environment follows the arguments, which may quote either text.
func ParseEnvLine(line string) TermEnv {
	var e TermEnv
	for _, tok := range strings.Fields(line) {
		switch {
		case strings.HasPrefix(tok, agtermKey):
			e.AgtermSession = strings.TrimPrefix(tok, agtermKey)
		case strings.HasPrefix(tok, tmuxKey):
			e.TmuxPane = strings.TrimPrefix(tok, tmuxKey)
		}
	}
	return e
}

// ParseEnviron reads the NUL-separated entries of /proc/PID/environ.
func ParseEnviron(b []byte) TermEnv {
	var e TermEnv
	for _, kv := range strings.Split(string(b), "\x00") {
		switch {
		case strings.HasPrefix(kv, agtermKey):
			e.AgtermSession = strings.TrimPrefix(kv, agtermKey)
		case strings.HasPrefix(kv, tmuxKey):
			e.TmuxPane = strings.TrimPrefix(kv, tmuxKey)
		}
	}
	return e
}
