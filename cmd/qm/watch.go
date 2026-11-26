package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
	"github.com/IZZY-Agency/quartermaster/internal/config"
	"github.com/IZZY-Agency/quartermaster/internal/declog"
	"github.com/IZZY-Agency/quartermaster/internal/hooks"
	"github.com/IZZY-Agency/quartermaster/internal/outcomes"
	"github.com/IZZY-Agency/quartermaster/internal/paths"
	"github.com/IZZY-Agency/quartermaster/internal/spawn"
	"github.com/IZZY-Agency/quartermaster/internal/transcript"
	"github.com/IZZY-Agency/quartermaster/internal/up"
	"github.com/IZZY-Agency/quartermaster/internal/watch"
)

const watchUsage = `usage:
  qm watch [--silent DURATION] [--timeout DURATION] [--interval DURATION] [--wait-start DURATION] [NAME...]

Waits until a worker of this project stops, goes silent or goes away, prints
why and exits 0. Run it in the background: its exit wakes the coordinator.
A worker that ended its turn while a background task of its own runs, or a
wakeup is scheduled, is waiting: that is no event, and the watch goes on.
With names it watches those workers, else every unfinished worker. Each stop,
silence and disappearance is reported once: a worker already stopped, silent
or gone at the start is reported at once, unless an earlier watch reported it,
and then it is listed as (reported), and only a silent one is waited for: it
can still stop. A watch prints and exits only for the events it reported
itself.

  --silent DURATION    silence threshold (default: watch.silent_minutes, 10m)
  --timeout DURATION   give up after this long, exit 0 (default: none)
  --interval DURATION  time between checks (default 15s)
  --wait-start DURATION  with names: how long a worker already reported
                       stopped is waited for to work again (default 2m, 0: not
                       waited for); --timeout still caps it
`

// watchNow and watchSleep are qm watch's clock; tests replace them.
var (
	watchNow   = time.Now
	watchSleep = watch.Sleep
)

// errNoProject: the working folder is in no qm up project.
var errNoProject = errors.New("no project here")

func init() {
	commands["watch"] = command{"wait until a worker of this project stops: qm watch [NAME...]", runWatch}
}

// runWatch runs qm watch. Exit 0 on an event, a timeout or nothing to wait
// for; 2 on bad input or outside a project; 1 on another failure; 128 plus
// the signal's number when interrupted.
func runWatch(args []string, stdout, stderr io.Writer) int {
	if isHelp(args) {
		_, _ = fmt.Fprint(stdout, watchUsage)
		return 0
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigs)
	got := make(chan syscall.Signal, 1)
	go func() {
		select {
		case s := <-sigs:
			if n, ok := s.(syscall.Signal); ok {
				got <- n
			}
			cancel()
		case <-ctx.Done():
		}
	}()
	err := watchMain(ctx, args, stdout, stderr)
	sig := 0
	select {
	case s := <-got:
		sig = int(s)
	default:
	}
	return watchExit(err, sig, stderr)
}

// watchExit is qm watch's exit code for err; sig is the signal that
// interrupted it (0 for none). An interrupt never exits 0.
func watchExit(err error, sig int, stderr io.Writer) int {
	switch {
	case err == nil && sig == 0:
		return 0
	case err == nil, errors.Is(err, context.Canceled):
		if sig == 0 {
			sig = int(syscall.SIGINT)
		}
		return 128 + sig
	case errors.Is(err, errUsage), errors.Is(err, watch.ErrUnknownName), errors.Is(err, up.ErrInput), errors.Is(err, errNoProject):
		printErr(stderr, "qm watch", err)
		if !errors.Is(err, errNoProject) {
			_, _ = fmt.Fprint(stderr, watchUsage)
		}
		return 2
	default:
		printErr(stderr, "qm watch", err)
		return 1
	}
}

type watchFlags struct {
	silent, timeout, interval, waitStart time.Duration
	names                                []string
}

// parseWatch reads flags and names in any order.
func parseWatch(args []string, cfg config.Config) (watchFlags, error) {
	silent, err := cfg.SilentThreshold()
	if err != nil {
		return watchFlags{}, err
	}
	f := watchFlags{silent: silent, interval: 15 * time.Second, waitStart: 2 * time.Minute}
	fl := flag.NewFlagSet("watch", flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	fl.DurationVar(&f.silent, "silent", f.silent, "")
	fl.DurationVar(&f.timeout, "timeout", 0, "")
	fl.DurationVar(&f.interval, "interval", f.interval, "")
	fl.DurationVar(&f.waitStart, "wait-start", f.waitStart, "")
	for {
		if err := fl.Parse(args); err != nil {
			return watchFlags{}, fmt.Errorf("%w: %v", errUsage, err)
		}
		args = fl.Args()
		if len(args) == 0 {
			break
		}
		f.names = append(f.names, args[0])
		args = args[1:]
	}
	switch {
	case f.silent <= 0:
		return watchFlags{}, fmt.Errorf("%w: --silent must be positive", errUsage)
	case f.interval <= 0:
		return watchFlags{}, fmt.Errorf("%w: --interval must be positive", errUsage)
	case f.waitStart < 0:
		return watchFlags{}, fmt.Errorf("%w: --wait-start must not be negative", errUsage)
	case f.timeout < 0:
		return watchFlags{}, fmt.Errorf("%w: --timeout must not be negative", errUsage)
	}
	return f, nil
}

func watchMain(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	start := watchNow() // --timeout counts from here: one deadline for the whole watch
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	f, err := parseWatch(args, cfg)
	if err != nil {
		return err
	}
	p, ok, err := projectHereBounded(ctx, f.timeout)
	switch {
	case errors.Is(err, errWatchTimeout):
		_, _ = fmt.Fprintf(stdout, "qm watch: timeout after %v; no change\n", f.timeout)
		return nil
	case err != nil:
		return err
	case !ok:
		return errNoProject
	}
	home, _ := os.UserHomeDir()
	tints, err := cfg.Tints()
	if err != nil {
		printLine(stderr, "qm watch: note: %s", tintNotes("", err)[0])
	}
	return watch.Run(ctx, watch.Options{Names: f.names, Prefixes: up.Prefixes(cfg, p), Timeout: f.timeout, Start: start, Interval: f.interval, WaitStart: f.waitStart}, watch.Deps{
		Snapshot: func(ctx context.Context) (watch.Snapshot, error) { return workersSnapshot(ctx, p.ID, f.silent) },
		Now:      watchNow,
		Sleep:    watchSleep,
		Marks:    fileMarks{path: marksPath(p.ID)},
		Notify: func(ctx context.Context, text string) error {
			return hooks.NotifyContext(ctx, cfg, "qm watch", text)
		},
		Tint:  watchTints(p.ID, tints, start),
		Tints: fileMarks{path: tintMarksPath(p.ID)},
		Home:  home,
		Out:   stdout,
		Err:   stderr,
	})
}

// errWatchTimeout: the project lookup used up --timeout.
var errWatchTimeout = errors.New("watch timeout")

// projectHereBounded is projectHere, given up on when ctx ends (its error) or
// timeout (0: none) passes (errWatchTimeout): the lookup counts against the
// watch's one deadline.
func projectHereBounded(ctx context.Context, timeout time.Duration) (up.Project, bool, error) {
	type result struct {
		p   up.Project
		ok  bool
		err error
	}
	done := make(chan result, 1)
	go func() {
		p, ok, err := projectHere()
		done <- result{p, ok, err}
	}()
	var expired <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		expired = t.C
	}
	select {
	case r := <-done:
		return r.p, r.ok, r.err
	case <-ctx.Done():
		return up.Project{}, false, ctx.Err()
	case <-expired:
		return up.Project{}, false, errWatchTimeout
	}
}

// projectHere is the qm up project the working folder belongs to, found the
// way qm up done finds it: QM_PROJECT_ID, else the folder's project (from a
// linked worktree, the main worktree's).
func projectHere() (up.Project, bool, error) {
	if id := os.Getenv("QM_PROJECT_ID"); id != "" {
		return up.LoadID(id)
	}
	wd, err := upWorkDir()
	if err != nil {
		return up.Project{}, false, err
	}
	return up.ProjectFor(wd)
}

// workersSnapshot reads the project's workers and their states now, or
// gives up when ctx ends. The project is read again each time, so a worker
// marked done or replaced since leaves.
func workersSnapshot(ctx context.Context, id string, silent time.Duration) (watch.Snapshot, error) {
	type result struct {
		s   watch.Snapshot
		err error
	}
	done := make(chan result, 1)
	go func() {
		s, err := readWorkers(id, silent)
		done <- result{s, err}
	}()
	select {
	case r := <-done:
		return r.s, r.err
	case <-ctx.Done():
		return watch.Snapshot{}, ctx.Err()
	}
}

// readWorkers is workersSnapshot's work. The process list is read only
// when the project has a worker to show; a failed one makes every state
// unknown, with the reason as the snapshot's note.
func readWorkers(id string, silent time.Duration) (watch.Snapshot, error) {
	p, ok, err := up.LoadID(id)
	if err != nil {
		return watch.Snapshot{}, err
	}
	if !ok {
		return watch.Snapshot{}, errNoProject
	}
	// With the workers a qm spawn worker that stopped left reserved: they may
	// run. One still being started is shown once it is logged.
	starting, err := spawn.ListStarting()
	if err != nil {
		return watch.Snapshot{}, err
	}
	starting = slices.DeleteFunc(starting, func(s spawn.Starting) bool { return s.Running })
	recs, err := declog.Read(time.Time{})
	if err != nil {
		return watch.Snapshot{}, err
	}
	recs = spawn.AddStarting(recs, starting)
	handoff, err := up.HandoffDir()
	if err != nil {
		return watch.Snapshot{}, err
	}
	cands, err := watch.Workers(p, recs, handoff)
	if err != nil || len(cands) == 0 {
		return watch.Snapshot{}, err
	}
	claudeDir, err := paths.ClaudeDir()
	if err != nil {
		return watch.Snapshot{}, err
	}
	cc, err := outcomes.DefaultClaude()
	if err != nil {
		return watch.Snapshot{}, err
	}
	m := watch.Machine{
		Now:      watchNow(),
		Silent:   silent,
		Reserved: append([]string{p.Coordinator.ID}, p.PastCoordinators...),
		Starts:   map[int]time.Time{},
		Turn:     hookTurn,
		Transcript: func(sid string) string {
			path, _ := cc.TranscriptPath(sid)
			return path
		},
		Activity: watch.Activity,
		Context: func(path string) int {
			last, _ := transcript.LastContext(path)
			return last.Context
		},
	}
	procs, err := upProcs()
	if err == nil {
		m.Live, err = claudecode.Live(claudeDir, procs)
		m.Files = watch.PIDFileSessions(claudeDir, m.Live)
	}
	m.Detect = err
	for _, pr := range procs {
		if t, ok := claudecode.ProcStart(pr); ok {
			m.Starts[pr.PID] = t
		}
	}
	s := watch.Snapshot{Rows: watch.Rows(p.Root, cands, m)}
	tabs := watch.Tabs(p, recs, cands)
	for i := range s.Rows {
		s.Rows[i].Tab = tabs[s.Rows[i].SessionID]
	}
	if m.Detect != nil {
		s.Note = m.Detect.Error()
	}
	return s, nil
}

// hookTurn is a session's turn as its turn hooks stored it; unknown when
// none ran or it cannot be read.
func hookTurn(sessionID string) watch.Turn {
	t, err := hooks.ReadTurn(sessionID)
	if err != nil || !t.Known {
		return watch.Turn{}
	}
	return watch.Turn{Known: true, Open: t.Open, At: t.At, PID: t.PID, Pending: t.Tasks > 0 || t.Crons > 0}
}

// marksPath is where qm watch keeps a project's marks:
// <state>/watch/<project id>.json; "" when the state folder is unknown.
func marksPath(id string) string {
	dir, err := paths.StateDir()
	if err != nil || id == "" || strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
		return ""
	}
	return filepath.Join(dir, "watch", id+".json")
}

// tintMarksPath is where qm watch keeps what it tinted, beside the marks of the
// events and under a lock of its own: a tint may take seconds, and must not
// hold up another watch's report.
func tintMarksPath(id string) string {
	p := marksPath(id)
	if p == "" {
		return ""
	}
	return strings.TrimSuffix(p, ".json") + ".tints.json"
}

// fileMarks keeps the marks in one JSON file. Update takes an exclusive
// flock on <file>.lock (giving up when ctx ends) and holds it while it reads, changes and rewrites the file
// (through a temporary file and a rename), so overlapping watches merge
// their marks instead of replacing each other's.
type fileMarks struct{ path string }

func (f fileMarks) Load() (watch.Marks, error) {
	if f.path == "" {
		return watch.Marks{}, errors.New("no state folder")
	}
	return readMarks(f.path)
}

func readMarks(path string) (watch.Marks, error) {
	m := watch.Marks{}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return watch.Marks{}, fmt.Errorf("%s: %w", path, err)
	}
	if m == nil { // the file holds JSON null
		m = watch.Marks{}
	}
	return m, nil
}

func (f fileMarks) Update(ctx context.Context, fn func(watch.Marks) bool) (err error) {
	if f.path == "" {
		return errors.New("no state folder")
	}
	dir := filepath.Dir(f.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(f.path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := lock.Close(); err == nil {
			err = cerr
		}
	}()
	if err := flockCtx(ctx, lock); err != nil {
		return fmt.Errorf("lock %s: %w", lock.Name(), err)
	}
	m, err := readMarks(f.path)
	if err != nil {
		m = watch.Marks{} // a broken file is rewritten whole
	}
	if !fn(m) {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".marks-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), f.path)
}

// flockCtx takes an exclusive flock on f, trying again every few
// milliseconds until ctx ends (then with ctx's error).
func flockCtx(ctx context.Context, f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}
