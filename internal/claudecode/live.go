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
// file that cannot be read is skipped.
func Live(claudeDir string, procs []Proc) (map[string]LiveSession, error) {
	return live(context.Background(), claudeDir, procs, time.Local)
}

// LiveCtx is Live that stops between session files when ctx ends, with
// ctx's error. A single read that stalls (a hung mount) is not cut short.
func LiveCtx(ctx context.Context, claudeDir string, procs []Proc) (map[string]LiveSession, error) {
	return live(ctx, claudeDir, procs, time.Local)
}

// live is LiveCtx with the zone ps prints lstart in.
func live(ctx context.Context, claudeDir string, procs []Proc, local *time.Location) (map[string]LiveSession, error) {
	byPID := make(map[int]Proc, len(procs))
	out := map[string]LiveSession{}
	for _, p := range procs {
		byPID[p.PID] = p
		args, ok := IsClaude(p.Cmd)
		if !ok {
			continue
		}
		if id, name := sessionOfArgs(args); id != "" {
			out[id] = LiveSession{PID: p.PID, SessionID: id, Name: name}
		}
	}
	files, err := filepath.Glob(filepath.Join(claudeDir, "sessions", "*.json"))
	if err != nil {
		return nil, fmt.Errorf("session files: %w", err)
	}
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		s, err := readSessionFile(f)
		if err != nil {
			continue // unreadable, gone since the glob, or not JSON: skipped
		}
		p, ok := byPID[s.PID]
		if !ok || s.SessionID == "" {
			continue
		}
		if _, ok := IsClaude(p.Cmd); !ok {
			continue
		}
		if s.ProcStart != "" && !sameStart(s.ProcStart, p.Start, local) {
			continue
		}
		out[s.SessionID] = LiveSession{PID: s.PID, SessionID: s.SessionID, Cwd: s.Cwd, Name: s.Name}
	}
	return out, nil
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

func readSessionFile(path string) (sessionFile, error) {
	var s sessionFile
	b, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, err
	}
	return s, nil
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
// into claude's arguments, when the executable is claude: argv[0] is
// "claude", or a path ending in "/claude", which may hold spaces as long as
// that path is an existing file.
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
	return nil, false
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
