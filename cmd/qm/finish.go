package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/spawn"
	"github.com/IZZY-Agency/quartermaster/internal/up"
)

const finishUsage = `usage:
  qm finish [--dry-run] NAME
`

func init() {
	commands["finish"] = command{"clean up a worker whose pull request is merged: qm finish [--dry-run] NAME", runFinish}
}

// qm finish runs git and gh, lists the processes' folders, closes terminal
// sessions and marks the worker done through these; tests replace them.
var (
	finishRun   up.DirRunner                                = up.ExecDir
	finishProcs func(context.Context) ([]up.ProcCwd, error) = up.ProcCwds
	finishSleep func(time.Duration)                         = time.Sleep
	finishTerms up.Terminals                                = refTerms{}
	finishLive  func() (map[string]bool, error)             = finishLiveSessions
	finishMark                                              = markDone
)

// finishStepTimeout bounds the proof, and each step after it.
const finishStepTimeout = 2 * time.Minute

// runFinish runs qm finish. Exit 0 when every step ran (a step that left
// something, and said why, ran too); 1 without the proof, or when a step
// failed; 2 on bad input, or outside a recorded project.
func runFinish(args []string, stdout, stderr io.Writer) int {
	err := finishMain(args, stdout, stderr)
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errUsage), errors.Is(err, up.ErrInput):
		printErr(stderr, "qm finish", err)
		_, _ = fmt.Fprint(stderr, finishUsage)
		return 2
	case errors.Is(err, errQuiet):
		return 1
	default:
		printErr(stderr, "qm finish", err)
		return 1
	}
}

// finishMain runs qm finish NAME: once the worker's pull request is proven
// merged at its branch's tip (up.ProveMerged), it marks the worker done and
// closes its terminal session, removes its worktree, deletes its local and
// remote branch, and fast-forwards the main checkout, each only on its own
// proof. The project is found as qm up done finds it.
func finishMain(args []string, stdout, stderr io.Writer) error {
	var dry bool
	fs := flag.NewFlagSet("qm finish", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.BoolVar(&dry, "dry-run", false, "print each step and its proof; change nothing")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	if fs.NArg() != 1 || strings.HasPrefix(fs.Arg(0), "-") {
		return fmt.Errorf("%w: qm finish takes one worker name", errUsage)
	}
	name := fs.Arg(0)
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
	p, found, err := up.LoadID(id)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: no qm up project here (%s)", up.ErrInput, where)
	}
	// Listed before the sessions are read, as markDone does: a spawn of the
	// name that was starting meanwhile is then seen.
	starting, err := spawn.ListStarting()
	if err != nil {
		return err
	}
	recs, err := readUpRecords(stdout)
	if err != nil {
		return err
	}
	cands, _, err := doneCandidates(p, recs)
	if err != nil {
		return err
	}
	if name, err = resolveWorkerName(stderr, "qm finish", p, cands, name); err != nil {
		return err
	}
	if cands, _, err = doneOf(name, cands, starting); err != nil {
		return err
	}
	c, ok := up.NewestWorker(cands, name)
	if !ok {
		return fmt.Errorf("%w: no worker %q in project %s", up.ErrInput, name, p.ID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), finishStepTimeout)
	pr, err := up.ProveMerged(ctx, finishRun, c, p.Root)
	cancel()
	if err != nil {
		printLine(stderr, "qm finish %s: nothing changed: %s", name, err)
		return errQuiet
	}
	printLine(stdout, "proof: %s", pr)
	failed, closed, marked := false, false, true
	if dry {
		finishWouldClose(stdout, name, c, up.RecordedTabs(p, recs))
	} else {
		closed, marked, failed = finishWorker(stdout, stderr, id, where, name, c)
	}
	fin := up.Finisher{Run: finishRun, Procs: finishProcs, Sleep: finishSleep, DryRun: dry}
	step := func(do func(context.Context) up.StepResult) {
		ctx, cancel := context.WithTimeout(context.Background(), finishStepTimeout)
		defer cancel()
		res := do(ctx)
		printLine(stdout, "%s", res.Line)
		failed = failed || res.Failed
	}
	if marked {
		var removed string
		step(func(ctx context.Context) up.StepResult {
			var res up.StepResult
			res, removed = fin.RemoveWorktree(ctx, pr, c.Cwd, closed)
			return res
		})
		step(func(ctx context.Context) up.StepResult { return fin.DeleteBranch(ctx, pr, removed) })
		step(func(ctx context.Context) up.StepResult { return fin.DeleteRemoteBranch(ctx, pr) })
	} else {
		// Another session of the name may use them by now.
		printLine(stdout, "worker %s: its worktree and branches are left: it was not marked done", name)
	}
	step(func(ctx context.Context) up.StepResult { return fin.FastForwardMain(ctx, pr) })
	if failed {
		return errQuiet
	}
	return nil
}

// finishWouldClose prints, for a dry run, what finishWorker would do.
func finishWouldClose(stdout io.Writer, name string, c up.Candidate, tabs map[string]launch.SessionRef) {
	var refs []string
	for _, sid := range []string{c.SessionID, c.Instead} {
		if ref, ok := tabs[sid]; ok && sid != "" && ref.ID != "" {
			refs = append(refs, ref.Launcher+" "+ref.ID)
		}
	}
	what := "it has no terminal session recorded"
	if len(refs) > 0 {
		what = "would close its " + strings.Join(refs, ", ") + " when it still runs the worker or an idle shell"
	}
	printLine(stdout, "worker %s: would mark done (qm up done); %s", name, what)
}

// finishWorker marks worker name done, takes qm's tint off its tabs and
// closes them (up.CloseFinished) under the project's lock. closed says
// whether it closed one; marked whether it marked the proven session done;
// failed whether a part of it failed (said on stderr).
func finishWorker(stdout, stderr io.Writer, id, where, name string, c up.Candidate) (closed, marked, failed bool) {
	sid, _, tabs, err := finishMark(id, where, name, c.SessionID, stdout, stderr)
	if err != nil {
		printErr(stderr, "qm finish: worker "+name+" not marked done", err)
		return false, false, true
	}
	printNotes(stdout, clearTabs(slices.Collect(maps.Values(tabs))...))
	printLine(stdout, "worker %s: marked done (%s): qm up will not restore it", name, sid)
	if len(tabs) == 0 {
		printLine(stdout, "worker %s: no terminal session recorded", name)
		return false, true, false
	}
	// A session not seen running is closed only in an idle shell, which
	// is safe whether or not it runs elsewhere.
	live, _ := finishLive()
	unlock, err := up.Lock(id)
	if err != nil {
		printErr(stderr, "qm finish: worker "+name+"'s terminal session not closed", err)
		return false, true, true
	}
	defer unlock()
	p, found, err := up.LoadID(id)
	if err != nil || !found {
		printErr(stderr, "qm finish: worker "+name+"'s terminal session not closed", errors.Join(err, errors.New("the project was not read")))
		return false, true, true
	}
	ctx, cancel := context.WithTimeout(context.Background(), finishStepTimeout)
	defer cancel()
	for _, s := range slices.Sorted(maps.Keys(tabs)) {
		ref := tabs[s]
		if ref.ID == "" {
			continue
		}
		step := up.Step{Candidate: c, Action: up.Skip}
		step.SessionID = s
		// A running session is closed in the tab that runs it.
		if live[s] {
			step.Action = up.Leave
		}
		got, err := up.CloseFinished(ctx, &p, step, ref, finishTerms)
		switch {
		case err != nil:
			printErr(stderr, "qm finish: worker "+name, err)
			failed = true
		case up.IsLeftOpen(got.Warning):
			printLine(stdout, "worker %s: its %s %s %s", name, ref.Launcher, ref.ID, got.Warning)
		case got.ID != "":
			printLine(stdout, "worker %s: closed %s %s", name, got.Launcher, got.ID)
			closed = true
		default:
			printLine(stdout, "worker %s: its %s %s is gone already", name, ref.Launcher, ref.ID)
		}
	}
	if err := up.Save(p); err != nil {
		printErr(stderr, "qm finish: the project was not saved", err)
		failed = true
	}
	return closed, true, failed
}

// finishLiveSessions are the Claude sessions running now.
func finishLiveSessions() (map[string]bool, error) {
	live, err := upLive()
	out := map[string]bool{}
	for sid := range live {
		out[sid] = true
	}
	return out, err
}
