package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
	"github.com/IZZY-Agency/quartermaster/internal/config"
	"github.com/IZZY-Agency/quartermaster/internal/declog"
	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/outcomes"
	"github.com/IZZY-Agency/quartermaster/internal/paths"
	"github.com/IZZY-Agency/quartermaster/internal/route"
	"github.com/IZZY-Agency/quartermaster/internal/up"
)

const upUsage = `usage:
  qm up [--yes] [--dry-run] [--full] [--launcher agterm|tmux] [--adopt | --adopt-coordinator ID | --new] [-- CLAUDE ARGS]
  qm up done NAME
  qm up handoff

  done NAME  mark the worker NAME finished: qm up never restores it
  handoff    print the coordinator's handoff note path (and create its
             folder), for a coordinator qm up adopted: it has no QM_HANDOFF
             until its next restart; it answers that coordinator only
  --adopt   first run: adopt the live Claude sessions in the project's
             folders without asking; the coordinator is the session running
             qm up, else the one session in the project root that no worker
             Record names
  --adopt-coordinator ID
             first run: adopt the session with this id (or a unique prefix)
             as the coordinator; implies --adopt
  --new      first run: start a new coordinator beside the live sessions and
             adopt nothing
  --         what follows is the coordinator's claude arguments, replacing the
             saved ones (leave it out to keep them); qm up sets --resume,
             --continue, --session-id, --name and --print itself, so they are
             refused
`

func init() {
	commands["up"] = command{"start this folder's project, or restore its coordinator and workers: qm up [--yes] [--dry-run] [--full]", runUp}
}

// The machine, as qm up reads it; tests replace these.
var (
	// upPick picks the launcher for a choice (agterm, tmux, auto).
	upPick = func(choice string) (launch.Launcher, error) {
		return launch.PickUp(choice, os.Getenv, agtermRunning, func(bin string) bool {
			_, err := exec.LookPath(bin)
			return err == nil
		})
	}
	upProcs              = claudecode.Procs
	upRun   up.DirRunner = up.ExecDir
	// upTerminal says whether stdin is a terminal, where qm up can ask.
	upTerminal = func() bool {
		fi, err := os.Stdin.Stat()
		return err == nil && fi.Mode()&os.ModeCharDevice != 0
	}
	upStdin io.Reader = os.Stdin
	// upGhNote says why gh cannot tell merged workers ("" when it can):
	// missing, or not signed in.
	upGhNote  = ghNote
	upWorkDir = os.Getwd
	// upAgtermPin pins an agterm session again when the launcher picked
	// this run is not agterm (a live session's recorded ref still is).
	upAgtermPin = launch.Agterm{Bin: "agtermctl", Run: launch.Exec}.Pin
	// upTmuxWindow is the tmux window holding pane (TMUX_PANE).
	// upAgtermCtl and upTmuxCtl list, move and pin terminal sessions for an
	// adoption, whatever launcher was picked.
	upAgtermCtl = launch.Agterm{Bin: "agtermctl", Run: launch.Exec}
	upTmuxCtl   = launch.Tmux{Bin: "tmux", Run: launch.Exec}
	// upTermOf reads a process's terminal session from its environment.
	upTermOf = claudecode.TermEnvOf
	// upSelfPID is the process qm up looks for its own claude session from:
	// CLAUDE_PID when it names a claude process, else qm's parent.
	upSelfPID = func(procs []claudecode.Proc) int {
		if pid, err := strconv.Atoi(os.Getenv("CLAUDE_PID")); err == nil && pid > 0 {
			for _, p := range procs {
				if _, ok := claudecode.IsClaude(p.Cmd); ok && p.PID == pid {
					return pid
				}
			}
		}
		return os.Getppid()
	}
	// upSleep waits between the checks that claude started in a reused tab.
	upSleep      = time.Sleep
	upTmuxWindow = func(pane string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		out, err := launch.Exec(ctx, "tmux", "display-message", "-p", "-t", pane, "#{window_id}")
		return strings.TrimSpace(string(out)), err
	}
)

// ghNote is the plan's note when gh cannot read pull requests: gh is
// missing, or gh auth status fails (not signed in, or GitHub unreachable).
func ghNote() string {
	if _, err := exec.LookPath("gh"); err != nil {
		return "gh missing: merged workers found by git only"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := launch.Exec(ctx, "gh", "auth", "status", "--hostname", "github.com"); err != nil {
		return "gh not signed in (gh auth status failed): merged workers found by git only"
	}
	return ""
}

// agtermRunning says whether agterm answers agtermctl tree --json: the app
// runs, though qm was started from another terminal.
func agtermRunning() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := launch.Exec(ctx, "agtermctl", "tree", "--json")
	return err == nil
}

// upOptions are qm up's flags and the coordinator arguments after "--".
type upOptions struct {
	yes, dryRun, full bool
	launcher          string
	// adopt, adoptCoordinator and newRun are the first-run choices:
	// adopt without asking (the coordinator given or found), or start a
	// new coordinator and adopt nothing.
	adopt, newRun    bool
	adoptCoordinator string
	claudeArgs       []string
	argsGiven        bool
}

// runUp runs qm up and qm up done. Exit 0 when done or nothing to do; 1 on a
// failure, no launcher, or no terminal to ask in without --yes; 2 on bad
// input.
func runUp(args []string, stdout, stderr io.Writer) int {
	if isHelp(args) {
		_, _ = fmt.Fprint(stdout, upUsage)
		return 0
	}
	var err error
	switch {
	case len(args) > 0 && args[0] == "done":
		err = upDone(args[1:], stdout)
	case len(args) > 0 && args[0] == "handoff":
		err = upHandoff(args[1:], stdout)
	default:
		err = upRunMain(args, stdout, stderr)
	}
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errUsage), errors.Is(err, up.ErrInput):
		_, _ = fmt.Fprintf(stderr, "qm up: %v\n%s", err, upUsage)
		return 2
	case errors.Is(err, errQuiet):
		return 1
	default:
		_, _ = fmt.Fprintf(stderr, "qm up: %v\n", err)
		return 1
	}
}

// errQuiet is a failure already reported to the user.
var errQuiet = errors.New("failed")

func parseUp(args []string, stderr io.Writer) (upOptions, error) {
	var o upOptions
	flags := args
	for i, a := range args {
		if a == "--" {
			flags, o.claudeArgs, o.argsGiven = args[:i], args[i+1:], true
			break
		}
	}
	fs := flag.NewFlagSet("qm up", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.BoolVar(&o.yes, "yes", false, "start without asking")
	fs.BoolVar(&o.dryRun, "dry-run", false, "print the plan; change nothing")
	fs.BoolVar(&o.full, "full", false, "resume every unfinished session in full; start none fresh")
	fs.StringVar(&o.launcher, "launcher", "", "agterm or tmux (default: the config's launcher)")
	fs.BoolVar(&o.adopt, "adopt", false, "first run: adopt the running sessions without asking")
	fs.StringVar(&o.adoptCoordinator, "adopt-coordinator", "", "first run: adopt the session with this id (or unique prefix) as the coordinator; implies --adopt")
	fs.BoolVar(&o.newRun, "new", false, "first run: start a new coordinator and adopt nothing")
	if err := parseFlags(fs, flags); err != nil {
		return o, err
	}
	if o.adoptCoordinator != "" {
		o.adopt = true
	}
	if o.newRun && o.adopt {
		return o, fmt.Errorf("%w: --new starts a new coordinator; it does not go with --adopt or --adopt-coordinator", errUsage)
	}
	if o.launcher != "" && o.launcher != "agterm" && o.launcher != "tmux" {
		return o, fmt.Errorf("%w: --launcher %q is not agterm or tmux", errUsage, o.launcher)
	}
	return o, up.CheckClaudeArgs(o.claudeArgs)
}

func upRunMain(args []string, stdout, stderr io.Writer) error {
	o, err := parseUp(args, stderr)
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	wd, err := upWorkDir()
	if err != nil {
		return err
	}
	root, err := up.Root(wd)
	if err != nil {
		return err
	}
	if !o.dryRun {
		unlock, err := up.Lock(up.ProjectID(root))
		if err != nil {
			return err
		}
		defer unlock()
	}
	p, found, err := up.Load(root)
	if err != nil {
		return err
	}
	recs, err := declog.Read(time.Time{})
	if err != nil {
		return err
	}
	choice := o.launcher
	if choice == "" {
		choice = cfg.Launcher
	}
	l, pickErr := upPick(choice)
	if pickErr != nil && !errors.Is(pickErr, launch.ErrNoLauncher) {
		return fmt.Errorf("%w: %v", errUsage, pickErr)
	}
	d := upDeps(l, exe, cfg)
	if found && (o.adopt || o.newRun) {
		return fmt.Errorf("%w: --adopt, --adopt-coordinator and --new are for a project's first run; %s has state already", errUsage, root)
	}
	if !found {
		return upFirstRun(root, recs, o, d, stdout, stderr)
	}
	return upRestore(&p, recs, o, d, pickErr, stdout, stderr)
}

// upDeps wires the launcher l (nil: none) and the real machine into up.Deps.
func upDeps(l launch.Launcher, exe string, cfg config.Config) up.Deps {
	d := up.Deps{
		Launcher: l, Exe: exe, Coordinator: cfg.Up.ClaudeArgs, Log: declog.Append,
		Now:     func() time.Time { return time.Now().UTC() },
		Self:    selfSessions(),
		Pin:     upAgtermPin,
		Trusted: trusted,
		Run:     upRun,
		Live:    upLive,
		Sleep:   upSleep,
	}
	if l == nil {
		return d
	}
	d.Lister, _ = l.(launch.Lister)
	d.Closer, _ = l.(launch.Closer)
	// The pin follows the recorded ref's launcher (Apply pins agterm refs
	// only), whatever was picked; a picked agterm's own runner is preferred.
	if a, ok := l.(launch.Agterm); ok {
		d.Pin = a.Pin
	}
	return d
}

// upLive reads the running Claude sessions now: Apply reads them again right
// before each start, and after typing into a reused tab.
func upLive() (map[string]claudecode.LiveSession, error) {
	procs, err := upProcs()
	if err != nil {
		return nil, err
	}
	dir, err := paths.ClaudeDir()
	if err != nil {
		return nil, err
	}
	return claudecode.Live(dir, procs)
}

// selfSessions are the terminal sessions qm up runs in, which stale-shell
// cleanup never closes: its agterm session and its tmux window.
func selfSessions() []string {
	var self []string
	if id := os.Getenv("AGTERM_SESSION_ID"); id != "" {
		self = append(self, id)
	}
	if pane := os.Getenv("TMUX_PANE"); pane != "" {
		if w, err := upTmuxWindow(pane); err == nil && w != "" {
			self = append(self, w)
		}
	}
	return self
}

// trusted reads Claude Code's trust of dir.
func trusted(dir string) (bool, error) {
	cj, err := paths.ClaudeJSON()
	if err != nil {
		return false, err
	}
	return claudecode.Trusted(cj, dir)
}

// trustOnce reads Claude Code's .claude.json once for a whole run
// (claudecode.ReadTrust retries a file caught mid-write). When it cannot be
// read, every folder reads as not trusted and note says why.
func trustOnce() (check func(dir string) (bool, error), note string) {
	cj, err := paths.ClaudeJSON()
	var t claudecode.Trust
	if err == nil {
		t, err = claudecode.ReadTrust(cj)
	}
	if err != nil {
		return func(string) (bool, error) { return false, nil }, "Claude Code's .claude.json not read, every folder counted as not trusted: " + err.Error()
	}
	return func(dir string) (bool, error) { return t.Trusted(dir), nil }, ""
}

// upFirstRun starts a new project's coordinator (spec 5.4).
func upFirstRun(root string, recs []route.Record, o upOptions, d up.Deps, stdout, stderr io.Writer) error {
	all, err := up.All()
	if err != nil {
		return err
	}
	if name, proot, ok := up.WorkerOf(root, all, recs); ok {
		return fmt.Errorf("this is worker %s's worktree; run qm up in %s", name, proot)
	}
	if !o.newRun {
		det, claudeDir, err := upCandidates(root, all, recs)
		if err != nil {
			return err
		}
		if note := claudecode.SessionsNote(claudeDir); note != "" {
			// Detection is blind: a live coordinator may be running here.
			return fmt.Errorf("%s; qm up cannot see which sessions run in this project, so it adopts nothing and starts no coordinator; qm up --new starts a new coordinator", note)
		}
		if len(det.Candidates) > 0 {
			return upAdopt(root, all, det, claudeDir, o, d, stdout, stderr)
		}
		if o.adopt {
			return errors.New("no running session to adopt; qm up --new starts a new coordinator")
		}
	}
	return upStartFirst(root, o, d, stdout, stderr)
}

// upLinkedWorktree is the main worktree of the git repository when root is
// one of its linked worktrees ("" and false otherwise); tests replace it.
var upLinkedWorktree = func(root string) (string, bool) {
	main, isGit := claudecode.MainWorktree(root)
	if !isGit || main == "" {
		return "", false
	}
	real := func(dir string) string {
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			return r
		}
		return filepath.Clean(dir)
	}
	if real(main) == real(root) {
		return "", false
	}
	return main, true
}

// upStartFirst starts a new project's coordinator, adopting nothing.
func upStartFirst(root string, o upOptions, d up.Deps, stdout, stderr io.Writer) error {
	if o.dryRun {
		args := o.claudeArgs
		if !o.argsGiven {
			args = d.Coordinator
		}
		_, err := fmt.Fprintf(stdout, "first run in %s: would start the coordinator in a new workspace, launcher %s, claude arguments %q\ntrust: %s\n(dry run: nothing started)\n", root, launcherNote(d.Launcher), args, trustLine(d.Trusted, root))
		return err
	}
	p, out, err := up.Start(context.Background(), root, o.claudeArgs, d)
	if errors.Is(err, launch.ErrNoLauncher) {
		_, _ = fmt.Fprintf(stdout, "no launcher (agterm or tmux) found: run this line in a terminal of your choice, then qm up again:\n%s\n", launch.Command(out.Script))
		return errQuiet
	}
	if err != nil {
		return err
	}
	warn(stderr, out)
	_, _ = fmt.Fprintf(stdout, "project %s: started coordinator %s (%s %s) in workspace %s\n", p.ID, out.Step.Name, out.Ref.Launcher, out.Ref.ID, p.Workspace)
	attachHint(stdout, d.Launcher, p.Workspace, 1)
	return nil
}

// trustLine says whether Claude Code trusts root, for a first-run dry run.
func trustLine(check func(string) (bool, error), root string) string {
	if check == nil {
		return "not checked"
	}
	ok, err := check(root)
	switch {
	case err != nil:
		return fmt.Sprintf("not read (%v): the run would stop", err)
	case !ok:
		return fmt.Sprintf("not trusted by Claude Code: run claude in %s once and accept the dialog, or the run stops", root)
	}
	return "trusted"
}

func launcherNote(l launch.Launcher) string {
	switch v := l.(type) {
	case nil:
		return "none found"
	case launch.Agterm:
		return "agterm"
	case launch.Tmux:
		if v.Inside {
			return "tmux (this session)"
		}
		return "tmux (detached)"
	}
	return fmt.Sprintf("%T", l)
}

// upRestore is a later run (spec 6): gather, decide, show the plan, ask,
// carry it out.
func upRestore(p *up.Project, recs []route.Record, o upOptions, d up.Deps, pickErr error, stdout, stderr io.Writer) error {
	if o.argsGiven && !o.dryRun {
		p.ClaudeArgs = append([]string{}, o.claudeArgs...)
	}
	steps, now, notes, err := upPlan(p, recs, o.full, "started")
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "project %s (%s), workspace %s, launcher %s\n", p.ID, p.Root, p.Workspace, launcherNote(d.Launcher))
	_, _ = fmt.Fprint(stdout, up.Render(steps, now))
	printNotes(stdout, notes)
	if pickErr == nil {
		// The stale shells closed are shown, so the answer covers them.
		d.Stale = up.PlanStale(context.Background(), p, steps, d)
		_, _ = fmt.Fprint(stdout, up.RenderStale(d.Stale))
	}
	if o.dryRun {
		_, err := fmt.Fprintln(stdout, "dry run: nothing started")
		return err
	}
	starts := up.Starts(steps)
	if pickErr != nil && starts > 0 {
		_, _ = fmt.Fprintln(stdout, "no launcher (agterm or tmux) found: qm up will write the launch scripts and record the new sessions' ids, for you to run")
	}
	if starts > 0 && !o.yes {
		if !upTerminal() {
			_, _ = fmt.Fprintln(stderr, "qm up: no terminal to ask in: run qm up --yes to start these sessions")
			return errQuiet
		}
		_, _ = fmt.Fprint(stdout, "Proceed? [y/N] ")
		line, _ := bufio.NewReader(upStdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			_, err := fmt.Fprintln(stdout, "nothing started")
			return err
		}
	}
	if o.argsGiven {
		if err := up.Save(*p); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "coordinator claude arguments replaced: %q\n", p.ClaudeArgs)
	}
	if pickErr != nil {
		if starts == 0 {
			return nil
		}
		d.Launcher = nil
	}
	d.Progress = func(line string) { _, _ = fmt.Fprintln(stdout, line) }
	outs := up.Apply(context.Background(), p, steps, d)
	if d.Launcher == nil {
		_, _ = fmt.Fprintln(stdout, "no launcher (agterm or tmux) found: run each line in a terminal of your choice, then qm up again:")
		for _, out := range outs {
			if out.Script != "" {
				_, _ = fmt.Fprintln(stdout, launch.Command(out.Script))
			}
		}
		for _, out := range outs {
			if out.Err != nil {
				_, _ = fmt.Fprintf(stderr, "qm up: %s: %v\n", out.Step.Name, out.Err)
			}
		}
		return errQuiet
	}
	failed := false
	for _, out := range outs {
		warn(stderr, out)
		if out.Err != nil {
			failed = true
			_, _ = fmt.Fprintf(stderr, "qm up: %s: %v\n", out.Step.Name, out.Err)
			continue
		}
		_, _ = fmt.Fprintln(stdout, resultLine(out))
	}
	attachHint(stdout, d.Launcher, p.Workspace, starts)
	if failed {
		return errQuiet
	}
	return nil
}

// upPlan gathers the project's coordinator and workers the way qm up reads
// them (ps, Claude Code's session files and transcripts, git, gh) and
// decides each one; now is when it decided; notes say what it could read
// only in part (spec 8). Without ps there is no duplicate guard, so a ps
// failure is an error ("nothing <nothing>" says what was not done).
func upPlan(p *up.Project, recs []route.Record, full bool, nothing string) ([]up.Step, time.Time, []string, error) {
	procs, err := upProcs()
	if err != nil {
		return nil, time.Time{}, nil, fmt.Errorf("%w; nothing %s (without ps qm cannot see which sessions run)", err, nothing)
	}
	claudeDir, err := paths.ClaudeDir()
	if err != nil {
		return nil, time.Time{}, nil, err
	}
	var notes []string
	if n := claudecode.SessionsNote(claudeDir); n != "" {
		notes = append(notes, n)
	}
	live, err := claudecode.Live(claudeDir, procs)
	if err != nil {
		return nil, time.Time{}, nil, err
	}
	cc, err := outcomes.DefaultClaude()
	if err != nil {
		return nil, time.Time{}, nil, err
	}
	handoff, err := up.HandoffDir()
	if err != nil {
		return nil, time.Time{}, nil, err
	}
	check, n := trustOnce()
	if n != "" {
		notes = append(notes, n)
	}
	if n := upGhNote(); n != "" {
		notes = append(notes, n)
	}
	now := time.Now().UTC()
	cands, err := up.Gather(p, recs, up.Env{
		Now: now, Live: live, TranscriptPath: cc.TranscriptPath, Trusted: check,
		Finished: up.Finished(*p, upRun), HandoffDir: handoff, Run: upRun,
	})
	if err != nil {
		return nil, time.Time{}, nil, err
	}
	return up.Plan(cands, full, now), now, notes, nil
}

// printNotes prints the plan's notes, one per line.
func printNotes(stdout io.Writer, notes []string) {
	for _, n := range notes {
		_, _ = fmt.Fprintf(stdout, "note: %s\n", n)
	}
}

// attachHint prints how to reach sessions started in tmux: attach to the
// detached tmux session, or, inside tmux, switch to the project's one.
func attachHint(stdout io.Writer, l launch.Launcher, workspace string, started int) {
	t, ok := l.(launch.Tmux)
	if !ok || started == 0 {
		return
	}
	if t.Inside {
		_, _ = fmt.Fprintf(stdout, "switch: tmux switch-client -t %s\n", launch.Quote("="+workspace))
		return
	}
	_, _ = fmt.Fprintf(stdout, "attach: tmux attach -t %s\n", launch.Quote("="+workspace))
}

// warn prints a session's warning (a failed pin, a stale shell left open).
func warn(stderr io.Writer, out up.Outcome) {
	if out.Ref.Warning != "" {
		_, _ = fmt.Fprintf(stderr, "qm up: warning: %s: %s\n", out.Step.Name, out.Ref.Warning)
	}
}

// resultLine says what qm up did with one session.
func resultLine(out up.Outcome) string {
	s := out.Step
	switch s.Action {
	case up.Leave:
		return fmt.Sprintf("left %s running", s.Name)
	case up.Skip:
		return fmt.Sprintf("skipped %s: %s", s.Name, s.Why)
	case up.Fresh:
		return fmt.Sprintf("started %s fresh as %s (%s %s)", s.Name, out.NewSessionID, out.Ref.Launcher, out.Ref.ID)
	}
	return fmt.Sprintf("resumed %s (%s %s)", s.Name, out.Ref.Launcher, out.Ref.ID)
}

// doneCandidates are p's sessions as qm up's Gather shows them, read with
// only what picks the session (the running processes, when ps answers, and
// the transcripts): a worker whose fresh restart never ran shows as the
// session restored in its place. p itself is not changed.
func doneCandidates(p up.Project, recs []route.Record) ([]up.Candidate, error) {
	var live map[string]claudecode.LiveSession
	if procs, err := upProcs(); err == nil {
		if dir, err := paths.ClaudeDir(); err == nil {
			live, _ = claudecode.Live(dir, procs)
		}
	}
	cc, err := outcomes.DefaultClaude()
	if err != nil {
		return nil, err
	}
	p.Sessions = maps.Clone(p.Sessions)
	return up.Gather(&p, recs, up.Env{Live: live, TranscriptPath: cc.TranscriptPath})
}

// upDone runs qm up done NAME: the worker is finished and never restored.
// The project is QM_PROJECT_ID's when set (a coordinator whose shell sits
// in a worker's worktree), else the current folder's.
func upDone(args []string, stdout io.Writer) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("%w: qm up done takes one worker name", errUsage)
	}
	id, where := os.Getenv("QM_PROJECT_ID"), "QM_PROJECT_ID"
	if id == "" {
		// From a linked worktree of the project the main worktree's state
		// answers (up.ProjectFor).
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
		id = found.ID
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
	recs, err := declog.Read(time.Time{})
	if err != nil {
		return err
	}
	cands, err := doneCandidates(p, recs)
	if err != nil {
		return err
	}
	sid, err := up.MarkDone(&p, cands, args[0], time.Now().UTC())
	if err != nil {
		return err
	}
	if err := up.Save(p); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "marked %s done (%s): qm up will not restore it\n", args[0], sid)
	return err
}

// upCandidates detects the live sessions a first run in root may adopt
// (adopt design 4.1), and returns the Claude folder it read them from.
// Without ps it cannot tell, so a ps failure stops the run.
func upCandidates(root string, all []up.Project, recs []route.Record) (up.Detected, string, error) {
	procs, err := upProcs()
	if err != nil {
		return up.Detected{}, "", fmt.Errorf("%w; nothing started (without ps qm cannot see the running sessions it could adopt; qm up --new starts a new coordinator)", err)
	}
	claudeDir, err := paths.ClaudeDir()
	if err != nil {
		return up.Detected{}, "", err
	}
	live, err := claudecode.Live(claudeDir, procs)
	if err != nil {
		return up.Detected{}, "", err
	}
	cc, err := outcomes.DefaultClaude()
	if err != nil {
		return up.Detected{}, "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	det := up.Detect(root, all, recs, up.DetectEnv{
		Procs: procs, Live: live, Worktrees: up.Worktrees(ctx, root, upRun),
		TranscriptPath: cc.TranscriptPath, Run: upRun, SelfPID: upSelfPID(procs),
	})
	// The tab names are for the list only; without agterm there are none.
	if len(det.Candidates) > 0 {
		if wss, err := upAgtermCtl.Workspaces(ctx); err == nil {
			up.LabelTabs(det.Candidates, wss, upTermOf)
		}
	}
	return det, claudeDir, nil
}

// upAdoptDeps wires the machine into up.AdoptDeps: agterm and tmux through
// their own control commands, whatever launcher was picked.
func upAdoptDeps(d up.Deps, claudeDir string) up.AdoptDeps {
	a, t := upAgtermCtl, upTmuxCtl
	return up.AdoptDeps{
		Workspaces: a.Workspaces, Move: a.Move, Pin: a.Pin, PaneOf: t.PaneOf, TermOf: upTermOf,
		WindowPanes: func(ctx context.Context, windowID string) (int, error) {
			out, err := t.Run(ctx, t.Bin, "display-message", "-p", "-t", windowID, "#{window_panes}")
			if err != nil {
				return 0, fmt.Errorf("tmux: %w", err)
			}
			return strconv.Atoi(strings.TrimSpace(string(out)))
		},
		Live: func() (map[string]claudecode.LiveSession, error) {
			procs, err := upProcs()
			if err != nil {
				return nil, err
			}
			return claudecode.Live(claudeDir, procs)
		},
		Trusted: d.Trusted, Log: d.Log, Now: d.Now, Config: d.Coordinator,
	}
}

// upAdopt is a first run with live sessions in the project's folders (adopt
// design 4.1): --adopt picks without asking; --yes or no terminal refuses;
// a terminal asks. The project lock, held by the caller, covers the
// questions.
func upAdopt(root string, all []up.Project, det up.Detected, claudeDir string, o upOptions, d up.Deps, stdout, stderr io.Writer) error {
	cands := det.Candidates
	if main, ok := upLinkedWorktree(root); ok && o.adoptCoordinator == "" {
		// A coordinator whose shell sits in a worker's worktree would make
		// that worker the coordinator of a project of its own.
		return fmt.Errorf("%s is a linked worktree of %s: run qm up in %s; to adopt here all the same, name the coordinator with qm up --adopt-coordinator ID (qm up --new starts a new coordinator here)", root, main, main)
	}
	rows := up.RenderCandidates(cands, time.Now().UTC())
	if o.dryRun && !o.adopt {
		would := "qm up would ask which to adopt as the coordinator and the workers (qm up --adopt picks without asking; qm up --new starts a new coordinator beside them)"
		if o.yes || !upTerminal() {
			would = "qm up would refuse: adopt them with qm up --adopt, or start a new coordinator beside them with qm up --new"
		}
		_, err := fmt.Fprintf(stdout, "first run in %s: live Claude sessions in the project's folders:\n%s%s\n(dry run: nothing adopted)\n", root, rows, would)
		return err
	}
	var ch up.Choice
	var in *bufio.Reader
	var err error
	switch {
	case o.adopt:
		if ch, err = up.ChooseAuto(det, root, o.adoptCoordinator); err != nil {
			return err
		}
	case o.yes || !upTerminal():
		_, _ = fmt.Fprintf(stderr, "qm up: live Claude sessions in the project's folders:\n%sadopt them with qm up --adopt, or start a new coordinator beside them with qm up --new\n", rows)
		return errQuiet
	default:
		_, _ = fmt.Fprintf(stdout, "live Claude sessions in the project's folders:\n%s", rows)
		in = bufio.NewReader(upStdin)
		if ch, err = up.AskAdopt(cands, in, stdout); err != nil {
			return err
		}
		if ch.New {
			return upStartFirst(root, o, d, stdout, stderr)
		}
	}
	ad := upAdoptDeps(d, claudeDir)
	ap, err := up.PlanAdopt(context.Background(), root, all, cands, ch, o.claudeArgs, o.argsGiven, ad)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprint(stdout, up.RenderAdopt(ap))
	if o.dryRun {
		_, err := fmt.Fprintln(stdout, "(dry run: nothing adopted)")
		return err
	}
	if in != nil {
		_, _ = fmt.Fprint(stdout, "Proceed? [y/N] ")
		line, _ := in.ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			_, err := fmt.Fprintln(stdout, "nothing adopted")
			return err
		}
	}
	planned := len(ap.Notes)
	if err := up.Adopt(context.Background(), &ap, ad); err != nil {
		return err
	}
	printNotes(stdout, ap.Notes[planned:])
	_, err = fmt.Fprintf(stdout, "project %s: adopted coordinator %s and %d worker(s) in workspace %s; nothing restarted\n", ap.Project.ID, ap.Project.Coordinator.Name, len(ap.Workers), ap.Project.Workspace)
	return err
}

// upHandoff runs qm up handoff: it prints the handoff note path of the
// project's coordinator (QM_PROJECT_ID's project, else the current
// folder's) and creates its folder, for a coordinator qm up adopted, which
// has no QM_HANDOFF until its next restart.
func upHandoff(args []string, stdout io.Writer) error {
	if len(args) != 0 {
		return fmt.Errorf("%w: qm up handoff takes no arguments", errUsage)
	}
	var p up.Project
	if id := os.Getenv("QM_PROJECT_ID"); id != "" {
		found, ok, err := up.LoadID(id)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: no qm up project here (QM_PROJECT_ID %s)", up.ErrInput, id)
		}
		p = found
	} else {
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
		p = found
	}
	// Only the coordinator writes its handoff note: a worker or a side
	// session with the coordinator plugin would overwrite it.
	ids, err := callerSessions()
	if err != nil {
		return fmt.Errorf("the calling session not read: %w", err)
	}
	if p.Coordinator.ID == "" || !slices.Contains(ids, p.Coordinator.ID) {
		return fmt.Errorf("%w: not this project's coordinator: qm up handoff is for the session qm up adopted as %s's coordinator", up.ErrInput, p.ID)
	}
	path, err := up.HandoffPath(p.ID, p.Coordinator.Name)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, path)
	return err
}
