package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/plain"
	"github.com/IZZY-Agency/quartermaster/internal/up"
)

const downUsage = `usage:
  qm down [--yes] [--keep-open]
`

func init() {
	commands["down"] = command{"record this project's sessions and close them: qm down [--yes] [--keep-open]", runDown}
}

// runDown runs qm down. Exit 0 when done or declined; 1 on a failed close, or
// no terminal to ask in without --yes; 2 on bad input, or outside a recorded
// project.
func runDown(args []string, stdout, stderr io.Writer) int {
	err := downMain(args, stdout, stderr)
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errUsage), errors.Is(err, up.ErrInput):
		printErr(stderr, "qm down", err)
		_, _ = fmt.Fprint(stderr, downUsage)
		return 2
	case errors.Is(err, errQuiet):
		return 1
	default:
		printErr(stderr, "qm down", err)
		return 1
	}
}

// refTerms lists and closes terminal sessions through the launcher a
// recorded session ran in, which need not be the one qm would pick now.
type refTerms struct{}

func (refTerms) Sessions(ctx context.Context, launcher, workspace string) ([]launch.SessionRef, error) {
	l, err := upPick(launcher)
	if err != nil {
		return nil, err
	}
	ls, ok := l.(launch.Lister)
	if !ok {
		return nil, fmt.Errorf("launcher %q cannot list sessions", launcher)
	}
	return ls.Sessions(ctx, workspace)
}

// PaneRuns says whether tmux window ref's pane runs session sessionID's
// process in its foreground; false when the session does not run.
func (refTerms) PaneRuns(ctx context.Context, ref launch.SessionRef, sessionID string) (bool, error) {
	// Sessions not read matter only when this one is not among those read.
	live, err := upLive()
	l, ok := live[sessionID]
	if !ok {
		return false, err
	}
	target := ref.ID
	if ref.Workspace != "" {
		target = "=" + ref.Workspace + ":" + ref.ID
	}
	return upPaneRuns(ctx, target, l.PID)
}

// Locate finds a window wherever it is now; a launcher that cannot find one
// finds none.
func (refTerms) Locate(ctx context.Context, ref launch.SessionRef) ([]launch.SessionRef, error) {
	l, err := upPick(ref.Launcher)
	if err != nil {
		return nil, err
	}
	f, ok := l.(launch.Locator)
	if !ok {
		return nil, nil
	}
	return f.Locate(ctx, ref)
}

func (refTerms) Close(ctx context.Context, ref launch.SessionRef) error {
	l, err := upPick(ref.Launcher)
	if err != nil {
		return err
	}
	c, ok := l.(launch.Closer)
	if !ok {
		return fmt.Errorf("launcher %q cannot close sessions", ref.Launcher)
	}
	return c.Close(ctx, ref)
}

func downMain(args []string, stdout, stderr io.Writer) error {
	var yes, keepOpen bool
	fs := flag.NewFlagSet("qm down", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.BoolVar(&yes, "yes", false, "close without asking")
	fs.BoolVar(&keepOpen, "keep-open", false, "record the state; close nothing")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%w: qm down takes no arguments", errUsage)
	}
	// The project is QM_PROJECT_ID's when set, else the current folder's
	// (up.ProjectFor: from a linked worktree, the main worktree's), as for
	// qm up done and qm up handoff.
	id, where := os.Getenv("QM_PROJECT_ID"), "QM_PROJECT_ID"
	if id == "" {
		wd, err := upWorkDir()
		if err != nil {
			return err
		}
		found, ok, err := up.ProjectFor(wd)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: no qm up project here (%s)", up.ErrInput, wd)
		}
		id, where = found.ID, wd
	}
	unlock, err := up.Lock(id)
	if err != nil {
		return err
	}
	defer unlock()
	p, found, err := up.LoadID(id)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: no qm up project here (%s)", up.ErrInput, where)
	}
	recs, err := readUpRecords(stdout)
	if err != nil {
		return err
	}
	steps, _, notes, _, inc, err := upPlan(&p, recs, false, "closed")
	if err != nil {
		return err
	}
	for i := range steps {
		steps[i].MaybeLive = inc != nil && steps[i].Action != up.Leave
	}
	var listed map[string]map[string]launch.SessionRef
	if !keepOpen {
		listed = up.DownListing(context.Background(), &p, refTerms{})
	}
	shown := showDown(stdout, &p, steps, keepOpen, listed)
	printNotes(stdout, notes)
	if !keepOpen && !yes && shown > 0 {
		if !upTerminal() {
			_, _ = fmt.Fprintln(stderr, "qm down: no terminal to ask in: run qm down --yes to close these sessions")
			return errQuiet
		}
		_, _ = fmt.Fprint(stdout, "Close them? [y/N] ")
		line, _ := bufio.NewReader(upStdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			_, err := fmt.Fprintln(stdout, "nothing closed")
			return err
		}
	}
	terms := &downTerms{}
	outs := up.Down(context.Background(), &p, steps, terms, keepOpen)
	failed := false
	for _, o := range outs {
		if o.Err != nil {
			failed = true
			printErr(stderr, "qm down", o.Err)
			continue
		}
		printDown(stdout, o, keepOpen)
	}
	if terms.note != "" {
		printNotes(stdout, []string{terms.note})
	}
	if failed {
		return errQuiet
	}
	return nil
}

// printDown prints what qm down did with one session.
func printDown(stdout io.Writer, o up.Outcome, keepOpen bool) {
	switch {
	case up.IsLeftOpen(o.Ref.Warning):
		printLine(stdout, "recorded %s; its %s %s %s", o.Step.Name, o.Ref.Launcher, o.Ref.ID, o.Ref.Warning)
	case o.Step.Action == up.Skip:
		printLine(stdout, "closed %s, finished or skipped (%s %s)", o.Step.Name, o.Ref.Launcher, o.Ref.ID)
	case keepOpen:
		printLine(stdout, "recorded %s", o.Step.Name)
	case o.Ref.ID != "":
		printLine(stdout, "closed %s (%s %s)", o.Step.Name, o.Ref.Launcher, o.Ref.ID)
	default:
		printLine(stdout, "recorded %s (no terminal session to close)", o.Step.Name)
	}
}

// showDown lists what qm down records and, unless keepOpen, closes: the
// sessions qm up would run or leave, and skipped ones that still have a
// terminal session. A terminal session Down will leave open (it now runs
// something else, by listed) is marked so. It returns how many it listed.
func showDown(stdout io.Writer, p *up.Project, steps []up.Step, keepOpen bool, listed map[string]map[string]launch.SessionRef) int {
	var rows []string
	for _, s := range steps {
		ref, hasRef := p.Sessions[s.SessionID]
		if s.Action == up.Skip && (keepOpen || !hasRef) {
			continue
		}
		term := "no terminal session recorded"
		if hasRef {
			term = ref.Launcher + " " + ref.ID
		}
		note := ""
		if s.Action == up.Skip {
			note = " (skipped by qm up: closed, not recorded)"
		}
		if hasRef && !keepOpen {
			if ok, why := up.WillClose(s, ref, listed); !ok {
				note = " (" + why + ")"
			}
		}
		rows = append(rows, plain.Line(fmt.Sprintf("  %s %s: %s%s", s.Role, s.Name, term, note))+"\n")
	}
	verb := "will record and close"
	if keepOpen {
		verb = "will record (--keep-open)"
	}
	_, _ = fmt.Fprintf(stdout, "project %s (%s): %s %d session(s)\n%s", p.ID, plain.Line(p.Root), verb, len(rows), strings.Join(rows, ""))
	return len(rows)
}

// qm down finds a window that was moved only through these.
var (
	_ launch.Locator = refTerms{}
	_ launch.Locator = (*downTerms)(nil)
)
