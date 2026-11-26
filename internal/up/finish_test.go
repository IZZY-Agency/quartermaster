package up

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/route"
)

// finishGit runs git in dir for a fixture, without the GIT_* variables of
// the test's environment and with a fixed identity.
func finishGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "GIT_") })
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// finishFixture is a repository whose main checkout is on main, pushed to a
// bare origin, with a linked worktree on branch feature-x one commit ahead.
type finishFixture struct {
	main, wt, origin, head string
	gh                     *ghRunner
}

func newFinishFixture(t *testing.T) *finishFixture {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &finishFixture{main: filepath.Join(base, "main"), wt: filepath.Join(base, "wt"), origin: filepath.Join(base, "origin.git")}
	finishGit(t, base, "init", "-q", "--bare", "-b", "main", f.origin)
	finishGit(t, base, "init", "-q", "-b", "main", f.main)
	finishGit(t, f.main, "commit", "-q", "--allow-empty", "-m", "init")
	// origin names the pull request's repository; git reaches it in the
	// bare folder.
	finishGit(t, f.main, "remote", "add", "origin", "https://github.com/o/r.git")
	finishGit(t, f.main, "config", "url."+f.origin+".insteadOf", "https://github.com/o/r.git")
	finishGit(t, f.main, "push", "-q", "-u", "origin", "main")
	finishGit(t, f.main, "worktree", "add", "-q", "-b", "feature-x", f.wt)
	if err := os.WriteFile(filepath.Join(f.wt, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	finishGit(t, f.wt, "add", "a.txt")
	finishGit(t, f.wt, "commit", "-q", "-m", "feature")
	f.head = finishGit(t, f.wt, "rev-parse", "HEAD")
	finishGit(t, f.wt, "push", "-q", "origin", "feature-x")
	f.gh = &ghRunner{out: map[string]string{ghFinishList: f.prJSON("MERGED", f.head)}}
	return f
}

const ghFinishList = "gh pr list --head feature-x --state all --json " + ghFinishFields + " --limit 20"

// prJSON is gh's answer: one pull request of feature-x in state at head.
func (f *finishFixture) prJSON(state, head string) string {
	return fmt.Sprintf(`[{"number":7,"state":%q,"headRefOid":%q,"isCrossRepository":false,"createdAt":"2026-11-20T10:00:00Z","url":"https://github.com/o/r/pull/7","headRepository":{"name":"r"},"headRepositoryOwner":{"login":"o"}}]`, state, head)
}

// moveWorktreeAway moves the worker's folder away; git still lists it.
func (f *finishFixture) moveWorktreeAway(t *testing.T) {
	t.Helper()
	if err := os.Rename(f.wt, f.wt+"-moved"); err != nil {
		t.Fatal(err)
	}
}

func (f *finishFixture) worker() Candidate {
	return Candidate{Role: RoleWorker, SessionID: "w1", Name: "w1", Cwd: f.wt, Branch: "feature-x", Record: &route.Record{Project: f.main}}
}

func (f *finishFixture) proof(t *testing.T) Proof {
	t.Helper()
	pr, err := ProveMerged(context.Background(), f.gh.run, f.worker())
	if err != nil {
		t.Fatalf("ProveMerged: %v", err)
	}
	return pr
}

func (f *finishFixture) finisher(procs ...ProcCwd) Finisher {
	return Finisher{Run: f.gh.run, Procs: func(context.Context) ([]ProcCwd, error) { return procs, nil }, Sleep: func(time.Duration) {}}
}

// ghRunner runs git for real (ExecDir) and answers gh from a table keyed by
// "gh args"; a missing key fails, an answer starting "ERR " fails with the
// rest. It records each gh call as "dir: gh args".
type ghRunner struct {
	out   map[string]string
	calls []string
}

func (g *ghRunner) run(ctx context.Context, dir, bin string, args ...string) ([]byte, error) {
	if bin == "git" {
		return ExecDir(ctx, dir, bin, args...)
	}
	key := bin + " " + strings.Join(args, " ")
	g.calls = append(g.calls, dir+": "+key)
	out, ok := g.out[key]
	if !ok {
		return nil, errors.New(bin + ": failed")
	}
	if msg, failed := strings.CutPrefix(out, "ERR "); failed {
		return nil, errors.New(msg)
	}
	return []byte(out), nil
}

func TestProveMergedFindsTheMergedHead(t *testing.T) {
	f := newFinishFixture(t)
	pr := f.proof(t)
	if pr.Head != f.head || pr.Number != 7 || pr.Repo != "o/r" || pr.Branch != "feature-x" || pr.Default != "main" || realPath(pr.RepoDir) != f.main {
		t.Errorf("proof = %+v", pr)
	}
	// gh runs in the main checkout, not in the worktree that goes.
	if len(f.gh.calls) != 1 || !strings.HasPrefix(f.gh.calls[0], f.main+": ") {
		t.Errorf("gh calls = %q, want one in %s", f.gh.calls, f.main)
	}
}

func TestProveMergedRefusals(t *testing.T) {
	cases := []struct {
		name string
		prs  func(f *finishFixture) string
		want string
	}{
		{"unmerged", func(f *finishFixture) string { return f.prJSON("OPEN", f.head) }, "still open"},
		{"closed", func(f *finishFixture) string { return f.prJSON("CLOSED", f.head) }, "is closed, not merged"},
		{"head moved", func(f *finishFixture) string { return f.prJSON("MERGED", strings.Repeat("a", 40)) }, "the branch moved"},
		{"no pull request", func(*finishFixture) string { return "[]" }, "has no pull request"},
		{"fork only", func(f *finishFixture) string {
			return strings.Replace(f.prJSON("MERGED", f.head), `"isCrossRepository":false`, `"isCrossRepository":true`, 1)
		}, "has no pull request"},
		{"gh fails", func(*finishFixture) string { return "ERR gh: auth" }, "were not read"},
		{"no head repository", func(f *finishFixture) string {
			return strings.Replace(f.prJSON("MERGED", f.head), `"login":"o"`, `"login":""`, 1)
		}, "did not name the repository"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFinishFixture(t)
			f.gh.out[ghFinishList] = c.prs(f)
			if _, err := ProveMerged(context.Background(), f.gh.run, f.worker()); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("ProveMerged error = %v, want %q", err, c.want)
			}
		})
	}
}

func TestProveMergedOpenPullRequestBesideTheMergedOne(t *testing.T) {
	f := newFinishFixture(t)
	merged := f.prJSON("MERGED", f.head)
	open := strings.Replace(f.prJSON("OPEN", f.head), `"number":7`, `"number":8`, 1)
	f.gh.out[ghFinishList] = merged[:len(merged)-1] + "," + open[1:]
	if _, err := ProveMerged(context.Background(), f.gh.run, f.worker()); err == nil || !strings.Contains(err.Error(), "#8") {
		t.Errorf("ProveMerged error = %v, want pull request #8 still open", err)
	}
}

func TestProveMergedNeedsABranchItCanName(t *testing.T) {
	f := newFinishFixture(t)
	// The folder is gone, so the Record's branch is the one.
	f.moveWorktreeAway(t)
	for _, b := range []string{"", "HEAD", "main", "-x", "a..b"} {
		c := f.worker()
		c.Branch = b
		if _, err := ProveMerged(context.Background(), f.gh.run, c); err == nil {
			t.Errorf("branch %q: proved", b)
		}
	}
	if len(f.gh.calls) != 0 {
		t.Errorf("gh asked: %q", f.gh.calls)
	}
}

func TestProveMergedNeedsTheLocalBranch(t *testing.T) {
	f := newFinishFixture(t)
	f.moveWorktreeAway(t)
	c := f.worker()
	c.Branch = "feature-y"
	if _, err := ProveMerged(context.Background(), f.gh.run, c); err == nil || !strings.Contains(err.Error(), "no local branch") {
		t.Errorf("ProveMerged error = %v, want no local branch", err)
	}
}

func TestProveMergedWhenTheWorktreeIsGone(t *testing.T) {
	f := newFinishFixture(t)
	// Gone, but git still lists it: the repository held it.
	f.moveWorktreeAway(t)
	if pr, err := ProveMerged(context.Background(), f.gh.run, f.worker()); err != nil || realPath(pr.RepoDir) != f.main {
		t.Errorf("listed: ProveMerged = %+v, %v; want the main checkout", pr, err)
	}
	// Gone from inside the main checkout.
	c := f.worker()
	c.Cwd = filepath.Join(f.main, ".claude", "worktrees", "gone")
	if pr, err := ProveMerged(context.Background(), f.gh.run, c); err != nil || realPath(pr.RepoDir) != f.main {
		t.Errorf("inside: ProveMerged = %+v, %v; want the main checkout", pr, err)
	}
	// Gone from elsewhere: the project's repository need not be its.
	c.Cwd = filepath.Join(filepath.Dir(f.main), "elsewhere")
	if _, err := ProveMerged(context.Background(), f.gh.run, c); err == nil || !strings.Contains(err.Error(), "no repository of the project held it") {
		t.Errorf("elsewhere: ProveMerged error = %v", err)
	}
}

func TestProveMergedReadsTheBranchOfTheFolder(t *testing.T) {
	f := newFinishFixture(t)
	// The worker switched branches since its Record named feature-x.
	finishGit(t, f.wt, "switch", "-q", "-c", "feature-y")
	f.gh.out["gh pr list --head feature-y --state all --json "+ghFinishFields+" --limit 20"] = "[]"
	if _, err := ProveMerged(context.Background(), f.gh.run, f.worker()); err == nil || !strings.Contains(err.Error(), "branch feature-y has no pull request") {
		t.Errorf("ProveMerged error = %v, want feature-y read from the folder", err)
	}
	// A hand-started worker has no branch of its own: the folder's is used.
	finishGit(t, f.wt, "switch", "-q", "feature-x")
	c := f.worker()
	c.Branch, c.Record = "", nil
	if pr, err := ProveMerged(context.Background(), f.gh.run, c); err != nil || pr.Branch != "feature-x" {
		t.Errorf("adopted: ProveMerged = %+v, %v", pr, err)
	}
	// A detached folder is on no branch.
	finishGit(t, f.wt, "switch", "-q", "--detach")
	if _, err := ProveMerged(context.Background(), f.gh.run, f.worker()); err == nil || !strings.Contains(err.Error(), "on no branch") {
		t.Errorf("detached: ProveMerged error = %v", err)
	}
}

func TestRemoveWorktreeRemovesACleanOne(t *testing.T) {
	f := newFinishFixture(t)
	pr := f.proof(t)
	res, removed := f.finisher(ProcCwd{PID: 9, Cmd: "zsh", Cwd: f.main}).RemoveWorktree(context.Background(), pr, f.wt, false)
	if removed == "" || !strings.HasSuffix(res.Line, ": removed") {
		t.Fatalf("RemoveWorktree = %q, %q", res.Line, removed)
	}
	if _, err := os.Stat(f.wt); !os.IsNotExist(err) {
		t.Errorf("worktree still there: %v", err)
	}
}

func TestRemoveWorktreeDryRunChangesNothing(t *testing.T) {
	f := newFinishFixture(t)
	fin := f.finisher()
	fin.DryRun = true
	res, removed := fin.RemoveWorktree(context.Background(), f.proof(t), f.wt, false)
	if removed == "" || !strings.Contains(res.Line, "would remove") {
		t.Errorf("RemoveWorktree = %q, %q", res.Line, removed)
	}
	if !isDir(f.wt) {
		t.Error("dry run removed the worktree")
	}
}

func TestRemoveWorktreeRefusals(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, f *finishFixture) (cwd string, procs []ProcCwd)
		want  string
	}{
		{"modified", func(t *testing.T, f *finishFixture) (string, []ProcCwd) {
			mustWrite(t, filepath.Join(f.wt, "a.txt"), "changed\n")
			return f.wt, nil
		}, "1 uncommitted or untracked file(s)"},
		{"untracked", func(t *testing.T, f *finishFixture) (string, []ProcCwd) {
			mustWrite(t, filepath.Join(f.wt, "new", "b.txt"), "b\n")
			return f.wt, nil
		}, "1 uncommitted or untracked file(s)"},
		{"in use", func(_ *testing.T, f *finishFixture) (string, []ProcCwd) {
			return f.wt, []ProcCwd{{PID: 42, Cmd: "fish", Cwd: filepath.Join(f.wt, ".")}}
		}, "current folder of pid 42 (fish)"},
		{"in use below", func(t *testing.T, f *finishFixture) (string, []ProcCwd) {
			sub := filepath.Join(f.wt, "sub")
			if err := os.Mkdir(sub, 0o755); err != nil {
				t.Fatal(err)
			}
			return f.wt, []ProcCwd{{PID: 43, Cmd: "node", Cwd: sub}}
		}, "pid 43 (node)"},
		{"main checkout", func(_ *testing.T, f *finishFixture) (string, []ProcCwd) { return f.main, nil }, "main checkout, never removed"},
		{"other branch", func(t *testing.T, f *finishFixture) (string, []ProcCwd) {
			finishGit(t, f.wt, "switch", "-q", "-c", "other")
			return f.wt, nil
		}, "not on branch feature-x"},
		{"head moved", func(t *testing.T, f *finishFixture) (string, []ProcCwd) {
			finishGit(t, f.wt, "commit", "-q", "--allow-empty", "-m", "more")
			return f.wt, nil
		}, "not the merged head"},
		{"not absolute", func(*testing.T, *finishFixture) (string, []ProcCwd) { return "wt", nil }, "not an absolute path"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFinishFixture(t)
			pr := f.proof(t)
			cwd, procs := c.setup(t, f)
			res, removed := f.finisher(procs...).RemoveWorktree(context.Background(), pr, cwd, false)
			if removed != "" || !strings.Contains(res.Line, c.want) {
				t.Errorf("RemoveWorktree = %q, %q; want left: %q", res.Line, removed, c.want)
			}
			if !isDir(f.wt) {
				t.Error("worktree removed")
			}
		})
	}
}

func TestRemoveWorktreeProcessesNotReadLeavesIt(t *testing.T) {
	f := newFinishFixture(t)
	fin := f.finisher()
	fin.Procs = func(context.Context) ([]ProcCwd, error) { return nil, errors.New("lsof: failed") }
	if res, removed := fin.RemoveWorktree(context.Background(), f.proof(t), f.wt, false); removed != "" || !strings.Contains(res.Line, "were not read") {
		t.Errorf("RemoveWorktree = %q, %q", res.Line, removed)
	}
}

func TestRemoveWorktreeWaitsForTheClosedWorkerToEnd(t *testing.T) {
	f := newFinishFixture(t)
	reads := 0
	fin := f.finisher()
	fin.Procs = func(context.Context) ([]ProcCwd, error) {
		reads++
		if reads < 3 {
			return []ProcCwd{{PID: 42, Cmd: "claude", Cwd: f.wt}}, nil
		}
		return nil, nil
	}
	if res, removed := fin.RemoveWorktree(context.Background(), f.proof(t), f.wt, true); removed == "" {
		t.Errorf("RemoveWorktree = %q, want removed once the worker ended", res.Line)
	}
	// Without a close it does not wait.
	f = newFinishFixture(t)
	fin = f.finisher(ProcCwd{PID: 42, Cmd: "claude", Cwd: f.wt})
	fin.Sleep = func(time.Duration) { t.Error("waited without a close") }
	if _, removed := fin.RemoveWorktree(context.Background(), f.proof(t), f.wt, false); removed != "" {
		t.Error("removed a worktree in use")
	}
}

func TestRemoveWorktreeAlreadyGone(t *testing.T) {
	f := newFinishFixture(t)
	pr := f.proof(t)
	if res, _ := f.finisher().RemoveWorktree(context.Background(), pr, filepath.Join(f.wt, "..", "nowhere"), false); !strings.Contains(res.Line, "already gone") || res.Failed {
		t.Errorf("RemoveWorktree = %+v", res)
	}
}

func TestRemoveWorktreeGoneButListedLosesItsEntry(t *testing.T) {
	f := newFinishFixture(t)
	pr := f.proof(t)
	f.moveWorktreeAway(t)
	fin := f.finisher()
	res, removed := fin.RemoveWorktree(context.Background(), pr, f.wt, false)
	if removed == "" || !strings.Contains(res.Line, "git's entry for it removed") {
		t.Fatalf("RemoveWorktree = %+v, %q", res, removed)
	}
	if res := fin.DeleteBranch(context.Background(), pr, removed); !strings.Contains(res.Line, "deleted") {
		t.Errorf("DeleteBranch = %+v", res)
	}
}

func TestRemoveWorktreeLeavesNestedWork(t *testing.T) {
	f := newFinishFixture(t)
	// An ignored folder of the worktree holds a worktree of its own.
	mustWrite(t, filepath.Join(f.wt, ".gitignore"), ".claude/\n")
	finishGit(t, f.wt, "add", ".gitignore")
	finishGit(t, f.wt, "commit", "-q", "-m", "ignore")
	f.gh.out[ghFinishList] = f.prJSON("MERGED", finishGit(t, f.wt, "rev-parse", "HEAD"))
	finishGit(t, f.main, "branch", "nested", "main")
	finishGit(t, f.main, "worktree", "add", "-q", filepath.Join(f.wt, ".claude", "worktrees", "n"), "nested")
	pr := f.proof(t)
	res, removed := f.finisher().RemoveWorktree(context.Background(), pr, f.wt, false)
	if removed != "" || !strings.Contains(res.Line, "is inside it") {
		t.Errorf("registered: RemoveWorktree = %+v, %q", res, removed)
	}
	// A repository there that git does not list as a worktree.
	finishGit(t, f.main, "worktree", "remove", filepath.Join(f.wt, ".claude", "worktrees", "n"))
	finishGit(t, f.wt, "init", "-q", filepath.Join(f.wt, ".claude", "clone"))
	res, removed = f.finisher().RemoveWorktree(context.Background(), pr, f.wt, false)
	if removed != "" || !strings.Contains(res.Line, "holds another repository") {
		t.Errorf("clone: RemoveWorktree = %+v, %q", res, removed)
	}
	if !isDir(filepath.Join(f.wt, ".claude", "clone")) {
		t.Error("the nested repository is gone")
	}
}

func TestRemoveWorktreeNamesTheIgnoredFiles(t *testing.T) {
	f := newFinishFixture(t)
	mustWrite(t, filepath.Join(f.wt, ".gitignore"), "*.env\n")
	finishGit(t, f.wt, "add", ".gitignore")
	finishGit(t, f.wt, "commit", "-q", "-m", "ignore")
	f.gh.out[ghFinishList] = f.prJSON("MERGED", finishGit(t, f.wt, "rev-parse", "HEAD"))
	mustWrite(t, filepath.Join(f.wt, "local.env"), "x\n")
	fin := f.finisher()
	fin.DryRun = true
	if res, _ := fin.RemoveWorktree(context.Background(), f.proof(t), f.wt, false); !strings.Contains(res.Line, "its 1 ignored file(s) or folder(s) go with it: local.env") {
		t.Errorf("RemoveWorktree = %+v", res)
	}
}

func TestUsersMatchesEscapedFolders(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a b \u00e9")
	fin := Finisher{Procs: func(context.Context) ([]ProcCwd, error) {
		return parseLsof([]byte("p7\ncsh\nn" + strings.ReplaceAll(dir, "\u00e9", `\xc3\xa9`) + "\n")), nil
	}}
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if users, err := fin.users(context.Background(), dir); err != nil || len(users) != 1 {
		t.Errorf("users = %+v, %v; want pid 7", users, err)
	}
}

func TestDeleteBranchAtTheMergedHead(t *testing.T) {
	f := newFinishFixture(t)
	pr := f.proof(t)
	finishGit(t, f.main, "config", "branch.feature-x.remote", "origin")
	fin := f.finisher()
	if _, removed := fin.RemoveWorktree(context.Background(), pr, f.wt, false); removed == "" {
		t.Fatal("worktree not removed")
	}
	if res := fin.DeleteBranch(context.Background(), pr, ""); !strings.Contains(res.Line, "deleted") {
		t.Fatalf("DeleteBranch = %q", res.Line)
	}
	if out := finishGit(t, f.main, "branch", "--list", "feature-x"); out != "" {
		t.Errorf("branch still there: %q", out)
	}
	if out := finishGit(t, f.main, "config", "--list", "--local"); strings.Contains(out, "branch.feature-x.") {
		t.Errorf("branch settings left: %q", out)
	}
}

func TestDeleteBranchRefusals(t *testing.T) {
	f := newFinishFixture(t)
	pr := f.proof(t)
	// Checked out in the worker's worktree, which was not removed.
	if res := f.finisher().DeleteBranch(context.Background(), pr, ""); !strings.Contains(res.Line, "checked out in") {
		t.Errorf("DeleteBranch = %q, want checked out", res.Line)
	}
	// A dry run whose worktree step would remove it says it would delete.
	fin := f.finisher()
	fin.DryRun = true
	if res := fin.DeleteBranch(context.Background(), pr, f.wt); !strings.Contains(res.Line, "would delete") {
		t.Errorf("dry run DeleteBranch = %q", res.Line)
	}
	// The tip differs from the merged head.
	moved := pr
	moved.Head = strings.Repeat("b", 40)
	if res := f.finisher().DeleteBranch(context.Background(), moved, f.wt); !strings.Contains(res.Line, "not the merged head") {
		t.Errorf("DeleteBranch = %q, want tip differs", res.Line)
	}
	if finishGit(t, f.main, "branch", "--list", "feature-x") == "" {
		t.Error("branch deleted")
	}
}

func TestDeleteBranchAlreadyGone(t *testing.T) {
	f := newFinishFixture(t)
	pr := f.proof(t)
	pr.Branch = "feature-y"
	if res := f.finisher().DeleteBranch(context.Background(), pr, ""); !strings.Contains(res.Line, "already gone") {
		t.Errorf("DeleteBranch = %q", res.Line)
	}
}

func TestDeleteRemoteBranch(t *testing.T) {
	f := newFinishFixture(t)
	pr := f.proof(t)
	fin := f.finisher()
	fin.DryRun = true
	if res := fin.DeleteRemoteBranch(context.Background(), pr); !strings.Contains(res.Line, "would delete") {
		t.Errorf("dry run: %+v", res)
	}
	if finishGit(t, f.origin, "branch", "--list", "feature-x") == "" {
		t.Fatal("dry run deleted the remote branch")
	}
	finishGit(t, f.main, "fetch", "-q", "origin")
	if res := f.finisher().DeleteRemoteBranch(context.Background(), pr); !strings.Contains(res.Line, "deleted") || res.Failed {
		t.Fatalf("DeleteRemoteBranch = %+v", res)
	}
	if out := finishGit(t, f.origin, "branch", "--list", "feature-x"); out != "" {
		t.Errorf("remote branch still there: %q", out)
	}
	if out := finishGit(t, f.main, "for-each-ref", "refs/remotes/origin/feature-x"); out != "" {
		t.Errorf("remote-tracking branch left: %q", out)
	}
	if res := f.finisher().DeleteRemoteBranch(context.Background(), pr); !strings.Contains(res.Line, "already gone") || res.Failed {
		t.Errorf("again: %+v", res)
	}
}

func TestDeleteRemoteBranchRefusals(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, f *finishFixture, pr *Proof)
		want  string
	}{
		{"remote tip differs", func(t *testing.T, f *finishFixture, _ *Proof) {
			finishGit(t, f.wt, "commit", "-q", "--allow-empty", "-m", "more")
			finishGit(t, f.wt, "push", "-q", "origin", "feature-x")
		}, "not the merged head"},
		{"no remote of the repository", func(t *testing.T, f *finishFixture, _ *Proof) {
			finishGit(t, f.main, "remote", "set-url", "origin", "https://github.com/o/other.git")
		}, "no git remote"},
		{"push address elsewhere", func(t *testing.T, f *finishFixture, _ *Proof) {
			finishGit(t, f.main, "remote", "set-url", "--push", "origin", "https://example.org/o/r.git")
		}, "no git remote"},
		{"another host", func(_ *testing.T, _ *finishFixture, pr *Proof) { pr.Host = "git.example.org" }, "no git remote"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFinishFixture(t)
			pr := f.proof(t)
			c.setup(t, f, &pr)
			if res := f.finisher().DeleteRemoteBranch(context.Background(), pr); !strings.Contains(res.Line, c.want) || res.Failed {
				t.Errorf("DeleteRemoteBranch = %+v, want %q", res, c.want)
			}
			if finishGit(t, f.origin, "branch", "--list", "feature-x") == "" {
				t.Error("remote branch deleted")
			}
		})
	}
}

func TestDeleteRemoteBranchMovedRightBeforeThePush(t *testing.T) {
	f := newFinishFixture(t)
	pr := f.proof(t)
	fin := f.finisher()
	fin.Run = func(ctx context.Context, dir, bin string, args ...string) ([]byte, error) {
		if bin == "git" && len(args) > 0 && args[0] == "push" {
			// Someone pushes between the read and the delete.
			finishGit(t, f.wt, "commit", "-q", "--allow-empty", "-m", "late")
			finishGit(t, f.wt, "push", "-q", "origin", "feature-x")
		}
		return f.gh.run(ctx, dir, bin, args...)
	}
	if res := fin.DeleteRemoteBranch(context.Background(), pr); !strings.Contains(res.Line, "moved since its tip was read") || res.Failed {
		t.Errorf("DeleteRemoteBranch = %+v", res)
	}
	if finishGit(t, f.origin, "branch", "--list", "feature-x") == "" {
		t.Error("the lease let the moved branch be deleted")
	}
}

func TestNamesRepo(t *testing.T) {
	for u, want := range map[string]bool{
		"https://github.com/o/r.git":      true,
		"https://GitHub.com/O/R":          true,
		"ssh://git@github.com:22/o/r.git": true,
		"git@github.com:o/r.git":          true,
		"github.com:o/r":                  true,
		"https://github.com/o/r2.git":     false,
		"https://example.org/o/r.git":     false,
		"/srv/git/o/r.git":                false,
		"./github.com:o/r":                false,
	} {
		if got := namesRepo(u, "github.com", "o/r"); got != want {
			t.Errorf("namesRepo(%q) = %v, want %v", u, got, want)
		}
	}
}

func TestFastForwardMain(t *testing.T) {
	f := newFinishFixture(t)
	pr := f.proof(t)
	// Another clone moves origin's main on.
	other := filepath.Join(filepath.Dir(f.main), "other")
	finishGit(t, filepath.Dir(f.main), "clone", "-q", f.origin, other)
	finishGit(t, other, "commit", "-q", "--allow-empty", "-m", "merged")
	finishGit(t, other, "push", "-q", "origin", "main")
	want := finishGit(t, other, "rev-parse", "HEAD")
	if res := f.finisher().FastForwardMain(context.Background(), pr); !strings.Contains(res.Line, "fast-forwarded main") {
		t.Fatalf("FastForwardMain = %q", res.Line)
	}
	if got := finishGit(t, f.main, "rev-parse", "HEAD"); got != want {
		t.Errorf("main at %s, want %s", got, want)
	}
}

func TestFastForwardMainRefusals(t *testing.T) {
	f := newFinishFixture(t)
	pr := f.proof(t)
	before := finishGit(t, f.main, "rev-parse", "HEAD")
	mustWrite(t, filepath.Join(f.main, "m.txt"), "m\n")
	finishGit(t, f.main, "add", "m.txt")
	if res := f.finisher().FastForwardMain(context.Background(), pr); !strings.Contains(res.Line, "1 uncommitted change(s)") {
		t.Errorf("dirty: FastForwardMain = %q", res.Line)
	}
	finishGit(t, f.main, "reset", "-q", "--hard")
	finishGit(t, f.main, "switch", "-q", "-c", "elsewhere")
	if res := f.finisher().FastForwardMain(context.Background(), pr); !strings.Contains(res.Line, "on elsewhere, not main") {
		t.Errorf("other branch: FastForwardMain = %q", res.Line)
	}
	if got := finishGit(t, f.main, "rev-parse", "HEAD"); got != before {
		t.Errorf("main moved to %s", got)
	}
}

func TestCloseFinished(t *testing.T) {
	cases := []struct {
		name   string
		action Action
		set    func(fc *fakeCloser)
		closed bool
		left   bool
	}{
		{"running worker in its tab", Leave, func(*fakeCloser) {}, true, false},
		{"idle shell of an ended worker", Skip, func(*fakeCloser) {}, true, false},
		{"tab runs something else", Leave, func(fc *fakeCloser) { fc.other = map[string]bool{"t-w1": true} }, false, true},
		{"ended worker's tab busy", Skip, func(fc *fakeCloser) { fc.busy = map[string]bool{"t-w1": true} }, false, true},
		{"split tab", Leave, func(fc *fakeCloser) { fc.split = map[string]bool{"t-w1": true} }, false, true},
		{"no longer listed", Leave, func(fc *fakeCloser) { fc.unlisted = map[string]bool{"t-w1": true} }, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, steps := downFixture(t)
			fc := &fakeCloser{p: p}
			c.set(fc)
			s := steps[0]
			s.Action = c.action
			ref := p.Sessions["w1"]
			got, err := CloseFinished(context.Background(), p, s, ref, fc)
			if err != nil {
				t.Fatal(err)
			}
			if closed := slices.Contains(fc.closed, "t-w1"); closed != c.closed {
				t.Errorf("closed = %v, want %v", closed, c.closed)
			}
			if IsLeftOpen(got.Warning) != c.left {
				t.Errorf("ref = %+v, left open want %v", got, c.left)
			}
			if _, still := p.Sessions["w1"]; still {
				t.Error("ref still recorded")
			}
			if _, other := p.Sessions["w2"]; !other {
				t.Error("another worker's ref dropped")
			}
		})
	}
}

func TestCloseFinishedListingFails(t *testing.T) {
	p, steps := downFixture(t)
	fc := &fakeCloser{p: p, listErr: errors.New("agterm: down")}
	if _, err := CloseFinished(context.Background(), p, steps[0], p.Sessions["w1"], fc); err == nil || len(fc.closed) != 0 {
		t.Errorf("err = %v, closed %q; want an error and no close", err, fc.closed)
	}
	if _, still := p.Sessions["w1"]; !still {
		t.Error("ref dropped although nothing was listed")
	}
}

func TestCloseFinishedFailedCloseKeepsTheRef(t *testing.T) {
	p, steps := downFixture(t)
	fc := &fakeCloser{p: p, fail: map[string]string{"t-w1": "agterm: busy"}}
	if _, err := CloseFinished(context.Background(), p, steps[0], p.Sessions["w1"], fc); err == nil {
		t.Error("no error")
	}
	if _, still := p.Sessions["w1"]; !still {
		t.Error("ref dropped although the close failed")
	}
	fc = &fakeCloser{p: p, gone: map[string]bool{"t-w1": true}}
	if _, err := CloseFinished(context.Background(), p, steps[0], p.Sessions["w1"], fc); err != nil {
		t.Errorf("gone: %v", err)
	}
}

func TestNewestWorkerTieGoesToTheLaterListed(t *testing.T) {
	at := time.Now()
	cands := []Candidate{{Role: RoleWorker, Name: "w", SessionID: "a", Started: at}, {Role: RoleWorker, Name: "w", SessionID: "b", Started: at}, {Role: RoleWorker, Name: "x", SessionID: "c", Started: at.Add(time.Hour)}}
	if c, ok := NewestWorker(cands, "w"); !ok || c.SessionID != "b" {
		t.Errorf("NewestWorker = %+v, %v", c, ok)
	}
	if _, ok := NewestWorker(cands, "none"); ok {
		t.Error("found a worker that is not there")
	}
}

func TestParseLsofFields(t *testing.T) {
	out := "p12\nccl aude\nfcwd\nn/work/a b\np13\ncfish\nfcwd\nn/\n"
	got := parseLsof([]byte(out))
	want := []ProcCwd{{PID: 12, Cmd: "cl aude", Cwd: "/work/a b"}, {PID: 13, Cmd: "fish", Cwd: "/"}}
	if !slices.Equal(got, want) {
		t.Errorf("parseLsof = %+v, want %+v", got, want)
	}
}

func TestUnescapeLsof(t *testing.T) {
	for in, want := range map[string]string{
		`/a b \xc3\xa9`: "/a b \u00e9",
		`/t\tab`:        "/t\tab",
		`/c^Ad`:         "/c\x01d",
		`/back\\slash`:  `/back\slash`,
		"/plain":        "/plain",
	} {
		if got := unescapeLsof(in); got != want {
			t.Errorf("unescapeLsof(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProcCwdsSeesThisProcess(t *testing.T) {
	// A folder lsof prints with escapes in the C locale.
	wd := filepath.Join(t.TempDir(), "a b \u00e9\tc")
	if err := os.Mkdir(wd, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(wd)
	t.Setenv("LC_ALL", "C")
	procs, err := ProcCwds(context.Background())
	if err != nil {
		t.Skipf("process folders not listed here: %v", err)
	}
	if !slices.ContainsFunc(procs, func(p ProcCwd) bool { return p.PID == os.Getpid() && realPath(p.Cwd) == realPath(wd) }) {
		t.Errorf("this process (%d in %s) is not listed", os.Getpid(), wd)
	}
}

func mustWrite(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}
