// Package claudecode reads what Claude Code keeps about its sessions and
// folders: the live session files, the running claude processes and folder
// trust. It only reads; it never writes Claude Code's files.
package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Proc is one running process as ps lists it.
type Proc struct {
	PID int
	// PPID is the parent process's pid.
	PPID int
	// Start is the start time as ps -o lstart= prints it, in local time, for
	// example "Fri Nov 20 09:00:00 2026".
	Start string
	// Cmd is the command line: the arguments joined by spaces.
	Cmd string
}

// LiveSession is a Claude Code session that a running claude process holds.
// Cwd and Name are empty when only the process's arguments named the session.
type LiveSession struct {
	PID       int
	SessionID string
	Cwd       string
	Name      string
}

// lstartLen is the width of ps's lstart column ("Fri Nov 20 09:00:00 2026";
// a one-digit day is padded with a space).
const lstartLen = 24

// lstartLayout parses lstart and a session file's procStart.
const lstartLayout = "Mon Jan _2 15:04:05 2006"

// startSlack is how far a session file's procStart may be from the process's
// start time and still name the same process.
const startSlack = 2 * time.Second

// Procs lists every running process with ps.
func Procs() ([]Proc, error) {
	return ProcsCtx(context.Background())
}

// ProcsCtx is Procs, with ps killed once ctx ends; the error then wraps
// ctx's.
func ProcsCtx(ctx context.Context) ([]Proc, error) {
	cmd := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=", "-o", "ppid=", "-o", "lstart=", "-o", "command=")
	// lstart's day and month names are English only in the C locale.
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("ps: %w", ctx.Err())
	}
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	return ParseProcs(out), nil
}

// ForegroundOf reads, with ps, process pid's terminal and whether pid is
// in that terminal's foreground process group: tty is the terminal's name
// as ps prints it (ttys012, pts/3), "" when pid has none. A process
// suspended, or under another job started in its terminal, is not in the
// foreground.
func ForegroundOf(ctx context.Context, pid int) (tty string, foreground bool, err error) {
	out, err := exec.CommandContext(ctx, "ps", "-o", "tty=", "-o", "pgid=", "-o", "tpgid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", false, fmt.Errorf("ps %d: %w", pid, err)
	}
	return parseForeground(out)
}

// parseForeground reads ps -o tty= -o pgid= -o tpgid= of one process.
func parseForeground(out []byte) (tty string, foreground bool, err error) {
	f := strings.Fields(string(out))
	if len(f) != 3 {
		return "", false, fmt.Errorf("ps: unexpected output %q", out)
	}
	pgid, err1 := strconv.Atoi(f[1])
	tpgid, err2 := strconv.Atoi(f[2])
	if err1 != nil || err2 != nil {
		return "", false, fmt.Errorf("ps: unexpected output %q", out)
	}
	if f[0] == "??" || f[0] == "?" || f[0] == "-" {
		return "", false, nil
	}
	return f[0], tpgid > 0 && pgid == tpgid, nil
}

// ParseProcs reads the output of ps -A -o pid= -o ppid= -o lstart= -o
// command=: on each line the pid, the parent pid, the 24-character start
// time, then the command. Lines that do not have that shape are skipped.
func ParseProcs(out []byte) []Proc {
	var procs []Proc
	for line := range strings.Lines(string(out)) {
		line = strings.TrimLeft(strings.TrimRight(line, "\r\n"), " \t")
		first, rest, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(first)
		if err != nil || pid <= 0 {
			continue
		}
		rest = strings.TrimLeft(rest, " ")
		second, rest, ok := strings.Cut(rest, " ")
		if !ok {
			continue
		}
		ppid, err := strconv.Atoi(second)
		if err != nil || ppid < 0 {
			continue
		}
		rest = strings.TrimLeft(rest, " ")
		if len(rest) < lstartLen {
			continue
		}
		procs = append(procs, Proc{
			PID:   pid,
			PPID:  ppid,
			Start: rest[:lstartLen],
			Cmd:   strings.TrimSpace(rest[lstartLen:]),
		})
	}
	return procs
}

// sessionFile is the part of <claude dir>/sessions/<pid>.json qm reads.
type sessionFile struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	Name      string `json:"name"`
	ProcStart string `json:"procStart"`
}

// Live returns the sessions running claude processes hold, by session id
// (design 5.7, duplicate guard). A session is live when (a) a
// <claudeDir>/sessions/*.json file names it, its pid is among procs, that
// process is claude, and, when the file has a procStart, it is the process's
// start time (a pid reused after a reboot fails this); or (b) a claude
// process's arguments carry --resume ID, -r ID, --resume=ID, --session-id ID
// or --session-id=ID. A missing sessions folder leaves (b) only; a session
// file that cannot be read, and a folder that cannot be listed, are skipped:
// a session missing from the result may run. A caller that acts on a
// session's absence uses LiveComplete.
func Live(claudeDir string, procs []Proc) (map[string]LiveSession, error) {
	return LiveCtx(context.Background(), claudeDir, procs)
}

// LiveCtx is Live that stops between session files when ctx ends, with
// ctx's error. A single read that stalls (a hung mount) is not cut short.
func LiveCtx(ctx context.Context, claudeDir string, procs []Proc) (map[string]LiveSession, error) {
	return live(ctx, claudeDir, procs, time.Local)
}

// LiveComplete is LiveCtx that tells a session's absence from data it could
// not read: what it skips is an *IncompleteError, returned together with the
// sessions it did read. That is a sessions folder that exists but cannot be
// listed, and a session file that cannot be read, or names no session, or
// names another process than its name does, or has a start time that cannot
// be compared, unless it is certain that no
// running claude process wrote it: a file that is gone when it is read, and
// one whose name is the pid of a process that is not running or not claude,
// are absence. A file caught half-written is read again a few times first. A
// process run by the path of an installed claude version is claude here.
func LiveComplete(ctx context.Context, claudeDir string, procs []Proc) (map[string]LiveSession, error) {
	out, unread, err := probe(ctx, claudeDir, procs, time.Local, true)
	if err == nil && len(unread) > 0 {
		err = &IncompleteError{Unread: unread}
	}
	return out, err
}

// Unread is a session file, or the sessions folder (Folder), that could not
// be read while it may name a running session. PID is the claude process
// that wrote the file, 0 when its name does not say.
type Unread struct {
	Path   string
	PID    int
	Folder bool
	Err    error
}

func (u Unread) String() string {
	err := u.Err
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = pe.Err // the path is named once
	}
	switch {
	case u.Folder:
		return fmt.Sprintf("Claude Code's sessions folder %s cannot be listed (%v): make it readable", u.Path, err)
	case u.PID > 0:
		return fmt.Sprintf("the session file %s of the running claude process %d cannot be read (%v): make it readable, or exit that Claude session", u.Path, u.PID, err)
	}
	return fmt.Sprintf("the session file %s cannot be read (%v): make it readable, or remove it", u.Path, err)
}

// IncompleteError says which session data LiveComplete could not read, so a
// session it did not return may still be running, and how to recover.
type IncompleteError struct{ Unread []Unread }

func (e *IncompleteError) Error() string {
	parts := make([]string, len(e.Unread))
	for i, u := range e.Unread {
		parts[i] = u.String()
	}
	return "the running Claude sessions are not all known: " + strings.Join(parts, "; ")
}

func (e *IncompleteError) Unwrap() []error {
	errs := make([]error, len(e.Unread))
	for i, u := range e.Unread {
		errs[i] = u.Err
	}
	return errs
}

// live is LiveCtx with the zone ps prints lstart in.
func live(ctx context.Context, claudeDir string, procs []Proc, local *time.Location) (map[string]LiveSession, error) {
	out, _, err := probe(ctx, claudeDir, procs, local, false)
	return out, err
}

// probe reads the live sessions, and what it could not read. Only a complete
// probe reads a half-written file again.
func probe(ctx context.Context, claudeDir string, procs []Proc, local *time.Location, complete bool) (map[string]LiveSession, []Unread, error) {
	claudes := make(map[int]Proc, len(procs))
	out := map[string]LiveSession{}
	for _, p := range procs {
		args, ok := IsClaude(p.Cmd)
		if complete {
			args, ok = ClaudeArgs(p.Cmd)
		}
		if !ok {
			continue
		}
		claudes[p.PID] = p
		if id, name := sessionOfArgs(args); id != "" {
			out[id] = LiveSession{PID: p.PID, SessionID: id, Name: name}
		}
	}
	dir := filepath.Join(claudeDir, "sessions")
	var unread []Unread
	// Entries listed before a failure are read all the same.
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		unread = append(unread, Unread{Path: dir, Folder: true, Err: err})
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		path := filepath.Join(dir, e.Name())
		pid := pidOfName(e.Name())
		_, running := claudes[pid]
		// Only a claude process that runs, or may (no pid in the name), makes
		// an unreadable file worth another read, and the data incomplete.
		mayRun := running || pid == 0
		s, err := readSessionFile(path, pid)
		for i := 0; complete && mayRun && i < liveRetries && errors.Is(err, errHalfWritten); i++ {
			liveWait(ctx)
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
			s, err = readSessionFile(path, pid)
		}
		p, ok := claudes[s.PID]
		same := s.ProcStart == "" || ok && sameStart(s.ProcStart, p.Start, local)
		if err == nil && ok && !same && !comparableStarts(s.ProcStart, p.Start) {
			err = fmt.Errorf("its start time %q cannot be compared with the process's %q", s.ProcStart, p.Start)
		}
		if err != nil && mayRun && !errors.Is(err, fs.ErrNotExist) {
			unread = append(unread, Unread{Path: path, PID: pid, Err: err})
		}
		if complete && err != nil {
			continue // not a session file to go by, whoever it names
		}
		if !ok || s.SessionID == "" || !same {
			continue // not read, or not a running claude's
		}
		out[s.SessionID] = LiveSession{PID: s.PID, SessionID: s.SessionID, Cwd: s.Cwd, Name: s.Name}
	}
	return out, unread, nil
}

// pidOfName is the pid a session file is named by (<pid>.json), 0 when its
// name is no pid.
func pidOfName(name string) int {
	pid, err := strconv.Atoi(strings.TrimSuffix(name, ".json"))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// liveRetries is how often LiveComplete reads a half-written session file
// again, waiting liveRetry before each.
const (
	liveRetries = 4
	liveRetry   = 50 * time.Millisecond
)

// liveWait waits liveRetry, or until ctx ends; tests replace it.
var liveWait = func(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-time.After(liveRetry):
	}
}

// SessionsNote says, when <claudeDir>/sessions is missing or cannot be
// read, that the duplicate guard reads the processes' arguments only (spec
// 8); "" when the folder reads.
func SessionsNote(claudeDir string) string {
	dir := filepath.Join(claudeDir, "sessions")
	_, err := os.ReadDir(dir)
	switch {
	case err == nil:
		return ""
	case errors.Is(err, fs.ErrNotExist):
		return "Claude Code's sessions folder is missing: running sessions found from process arguments only"
	}
	return fmt.Sprintf("Claude Code's sessions folder not read (%v): running sessions found from process arguments only", err)
}

// errHalfWritten: a session file was read but its content is not a whole
// session, as a writer caught midway leaves it.
var errHalfWritten = errors.New("not a whole session file")

// readSessionFile reads the session file at path, named by pid (0: by none).
// A file that parses is returned even when it is not a whole session.
func readSessionFile(path string, pid int) (sessionFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return sessionFile{}, err
	}
	var s sessionFile
	if err := json.Unmarshal(b, &s); err != nil {
		return sessionFile{}, fmt.Errorf("%w: %w", errHalfWritten, err)
	}
	switch {
	case pid == 0:
		// Not named as Claude Code names a session file: it counts for what
		// it says, and may well be none.
	case s.SessionID == "" || s.PID <= 0:
		return s, fmt.Errorf("%w: it names no session", errHalfWritten)
	case s.PID != pid:
		return s, fmt.Errorf("%w: it names process %d", errHalfWritten, s.PID)
	}
	return s, nil
}

// comparableStarts says whether procStart and lstart both parse, so that they
// differ only when they name different instants (sameStart).
func comparableStarts(procStart, lstart string) bool {
	_, err1 := time.Parse(lstartLayout, collapse(procStart))
	_, err2 := time.Parse(lstartLayout, collapse(lstart))
	return err1 == nil && err2 == nil
}

// sameStart reports whether a session file's procStart and ps's lstart name
// the same instant. Claude Code writes procStart in UTC, while ps prints
// lstart in local time; both have whole seconds, so they may differ by a
// second at most. A local time in the hour repeated when daylight saving
// ends names two instants; either one matches. When either text does not
// parse, the two must be the same text once runs of spaces are collapsed.
func sameStart(procStart, lstart string, local *time.Location) bool {
	ps, err1 := time.ParseInLocation(lstartLayout, collapse(procStart), time.UTC)
	wall, err2 := time.ParseInLocation(lstartLayout, collapse(lstart), time.UTC)
	if err1 != nil || err2 != nil {
		return collapse(procStart) == collapse(lstart)
	}
	for _, ls := range localReadings(wall, local) {
		if d := ps.Sub(ls); d <= startSlack && d >= -startSlack {
			return true
		}
	}
	return false
}

// localReadings returns the instants at which local's clocks show wall (a
// clock reading held in a UTC time): one in most hours, two in the hour
// repeated when daylight saving ends. For a reading skipped when daylight
// saving starts, it falls back to time.Date's normalised instant.
func localReadings(wall time.Time, local *time.Location) []time.Time {
	var out []time.Time
	// The zone offsets in force a day either side of wall cover both sides
	// of any change near it.
	for _, probe := range []time.Time{wall.Add(-24 * time.Hour), wall, wall.Add(24 * time.Hour)} {
		_, off := probe.In(local).Zone()
		t := wall.Add(-time.Duration(off) * time.Second)
		if t.In(local).Format(lstartLayout) == wall.Format(lstartLayout) && !slices.ContainsFunc(out, t.Equal) {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		y, mo, d := wall.Date()
		h, mi, s := wall.Clock()
		out = append(out, time.Date(y, mo, d, h, mi, s, 0, local))
	}
	return out
}

func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// sessionOfArgs returns the session id claude's arguments resume or set, and
// the session's name (-n or --name), if any.
//
// ps prints a worker's first prompt as part of its command line, so a prompt
// that quotes "--resume ID" would mark that id live. The scan stops at "--"
// and at the first token holding a newline (ps prints it as \012), which
// cuts every prompt after its first line. A first line quoting the flag is
// still read as one: a cut at the first positional argument is not safe,
// since a flag's value (a settings JSON) may itself hold spaces.
func sessionOfArgs(args []string) (id, name string) {
	value := func(i int) string {
		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") && !strings.Contains(args[i+1], `\012`) {
			return args[i+1]
		}
		return ""
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" || strings.Contains(a, `\012`) {
			break
		}
		switch {
		case a == "--resume" || a == "-r" || a == "--session-id":
			if v := value(i); v != "" {
				id = v
				i++
			}
		case strings.HasPrefix(a, "--resume="):
			id = strings.TrimPrefix(a, "--resume=")
		case strings.HasPrefix(a, "--session-id="):
			id = strings.TrimPrefix(a, "--session-id=")
		case a == "-n" || a == "--name":
			if v := value(i); v != "" {
				name = v
				i++
			}
		case strings.HasPrefix(a, "--name="):
			name = strings.TrimPrefix(a, "--name=")
		}
	}
	return id, name
}

// SessionOfArgs is the session id claude's arguments name (--resume, -r,
// --session-id, with or without "=") and the session's name (-n, --name),
// read as Live reads them: the scan stops at "--" and at a token holding a
// newline.
func SessionOfArgs(args []string) (id, name string) {
	return sessionOfArgs(args)
}

// statFile reports whether a file exists at path; tests replace it.
var statFile = func(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

// IsClaude splits a command line as ps prints it (argv joined by spaces)
// into claude's arguments, when it runs claude: argv[0] is "claude", or a
// path ending in "/claude", which may hold spaces as long as that path is an
// existing file; or argv[0] is the path of an existing version file of
// Claude Code's own installer (.../claude/versions/<version>). A process
// writes its own command line, so this alone proves nothing: what gives a
// permission flag away also checks the executable (spawn.ParentPermission).
// A claude that node runs is not taken: its executable is node's, and only
// its command line would tell it from any other script.
func IsClaude(cmd string) ([]string, bool) {
	f := strings.Fields(cmd)
	if len(f) == 0 {
		return nil, false
	}
	if filepath.Base(f[0]) == "claude" {
		return f[1:], true
	}
	for i := strings.Index(cmd, "/claude"); i >= 0; {
		end := i + len("/claude")
		if (end == len(cmd) || cmd[end] == ' ') && statFile(cmd[:end]) {
			return strings.Fields(cmd[end:]), true
		}
		next := strings.Index(cmd[end:], "/claude")
		if next < 0 {
			break
		}
		i = end + next
	}
	if path, args, ok := versionedClaude(cmd); ok && statFile(path) {
		return args, true
	}
	return nil, false
}

// ClaudeArgs are claude's arguments in the command line of a process that
// LiveComplete takes for claude: IsClaude, or run by the path of an installed
// version whose file is gone (an update removes old ones while they run).
// What acts on a session LiveComplete returned reads them so.
func ClaudeArgs(cmd string) ([]string, bool) {
	if args, ok := IsClaude(cmd); ok {
		return args, true
	}
	_, args, ok := versionedClaude(cmd)
	return args, ok
}

// versionedClaude splits a command line whose argv[0] is the path of a
// version of Claude Code's own installer (.../claude/versions/<version>)
// into that path and the arguments. It does not look for the file.
func versionedClaude(cmd string) (path string, args []string, ok bool) {
	const dir = "/claude/versions/"
	i := strings.Index(cmd, dir)
	// The path is argv[0]: absolute, with no argument before it (a space in
	// it is one in a folder's name).
	if i < 0 || !strings.HasPrefix(cmd, "/") || strings.Contains(cmd[:i], " /") || strings.Contains(cmd[:i], " -") {
		return "", nil, false
	}
	version, rest, _ := strings.Cut(cmd[i+len(dir):], " ")
	if version == "" || strings.Contains(version, "/") {
		return "", nil, false
	}
	return cmd[:i+len(dir)+len(version)], strings.Fields(rest), true
}

// maxAncestors bounds the walk of Ancestor (as spawn.ParentPermission's).
const maxAncestors = 64

// Ancestor returns the first process, starting at pid itself and following
// PPID, whose command IsClaude accepts; false when the chain has none, ends
// at a pid procs does not list, loops, or is longer than 64 steps.
func Ancestor(procs []Proc, pid int) (Proc, bool) {
	byPID := make(map[int]Proc, len(procs))
	for _, p := range procs {
		byPID[p.PID] = p
	}
	for range maxAncestors {
		p, ok := byPID[pid]
		if !ok {
			break
		}
		if _, ok := IsClaude(p.Cmd); ok {
			return p, true
		}
		if p.PPID <= 0 || p.PPID == pid {
			break
		}
		pid = p.PPID
	}
	return Proc{}, false
}
