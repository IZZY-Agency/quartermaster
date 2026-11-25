package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/outcomes"
	"github.com/IZZY-Agency/quartermaster/internal/paths"
	"github.com/IZZY-Agency/quartermaster/internal/up"
)

// How qm up note --notify waits for the sessions agterm starts again by
// itself: it reads the sessions every noteStep, until no claude process came
// or went over noteQuiet reads in a row. After noteMax steps it gives up,
// with no note.
const (
	noteStep  = 5 * time.Second
	noteQuiet = 6
	noteMax   = 48
)

// noteGhWait bounds what the pull requests of one note may take.
const noteGhWait = 10 * time.Second

// noteNames is how many projects the notification names.
const noteNames = 5

// noteNameLen is how many characters of a project's name it shows.
const noteNameLen = 40

// errNotSettled: the Claude sessions kept starting and ending.
var errNotSettled = errors.New("claude sessions are still starting or ending: no note")

// upNote runs qm up note: it lists the projects qm up would restore sessions
// of, or with --notify sends their names as one agterm notification. It
// starts and changes nothing. What cannot be judged gives no note: an error,
// but with --notify (run by an agterm hook, which shows a banner when it
// fails) only a line on stderr.
func upNote(args []string, stdout, stderr io.Writer) error {
	var notify bool
	fs := flag.NewFlagSet("qm up note", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.BoolVar(&notify, "notify", false, "send the list as one agterm notification, once no session is starting any more")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if !notify {
		r, err := readNote(stderr)
		if err != nil {
			return err
		}
		ls, err := r.left(true)
		if err == nil {
			_, err = fmt.Fprint(stdout, renderNote(ls, time.Now()))
		}
		return err
	}
	ls, err := settledNote()
	if err != nil {
		printErr(stderr, "qm up note", err)
		return nil
	}
	if len(ls) == 0 {
		return nil
	}
	socket := upEnv("AGT_SOCKET")
	if socket == "" {
		socket = upEnv("AGTERM_SOCKET")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return upAgtermCtl.Notify(ctx, socket, "quartermaster", noteBody(ls))
}

// noteRead is one reading of the machine for a note. claudes are the ids of
// the running claude processes, sorted.
type noteRead struct {
	projects []up.Project
	claudes  []int
	left     func(prs bool) ([]up.Left, error)
}

// readNote reads the projects, the decision log and the running sessions.
// Its left says what is left behind; with prs, after the pull requests that
// decides. It is an error when the sessions, or the folders Claude Code
// trusts, are not all known.
func readNote(notes io.Writer) (noteRead, error) {
	projects, err := up.All()
	if err != nil || len(projects) == 0 {
		return noteRead{left: func(bool) ([]up.Left, error) { return nil, nil }}, err
	}
	recs, err := readUpRecords(notes)
	if err != nil {
		return noteRead{}, err
	}
	procs, err := upProcs()
	if err != nil {
		return noteRead{}, err
	}
	claudeDir, err := paths.ClaudeDir()
	if err != nil {
		return noteRead{}, err
	}
	live, err := claudecode.LiveComplete(context.Background(), claudeDir, procs)
	if err != nil {
		return noteRead{}, err
	}
	if n := claudecode.SessionsNote(claudeDir); n != "" {
		return noteRead{}, errors.New(n)
	}
	cc, err := outcomes.DefaultClaude()
	if err != nil {
		return noteRead{}, err
	}
	handoff, err := up.HandoffDir()
	if err != nil {
		return noteRead{}, err
	}
	check, n := trustOnce()
	if n != "" {
		return noteRead{}, errors.New(n)
	}
	all, err := cc.AllTranscripts()
	if err != nil {
		return noteRead{}, err
	}
	// One listing for every session, and one answer for each folder.
	transcripts := make(map[string]string, len(all))
	for _, path := range all {
		id := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		if _, ok := transcripts[id]; !ok {
			transcripts[id] = path
		}
	}
	trustedDirs := map[string]bool{}
	r := noteRead{projects: projects}
	for _, p := range procs {
		if _, ok := claudecode.ClaudeArgs(p.Cmd); ok {
			r.claudes = append(r.claudes, p.PID)
		}
	}
	slices.Sort(r.claudes)
	env := up.Env{
		Now: time.Now().UTC(), Live: live, HandoffDir: handoff, Shallow: true,
		TranscriptPath: func(id string) (string, error) { return transcripts[id], nil },
		Trusted: func(dir string) (ok bool, err error) {
			if ok, known := trustedDirs[dir]; known {
				return ok, nil
			}
			if ok, err = check(dir); err == nil {
				trustedDirs[dir] = ok
			}
			return ok, err
		},
	}
	r.left = func(prs bool) ([]up.Left, error) {
		var run up.DirRunner
		if prs {
			ctx, cancel := context.WithTimeout(context.Background(), noteGhWait)
			defer cancel()
			run = func(_ context.Context, dir, bin string, args ...string) ([]byte, error) {
				return upRun(ctx, dir, bin, args...)
			}
		}
		var out []up.Left
		for _, p := range projects {
			l, ok, err := up.LeftBehind(p, recs, env, run)
			if err != nil {
				return nil, err
			}
			if ok {
				out = append(out, l)
			}
		}
		return out, nil
	}
	return r, nil
}

// settledNote is what is left behind once no session starts any more: the
// hook of agterm's launch runs while agterm starts the commands of its
// restored panes again, one after another. A project whose state was saved
// meanwhile (a qm up is at work on it) is left out.
func settledNote() ([]up.Left, error) {
	var first map[string]time.Time // when each project was last saved, at the first read
	var before []int
	quiet, known := 0, false
	var failed error
	for i := range noteMax {
		if i > 0 {
			upSleep(noteStep)
		}
		r, err := readNote(io.Discard)
		if err != nil {
			failed, known = err, false
			continue
		}
		failed = nil
		if first == nil {
			first = map[string]time.Time{}
			for _, p := range r.projects {
				first[p.ID] = p.Updated
			}
		}
		ls, err := r.left(false)
		if err != nil || len(ls) == 0 {
			return nil, err
		}
		if known && slices.Equal(before, r.claudes) {
			quiet++
		} else {
			quiet = 0
		}
		before, known = r.claudes, true
		if quiet < noteQuiet {
			continue
		}
		ls, err = r.left(true)
		return slices.DeleteFunc(ls, func(l up.Left) bool {
			at, ok := first[l.Project.ID]
			return !ok || !at.Equal(l.Project.Updated)
		}), err
	}
	if failed == nil {
		failed = errNotSettled
	}
	return nil, failed
}

// renderNote is the list qm up note prints: for each project its folder, how
// many sessions are not running, when the last of them was active, and the
// line that restores them.
func renderNote(ls []up.Left, now time.Time) string {
	var b strings.Builder
	for _, l := range ls {
		var what []string
		if l.Coordinator {
			what = append(what, "coordinator")
		}
		if l.Workers > 0 {
			n := fmt.Sprintf("%d worker(s)", l.Workers)
			if l.Coordinator { // no pull request was asked
				n = "up to " + n
			}
			what = append(what, n)
		}
		last := l.Ago(now)
		if last != "" {
			last = ", last active " + last
		}
		root := printable(l.Project.Root)
		line := "cd " + launch.Quote(root) + " && qm up"
		if root != l.Project.Root { // never printed raw to a terminal
			line = "run qm up in that folder (its name holds characters that are not shown)"
		}
		_, _ = fmt.Fprintf(&b, "%s: %s not running%s\n  %s\n", root, strings.Join(what, " and "), last, line)
	}
	return b.String()
}

// noteBody is the notification: the projects' names (no path), and that
// qm up restores them.
func noteBody(ls []up.Left) string {
	var names []string
	count := map[string]int{}
	for _, l := range ls {
		n := noteName(l.Project.Root)
		if count[n]++; count[n] == 1 {
			names = append(names, n)
		}
	}
	for i, n := range names {
		if count[n] > 1 {
			names[i] = fmt.Sprintf("%s (%d folders)", n, count[n])
		}
	}
	more := ""
	if len(names) > noteNames {
		more = fmt.Sprintf(" and %d more", len(names)-noteNames)
		names = names[:noteNames]
	}
	where := "qm up in the project folder restores them."
	if len(ls) > 1 {
		where = "qm up in each project folder restores them; qm up note lists them."
	}
	return "Sessions not running: " + strings.Join(names, ", ") + more + ". " + where
}

// noteName is the name of the project at root: its folder's, cut short.
func noteName(root string) string {
	n := []rune(printable(filepath.Base(root)))
	if len(n) > noteNameLen {
		return string(n[:noteNameLen]) + "…"
	}
	return string(n)
}

// printable is s without its control and other unprintable characters.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, s)
}
