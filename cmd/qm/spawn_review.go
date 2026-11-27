package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/config"
	"github.com/IZZY-Agency/quartermaster/internal/limits"
	"github.com/IZZY-Agency/quartermaster/internal/plain"
	"github.com/IZZY-Agency/quartermaster/internal/policy"
	"github.com/IZZY-Agency/quartermaster/internal/reviewin"
	"github.com/IZZY-Agency/quartermaster/internal/route"
	"github.com/IZZY-Agency/quartermaster/internal/spawn"
)

// claudeLook finds the claude command for the Claude reviewer; tests replace it.
var claudeLook = exec.LookPath

// startReview runs a review job in the background and returns its runner
// process, or nil when there is none to wait for; tests replace it.
var startReview = startDetached

// reviewTimeout bounds a review job's run; tests shorten it.
var reviewTimeout = spawn.DefaultReviewTimeout

// reviewPoll is how often --wait reads a job's status.
var reviewPoll = time.Second

// pidFile holds the process id of a job's background runner.
const pidFile = "pid"

// spawnReview routes a review, starts it in the background and prints its
// job id and the decision; --wait then waits for its findings. --status
// reports on a job; --run (internal) runs a job in this process. The review
// is logged when it ends, with its finding counts. Exit 2 on bad input, 1 on
// failure.
func spawnReview(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("qm spawn review", flag.ContinueOnError)
	fs.SetOutput(stderr)
	promptFile := fs.String("prompt", "", "the review prompt file")
	round := fs.Int("round", 0, "review round (default: 1 plus the reviews logged for this branch)")
	base := fs.String("base", "", "the ref the diff under review is taken against (default: origin/HEAD's branch, else "+reviewin.DefaultBase+")")
	diff := fs.String("diff", "", "review this range, A..B or A...B (e.g. the commits of one fix round), instead of BASE...HEAD; the size used to route follows it")
	repo := fs.String("repo", ".", "the repository under review")
	wait := fs.Bool("wait", false, "wait for the review to end and print its findings")
	force := fs.String("force", "", "override the table, model/effort: a Claude tier or a Codex one (needs --why)")
	why := fs.String("why", "", "why the tier is forced")
	status := fs.String("status", "", "report on the review job with this id")
	runID := fs.String("run", "", "run the review job with this id in this process (used by qm itself)")
	err := parseFlags(fs, args)
	if err == nil {
		switch {
		case *runID != "":
			err = runReviewJob(*runID)
		case *status != "":
			err = reviewStatus(*status, *wait, stdout)
		default:
			err = newReview(reviewRequest{prompt: *promptFile, round: *round, base: *base, diff: *diff, repo: *repo, force: *force, why: *why, wait: *wait}, stdout, stderr)
		}
	}
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errUsage):
		printErr(stderr, "qm spawn review", err)
		_, _ = fmt.Fprint(stderr, spawnUsage)
		return 2
	default:
		printErr(stderr, "qm spawn review", err)
		return 1
	}
}

type reviewRequest struct {
	prompt, base, diff, repo, force, why string
	round                                int
	wait                                 bool
}

// reviewInput is the input of a review, from reviewin.ForRange, refused when
// the range has no change in it: a review started in the wrong folder (the
// main checkout, for a branch that lives in a worktree) would be sized and
// counted for the wrong branch, and the reviewer would be given nothing to
// review. qm route review is the preview of qm spawn review and refuses alike.
func reviewInput(repo, base, diff, prompt string, round int) (route.ReviewInput, reviewin.Range, error) {
	in, rng, err := reviewin.ForRange(repo, base, diff, prompt, round)
	if err != nil {
		return in, rng, err
	}
	if in.DiffFiles == 0 {
		return in, rng, fmt.Errorf("%s has no changes in git diff %s, so there is nothing to review; commit the change, run qm in the folder of the branch under review, or pass --repo, --base or --diff", branchLabel(rng.Branch), rng.Spec)
	}
	return in, rng, nil
}

func newReview(r reviewRequest, stdout, stderr io.Writer) error {
	switch {
	case r.prompt == "":
		return fmt.Errorf("%w: --prompt is required", errUsage)
	case r.round < 0:
		return fmt.Errorf("%w: --round %d is negative", errUsage, r.round)
	case r.force != "" && strings.TrimSpace(r.why) == "":
		return fmt.Errorf("%w: --force needs --why", errUsage)
	case r.force == "" && r.why != "":
		return fmt.Errorf("%w: --why goes with --force", errUsage)
	}
	forced, err := parseTier("--force", r.force)
	if err != nil {
		return err
	}
	in, rng, err := reviewInput(r.repo, r.base, r.diff, r.prompt, r.round)
	if err != nil {
		return err
	}
	branch := rng.Branch
	if forced != nil && slices.Contains(policy.CodexModels, forced.Model) {
		in.ForceCodex = &route.CodexTier{Model: forced.Model, Effort: forced.Effort}
	} else {
		in.Force = forced
	}

	// The change is read before the job exists, so a failure leaves no job
	// behind, and the files are those of the commits the size was taken from.
	change, err := reviewin.BuildChange(in.Repo, rng)
	if err != nil {
		return fmt.Errorf("reading the change under review: %w", err)
	}

	deps, err := routeDeps()
	if err != nil {
		return err
	}
	d, err := route.DecideReview(context.Background(), in, deps.ask, deps.gate, deps.codex(limits.CodexAsk), claudeState(stderr))
	if err != nil {
		return routeError(err)
	}
	d = deps.mark(d)

	rec := route.Record{Project: in.Repo, Branch: branch, Round: in.Round, Override: strings.TrimSpace(r.why)}
	dir, err := spawn.NewReviewJob(spawn.ReviewJob{Repo: in.Repo, Decision: d, Record: rec}, []byte(in.Prompt), change)
	if err != nil {
		return err
	}
	id := filepath.Base(dir)
	proc, err := startReview(id, dir)
	if err != nil {
		// No runner will write the status: say failed, so no wait hangs on it.
		err = fmt.Errorf("starting review %s: %w", id, err)
		if serr := spawn.MarkReviewFailed(dir); serr != nil {
			err = errors.Join(err, serr)
		}
		return err
	}
	// alive: with --wait, qm is the runner's parent and reaps it, so a runner
	// that dies is seen at once (an unreaped zombie would still answer kill 0).
	alive := func() bool { return true }
	switch {
	case proc == nil:
	case r.wait:
		exited := make(chan struct{})
		go func() { _, _ = proc.Wait(); close(exited) }()
		alive = func() bool {
			select {
			case <-exited:
				return false
			default:
				return true
			}
		}
	default:
		if err := proc.Release(); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(stdout, "review %s started: %s\n", id, decisionLine(d)); err != nil {
		return err
	}
	if !r.wait {
		_, err := fmt.Fprintf(stdout, "qm spawn review --status %s --wait\n", id)
		return err
	}
	return waitReview(id, dir, alive, stdout)
}

// startDetached starts `qm spawn review --run ID` in its own process group,
// its output going to the job's log, and records its pid; it does not wait.
// When the pid cannot be recorded the runner's group is killed, so a failed
// start leaves no review running.
func startDetached(id, dir string) (*os.Process, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	log, err := os.OpenFile(filepath.Join(dir, "log"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	defer func() { _ = log.Close() }()
	cmd := exec.Command(exe, "spawn", "review", "--run", id)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, pidFile), []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o600); err != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
		return nil, err
	}
	return cmd.Process, nil
}

// runReviewJob runs a job to its end in this process. SIGTERM and SIGINT
// end the run like its deadline does: the reviewer's process group, which is
// not the runner's, is killed and the job fails.
func runReviewJob(id string) error {
	dir, err := spawn.ReviewJobDir(id)
	if err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	r := spawn.ReviewRunner{Claude: "claude"}
	if p, err := codexLook("codex"); err == nil {
		r.Codex = p
	}
	if p, err := claudeLook("claude"); err == nil {
		r.Claude = p
	}
	if cfg, err := config.Load(); err == nil {
		r.CodexRead = limits.CodexAsk.If(cfg.CodexUsageRead())
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, reviewTimeout)
	defer cancel()
	return r.Run(ctx, dir)
}

// reviewStatus prints a job's status; a done job's findings follow it. With
// wait it first waits for the job to end. A failed job is an error.
func reviewStatus(id string, wait bool, stdout io.Writer) error {
	dir, err := spawn.ReviewJobDir(id)
	if err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	if !wait {
		return printReview(id, dir, stdout)
	}
	return waitReview(id, dir, func() bool { return runnerAlive(dir) }, stdout)
}

// waitReview waits while the job is running and its runner is alive, then
// prints it. A runner gone while the status still says running is an error;
// the status is read once more after the runner is seen gone, since it may
// have written its status just before ending.
func waitReview(id, dir string, alive func() bool, stdout io.Writer) error {
	for {
		st, err := spawn.ReviewStatus(dir)
		if err != nil {
			return fmt.Errorf("review %s: %w", id, err)
		}
		if st != spawn.StatusRunning {
			break
		}
		if !alive() {
			if st, err = spawn.ReviewStatus(dir); err == nil && st == spawn.StatusRunning {
				return fmt.Errorf("review %s: its runner ended without a status; log: %s", id, filepath.Join(dir, "log"))
			}
			break
		}
		time.Sleep(reviewPoll)
	}
	return printReview(id, dir, stdout)
}

// printReview prints a job's status; a done job's findings follow it. A
// failed job is an error.
func printReview(id, dir string, stdout io.Writer) error {
	st, err := spawn.ReviewStatus(dir)
	if err != nil {
		return fmt.Errorf("review %s: %w", id, err)
	}
	switch st {
	case spawn.StatusDone:
		b, err := os.ReadFile(filepath.Join(dir, "findings.json"))
		if err != nil {
			return fmt.Errorf("review %s: %w", id, err)
		}
		_, err = fmt.Fprintf(stdout, "review %s: done\n%s", id, plain.JSON(b))
		return err
	case spawn.StatusFailed:
		return fmt.Errorf("review %s failed; log: %s", id, filepath.Join(dir, "log"))
	default:
		_, err := fmt.Fprintf(stdout, "review %s: %s\n", id, st)
		return err
	}
}

// runnerAlive reports whether the job's background runner may still be
// running: true unless its pid file names a process that is gone. A job
// started without a pid file (in-process) counts as alive. Used by
// --status --wait from another qm, which is not the runner's parent, so the
// runner is reaped by init and kill 0 sees it gone. Two cases still read as
// alive: the pid reused by a new process after the runner died, and EPERM
// (a process of another user). Both need the runner to die without writing
// its status and are rare; such a wait then lasts until interrupted.
func runnerAlive(dir string) bool {
	b, err := os.ReadFile(filepath.Join(dir, pidFile))
	if err != nil {
		return true
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return true
	}
	return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

// branchLabel names a branch in a note; a detached HEAD has none.
func branchLabel(branch string) string {
	if branch == "" {
		return "this checkout (detached HEAD)"
	}
	return "branch " + branch
}
