package claudecode

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
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

// The shapes of the two ids. A value of another shape is not read: qm
// compares them with the terminal's own ids, and a pane id goes to
// tmux -t, where any other text is a target of its own. agterm's ids are
// UUIDs; any short word of letters, digits and "-" that does not start
// with "-" is accepted. A tmux pane id is "%" and a number.
var (
	agtermShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)
	paneShape   = regexp.MustCompile(`^%[0-9]{1,19}$`)
)

// shaped is v when it has shape, else "".
func shaped(v string, shape *regexp.Regexp) string {
	if !shape.MatchString(v) {
		return ""
	}
	return v
}

// goos is the operating system TermEnvOf reads the environment for; tests
// replace it.
var goos = runtime.GOOS

// readEnviron reads /proc/PID/environ; tests replace it.
var readEnviron = func(pid int) ([]byte, error) {
	return os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
}

// psArgs and psEnv return ps's command column, without and with the
// environment appended (macOS's ps ww and ps eww); tests replace them.
var (
	psArgs = func(ctx context.Context, pid int) (string, error) { return psCommand(ctx, "ww", pid) }
	psEnv  = func(ctx context.Context, pid int) (string, error) { return psCommand(ctx, "eww", pid) }
)

func psCommand(ctx context.Context, flags string, pid int) (string, error) {
	out, err := exec.CommandContext(ctx, "ps", flags, "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// TermEnvOf reads the terminal session of the process pid from its
// environment: on Linux the NUL-separated /proc/PID/environ, elsewhere what
// ps eww -o command= -p PID adds to the line of ps ww.
func TermEnvOf(pid int) (TermEnv, error) {
	return TermEnvOfCtx(context.Background(), pid)
}

// TermEnvOfCtx is TermEnvOf with ps's run cancelled when ctx ends (the
// Linux read is a single /proc file).
func TermEnvOfCtx(ctx context.Context, pid int) (TermEnv, error) {
	if err := ctx.Err(); err != nil {
		return TermEnv{}, err
	}
	if goos == "linux" {
		b, err := readEnviron(pid)
		if err != nil {
			return TermEnv{}, fmt.Errorf("environment of %d: %w", pid, err)
		}
		return ParseEnviron(b), nil
	}
	args, err := psArgs(ctx, pid)
	if err != nil {
		return TermEnv{}, fmt.Errorf("environment of %d: %w", pid, err)
	}
	line, err := psEnv(ctx, pid)
	if err != nil {
		return TermEnv{}, fmt.Errorf("environment of %d: %w", pid, err)
	}
	env, ok := afterCommand(line, args)
	if !ok {
		return TermEnv{}, fmt.Errorf("environment of %d: its command line changed while it was read", pid)
	}
	return ParseEnvLine(env), nil
}

// afterCommand is what follows the command line args in line, ps's line with
// the environment: "" when ps showed no environment. false when line does not
// start with args, so that nothing tells the two apart.
func afterCommand(line, args string) (string, bool) {
	line, args = strings.TrimRight(line, " \n"), strings.TrimRight(args, " \n")
	rest, ok := strings.CutPrefix(line, args)
	if args == "" || !ok || rest != "" && rest[0] != ' ' {
		return "", false
	}
	return rest, true
}

// ParseEnvLine reads the environment part of ps eww's line, entries
// separated by spaces: the tokens starting AGTERM_SESSION_ID= and
// TMUX_PANE=. The caller cuts the command line off first, whose words are
// not environment. A value with a space can hold such a token too, and the
// line cannot say which one is the variable, so a key whose tokens differ
// is not read.
func ParseEnvLine(env string) TermEnv {
	var agterm, pane lineValue
	for _, tok := range strings.Fields(env) {
		if v, ok := strings.CutPrefix(tok, agtermKey); ok {
			agterm.add(v)
		} else if v, ok := strings.CutPrefix(tok, tmuxKey); ok {
			pane.add(v)
		}
	}
	return TermEnv{AgtermSession: shaped(agterm.v, agtermShape), TmuxPane: shaped(pane.v, paneShape)}
}

// lineValue is the value of one key in ps's line: "" when none was seen, or
// two that differ.
type lineValue struct {
	v            string
	seen, differ bool
}

func (l *lineValue) add(v string) {
	l.differ = l.differ || l.seen && v != l.v
	l.seen = true
	if l.v = v; l.differ {
		l.v = ""
	}
}

// ParseEnviron reads the NUL-separated entries of /proc/PID/environ.
func ParseEnviron(b []byte) TermEnv {
	var e TermEnv
	for _, kv := range strings.Split(string(b), "\x00") {
		switch {
		case strings.HasPrefix(kv, agtermKey):
			e.AgtermSession = shaped(strings.TrimPrefix(kv, agtermKey), agtermShape)
		case strings.HasPrefix(kv, tmuxKey):
			e.TmuxPane = shaped(strings.TrimPrefix(kv, tmuxKey), paneShape)
		}
	}
	return e
}
