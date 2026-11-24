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
	"sync"
	"syscall"
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
  qm up [--yes] [--dry-run] [--full] [--launcher agterm|tmux] [--here | --new-tab] [--adopt | --adopt-coordinator ID | --new] [-- CLAUDE ARGS]
  qm up done NAME
  qm up handoff

  done NAME  mark the worker NAME finished: qm up never restores it
  handoff    print the coordinator's handoff note path (and create its
             folder), for a coordinator qm up adopted: it has no QM_HANDOFF
             until its next restart; it answers that coordinator only
  --adopt    first run: adopt the live Claude sessions in the project's
             folders without asking; the coordinator is the session running
             qm up, else the one session in the project root that no worker
             Record names. On a project with state: adopt running sessions
             it does not hold as workers (it asks which; with --yes or no
             terminal only the ones a worker Record names), restart nothing
  --adopt-coordinator ID
             first run: adopt the session with this id (or a unique prefix)
             as the coordinator; implies --adopt
  --new      first run: start a new coordinator beside the live sessions and
             adopt nothing
  --here     start the coordinator in this tab, once the workers are started
             and the state saved: qm is replaced by it, and qm down closes
             this tab last. Needs a terminal; only when a coordinator starts
             (a first run, or a restore that resumes or restarts it). The
             config's up.coordinator_tab "here" does the same
  --new-tab  start the coordinator in a new tab (the default), whatever the
             config says
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
	upProcs = claudecode.Procs
	// upProcsCtx lists the processes for a detection, ps stopped at its
	// deadline.
	upProcsCtx              = claudecode.ProcsCtx
	upRun      up.DirRunner = up.ExecDir
	// upTerminal says whether stdin is a terminal, where qm up can ask.
	upTerminal = func() bool {
		return isTTY(os.Stdin.Fd())
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
	// upPaneRuns says whether tmux target's pane runs process pid in its
	// foreground: the pane's tty is pid's, and pid's process group is that
	// tty's foreground group (a job started there, or pid suspended, is not).
	upPaneRuns = func(ctx context.Context, target string, pid int) (bool, error) {
		t := upTmuxCtl
		out, err := t.Run(ctx, t.Bin, "display-message", "-p", "-t", target, "#{pane_tty}")
		if err != nil {
			return false, fmt.Errorf("tmux: %w", err)
		}
		tty, fg, err := claudecode.ForegroundOf(ctx, pid)
		if err != nil {
			return false, err
		}
		pane := strings.TrimPrefix(strings.TrimSpace(string(out)), "/dev/")
		return fg && tty != "" && pane == tty, nil
	}
	// upTermOf reads a process's terminal session from its environment.
	upTermOf = claudecode.TermEnvOfCtx
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
	upSleep = time.Sleep
	// upHereTerminal says whether standard input and output are both a
	// terminal, which a coordinator started in this tab needs.
	upHereTerminal = func() bool {
		return isTTY(os.Stdin.Fd()) && isTTY(os.Stdout.Fd())
	}
	// upExec replaces qm with the coordinator's launch script: no process of
	// qm is left behind.
	upExec = func(script string) error {
		return syscall.Exec("/bin/sh", []string{"/bin/sh", script}, os.Environ())
	}
	// upEnv reads qm up's environment (AGTERM_SESSION_ID, TMUX_PANE).
	upEnv = os.Getenv
	// upArgv is qm's own command line, which the tab qm up runs in shows
	// as its foreground.
	upArgv       = os.Args
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
	// coordinatorGiven says --adopt-coordinator was on the command line,
	// whatever its value: it is a first-run flag.
	coordinatorGiven bool
	claudeArgs       []string
	argsGiven        bool
	// hereFlag and newTab are --here and --new-tab. here says the
	// coordinator starts in this tab (the flags, else the config); hereGiven
	// says --here asked for it, so a run that cannot is a usage error, not a
	// fallback. hereNote says why the config's "here" fell back to a new tab.
	hereFlag, newTab, here, hereGiven bool
	hereNote                          string
	// pending is the coordinator to run in place of qm once the run is done
	// and its locks released (an empty script for none).
	pending *upPending
}

// upPending is a coordinator prepared to start in this tab: its launch
// script, and the project root, session id and terminal session recorded for
// it, which a failed exec takes back.
type upPending struct {
	script, root, sessionID string
	ref                     launch.SessionRef
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
	fs.BoolVar(&o.adopt, "adopt", false, "adopt the running sessions as the project's first run does, without asking; on a project with state, adopt workers only (asks which)")
	fs.StringVar(&o.adoptCoordinator, "adopt-coordinator", "", "first run: adopt the session with this id (or unique prefix) as the coordinator; implies --adopt")
	fs.BoolVar(&o.newRun, "new", false, "first run: start a new coordinator and adopt nothing")
	fs.BoolVar(&o.hereFlag, "here", false, "start the coordinator in this tab, after the workers")
	fs.BoolVar(&o.newTab, "new-tab", false, "start the coordinator in a new tab (the default)")
	if err := parseFlags(fs, flags); err != nil {
		return o, err
	}
	fs.Visit(func(f *flag.Flag) { o.coordinatorGiven = o.coordinatorGiven || f.Name == "adopt-coordinator" })
	if o.coordinatorGiven && o.adoptCoordinator == "" {
		return o, fmt.Errorf("%w: --adopt-coordinator takes a session id (or a unique prefix), and got none", errUsage)
	}
	if o.adoptCoordinator != "" {
		o.adopt = true
	}
	if o.newRun && o.adopt {
		return o, fmt.Errorf("%w: --new starts a new coordinator; it does not go with --adopt or --adopt-coordinator", errUsage)
	}
	if o.hereFlag && o.newTab {
		return o, fmt.Errorf("%w: --here and --new-tab are opposites; give one", errUsage)
	}
	if o.hereFlag && o.adopt {
		return o, fmt.Errorf("%w: --here starts a coordinator in this tab; --adopt and --adopt-coordinator start none", errUsage)
	}
	if o.launcher != "" && o.launcher != "agterm" && o.launcher != "tmux" {
		return o, fmt.Errorf("%w: --launcher %q is not agterm or tmux", errUsage, o.launcher)
	}
	return o, up.CheckClaudeArgs(o.claudeArgs)
}

// setPending leaves the coordinator of out, prepared in project root, to
// run in place of qm once the run is done.
func (o upOptions) setPending(root string, out up.Outcome) {
	if o.pending == nil {
		return
	}
	sid := out.Step.SessionID
	if out.NewSessionID != "" {
		sid = out.NewSessionID
	}
	*o.pending = upPending{script: out.Script, root: root, sessionID: sid, ref: out.Ref}
}

// upRunMain runs qm up; when the run leaves a coordinator to start in this
// tab (o.pending), qm is replaced by its script once the run is done and its
// locks released, and nothing more is printed.
func upRunMain(args []string, stdout, stderr io.Writer) error {
	o, err := parseUp(args, stderr)
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := upResolveHere(&o, cfg); err != nil {
		return err
	}
	o.pending = new(upPending)
	err = upRunLocked(o, cfg, stdout, stderr)
	pd := *o.pending
	if pd.script == "" {
		return err
	}
	if err := upExec(pd.script); err != nil {
		msg := fmt.Sprintf("the coordinator was not started in this tab: %v; run: %s", err, launch.Command(pd.script))
		if ferr := forgetHere(pd); ferr != nil {
			msg += fmt.Sprintf("; its tab (%s %s) is still recorded (%v): qm down may close it", pd.ref.Launcher, pd.ref.ID, ferr)
		}
		return errors.New(msg)
	}
	return nil
}

// forgetHere takes back the terminal session recorded for a coordinator
// whose exec failed, so qm down never closes a tab that never ran it. It
// changes the saved state only while it still records that session there.
func forgetHere(pd upPending) error {
	if pd.ref.ID == "" {
		return nil
	}
	unlock, err := up.Lock(up.ProjectID(pd.root))
	if err != nil {
		return err
	}
	defer unlock()
	p, ok, err := up.Load(pd.root)
	if err != nil || !ok {
		return err
	}
	if ref, ok := p.Sessions[pd.sessionID]; ok && ref.Launcher == pd.ref.Launcher && ref.ID == pd.ref.ID {
		delete(p.Sessions, pd.sessionID)
		return up.Save(p)
	}
	return nil
}

// upResolveHere sets o.here from --here, --new-tab and the config's
// up.coordinator_tab (the flags win). The tab must be a terminal: without
// one, --here is a usage error and the config's "here" falls back to a new
// tab with a note.
func upResolveHere(o *upOptions, cfg config.Config) error {
	tab, err := cfg.CoordinatorTab()
	if err != nil {
		return err
	}
	switch {
	case o.hereFlag:
		o.here, o.hereGiven = true, true
	case o.newTab:
		o.here = false
	default:
		o.here = tab == "here" && !o.adopt
	}
	if o.here && !upHereTerminal() {
		if o.hereGiven {
			return fmt.Errorf("%w: --here needs a terminal on standard input and output, to start the coordinator in", errUsage)
		}
		o.here = false
		o.hereNote = "up.coordinator_tab is here, but standard input or output is not a terminal: the coordinator starts in a new tab"
	}
	return nil
}

// upRunLocked is qm up's run under the project's lock, released when it
// returns.
func upRunLocked(o upOptions, cfg config.Config, stdout, stderr io.Writer) error {
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
	recs, err := readUpRecords(stdout)
	if err != nil {
		return err
	}
	choice := o.launcher
	if choice == "" {
		choice = cfg.Launcher
	}
	if found && (o.coordinatorGiven || o.newRun) {
		return fmt.Errorf("%w: --adopt-coordinator and --new are for a project's first run; %s has state already", errUsage, root)
	}
	if found && o.adopt {
		// Workers only: nothing is started, so no launcher is picked. --here
		// with --adopt is refused when parsed.
		if o.argsGiven || o.full {
			return fmt.Errorf("%w: on a project with state, --adopt adopts workers only: it does not go with --full or coordinator arguments after --", errUsage)
		}
		return upAdoptLater(&p, recs, o, upDeps(nil, exe, cfg), stdout, stderr)
	}
	l, pickErr := upPick(choice)
	if pickErr != nil && !errors.Is(pickErr, launch.ErrNoLauncher) {
		return fmt.Errorf("%w: %v", errUsage, pickErr)
	}
	d := upDeps(l, exe, cfg)
	if o.here {
		d.Here = upHere(l)
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
		MCPConfigs: slices.Collect(maps.Values(cfg.ProjectMCP)),
		Now:        func() time.Time { return time.Now().UTC() },
		Self:       selfSessions(),
		Pin:        upAgtermPin,
		Trusted:    trusted,
		Run:        upRun,
		Live:       upLive,
		Sleep:      upSleep,
		// A worker's permission is capped at the running coordinator's own
		// flags, read off its process right before the worker starts.
		CoordinatorArgs: upCoordinatorArgs,
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

// upHere is the tab qm up runs in, as a coordinator started in it records
// it: the launcher picked (l, nil for none) settles agterm or tmux when both
// are in the environment.
func upHere(l launch.Launcher) func(context.Context, *up.Project, []string, bool) (launch.SessionRef, string) {
	a, t := upAgtermCtl, upTmuxCtl
	prefer := ""
	switch l.(type) {
	case launch.Agterm:
		prefer = "agterm"
	case launch.Tmux:
		prefer = "tmux"
	}
	ad := upAdoptDeps(up.Deps{}, "")
	return up.HereTerm{
		AgtermSession: upEnv("AGTERM_SESSION_ID"), TmuxPane: upEnv("TMUX_PANE"), Prefer: prefer,
		Argv:       strings.Join(upArgv, " "),
		Cwd:        upHereCwd(),
		Workspaces: a.Workspaces, NewWorkspace: a.WorkspaceNew, Move: a.Move, Pin: a.Pin,
		PaneOf: t.PaneOf, WindowPanes: ad.WindowPanes,
		// The pane's foreground is qm itself: its tty is qm's and qm's
		// process group is the tty's foreground group.
		PaneRuns: func(ctx context.Context, pane string) (bool, error) {
			return upPaneRuns(ctx, pane, os.Getpid())
		},
		RenameWindow: func(ctx context.Context, windowID, name string) error {
			if _, err := t.Run(ctx, t.Bin, "rename-window", "-t", windowID, "--", name); err != nil {
				return fmt.Errorf("tmux: %w", err)
			}
			return nil
		},
		All:     up.All,
		Records: func() ([]route.Record, error) { return readUpRecords(io.Discard) },
	}.Resolve
}

// upHereCwd is qm's working folder ("" when unread).
func upHereCwd() string {
	wd, err := upWorkDir()
	if err != nil {
		return ""
	}
	return wd
}

// whereLine says where the coordinator starts: in this tab or a new one.
func whereLine(o upOptions, would bool) string {
	verb := "will start"
	if would {
		verb = "would start"
	}
	if o.here {
		return "the coordinator " + verb + " in this tab, once the rest is done"
	}
	return "the coordinator " + verb + " in a new tab"
}

// restoreWhere is whereLine for a restore: the coordinator's earlier tab
// when the plan reuses it, its launch line when there is no launcher.
func restoreWhere(o upOptions, steps []up.Step, sp *up.StalePlan, noLauncher bool) string {
	verb := "will start"
	if o.dryRun {
		verb = "would start"
	}
	switch {
	case o.here:
		return whereLine(o, o.dryRun)
	case noLauncher:
		return "the coordinator " + verb + " from its launch line, run in a terminal of your choice"
	}
	if sp != nil {
		for _, s := range steps {
			if ref, ok := sp.Reuse[s.SessionID]; ok && s.Role == up.RoleCoordinator {
				return fmt.Sprintf("the coordinator %s in its earlier tab (%s %s), reused", verb, ref.Launcher, ref.ID)
			}
		}
	}
	return whereLine(o, o.dryRun)
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

// upCoordinatorArgs reads the permission flags of running session id off
// its process (up.LiveArgs); live is false when it does not run.
func upCoordinatorArgs(id string) (args []string, live bool, err error) {
	procs, err := upProcs()
	if err != nil {
		return nil, false, err
	}
	dir, err := paths.ClaudeDir()
	if err != nil {
		return nil, false, err
	}
	sessions, err := claudecode.Live(dir, procs)
	if err != nil {
		return nil, false, err
	}
	return up.LiveArgs(id, sessions, procs)
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
	if o.hereNote != "" {
		_, _ = fmt.Fprintf(stdout, "note: %s\n", o.hereNote)
	}
	if o.dryRun {
		args := o.claudeArgs
		if !o.argsGiven {
			args = d.Coordinator
		}
		where, line := "in a new workspace", whereLine(o, true)
		switch {
		case o.here:
			where = "in the project's workspace (under tmux, this tab's tmux session)"
		case d.Launcher == nil:
			line = "the coordinator would start from its launch line, run in a terminal of your choice"
		}
		_, err := fmt.Fprintf(stdout, "first run in %s: would start the coordinator %s, launcher %s, claude arguments %q\n%s\ntrust: %s\n(dry run: nothing started)\n", root, where, launcherNote(d.Launcher), args, line, trustLine(d.Trusted, root))
		return err
	}
	p, out, err := up.Start(context.Background(), root, o.claudeArgs, d)
	if err == nil && out.Here {
		warn(stderr, out)
		_, _ = fmt.Fprintf(stdout, "project %s: starting coordinator %s in this tab (%s) in workspace %s\n", p.ID, out.Step.Name, hereRef(out.Ref), p.Workspace)
		o.setPending(p.Root, out)
		return nil
	}
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

// hereRef names the terminal session recorded for a coordinator started in
// this tab.
func hereRef(ref launch.SessionRef) string {
	if ref.ID == "" {
		return "no terminal session recorded"
	}
	return ref.Launcher + " " + ref.ID
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

	steps, now, notes, procs, err := upPlan(p, recs, o.full, "started")
	if err != nil {
		return err
	}
	coordStarts, why := false, "the plan has no coordinator"
	for _, s := range steps {
		switch {
		case s.Role != up.RoleCoordinator:
		case s.Action == up.Resume || s.Action == up.Fresh:
			coordStarts = true
		case s.Action == up.Skip:
			why = "it is skipped: " + s.Why
		default:
			why = "it runs already and is left as it is"
		}
	}
	if !coordStarts && o.hereGiven {
		return fmt.Errorf("%w: --here starts the coordinator in this tab, but no coordinator starts: %s", errUsage, why)
	}
	if !coordStarts {
		// The config's "here" is not applied: no coordinator starts.
		o.here, d.Here = false, nil
	}
	_, _ = fmt.Fprintf(stdout, "project %s (%s), workspace %s, launcher %s\n", p.ID, p.Root, p.Workspace, launcherNote(d.Launcher))
	_, _ = fmt.Fprint(stdout, up.Render(steps, now))
	printNotes(stdout, notes)
	if n := upUnheldNote(p, recs, procs); n != "" {
		_, _ = fmt.Fprintf(stdout, "note: %s\n", n)
	}
	if pickErr == nil {
		// The stale shells closed are shown, so the answer covers them.
		d.Stale = up.PlanStale(context.Background(), p, steps, d)
		_, _ = fmt.Fprint(stdout, up.RenderStale(d.Stale))
	}
	if coordStarts {
		_, _ = fmt.Fprintln(stdout, restoreWhere(o, steps, d.Stale, pickErr != nil))
		if o.hereNote != "" {
			_, _ = fmt.Fprintf(stdout, "note: %s\n", o.hereNote)
		}
	}
	if o.dryRun {
		_, err := fmt.Fprintln(stdout, "dry run: nothing started")
		return err
	}
	starts := up.Starts(steps)
	if starts > 0 && !o.argsGiven {
		// Saved before qm refused a lone --: a coordinator would not start
		// as qm means, and the workers' cap would read past it.
		if err := up.CheckClaudeArgs(p.ClaudeArgs); err != nil {
			return fmt.Errorf("the saved coordinator arguments: %w (replace them with qm up -- ARGS)", err)
		}
	}
	launched := starts
	if o.here {
		launched-- // the coordinator starts in this tab
	}
	if pickErr != nil && launched > 0 {
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
	// A coordinator to start in this tab runs once qm returns (o.pending),
	// whatever happened to the workers; one whose step failed does not.
	for _, out := range outs {
		if out.Here && out.Err == nil {
			o.setPending(p.Root, out)
		}
	}
	if d.Launcher == nil {
		printed := false
		for _, out := range outs {
			if out.Script != "" && !out.Here {
				if !printed {
					_, _ = fmt.Fprintln(stdout, "no launcher (agterm or tmux) found: run each line in a terminal of your choice, then qm up again:")
					printed = true
				}
				_, _ = fmt.Fprintln(stdout, launch.Command(out.Script))
			}
		}
		for _, out := range outs {
			if out.Here && out.Err == nil {
				warn(stderr, out)
				_, _ = fmt.Fprintln(stdout, resultLine(out))
			}
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
func upPlan(p *up.Project, recs []route.Record, full bool, nothing string) ([]up.Step, time.Time, []string, []claudecode.Proc, error) {
	procs, err := upProcs()
	if err != nil {
		return nil, time.Time{}, nil, nil, fmt.Errorf("%w; nothing %s (without ps qm cannot see which sessions run)", err, nothing)
	}
	claudeDir, err := paths.ClaudeDir()
	if err != nil {
		return nil, time.Time{}, nil, nil, err
	}
	var notes []string
	if n := claudecode.SessionsNote(claudeDir); n != "" {
		notes = append(notes, n)
	}
	live, err := claudecode.Live(claudeDir, procs)
	if err != nil {
		return nil, time.Time{}, nil, nil, err
	}
	cc, err := outcomes.DefaultClaude()
	if err != nil {
		return nil, time.Time{}, nil, nil, err
	}
	handoff, err := up.HandoffDir()
	if err != nil {
		return nil, time.Time{}, nil, nil, err
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
		return nil, time.Time{}, nil, nil, err
	}
	return up.Plan(cands, full, now), now, notes, procs, nil
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
	if out.Here {
		how := "resuming"
		if s.Action == up.Fresh {
			how = "starting fresh as " + out.NewSessionID + ":"
		}
		return fmt.Sprintf("%s %s in this tab (%s)", how, s.Name, hereRef(out.Ref))
	}
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
	recs, err := readUpRecords(stdout)
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
	ctx, cancel := context.WithTimeout(context.Background(), upDetectTimeout)
	defer cancel()
	det, claudeDir, err := upDetect(ctx, root, all, recs, nil, nil, false, "nothing started (without ps qm cannot see the running sessions it could adopt; qm up --new starts a new coordinator)")
	if errors.Is(err, errDetectIncomplete) {
		return det, claudeDir, fmt.Errorf("%w: nothing started (a running coordinator may be among the sessions not read); run qm up again, or qm up --new starts a new coordinator", err)
	}
	if err != nil {
		return det, claudeDir, err
	}
	upLabelTabs(ctx, det.Candidates)
	return det, claudeDir, nil
}

// upDetect reads the machine and detects the candidates of the project at
// root, all under ctx's deadline; held are the sessions the project holds
// already (nil on a first run). procs is the process list to use (nil: read
// it); quick reads only what makes a session a candidate. psFail says what
// was not done when ps fails, for the error.
func upDetect(ctx context.Context, root string, all []up.Project, recs []route.Record, held map[string]bool, procs []claudecode.Proc, quick bool, psFail string) (up.Detected, string, error) {
	claudeDir, err := paths.ClaudeDir()
	if err != nil {
		return up.Detected{}, "", err
	}
	if procs == nil {
		// A ps that stalls stops at the deadline: the project lock is never
		// held past it.
		if procs, err = upProcsCtx(ctx); err != nil {
			if ctx.Err() != nil {
				return up.Detected{}, claudeDir, errDetectIncomplete
			}
			return up.Detected{}, "", fmt.Errorf("%w; %s", err, psFail)
		}
	}
	live, err := claudecode.LiveCtx(ctx, claudeDir, procs)
	if err != nil && ctx.Err() != nil {
		return up.Detected{}, claudeDir, errDetectIncomplete
	}
	if err != nil {
		return up.Detected{}, "", err
	}
	cc, err := outcomes.DefaultClaude()
	if err != nil {
		return up.Detected{}, "", err
	}
	det := up.Detect(root, all, recs, up.DetectEnv{
		Procs: procs, Live: live, Worktrees: up.Worktrees(ctx, root, upRun),
		TranscriptPath: cc.TranscriptPath, Run: upRun, SelfPID: upSelfPID(procs), Held: held,
		Ctx: ctx, Quick: quick,
	})
	if ctx.Err() != nil {
		// A probe cut short leaves the list empty or partial: never act on it.
		return up.Detected{}, claudeDir, errDetectIncomplete
	}
	return det, claudeDir, nil
}

// upDetectTimeout bounds a first run's and a later adoption's detection,
// and the tab labels that follow it.
var upDetectTimeout = 10 * time.Second

// errDetectIncomplete: the detection's deadline passed before it read every
// running session, so its candidate list may be empty or partial.
var errDetectIncomplete = errors.New("the running sessions in the project's folders were not all read before the deadline")

// upLabelTabs names the tabs of the candidates, for the printed list only;
// without agterm there are none. It runs under the detection's deadline.
func upLabelTabs(ctx context.Context, cands []up.AdoptCandidate) {
	if len(cands) == 0 {
		return
	}
	if wss, err := upAgtermCtl.Workspaces(ctx); err == nil {
		up.LabelTabs(ctx, cands, wss, upTermOf)
	}
}

// upAdoptLater is qm up --adopt on a project with state (adopt design 4.8):
// it adopts workers only and restores nothing. The project lock, held by the
// caller unless this is a dry run, covers the questions.
func upAdoptLater(p *up.Project, recs []route.Record, o upOptions, d up.Deps, stdout, stderr io.Writer) error {
	all, err := up.All()
	if err != nil {
		return err
	}
	dctx, cancel := context.WithTimeout(context.Background(), upDetectTimeout)
	defer cancel()
	det, claudeDir, err := upDetect(dctx, p.Root, all, recs, up.Held(*p, recs), nil, false, "nothing adopted (without ps qm cannot see the running sessions it could adopt)")
	if errors.Is(err, errDetectIncomplete) {
		return fmt.Errorf("%w: nothing adopted; run qm up --adopt again", err)
	}
	if err != nil {
		return err
	}
	if note := claudecode.SessionsNote(claudeDir); note != "" {
		return fmt.Errorf("%s; qm up cannot see which sessions run in this project, so it adopts nothing", note)
	}
	cands := det.Candidates
	if len(cands) == 0 {
		return errors.New("no running session to adopt")
	}
	interactive := !o.yes && upTerminal()
	var ids []string
	if !interactive {
		ids = up.RecordedWorkers(cands)
	}
	if interactive || len(ids) == 0 {
		upLabelTabs(dctx, cands)
	}
	rows := up.RenderCandidates(cands, time.Now().UTC())
	const hint = "run qm up --adopt in a terminal to pick it"
	if !interactive && len(ids) == 0 {
		_, _ = fmt.Fprintf(stderr, "qm up: running Claude sessions in the project's folders that the project does not hold:\n%s", rows)
		if slices.ContainsFunc(cands, func(c up.AdoptCandidate) bool { return !c.Subfolder }) {
			_, _ = fmt.Fprintf(stderr, "%s\n", hint)
		}
		return errors.New("no running session to adopt")
	}
	if interactive && o.dryRun {
		_, err := fmt.Fprintf(stdout, "running Claude sessions in the project's folders that the project does not hold:\n%sqm up --adopt would ask which of these to adopt as workers\n(dry run: nothing adopted)\n", rows)
		return err
	}
	var in *bufio.Reader
	if interactive {
		_, _ = fmt.Fprintf(stdout, "running Claude sessions in the project's folders that the project does not hold:\n%s", rows)
		in = bufio.NewReader(upStdin)
		if ids, err = up.AskWorkers(cands, in, stdout); err != nil {
			return err
		}
		if len(ids) == 0 {
			_, err := fmt.Fprintln(stdout, "nothing adopted")
			return err
		}
	}
	ad := upAdoptDeps(d, claudeDir)
	ap, err := up.PlanAdoptWorkers(context.Background(), *p, all, cands, ids, ad)
	if err != nil {
		return err
	}
	if o.dryRun && !interactive {
		// A dry run shows the rows as well as the plan (design 4.8).
		upLabelTabs(dctx, cands)
		_, _ = fmt.Fprintf(stdout, "running Claude sessions in the project's folders that the project does not hold:\n%s", up.RenderCandidates(cands, time.Now().UTC()))
	}
	_, _ = fmt.Fprint(stdout, up.RenderAdopt(ap))
	if !interactive && slices.ContainsFunc(ap.Left, func(c up.AdoptCandidate) bool { return !c.Subfolder }) {
		_, _ = fmt.Fprintln(stdout, hint)
	}
	if o.dryRun {
		_, err := fmt.Fprintln(stdout, "(dry run: nothing adopted)")
		return err
	}
	if in != nil {
		_, _ = fmt.Fprint(stdout, "Proceed? [y/N] ")
		line, _ := in.ReadString('\n')
		// Piped input is not echoed: end the prompt's line, so the last line
		// of the run stands alone.
		_, _ = fmt.Fprintln(stdout)
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
	_, err = fmt.Fprintf(stdout, "project %s: adopted %d worker(s); nothing restarted\n", p.ID, len(ap.Workers))
	return err
}

// upNoteTimeout bounds the restore's note: its detection and every
// subprocess of it share this one deadline (adopt design 4.8).
var upNoteTimeout = 10 * time.Second

// upNoteRunning counts the note's detections still running: one that
// outlasts the deadline goes on (its subprocesses cancelled) after the
// restore stops waiting for it, until the process exits. Tests wait on it
// before they put back what it reads.
var upNoteRunning sync.WaitGroup

// upUnheldNote is the note a restore prints when running sessions in the
// project's folders are not in the project (adopt design 4.8), "" when there
// are none. It is a hint: any failure, and a detection that outlasts the
// deadline (which cancels its subprocesses), give "". procs is the process
// list the restore read already.
func upUnheldNote(p *up.Project, recs []route.Record, procs []claudecode.Proc) string {
	ctx, cancel := context.WithTimeout(context.Background(), upNoteTimeout)
	defer cancel()
	root, held := p.Root, up.Held(*p, recs)
	res := make(chan int, 1)
	upNoteRunning.Add(1)
	go func() {
		defer upNoteRunning.Done()
		// The reads stop between files and sessions once ctx ends; a single
		// read that stalls (a hung mount) keeps this goroutine until it
		// returns or qm exits: the restore does not wait for it.
		all, err := up.AllCtx(ctx)
		if err != nil {
			res <- 0
			return
		}
		det, claudeDir, err := upDetect(ctx, root, all, recs, held, procs, true, "")
		if err != nil || ctx.Err() != nil || claudecode.SessionsNote(claudeDir) != "" {
			res <- 0
			return
		}
		n := 0
		for _, c := range det.Candidates {
			if !c.Subfolder {
				n++
			}
		}
		res <- n
	}()
	select {
	case n := <-res:
		if n > 0 {
			return fmt.Sprintf("%d running session(s) in the project's folders are not in the project: qm up --adopt lists them", n)
		}
	case <-ctx.Done():
	}
	return ""
}

// upAdoptDeps wires the machine into up.AdoptDeps: agterm and tmux through
// their own control commands, whatever launcher was picked.
func upAdoptDeps(d up.Deps, claudeDir string) up.AdoptDeps {
	a, t := upAgtermCtl, upTmuxCtl
	return up.AdoptDeps{
		Workspaces: a.Workspaces, Move: a.Move, Pin: a.Pin, PaneOf: t.PaneOf,
		TermOf: func(pid int) (claudecode.TermEnv, error) { return upTermOf(context.Background(), pid) },
		WindowPanes: func(ctx context.Context, windowID string) (int, error) {
			out, err := t.Run(ctx, t.Bin, "display-message", "-p", "-t", windowID, "#{window_panes}")
			if err != nil {
				return 0, fmt.Errorf("tmux: %w", err)
			}
			return strconv.Atoi(strings.TrimSpace(string(out)))
		},
		PaneRuns: func(ctx context.Context, windowID string, pid int) (bool, error) {
			return upPaneRuns(ctx, windowID, pid)
		},
		Live: func() (map[string]claudecode.LiveSession, error) {
			procs, err := upProcs()
			if err != nil {
				return nil, err
			}
			return claudecode.Live(claudeDir, procs)
		},
		Trusted: d.Trusted, Log: d.Log, Now: d.Now, Config: d.Coordinator,
		// Read again during adoption: its notes were printed by the run's
		// first read.
		Records: func() ([]route.Record, error) { return readUpRecords(io.Discard) },
		// Signal 0 checks that the process exists; EPERM: it runs as
		// another user.
		Running: func(pid int) bool {
			err := syscall.Kill(pid, 0)
			return err == nil || errors.Is(err, syscall.EPERM)
		},
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
		if o.hereGiven {
			return fmt.Errorf("%w: --here starts a coordinator in this tab, but adopting a running one starts none", errUsage)
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
		// Piped input is not echoed: end the prompt's line, so the last line
		// of the run stands alone.
		_, _ = fmt.Fprintln(stdout)
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
