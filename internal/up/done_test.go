package up

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/route"
)

// fakeDir is a DirRunner answering from a table keyed by "bin args"; a
// missing key fails; a key "dir: bin args" answers in that folder only. It records each call as "dir: bin args".
type fakeDir struct {
	out   map[string]string
	calls []string
}

func (f *fakeDir) run(_ context.Context, dir, bin string, args ...string) ([]byte, error) {
	key := bin + " " + strings.Join(args, " ")
	f.calls = append(f.calls, dir+": "+key)
	if out, ok := f.out[dir+": "+key]; ok {
		return []byte(out), nil
	}
	if out, ok := f.out[key]; ok {
		return []byte(out), nil
	}
	return nil, errors.New(bin + ": failed")
}

const ghList = "gh pr list --head feature-x --state all --json state,createdAt --limit 20"

// prs is gh's answer: one pull request per state, each created at the time
// that follows it.
func prs(stateAt ...any) string {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < len(stateAt); i += 2 {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"state":"` + stateAt[i].(string) + `","createdAt":"` + stateAt[i+1].(time.Time).UTC().Format(time.RFC3339) + `"}`)
	}
	b.WriteString("]")
	return b.String()
}

// worker is a worker candidate on branch feature-x in an existing folder,
// spawned from the project folder.
func worker(t *testing.T) (Candidate, string) {
	t.Helper()
	project := t.TempDir()
	cwd := t.TempDir()
	return Candidate{
		Role: RoleWorker, SessionID: warmID, Name: "w1", Cwd: cwd, Branch: "feature-x", Started: now,
		Record: &route.Record{Project: project, Branch: "main"},
	}, project
}

// commonDir is the question which repository a folder is of.
const commonDir = "git rev-parse --path-format=absolute --git-common-dir"

// originMain answers for one repository, whatever the folder.
func originMain() map[string]string {
	return map[string]string{
		"git symbolic-ref --short refs/remotes/origin/HEAD": "origin/main\n",
		commonDir: "/work/shop/.git\n",
	}
}

// gh answers for the repository of the folder it runs in: it is not asked in
// a Record's project folder that is not known to be of the worker's
// repository, and the worker is then not judged by it.
func TestFinishedAsksOnlyTheWorkersRepository(t *testing.T) {
	for _, tc := range []struct {
		name            string
		project, folder string // what git says in each; "" is not a repository
	}{
		{"another repository", "/work/other/.git\n", "/work/shop/.git\n"},
		{"project not a repository", "", "/work/shop/.git\n"},
		{"worker's folder not a repository", "/work/shop/.git\n", ""},
		{"neither a repository", "", ""},
		{"git names nothing", "\n", "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, project := worker(t)
			f := &fakeDir{out: originMain()}
			delete(f.out, commonDir)
			for dir, out := range map[string]string{project: tc.project, c.Cwd: tc.folder} {
				if out != "" {
					f.out[dir+": "+commonDir] = out
				}
			}
			f.out[ghList] = prs("MERGED", now.Add(time.Minute))
			if why, sure := finished(Project{}, f.run)(c); why != "" || sure {
				t.Errorf("finished = %q, sure %v, want not judged", why, sure)
			}
			for _, call := range f.calls {
				if strings.Contains(call, ": gh ") {
					t.Errorf("gh was run: %q", call)
				}
			}
		})
	}
}

// Two worktrees of one repository, by real git: the Record's project folder is
// one, the worker's folder a folder of the other.
func TestSameRepoLinkedWorktree(t *testing.T) {
	root, linked := projectRepo(t)
	other, _ := projectRepo(t)
	sub := filepath.Join(linked, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{root, linked, true}, {root, sub, true}, {sub, root, true}, {root, root, true},
		{root, other, false}, {other, linked, false}, {root, t.TempDir(), false},
	} {
		if got := sameRepo(context.Background(), ExecDir, tc.a, tc.b); got != tc.want {
			t.Errorf("sameRepo(%s, %s) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestFinishedGhMerged(t *testing.T) {
	c, project := worker(t)
	f := &fakeDir{out: originMain()}
	f.out[ghList] = prs("MERGED", now.Add(time.Minute))
	if got := Finished(Project{}, f.run)(c); got != "merged" {
		t.Errorf("Finished = %q, want merged", got)
	}
	if !strings.Contains(strings.Join(f.calls, "\n"), project+": "+ghList) {
		t.Errorf("gh not run in the Record's project folder: %v", f.calls)
	}
}

func TestFinishedGhClosed(t *testing.T) {
	c, _ := worker(t)
	f := &fakeDir{out: originMain()}
	f.out[ghList] = prs("CLOSED", now.Add(time.Minute))
	if got := Finished(Project{}, f.run)(c); got != "closed" {
		t.Errorf("Finished = %q, want closed", got)
	}
}

func TestFinishedGhIgnoresOlderPR(t *testing.T) {
	// A reused branch name: its earlier pull request, merged before this
	// worker started, is not the worker's. The newest one after the start
	// decides.
	c, _ := worker(t)
	f := &fakeDir{out: originMain()}
	f.out[ghList] = prs("MERGED", now.Add(-time.Hour))
	if got := Finished(Project{}, f.run)(c); got != "" {
		t.Errorf("an older merged PR: Finished = %q, want unfinished", got)
	}
	f.out[ghList] = prs("CLOSED", now.Add(-time.Hour), "OPEN", now.Add(time.Minute), "MERGED", now.Add(-2*time.Hour))
	if got := Finished(Project{}, f.run)(c); got != "" {
		t.Errorf("an open PR after the start: Finished = %q, want unfinished", got)
	}
	f.out[ghList] = prs("OPEN", now.Add(time.Minute), "MERGED", now.Add(2*time.Minute))
	if got := Finished(Project{}, f.run)(c); got != "merged" {
		t.Errorf("the newest PR merged: Finished = %q, want merged", got)
	}
}

func TestFinishedRestartedWorkerSeesItsLinePR(t *testing.T) {
	// A worker restarted fresh: its predecessor opened the pull request
	// before the restart, then it was merged. The line's start counts, not
	// the restart's.
	w := newWorld(t)
	w.project.Coordinator = Session{}
	old := profiled(w, largeID, "w1", "large", now.Add(-5*time.Hour))
	fresh := profiled(w, smallID, "w1", "small", now.Add(-time.Hour))
	fresh.Replaces = largeID
	f := &fakeDir{out: originMain()}
	for _, r := range []*route.Record{&old, &fresh} {
		r.Branch = "feature-x"
	}
	f.out[ghList] = prs("MERGED", now.Add(-3*time.Hour))
	env := w.env(t, nil)
	env.Finished = Finished(w.project, f.run)
	cs, err := Gather(&w.project, []route.Record{old, fresh}, env)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].SessionID != smallID || !cs[0].LineStarted.Equal(old.Time) || cs[0].Finished != "merged" {
		t.Errorf("candidates %+v: want the restarted worker, its line started at %v, merged", cs, old.Time)
	}
	// A pull request older than the whole line is still not this worker's.
	f.out[ghList] = prs("MERGED", now.Add(-6*time.Hour))
	cs, err = Gather(&w.project, []route.Record{old, fresh}, env)
	if err != nil || cs[0].Finished != "" {
		t.Errorf("a PR before the line started: Finished %q, %v", cs[0].Finished, err)
	}
}

func TestFinishedGhOpenOrNone(t *testing.T) {
	c, _ := worker(t)
	for _, out := range []string{prs("OPEN", now.Add(time.Minute)), `[]`} {
		f := &fakeDir{out: originMain()}
		f.out[ghList] = out
		f.out["git merge-base --is-ancestor feature-x main"] = ""
		if got := Finished(Project{}, f.run)(c); got != "" {
			t.Errorf("gh says %s: Finished = %q, want unfinished", out, got)
		}
	}
}

// tipLog is the git call that reads the branch tip's commit time.
const tipLog = "git log -1 --format=%ct feature-x"

func unix(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) + "\n" }

func TestFinishedGitFallback(t *testing.T) {
	c, _ := worker(t)
	f := &fakeDir{out: originMain()}
	f.out["git merge-base --is-ancestor feature-x main"] = ""
	f.out[tipLog] = unix(now.Add(time.Hour))
	if got := Finished(Project{}, f.run)(c); got != "merged (git)" {
		t.Errorf("Finished = %q, want merged (git)", got)
	}
	want := c.Cwd + ": git merge-base --is-ancestor feature-x main"
	if !strings.Contains(strings.Join(f.calls, "\n"), want) {
		t.Errorf("calls %v lack %q", f.calls, want)
	}
	// Not an ancestor: unfinished.
	f = &fakeDir{out: originMain()}
	if got := Finished(Project{}, f.run)(c); got != "" {
		t.Errorf("not an ancestor: Finished = %q", got)
	}
}

func TestFinishedGitFallbackNoCommitsYet(t *testing.T) {
	c, _ := worker(t)
	// The tip predates the worker: the branch has no commits of its own, so
	// it is an ancestor of main without being merged.
	for _, tip := range []string{unix(now.Add(-time.Hour)), unix(now), "not a time\n"} {
		f := &fakeDir{out: originMain()}
		f.out["git merge-base --is-ancestor feature-x main"] = ""
		f.out[tipLog] = tip
		if got := Finished(Project{}, f.run)(c); got != "" {
			t.Errorf("tip %q: Finished = %q, want unfinished", tip, got)
		}
	}
	f := &fakeDir{out: originMain()}
	f.out["git merge-base --is-ancestor feature-x main"] = ""
	if got := Finished(Project{}, f.run)(c); got != "" {
		t.Errorf("git log failing: Finished = %q, want unfinished", got)
	}
}

func TestFinishedDefaultBranchWithoutOrigin(t *testing.T) {
	c, _ := worker(t)
	f := &fakeDir{out: map[string]string{
		"git rev-parse --verify --quiet refs/heads/master": "abc\n",
		"git merge-base --is-ancestor feature-x master":    "",
		tipLog: unix(now.Add(time.Hour)),
	}}
	if got := Finished(Project{}, f.run)(c); got != "merged (git)" {
		t.Errorf("Finished = %q, want merged (git) against master", got)
	}
}

func TestFinishedIgnoresDefaultBranch(t *testing.T) {
	c, _ := worker(t)
	for _, b := range []string{"main", "HEAD", ""} {
		c.Branch = b
		f := &fakeDir{out: originMain()}
		f.out["git merge-base --is-ancestor main main"] = ""
		f.out["gh pr list --head main --state all --json state,createdAt --limit 20"] = prs("MERGED", now.Add(time.Minute))
		if got := Finished(Project{}, f.run)(c); got != "" {
			t.Errorf("branch %q: Finished = %q, want unfinished", b, got)
		}
		for _, call := range f.calls {
			if strings.Contains(call, "gh ") || strings.Contains(call, "merge-base") {
				t.Errorf("branch %q: ran %s", b, call)
			}
		}
	}
	// No default branch found: main and master are still never checked.
	c.Branch = "master"
	f := &fakeDir{out: map[string]string{"git merge-base --is-ancestor master master": ""}}
	if got := Finished(Project{}, f.run)(c); got != "" {
		t.Errorf("master without a default branch: Finished = %q", got)
	}
}

func TestFinishedMarkedDone(t *testing.T) {
	c, _ := worker(t)
	p := Project{Done: map[string]time.Time{c.SessionID: now}}
	f := &fakeDir{}
	if got := Finished(p, f.run)(c); got != "marked done" {
		t.Errorf("Finished = %q, want marked done", got)
	}
	if len(f.calls) != 0 {
		t.Errorf("ran %v for a worker marked done", f.calls)
	}
}

func TestFinishedWorktreeGone(t *testing.T) {
	c, _ := worker(t)
	c.Cwd = filepath.Join(c.Cwd, "removed")
	f := &fakeDir{}
	if got := Finished(Project{}, f.run)(c); got != "worktree gone" {
		t.Errorf("Finished = %q, want worktree gone", got)
	}
}

func TestExecDirRunsInDir(t *testing.T) {
	dir := t.TempDir()
	out, err := ExecDir(context.Background(), dir, "pwd")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := filepath.EvalSymlinks(strings.TrimSpace(string(out)))
	want, _ := filepath.EvalSymlinks(dir)
	if got != want {
		t.Errorf("ExecDir ran in %q, want %q", got, want)
	}
	if _, err := ExecDir(context.Background(), dir, "sh", "-c", "echo oops >&2; exit 3"); err == nil || !strings.Contains(err.Error(), "oops") {
		t.Errorf("failure = %v, want the standard error in it", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
}

// ExecDir answers for its folder: git and gh (which asks git) do not see
// the GIT_* variables of qm's environment.
func TestExecDirIgnoresInheritedGitVariables(t *testing.T) {
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q", "-b", "here").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "elsewhere.git"))
	out, err := ExecDir(context.Background(), dir, "git", "symbolic-ref", "--short", "HEAD")
	if err != nil || gitLine(out) != "here" {
		t.Errorf("ExecDir git = %q, %v, want here", out, err)
	}
	out, err = ExecDir(context.Background(), dir, "sh", "-c", `printf %s "$GIT_DIR"`)
	if err != nil || len(out) == 0 {
		t.Errorf("another command lost GIT_DIR: %q, %v", out, err)
	}
}

// gh answers for GH_REPO, whatever folder it runs in: it must not reach it.
// GH_HOST only picks among the folder's own remotes, and stays.
func TestExecDirKeepsGHRepoFromGh(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\necho \"repo=$GH_REPO host=$GH_HOST dir=$GIT_DIR\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GH_REPO", "owner/other")
	t.Setenv("GH_HOST", "github.example")
	t.Setenv("GIT_DIR", "/elsewhere")
	out, err := ExecDir(context.Background(), t.TempDir(), "gh")
	if want := "repo= host=github.example dir=\n"; err != nil || string(out) != want {
		t.Errorf("gh saw %q, %v, want %q", out, err, want)
	}
}
