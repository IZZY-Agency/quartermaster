package launch

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Tmux starts a window in the current tmux session when Inside, else a new
// detached tmux session. With LaunchSpec.WorkspaceName it starts a window in
// the tmux session of that name, creating the session when missing; it
// ignores LaunchSpec.Workspace.
type Tmux struct {
	Bin    string // tmux
	Run    Runner
	Inside bool // qm runs inside tmux (TMUX is set)
}

// Spawn runs, with a WorkspaceName WS: tmux has-session -t =WS, then, if it
// exists, tmux new-window -d -P -F '#{window_id}' -t =WS: -n NAME -c DIR
// "/bin/sh SCRIPT", else tmux new-session -d -P -F '#{window_id}' -s WS -n
// NAME -c DIR "/bin/sh SCRIPT". Without one, inside tmux: tmux new-window -d
// -P -F '#{window_id}' -n NAME -c DIR "/bin/sh SCRIPT"; outside: tmux
// new-session -d -P -F '#{session_id}' -s NAME -c DIR "/bin/sh SCRIPT". The
// id is what -P prints.
func (t Tmux) Spawn(ctx context.Context, s LaunchSpec) (SessionRef, error) {
	if err := s.check(); err != nil {
		return SessionRef{}, err
	}
	var args []string
	switch {
	case s.WorkspaceName != "":
		if _, err := t.Run(ctx, t.Bin, "has-session", "-t", "="+s.WorkspaceName); err == nil {
			args = []string{"new-window", "-d", "-P", "-F", "#{window_id}", "-t", "=" + s.WorkspaceName + ":", "-n", s.Name}
		} else {
			args = []string{"new-session", "-d", "-P", "-F", "#{window_id}", "-s", s.WorkspaceName, "-n", s.Name}
		}
	case t.Inside:
		args = []string{"new-window", "-d", "-P", "-F", "#{window_id}", "-n", s.Name}
	default:
		args = []string{"new-session", "-d", "-P", "-F", "#{session_id}", "-s", s.Name}
	}
	args = append(args, "-c", s.Cwd, Command(s.Script))
	out, err := t.Run(ctx, t.Bin, args...)
	if err != nil {
		return SessionRef{}, fmt.Errorf("tmux: %w", err)
	}
	id := firstWordOfLastLine(out)
	if id == "" {
		return SessionRef{}, fmt.Errorf("tmux: %s %s printed no id", t.Bin, args[0])
	}
	return SessionRef{Launcher: "tmux", ID: id, Name: s.Name}, nil
}

// sep separates the fields of tmux's formats: printable, so a client that
// is not UTF-8 does not turn it into "_" as it does a tab.
const sep = "|qm|"

// Sessions runs tmux -u list-windows -t =WS -F
// '#{window_id}|qm|#{window_panes}|qm|#{pane_dead}|qm|#{pane_current_command}|qm|#{window_name}'.
// A window is Idle when it has one pane and that pane is dead or runs a
// shell (its active pane's values; with one pane it is the only one). A tmux
// session that does not exist (tmux says it cannot find it, or that no
// server is running) has no windows, and is not an error.
func (t Tmux) Sessions(ctx context.Context, workspaceName string) ([]SessionRef, error) {
	format := strings.Join([]string{"#{window_id}", "#{window_panes}", "#{pane_dead}", "#{pane_current_command}", "#{window_name}"}, sep)
	out, err := t.Run(ctx, t.Bin, "-u", "list-windows", "-t", "="+workspaceName, "-F", format)
	if err != nil {
		// Only tmux's own "not found" answers mean no windows; a missing
		// binary or any other failure is an error, never "nothing running".
		msg := strings.ToLower(err.Error())
		for _, gone := range []string{"can't find session", "no server running", "error connecting to"} {
			if strings.Contains(msg, gone) {
				return nil, nil
			}
		}
		return nil, fmt.Errorf("tmux: %w", err)
	}
	var refs []SessionRef
	for _, line := range strings.Split(strings.TrimRight(string(out), "\r\n"), "\n") {
		f := strings.SplitN(strings.TrimRight(line, "\r"), sep, 5)
		if len(f) != 5 || f[0] == "" {
			continue
		}
		panes, _ := strconv.Atoi(f[1])
		idle := panes == 1 && (f[2] == "1" || IsShell(f[3]))
		refs = append(refs, SessionRef{Launcher: "tmux", ID: f[0], Name: f[4], Workspace: workspaceName, Idle: idle, Panes: panes})
	}
	return refs, nil
}

// Close runs: tmux kill-window -t =WS:ID, WS the tmux session the window
// was listed in (ref.Workspace). A window id alone is refused: tmux window
// ids restart with the server, so an old id can name a window of another
// tmux session. tmux's "can't find window", "can't find session" and "no
// server running" answers are ErrGone; a missing binary ("command not
// found") is not.
func (t Tmux) Close(ctx context.Context, ref SessionRef) error {
	if ref.Workspace == "" {
		return fmt.Errorf("tmux: window %s: no tmux session given; qm closes a window only within the project's tmux session", ref.ID)
	}
	if _, err := t.Run(ctx, t.Bin, "kill-window", "-t", "="+ref.Workspace+":"+ref.ID); err != nil {
		msg := strings.ToLower(err.Error())
		for _, gone := range []string{"can't find window", "can't find session", "no server running"} {
			if strings.Contains(msg, gone) {
				return fmt.Errorf("tmux: %w: %v", ErrGone, err)
			}
		}
		return fmt.Errorf("tmux: %w", err)
	}
	return nil
}

// PaneOf runs: tmux -u display-message -p -t PANE
// '#{window_id}|qm|#{session_name}|qm|#{window_name}' and returns the tmux
// session, window id and window name of the pane. -u and the printable
// separator keep a client that is not UTF-8 from turning tabs and non-ASCII
// characters into "_" (a name such as "Shop v2 · next"). A session or window
// name that holds the separator itself cannot be read back: an error.
func (t Tmux) PaneOf(ctx context.Context, pane string) (session, windowID, windowName string, err error) {
	out, err := t.Run(ctx, t.Bin, "-u", "display-message", "-p", "-t", pane, "#{window_id}"+sep+"#{session_name}"+sep+"#{window_name}")
	if err != nil {
		return "", "", "", fmt.Errorf("tmux: %w", err)
	}
	line := strings.TrimRight(string(out), "\r\n")
	parts := strings.Split(line, sep)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
		return "", "", "", fmt.Errorf("tmux: display-message for pane %s printed %q", pane, line)
	}
	return parts[1], parts[0], parts[2], nil
}
