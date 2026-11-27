package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/reviewin"
	"github.com/IZZY-Agency/quartermaster/internal/route"
	"github.com/IZZY-Agency/quartermaster/internal/spawn"
)

// spawnHermetic is routeHermetic plus a fake claude that replies with
// result and appends its arguments to the returned file, and reviews run in
// this process instead of in the background.
func spawnHermetic(t *testing.T, result string) (argsFile string) {
	t.Helper()
	routeHermetic(t)
	dir := t.TempDir()
	reply, err := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": result})
	if err != nil {
		t.Fatal(err)
	}
	replyFile := routeFile(t, dir, "reply.json", string(reply))
	argsFile = filepath.Join(dir, "args.txt")
	claude := filepath.Join(dir, "claude")
	script := "#!/bin/sh\necho \"$*\" >> '" + argsFile + "'\ncat > '" + filepath.Join(dir, "stdin.txt") + "'\ncat '" + replyFile + "'\n"
	if err := os.WriteFile(claude, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	oldLook, oldStart := claudeLook, startReview
	claudeLook = func(string) (string, error) { return claude, nil }
	startReview = func(id, _ string) (*os.Process, error) {
		run([]string{"spawn", "review", "--run", id}, io.Discard, io.Discard)
		return nil, nil
	}
	t.Cleanup(func() { claudeLook, startReview = oldLook, oldStart })
	return argsFile
}

func spawnRun(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(append([]string{"spawn", "review"}, args...), &out, &errOut)
	return code, out.String(), errOut.String()
}

const oneP1 = `{"verdict":"not-mergeable","findings":[{"severity":"P1","file":"notes.txt","line":2,"scenario":"s","fix":"f"}]}`

func TestSpawnReviewWaitAndRounds(t *testing.T) {
	argsFile := spawnHermetic(t, oneP1)
	repo := routeRepo(t)
	prompt := routeFile(t, t.TempDir(), "prompt.md", "Check that the notes file reads well.\n")

	code, out, errOut := spawnRun(t, "--prompt", prompt, "--base", "main", "--repo", repo, "--wait")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if !strings.Contains(out, "started: sonnet/high (row small)") || !strings.Contains(out, ": done\n") || !strings.Contains(out, `"severity": "P1"`) {
		t.Errorf("output lacks the decision, the status or the findings:\n%s", out)
	}
	code, _, errOut = spawnRun(t, "--prompt", prompt, "--base", "main", "--repo", repo, "--wait")
	if code != 0 {
		t.Fatalf("second review: exit %d, stderr %q", code, errOut)
	}

	recs := routeLogged(t)
	if len(recs) != 2 {
		t.Fatalf("%d records, want 2: %+v", len(recs), recs)
	}
	top, _, err := reviewin.Repo(repo)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range recs {
		if r.Kind != "review" || r.Project != top || r.Branch != "feature" || r.Round != i+1 {
			t.Errorf("record %d: kind %q project %q branch %q round %d", i, r.Kind, r.Project, r.Branch, r.Round)
		}
		if want := `{"verdict":"not-mergeable","p1":1,"p2":0,"p3":0}`; r.Detail != want {
			t.Errorf("record %d detail %q, want %s", i, r.Detail, want)
		}
	}
	if recs[1].Decision == nil || recs[1].Decision.Row != "fix-check" {
		t.Errorf("round 2 decision %+v, want row fix-check", recs[1].Decision)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSpace(string(args)), "\n"); len(lines) != 2 ||
		!strings.Contains(lines[0], "--model claude-sonnet-5-5 --effort high") || !strings.Contains(lines[1], "--model claude-sonnet-5-5 --effort medium") {
		t.Errorf("claude runs:\n%s\nwant sonnet/high then sonnet/medium", args)
	}
}

func TestSpawnReviewStatus(t *testing.T) {
	spawnHermetic(t, oneP1)
	repo := routeRepo(t)
	prompt := routeFile(t, t.TempDir(), "prompt.md", "Check the notes file.\n")
	code, out, errOut := spawnRun(t, "--prompt", prompt, "--base", "main", "--repo", repo)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	id := strings.Fields(out)[1]
	if !strings.Contains(out, "qm spawn review --status "+id+" --wait") {
		t.Errorf("output does not say how to wait:\n%s", out)
	}
	code, out, _ = spawnRun(t, "--status", id)
	if code != 0 || !strings.HasPrefix(out, "review "+id+": done\n") {
		t.Errorf("--status: exit %d, output %q", code, out)
	}
	if code, _, _ := spawnRun(t, "--status", "../x"); code != 2 {
		t.Errorf("--status ../x: exit %d, want 2", code)
	}
}

func TestSpawnReviewFailed(t *testing.T) {
	spawnHermetic(t, "Looks good to me.")
	repo := routeRepo(t)
	prompt := routeFile(t, t.TempDir(), "prompt.md", "Check the notes file.\n")
	code, _, errOut := spawnRun(t, "--prompt", prompt, "--base", "main", "--repo", repo, "--wait")
	if code != 1 || !strings.Contains(errOut, "failed; log: ") {
		t.Errorf("exit %d, stderr %q; want 1 and the log's path", code, errOut)
	}
	for _, r := range routeLogged(t) {
		if r.Kind != "error" {
			t.Errorf("a failed review logged a %q record", r.Kind)
		}
	}
}

func TestSpawnReviewRunnerGone(t *testing.T) {
	spawnHermetic(t, oneP1)
	repo := routeRepo(t)
	prompt := routeFile(t, t.TempDir(), "prompt.md", "Check the notes file.\n")
	// A real runner child that exits without writing a status: with --wait,
	// qm is its parent, so it stays a zombie unless qm reaps it.
	startReview = func(_, _ string) (*os.Process, error) {
		cmd := exec.Command("sh", "-c", "exit 0")
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return cmd.Process, nil
	}
	done := make(chan struct{})
	var code int
	var errOut string
	go func() {
		code, _, errOut = spawnRun(t, "--prompt", prompt, "--base", "main", "--repo", repo, "--wait")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("--wait still waiting 10 s after its runner exited")
	}
	if code != 1 || !strings.Contains(errOut, "ended without a status") {
		t.Errorf("exit %d, stderr %q; want 1, runner ended without a status", code, errOut)
	}

	// --status --wait from another qm reads the pid file; a runner reaped by
	// someone else is gone too.
	id := ""
	if entries, err := os.ReadDir(filepath.Join(os.Getenv("XDG_STATE_HOME"), "quartermaster", "reviews")); err == nil && len(entries) == 1 {
		id = entries[0].Name()
	} else {
		t.Fatalf("review jobs: %v, %v", entries, err)
	}
	gone := exec.Command("true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	dir, err := spawn.ReviewJobDir(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, pidFile), []byte(strconv.Itoa(gone.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := spawnRun(t, "--status", id, "--wait"); code != 1 || !strings.Contains(errOut, "ended without a status") {
		t.Errorf("--status --wait: exit %d, stderr %q; want 1, runner ended without a status", code, errOut)
	}
}

func TestWaitRunnerEndedAfterDone(t *testing.T) {
	spawnHermetic(t, oneP1)
	dir, err := spawn.NewReviewJob(spawn.ReviewJob{Repo: t.TempDir()}, []byte("Check.\n"), testChange())
	if err != nil {
		t.Fatal(err)
	}
	// The runner writes done and exits between the status read and the
	// liveness check.
	alive := func() bool {
		routeFile(t, dir, "findings.json", cleanJSON)
		routeFile(t, dir, "status", "done\n")
		return false
	}
	var out bytes.Buffer
	if err := waitReview(filepath.Base(dir), dir, alive, &out); err != nil {
		t.Fatalf("a review that finished was reported as %v", err)
	}
	if !strings.Contains(out.String(), ": done\n") {
		t.Errorf("output %q, want done", out.String())
	}
}

const cleanJSON = `{"verdict":"mergeable","findings":[]}`

func TestSpawnReviewUsage(t *testing.T) {
	spawnHermetic(t, oneP1)
	prompt := routeFile(t, t.TempDir(), "prompt.md", "Check.\n")
	for name, args := range map[string][]string{
		"no prompt":      {},
		"force no why":   {"--prompt", prompt, "--force", "opus/high"},
		"why no force":   {"--prompt", prompt, "--why", "x"},
		"bad force":      {"--prompt", prompt, "--force", "opus", "--why", "x"},
		"negative round": {"--prompt", prompt, "--round", "-1"},
		"extra argument": {"--prompt", prompt, "extra"},
	} {
		if code, _, _ := spawnRun(t, args...); code != 2 {
			t.Errorf("%s: exit %d, want 2", name, code)
		}
	}
	if code := run([]string{"spawn", "nothing"}, io.Discard, io.Discard); code != 2 {
		t.Errorf("qm spawn nothing: exit %d, want 2", code)
	}
}

// sleepyClaude is a claude whose child, sleep, writes its pid to pidFile
// and then runs for a minute.
func sleepyClaude(t *testing.T) (pidFile string) {
	t.Helper()
	dir := t.TempDir()
	pidFile = filepath.Join(dir, "sleep.pid")
	claude := filepath.Join(dir, "claude")
	script := "#!/bin/sh\ncat > /dev/null\nsh -c 'echo $$ > " + pidFile + ".tmp; mv " + pidFile + ".tmp " + pidFile + "; exec sleep 60'\n"
	if err := os.WriteFile(claude, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	claudeLook = func(string) (string, error) { return claude, nil }
	return pidFile
}

// gone waits up to 5 s for the process with the pid in file to end.
func gone(t *testing.T, file string) bool {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return true
		}
	}
	return false
}

func newReviewJobID(t *testing.T) (id, dir string) {
	t.Helper()
	dir, err := spawn.NewReviewJob(spawn.ReviewJob{Repo: t.TempDir(), Decision: route.Decision{Kind: "review", Tier: route.Tier{Model: "sonnet", Effort: "medium"}}}, []byte("Check.\n"), testChange())
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Base(dir), dir
}

func TestRunReviewJobTimeout(t *testing.T) {
	spawnHermetic(t, oneP1)
	pidFile := sleepyClaude(t)
	old := reviewTimeout
	reviewTimeout = time.Second
	t.Cleanup(func() { reviewTimeout = old })
	id, dir := newReviewJobID(t)
	if err := runReviewJob(id); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("runReviewJob: %v, want a deadline error", err)
	}
	if st, _ := spawn.ReviewStatus(dir); st != spawn.StatusFailed {
		t.Errorf("status %q, want failed", st)
	}
	if !gone(t, pidFile) {
		t.Error("the reviewer's child outlived the deadline")
	}
}

func TestRunReviewJobSignal(t *testing.T) {
	spawnHermetic(t, oneP1)
	pidFile := sleepyClaude(t)
	id, dir := newReviewJobID(t)
	done := make(chan error, 1)
	go func() { done <- runReviewJob(id) }()
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(pidFile); err == nil {
			break
		}
		if time.Now().After(end) {
			t.Fatal("the reviewer did not start")
		}
	}
	// runReviewJob catches SIGTERM while it runs, so this does not end the test.
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("runReviewJob: %v, want a cancel error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runReviewJob still running 10 s after SIGTERM")
	}
	if st, _ := spawn.ReviewStatus(dir); st != spawn.StatusFailed {
		t.Errorf("status %q, want failed", st)
	}
	if !gone(t, pidFile) {
		t.Error("the reviewer's child outlived SIGTERM to the runner")
	}
}

func TestStartFailureMarksFailed(t *testing.T) {
	spawnHermetic(t, oneP1)
	repo := routeRepo(t)
	prompt := routeFile(t, t.TempDir(), "prompt.md", "Check.\n")
	startReview = func(_, _ string) (*os.Process, error) { return nil, errors.New("no pid file") }
	if code, _, errOut := spawnRun(t, "--prompt", prompt, "--base", "main", "--repo", repo); code != 1 || !strings.Contains(errOut, "no pid file") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
	entries, err := os.ReadDir(filepath.Join(os.Getenv("XDG_STATE_HOME"), "quartermaster", "reviews"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("review jobs: %v, %v", entries, err)
	}
	if code, _, errOut := spawnRun(t, "--status", entries[0].Name(), "--wait"); code != 1 || !strings.Contains(errOut, "failed") {
		t.Errorf("--status --wait after a failed start: exit %d, stderr %q", code, errOut)
	}
}

// onlyJob is the one review job folder.
func onlyJob(t *testing.T) string {
	t.Helper()
	root, err := spawn.ReviewsDir()
	if err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(root)
	if err != nil || len(ents) != 1 {
		t.Fatalf("review folders %v (%v), want one", ents, err)
	}
	return filepath.Join(root, ents[0].Name())
}

func TestSpawnReviewRefusesAnEmptyDiff(t *testing.T) {
	prompt := func() string {
		return routeFile(t, t.TempDir(), "prompt.md", "Check that the notes file reads well.\n")
	}
	for name, tc := range map[string]struct {
		detach bool
		want   string
	}{
		"branch":   {false, "branch feature has no changes in git diff HEAD...HEAD"},
		"detached": {true, "this checkout (detached HEAD) has no changes"},
	} {
		t.Run(name, func(t *testing.T) {
			spawnHermetic(t, oneP1)
			repo := routeRepo(t)
			if tc.detach {
				routeGit(t, repo, "checkout", "-q", "--detach")
			}
			code, out, errOut := spawnRun(t, "--prompt", prompt(), "--base", "HEAD", "--repo", repo, "--wait")
			if code != 1 || out != "" || !strings.Contains(errOut, tc.want) || !strings.Contains(errOut, "nothing to review") || !strings.Contains(errOut, "--repo") {
				t.Errorf("exit %d, stdout %q, stderr %q: want a refusal containing %q", code, out, errOut, tc.want)
			}
			if root, err := spawn.ReviewsDir(); err == nil {
				if ents, _ := os.ReadDir(root); len(ents) != 0 {
					t.Errorf("a refused review left %d job folders", len(ents))
				}
			}
		})
	}
}

func TestSpawnReviewHandsTheChangeToTheReviewer(t *testing.T) {
	argsFile := spawnHermetic(t, oneP1)
	repo := routeRepo(t)
	prompt := routeFile(t, t.TempDir(), "prompt.md", "Check that the notes file reads well.\n")
	if code, _, errOut := spawnRun(t, "--prompt", prompt, "--base", "main", "--repo", repo, "--wait"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	job := onlyJob(t)
	for name, want := range map[string]string{"change.diff": "+three", "commits.txt": "second", "files.txt": "3\t0\tnotes.txt"} {
		b, err := os.ReadFile(filepath.Join(job, name))
		if err != nil || !strings.Contains(string(b), want) {
			t.Errorf("%s (%v) lacks %q:\n%s", name, err, want, b)
		}
		if fi, err := os.Stat(filepath.Join(job, name)); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v, %v, want 0600", name, fi, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(job, "prompt.md")); string(b) != "Check that the notes file reads well.\n" {
		t.Errorf("prompt.md changed: %q", b)
	}
	stdin, err := os.ReadFile(filepath.Join(filepath.Dir(argsFile), "stdin.txt"))
	if err != nil || !strings.Contains(string(stdin), "## The change under review") || !strings.Contains(string(stdin), filepath.Join(job, "change.diff")) {
		t.Errorf("reviewer prompt (%v):\n%s", err, stdin)
	}
	if args, _ := os.ReadFile(argsFile); !strings.Contains(string(args), "--tools Read,Grep,Glob ") || !strings.Contains(string(args), "--add-dir "+job+" ") || strings.Contains(string(args), "Bash") {
		t.Errorf("claude args %q", args)
	}
}

func TestSpawnReviewDiffRange(t *testing.T) {
	spawnHermetic(t, oneP1)
	repo := routeRepo(t)
	routeFile(t, repo, "notes.txt", "one\ntwo\nthree\nfour\n")
	routeGit(t, repo, "commit", "-q", "-am", "fix round")
	prompt := routeFile(t, t.TempDir(), "prompt.md", "Check the fix.\n")

	code, _, errOut := spawnRun(t, "--prompt", prompt, "--diff", "HEAD~1..HEAD", "--repo", repo, "--wait")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	job := onlyJob(t)
	diff, _ := os.ReadFile(filepath.Join(job, "change.diff"))
	commits, _ := os.ReadFile(filepath.Join(job, "commits.txt"))
	files, _ := os.ReadFile(filepath.Join(job, "files.txt"))
	if !strings.Contains(string(diff), "+four") || strings.Contains(string(diff), "README") || strings.Contains(string(diff), "+two") ||
		!strings.Contains(string(commits), "fix round") || strings.Contains(string(commits), "second") || !strings.Contains(string(files), "1\t0\tnotes.txt") {
		t.Errorf("a fix round got the whole branch:\ndiff:\n%s\ncommits:\n%s\nfiles:\n%s", diff, commits, files)
	}
}

func TestSpawnReviewDiffRefused(t *testing.T) {
	spawnHermetic(t, oneP1)
	repo := routeRepo(t)
	prompt := routeFile(t, t.TempDir(), "prompt.md", "Check.\n")
	for _, spec := range []string{"main", "--output=/tmp/x..HEAD", "main..--output=/tmp/x", "nosuch..HEAD", "main.."} {
		code, out, errOut := spawnRun(t, "--prompt", prompt, "--diff", spec, "--repo", repo, "--wait")
		if code != 1 || out != "" || !strings.Contains(errOut, "--diff") {
			t.Errorf("--diff %q: exit %d, stdout %q, stderr %q", spec, code, out, errOut)
		}
	}
	if root, err := spawn.ReviewsDir(); err == nil {
		if ents, _ := os.ReadDir(root); len(ents) != 0 {
			t.Errorf("refused ranges left %d job folders", len(ents))
		}
	}
}

// testChange is the change files of a job started by hand, as qm spawn review
// makes them.
func testChange() *reviewin.Change {
	return &reviewin.Change{
		Summary: reviewin.Summary{Spec: "main...HEAD", Mode: "...", Left: "1111", Right: "2222", Head: "2222"},
		Files:   []reviewin.File{{Name: "change.diff", Data: []byte("diff\n")}, {Name: "commits.txt", Data: []byte("commit 2222\n")}, {Name: "files.txt", Data: []byte("added\tremoved\tpath\n")}},
	}
}
