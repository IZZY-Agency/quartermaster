package launch

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
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
// id is what -P prints, checked to be one. A session or window name tmux
// would change, expand or run (checkSession, checkWindow) is refused before
// tmux runs; DIR is escaped (arg).
func (t Tmux) Spawn(ctx context.Context, s LaunchSpec) (SessionRef, error) {
	if err := s.check(); err != nil {
		return SessionRef{}, err
	}
	if err := checkWindow(s.Name); err != nil {
		return SessionRef{}, err
	}
	var args []string
	id := windowID
	switch {
	case s.WorkspaceName != "":
		if err := checkTarget(s.WorkspaceName); err != nil {
			return SessionRef{}, err
		}
		if _, err := t.Run(ctx, t.Bin, "has-session", "-t", "="+s.WorkspaceName); err == nil {
			args = []string{"new-window", "-d", "-P", "-F", "#{window_id}", "-t", "=" + s.WorkspaceName + ":", "-n", s.Name}
		} else {
			if err := checkSession(s.WorkspaceName); err != nil {
				return SessionRef{}, err
			}
			args = []string{"new-session", "-d", "-P", "-F", "#{window_id}", "-s", s.WorkspaceName, "-n", s.Name}
		}
	case t.Inside:
		args = []string{"new-window", "-d", "-P", "-F", "#{window_id}", "-n", s.Name}
	default:
		if err := checkSession(s.Name); err != nil {
			return SessionRef{}, err
		}
		id = sessionID
		args = []string{"new-session", "-d", "-P", "-F", "#{session_id}", "-s", s.Name}
	}
	args = append(args, "-c", arg(s.Cwd), Command(s.Script))
	out, err := t.Run(ctx, t.Bin, args...)
	if err != nil {
		if killed(err) {
			err = fmt.Errorf("%w (%w)", err, ErrMaybeStarted)
		}
		return SessionRef{}, fmt.Errorf("tmux: %w", err)
	}
	got := strings.TrimSpace(string(out))
	if !id.MatchString(got) {
		return SessionRef{}, fmt.Errorf("tmux: %s %s printed %q, not an id: %w", t.Bin, args[0], got, ErrMaybeStarted)
	}
	return SessionRef{Launcher: "tmux", ID: got, Name: s.Name}, nil
}

// The ids tmux gives windows, panes and sessions. Anything else in their
// place is not passed to tmux: a target may also be a name or a pattern.
var (
	windowID  = regexp.MustCompile(`^@[0-9]+$`)
	paneID    = regexp.MustCompile(`^%[0-9]+$`)
	sessionID = regexp.MustCompile(`^\$[0-9]+$`)
)

// checkWindow says why tmux would not name a window name ("" included): it
// expands formats in a name ("#(command)" runs a command), writes "\" and
// control characters otherwise, and ends a command at a last ";".
func checkWindow(name string) error {
	if why := badName(name, `#\`); why != "" {
		return fmt.Errorf("tmux: %q cannot name a tmux window: it %s", name, why)
	}
	return nil
}

// checkSession says why tmux would not give a new session the name name: what
// checkWindow and checkTarget refuse. (tmux writes "_" for "." and ":".)
func checkSession(name string) error {
	if err := checkTarget(name); err != nil {
		return err
	}
	if why := badName(name, `#\`); why != "" {
		return fmt.Errorf("tmux: the workspace name %q cannot name a tmux session: it %s", name, why)
	}
	return nil
}

// checkTarget says why name cannot name an existing session in a target,
// where tmux expands nothing: "." and ":" end it, a leading "$" makes it a
// session id, a last ";" ends the command, and the separator cannot be read
// back from a listing.
func checkTarget(name string) error {
	why := ""
	switch i := strings.IndexAny(name, ".:"); {
	case name == "":
		why = "is empty"
	case i >= 0:
		why = fmt.Sprintf("holds %q", name[i:i+1])
	case strings.HasPrefix(name, "$"):
		why = `starts with "$"`
	case strings.HasSuffix(name, ";"):
		why = `ends with ";"`
	case strings.Contains(name, sep):
		why = fmt.Sprintf("holds %q", sep)
	}
	if why != "" {
		return fmt.Errorf("tmux: the workspace name %q cannot name a tmux session: it %s", name, why)
	}
	return nil
}

func badName(name, chars string) string {
	switch i := strings.IndexAny(name, chars); {
	case name == "":
		return "is empty"
	case !utf8.ValidString(name):
		return "is not UTF-8"
	case i >= 0:
		return fmt.Sprintf("holds %q", name[i:i+1])
	case strings.Contains(name, sep):
		return fmt.Sprintf("holds %q", sep)
	case strings.HasSuffix(name, ";"):
		return `ends with ";"`
	case strings.ContainsFunc(name, func(r rune) bool { return !unicode.IsPrint(r) }):
		return "holds a character that does not print"
	}
	return ""
}

// arg is s as an argument tmux expands formats in: "#" doubled, and a last
// ";", which would end the command, escaped.
func arg(s string) string {
	s = strings.ReplaceAll(s, "#", "##")
	if strings.HasSuffix(s, ";") {
		s = s[:len(s)-1] + `\;`
	}
	return s
}

// sep separates the fields of tmux's formats: printable, so a client that
// is not UTF-8 does not turn it into "_" as it does a tab.
const sep = "|qm|"

// windowFields are the fields of a listed window, its name last: a name may
// hold anything. A window has its active pane's values; with one pane it is
// the only one.
var windowFields = []string{"#{window_id}", "#{session_id}", "#{window_panes}", "#{pane_dead}", "#{window_linked}", "#{pane_pid}", "#{pane_tty}", "#{pane_current_command}", "#{window_name}"}

// Sessions runs tmux -u list-windows -t =WS -F with windowFields. A window
// is Idle when it has one pane and that pane is dead, or every process of it
// is a shell that waits for input (shellsOnly). A tmux session that does not
// exist (tmux says it cannot find it, or that no server is running) has no
// windows, and is not an error.
func (t Tmux) Sessions(ctx context.Context, workspaceName string) ([]SessionRef, error) {
	return t.windows(ctx, workspaceName, func(string) bool { return true })
}

// windows is Sessions, which reads the processes (ps) only for the windows
// idle asks about; another that runs a shell is not Idle.
func (t Tmux) windows(ctx context.Context, workspaceName string, idle func(id string) bool) ([]SessionRef, error) {
	if err := checkTarget(workspaceName); err != nil {
		return nil, err
	}
	out, err := t.Run(ctx, t.Bin, "-u", "list-windows", "-t", "="+workspaceName, "-F", strings.Join(windowFields, sep))
	if err != nil {
		// Only tmux's own "not found" answers mean no windows; a missing
		// binary or any other failure is an error, never "nothing running".
		if notThere(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("tmux: %w", err)
	}
	var refs []SessionRef
	var ps []process
	read := false
	for line := range strings.Lines(string(out)) {
		line = strings.TrimRight(line, "\r\n")
		f := strings.SplitN(line, sep, len(windowFields))
		if len(f) != len(windowFields) || !windowID.MatchString(f[0]) || !sessionID.MatchString(f[1]) {
			return nil, fmt.Errorf("tmux: list-windows printed %q", line)
		}
		panes, err1 := strconv.Atoi(f[2])
		pid, err2 := strconv.Atoi(f[5])
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("tmux: list-windows printed %q", line)
		}
		r := SessionRef{Launcher: "tmux", ID: f[0], Name: f[len(f)-1], Workspace: workspaceName, Panes: panes, PanePID: pid, Linked: f[4] != "0", tmuxSession: f[1]}
		switch {
		case panes != 1:
		case f[3] == "1":
			r.Idle = true
		case IsShell(f[7]) && idle(f[0]):
			// tmux names the foreground command only: a script's shell is
			// named as an interactive one is, and a suspended or background
			// job is not named. The processes tell.
			if !read {
				read = true
				if ps, err = t.processes(ctx); err != nil {
					return nil, err
				}
			}
			r.Idle = shellsOnly(ps, pid, f[6])
		}
		refs = append(refs, r)
	}
	return refs, nil
}

// notThere says whether a tmux command failed because the window or session
// it names, or the server, does not exist.
func notThere(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, gone := range []string{"can't find window", "can't find session", "can't find pane", "no server running"} {
		if strings.Contains(msg, gone) {
			return true
		}
	}
	// A socket that cannot be used (permission denied) is another failure.
	return strings.Contains(msg, "error connecting to") && strings.Contains(msg, "no such file or directory")
}

// process is one line of ps.
type process struct {
	pid, ppid int
	tty       string
	fg        bool // in its terminal's foreground
	args      []string
}

// processes runs ps for every process but the ended ones (zombies).
func (t Tmux) processes(ctx context.Context) ([]process, error) {
	out, err := t.Run(ctx, "ps", "-A", "-ww", "-o", "pid=", "-o", "ppid=", "-o", "stat=", "-o", "tty=", "-o", "args=")
	if err != nil {
		return nil, fmt.Errorf("tmux: the processes of a window not read: %w", err)
	}
	var ps []process
	for line := range strings.Lines(string(out)) {
		f := strings.Fields(line)
		if len(f) < 4 || strings.HasPrefix(f[2], "Z") {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		ps = append(ps, process{pid, ppid, f[3], strings.Contains(f[2], "+"), f[4:]})
	}
	return ps, nil
}

// shellsOnly says whether the pane of process pid on terminal tty runs
// nothing but shells that wait for input: pid, every process on tty, and
// every descendant of one of these. One of the shells holds the terminal's
// foreground: else a job ps does not show (another user's, where processes are
// hidden) does. A prompt's helper (promptHelper) does not count. A pid that is
// not among ps is not known to: false.
func shellsOnly(ps []process, pid int, tty string) bool {
	tty = strings.TrimPrefix(tty, "/dev/")
	children := map[int][]process{}
	var todo []process
	found, fg := false, false
	for _, p := range ps {
		children[p.ppid] = append(children[p.ppid], p)
		if p.pid == pid || tty != "" && p.tty == tty {
			todo = append(todo, p)
		}
		found = found || p.pid == pid
	}
	seen := map[int]bool{}
	for len(todo) > 0 {
		p := todo[0]
		todo = todo[1:]
		if seen[p.pid] {
			continue
		}
		seen[p.pid] = true
		if promptHelper(p.args) && p.pid != pid {
			continue
		}
		if !waitingShell(p.args) {
			return false
		}
		fg = fg || p.fg && p.tty == tty
		todo = append(todo, children[p.pid]...)
	}
	return found && fg
}

// promptHelper says whether args are gitstatusd, the daemon the powerlevel10k
// prompt keeps beside every shell, and which ends with it.
func promptHelper(args []string) bool {
	return len(args) > 0 && strings.HasPrefix(args[0][strings.LastIndexByte(args[0], '/')+1:], "gitstatusd")
}

// waitingShell says whether args, as ps prints a command line, are a shell
// that was given nothing to run: flags only, none of them -c or the like. A
// shell that runs a script, or whose flag takes a value, is not. A shell in a
// loop of its own builtins looks the same as one at its prompt.
func waitingShell(args []string) bool {
	if len(args) == 0 || !IsShell(args[0]) {
		return false
	}
	for _, a := range args[1:] {
		switch {
		case strings.HasPrefix(a, "--"):
			if strings.Contains(a, "command") {
				return false
			}
		case len(a) < 2 || a[0] != '-' && a[0] != '+' || strings.ContainsAny(a, "cC"):
			return false
		}
	}
	return true
}

// Locate runs tmux -u list-windows -a and returns the windows of ref's id, in
// whatever tmux session, as ID, Name and Workspace. None is no error, and a
// session id, which Spawn gives without a workspace, names no window.
func (t Tmux) Locate(ctx context.Context, ref SessionRef) ([]SessionRef, error) {
	if sessionID.MatchString(ref.ID) {
		return nil, nil
	}
	if !windowID.MatchString(ref.ID) {
		return nil, fmt.Errorf("tmux: %q is not a window id", ref.ID)
	}
	out, err := t.Run(ctx, t.Bin, "-u", "list-windows", "-a", "-F", strings.Join([]string{"#{window_id}", "#{session_name}", "#{window_name}"}, sep))
	if err != nil {
		if notThere(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("tmux: %w", err)
	}
	var refs []SessionRef
	for line := range strings.Lines(string(out)) {
		f := strings.SplitN(strings.TrimRight(line, "\r\n"), sep, 3)
		if len(f) == 3 && f[0] == ref.ID {
			refs = append(refs, SessionRef{Launcher: "tmux", ID: f[0], Workspace: f[1], Name: f[2]})
		}
	}
	return refs, nil
}

// Close reads the window again in the tmux session it was listed in
// (ref.Workspace) and kills it only when it is as ref was listed: the same name
// and process, alone in its window and in no other tmux session, and idle if
// ref was. tmux checks all of that but the name and the idleness again, in the
// command that kills (if-shell -F). Any other window is left open, a
// *ChangedError; one that is gone is ErrGone. A window id alone is
// refused: tmux window ids restart with the server, so an old id can name a
// window of another tmux session.
func (t Tmux) Close(ctx context.Context, ref SessionRef) error {
	if ref.Workspace == "" {
		return fmt.Errorf("tmux: window %s: no tmux session given; qm closes a window only within the project's tmux session", ref.ID)
	}
	if !windowID.MatchString(ref.ID) {
		return fmt.Errorf("tmux: %q is not a window id", ref.ID)
	}
	refs, err := t.windows(ctx, ref.Workspace, func(id string) bool { return ref.Idle && id == ref.ID })
	if err != nil {
		return err
	}
	var now SessionRef
	for _, r := range refs {
		if r.ID == ref.ID {
			now = r
		}
	}
	if now.ID == "" {
		places, err := t.Locate(ctx, ref)
		if err != nil {
			return err
		}
		if why := Moved(ref, places); why != "" {
			return &ChangedError{ID: ref.ID, Why: why}
		}
		return fmt.Errorf("tmux: %w: no window %s in tmux session %s", ErrGone, ref.ID, ref.Workspace)
	}
	if why := changed(ref, now); why != "" {
		return &ChangedError{ID: ref.ID, Why: why}
	}
	// if-shell's target may be another window when ID is not in WS any more:
	// the condition compares its id and its session's, and then ID is the
	// window it looked at, in the session it was read in.
	same := fmt.Sprintf("#{&&:#{==:#{window_id},%s},#{&&:#{==:#{session_id},%s},#{&&:#{==:#{window_panes},1},#{&&:#{==:#{window_linked},0},#{==:#{pane_pid},%d}}}}}", ref.ID, now.tmuxSession, now.PanePID)
	out, err := t.Run(ctx, t.Bin, "if-shell", "-F", "-t", "="+ref.Workspace+":"+ref.ID, same, "kill-window -t "+ref.ID, "display-message -p "+changedWord)
	switch {
	case err != nil && notThere(err):
		return fmt.Errorf("tmux: %w: %v", ErrGone, err)
	case err != nil:
		return fmt.Errorf("tmux: %w", err)
	case strings.TrimSpace(string(out)) == changedWord:
		return &ChangedError{ID: ref.ID, Why: "it changed right before its close"}
	}
	return nil
}

// changedWord is what Close has tmux print for a window it did not kill.
const changedWord = "changed"

// Moved says where the window recorded as rec is, given what Locate found of
// its id: in another tmux session than rec.Workspace, under another name
// there, or both (or another window has the id since the server was started
// again). "" when no window of the id is anywhere.
func Moved(rec SessionRef, places []SessionRef) string {
	for _, p := range places {
		if p.Name == rec.Name && p.Workspace != rec.Workspace {
			return fmt.Sprintf("it is now in tmux session %s", p.Workspace)
		}
	}
	for _, p := range places {
		if p.Workspace == rec.Workspace && p.Name != rec.Name {
			return Renamed(rec, p)
		}
	}
	for _, p := range places {
		return fmt.Sprintf("a window of its id is now %q in tmux session %s", p.Name, p.Workspace)
	}
	return ""
}

// Renamed says that the window recorded as rec is listed as now, under
// another name.
func Renamed(rec, now SessionRef) string {
	return fmt.Sprintf("that window is named %q now, not %q", now.Name, rec.Name)
}

// changed says how window now, just read, differs from ref as it was listed
// ("" when it does not); what ref does not say is not compared.
func changed(ref, now SessionRef) string {
	switch {
	case ref.Name != "" && ref.Name != now.Name:
		return Renamed(ref, now)
	case now.Linked:
		return LinkedWhy
	case now.Panes != 1:
		return fmt.Sprintf("it has %d panes", now.Panes)
	case ref.PanePID != 0 && ref.PanePID != now.PanePID:
		return "its command was started anew"
	case ref.Idle && !now.Idle:
		return "it runs something now"
	}
	return ""
}

// LinkedWhy is why a window linked into several tmux sessions is left alone.
const LinkedWhy = "it is linked into more than one tmux session"

// ErrLinked: a window in more than one tmux session belongs to no single
// project, and closing it would take it from the others.
var ErrLinked = errors.New("is linked into more than one tmux session")

// PaneOf runs: tmux -u display-message -p -t PANE
// '#{pane_id}|qm|#{window_id}|qm|#{window_linked}|qm|#{session_group_size}|qm|#{session_name}|qm|#{window_name}' and
// returns the tmux session, window id and window name of the pane. -u and the
// printable separator keep a client that is not UTF-8 from turning tabs and
// non-ASCII characters into "_" (a name such as "Shop v2 · next"). A session
// or window name that holds the separator itself cannot be read back: an
// error. So is a window linked into several tmux sessions (ErrLinked) and a
// session of a group (new-session -t), of which tmux would name any one, a
// session qm could not name again (checkTarget), and an answer for another
// pane, which tmux may give for one that does not exist.
func (t Tmux) PaneOf(ctx context.Context, pane string) (session, window, windowName string, err error) {
	if !paneID.MatchString(pane) {
		return "", "", "", fmt.Errorf("tmux: %q is not a pane id", pane)
	}
	out, err := t.Run(ctx, t.Bin, "-u", "display-message", "-p", "-t", pane, strings.Join([]string{"#{pane_id}", "#{window_id}", "#{window_linked}", "#{session_group_size}", "#{session_name}", "#{window_name}"}, sep))
	if err != nil {
		return "", "", "", fmt.Errorf("tmux: %w", err)
	}
	line := strings.TrimRight(string(out), "\r\n")
	parts := strings.Split(line, sep)
	if len(parts) != 6 || parts[0] != pane || !windowID.MatchString(parts[1]) || parts[4] == "" {
		return "", "", "", fmt.Errorf("tmux: display-message for pane %s printed %q", pane, line)
	}
	if parts[2] != "0" {
		return "", "", "", fmt.Errorf("tmux: window %s %w", parts[1], ErrLinked)
	}
	if n := parts[3]; n != "" && n != "0" && n != "1" {
		return "", "", "", fmt.Errorf("tmux: window %s: its tmux session %s is one of a group (tmux new-session -t), of which tmux names any one", parts[1], parts[4])
	}
	if err := checkTarget(parts[4]); err != nil {
		return "", "", "", err
	}
	return parts[4], parts[1], parts[5], nil
}

// WindowOf is the window id of a pane, also of a window linked into several
// tmux sessions: tmux display-message -p -t PANE '#{window_id}'.
func (t Tmux) WindowOf(ctx context.Context, pane string) (string, error) {
	if !paneID.MatchString(pane) {
		return "", fmt.Errorf("tmux: %q is not a pane id", pane)
	}
	out, err := t.Run(ctx, t.Bin, "display-message", "-p", "-t", pane, "#{pane_id} #{window_id}")
	if err != nil {
		return "", fmt.Errorf("tmux: %w", err)
	}
	got, id, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
	if got != pane || !windowID.MatchString(id) {
		return "", fmt.Errorf("tmux: no window of pane %s", pane)
	}
	return id, nil
}

// pane is a listed pane: tty without /dev/, "" when its command has ended.
type pane struct{ window, id, tty string }

// panes runs tmux list-panes on a window: its id, "=SESSION:ID", or the id of
// one of its panes. A window that does not exist is an error matching ErrGone.
// list-panes is used because display-message would answer for another window.
func (t Tmux) panes(ctx context.Context, target string) ([]pane, error) {
	id := target
	if i := strings.LastIndexByte(target, ':'); i >= 0 && strings.HasPrefix(target, "=") {
		if err := checkTarget(target[1:i]); err != nil {
			return nil, err
		}
		id = target[i+1:]
	}
	if !windowID.MatchString(id) && (id != target || !paneID.MatchString(id)) {
		return nil, fmt.Errorf("tmux: %q is not a window id", target)
	}
	out, err := t.Run(ctx, t.Bin, "list-panes", "-t", target, "-F", strings.Join([]string{"#{window_id}", "#{pane_id}", "#{pane_dead}", "#{pane_tty}"}, sep))
	if err != nil {
		if notThere(err) {
			return nil, fmt.Errorf("tmux: %w: %v", ErrGone, err)
		}
		return nil, fmt.Errorf("tmux: %w", err)
	}
	var ps []pane
	for line := range strings.Lines(string(out)) {
		line = strings.TrimRight(line, "\r\n")
		f := strings.Split(line, sep)
		if len(f) != 4 || !windowID.MatchString(f[0]) || !paneID.MatchString(f[1]) || windowID.MatchString(id) && f[0] != id {
			return nil, fmt.Errorf("tmux: list-panes of %s printed %q", target, line)
		}
		p := pane{window: f[0], id: f[1]}
		if f[2] != "1" {
			// The terminal of a pane whose command ended may be another
			// pane's by now.
			p.tty = strings.TrimPrefix(f[3], "/dev/")
		}
		if paneID.MatchString(id) && p.id != id {
			continue
		}
		ps = append(ps, p)
	}
	if len(ps) == 0 {
		return nil, fmt.Errorf("tmux: list-panes of %s printed no pane", target)
	}
	return ps, nil
}

// WindowPanes is the number of panes of a window.
func (t Tmux) WindowPanes(ctx context.Context, window string) (int, error) {
	ps, err := t.panes(ctx, window)
	return len(ps), err
}

// PaneTTY is the terminal (pts/3) of a pane, or of the one pane of a window
// (its id, or "=SESSION:ID"); "" when its command has ended or the window has
// several panes.
func (t Tmux) PaneTTY(ctx context.Context, target string) (string, error) {
	ps, err := t.panes(ctx, target)
	if err != nil || len(ps) != 1 {
		return "", err
	}
	return ps[0].tty, nil
}

// RenameWindow runs tmux rename-window -t ID -- NAME.
func (t Tmux) RenameWindow(ctx context.Context, window, name string) error {
	if !windowID.MatchString(window) {
		return fmt.Errorf("tmux: %q is not a window id", window)
	}
	if err := checkWindow(name); err != nil {
		return err
	}
	if _, err := t.Run(ctx, t.Bin, "rename-window", "-t", window, "--", name); err != nil {
		return fmt.Errorf("tmux: %w", err)
	}
	return nil
}
