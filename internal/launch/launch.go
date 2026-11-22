// Package launch starts a worker's launch script in a new terminal session:
// an agterm session or a tmux window (design 5.12).
package launch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// LaunchSpec is one session to start: its name, working folder, the launch
// script it runs with /bin/sh, and where it goes. Workspace is an agterm
// workspace id ("" is the current one); WorkspaceName names the workspace
// instead: an agterm workspace created when missing, or the tmux session the
// window joins. Setting both is an error.
type LaunchSpec struct {
	Name, Cwd, Script, Workspace, WorkspaceName string
}

// errBothWorkspaces: a spec names its workspace twice.
var errBothWorkspaces = errors.New("launch: a spec sets both Workspace and WorkspaceName")

func (s LaunchSpec) check() error {
	if s.Workspace != "" && s.WorkspaceName != "" {
		return errBothWorkspaces
	}
	return nil
}

// SessionRef names a started session: the launcher, the launcher's id for it
// (agterm session id, tmux window or session id) and its name. It is stored
// in the project state. Warning is set when the session started but a
// follow-up step failed (the agterm restore pin). Workspace is the agterm
// workspace or tmux session a listing found the session in; it is not
// stored, and a tmux window is closed only within it. Idle is set by a
// listing when nothing but a shell runs in the session, so qm may close or
// reuse it: agterm, when its foreground is an idle shell (agterm's
// foregroundShell key); tmux, when the window has one pane and that pane is
// dead or runs a shell. It is not stored. Foreground (the argv of the pane's
// foreground process, joined by spaces as ps prints a command line) and Cwd
// (the session's folder) are what an agterm listing reports; they are not
// stored; Panes, a tmux listing's pane count, is not stored either. Adopted is stored: the session is the user's own tab or window,
// recorded when qm up adopted the Claude session running in it, not one qm
// opened.
type SessionRef struct {
	Launcher   string `json:"launcher"`
	ID         string `json:"id"`
	Name       string `json:"name"`
	Warning    string `json:"warning,omitempty"`
	Adopted    bool   `json:"adopted,omitempty"`
	Workspace  string `json:"-"`
	Idle       bool   `json:"-"`
	Foreground string `json:"-"`
	Cwd        string `json:"-"`
	// Panes is the number of panes of a listed tmux window (0 otherwise).
	Panes int `json:"-"`
}

// shells are the command names of the shells a terminal session falls back
// to; a login shell's name starts with "-".
var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "fish": true, "dash": true, "ksh": true, "mksh": true, "tcsh": true, "csh": true, "nu": true, "elvish": true, "xonsh": true}

// IsShell says whether command (a process name, or a path to one) is a shell.
func IsShell(command string) bool {
	c := strings.TrimPrefix(strings.TrimSpace(command), "-")
	if i := strings.LastIndexByte(c, '/'); i >= 0 {
		c = c[i+1:]
	}
	return shells[c]
}

// Launcher starts a session.
type Launcher interface {
	Spawn(ctx context.Context, s LaunchSpec) (SessionRef, error)
}

// Lister lists the sessions of a workspace (agterm) or tmux session by name;
// a workspace that does not exist has none.
type Lister interface {
	Sessions(ctx context.Context, workspaceName string) ([]SessionRef, error)
}

// Workspace is an agterm workspace as one tree read lists it.
type Workspace struct {
	ID, Name string
	Sessions []SessionRef
}

// FindSession returns the workspace whose listing holds the session id.
func FindSession(ws []Workspace, sessionID string) (Workspace, bool) {
	for _, w := range ws {
		for _, s := range w.Sessions {
			if s.ID == sessionID {
				return w, true
			}
		}
	}
	return Workspace{}, false
}

// ErrRefused: the terminal refused to type into a session (it is not ready,
// or it does not exist): nothing was typed, so starting the session
// elsewhere is safe.
var ErrRefused = errors.New("terminal refused the session")

// Reuser runs a launch script inside an existing idle session instead of
// starting a new one. It is an optional interface beside Lister and Closer:
// agterm implements it, tmux does not. The returned ref keeps ref's ID and
// Name.
type Reuser interface {
	Reuse(ctx context.Context, ref SessionRef, script string) (SessionRef, error)
}

// ErrGone: Close found the session already gone (closed by hand, or its
// terminal not running any more). Callers treat it as closed.
var ErrGone = errors.New("session already gone")

// Closer closes a session started earlier. A session that does not exist
// any more is an error that matches ErrGone.
type Closer interface {
	Close(ctx context.Context, ref SessionRef) error
}

// ErrNoLauncher: launcher is auto and qm runs in neither agterm nor tmux.
var ErrNoLauncher = errors.New(`no launcher: run qm inside agterm or tmux, or set "launcher" in the quartermaster config`)

// Runner runs bin with args and returns its standard output; tests replace it.
type Runner func(ctx context.Context, bin string, args ...string) ([]byte, error)

// Exec is the Runner that runs the real command. A failure carries the
// command's standard error.
func Exec(ctx context.Context, bin string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			// agtermctl reports ok:false on standard output.
			msg = strings.TrimSpace(stdout.String())
		}
		if msg != "" {
			return nil, fmt.Errorf("%s: %s: %w", bin, msg, err)
		}
		return nil, fmt.Errorf("%s: %w", bin, err)
	}
	return stdout.Bytes(), nil
}

// Pick returns the launcher for choice (agterm, tmux, or auto; "" is auto).
// Auto picks agterm when AGTERM_SESSION_ID is set, else tmux when TMUX is
// set, else fails with ErrNoLauncher. env reads the environment.
func Pick(choice string, env func(string) string) (Launcher, error) {
	inTmux := env("TMUX") != ""
	switch choice {
	case "agterm":
		return Agterm{Bin: "agtermctl", Run: Exec, Anchor: env("AGTERM_SESSION_ID")}, nil
	case "tmux":
		return Tmux{Bin: "tmux", Run: Exec, Inside: inTmux}, nil
	case "", "auto":
		switch {
		case env("AGTERM_SESSION_ID") != "":
			return Agterm{Bin: "agtermctl", Run: Exec, Anchor: env("AGTERM_SESSION_ID")}, nil
		case inTmux:
			return Tmux{Bin: "tmux", Run: Exec, Inside: true}, nil
		}
		return nil, ErrNoLauncher
	}
	return nil, fmt.Errorf("launcher %q is not agterm, tmux or auto", choice)
}

// PickUp returns the launcher for qm up (spec 5.6), by choice (agterm, tmux or
// auto; "" is auto). Auto order: AGTERM_SESSION_ID set, agterm; TMUX set, tmux
// inside it; agtermUp() true (agterm is running), agterm; have("tmux"), tmux
// outside; else ErrNoLauncher.
func PickUp(choice string, env func(string) string, agtermUp func() bool, have func(bin string) bool) (Launcher, error) {
	switch choice {
	case "", "auto":
		switch {
		case env("AGTERM_SESSION_ID") != "":
			return Agterm{Bin: "agtermctl", Run: Exec, Anchor: env("AGTERM_SESSION_ID")}, nil
		case env("TMUX") != "":
			return Tmux{Bin: "tmux", Run: Exec, Inside: true}, nil
		case agtermUp():
			return Agterm{Bin: "agtermctl", Run: Exec}, nil
		case have("tmux"):
			return Tmux{Bin: "tmux", Run: Exec}, nil
		}
		return nil, ErrNoLauncher
	}
	return Pick(choice, env)
}

// Command is the shell command a session runs: /bin/sh on the script.
func Command(script string) string {
	return "/bin/sh " + Quote(script)
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// Quote quotes s as one POSIX shell word; a word that needs no quoting is
// returned as it is.
func Quote(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// firstWordOfLastLine is the id a create command printed: the first word of
// its last non-empty line.
func firstWordOfLastLine(out []byte) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	f := strings.Fields(lines[len(lines)-1])
	if len(f) == 0 {
		return ""
	}
	return f[0]
}
