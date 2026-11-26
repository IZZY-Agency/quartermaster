package main

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

	"io"

	"github.com/IZZY-Agency/quartermaster/internal/declog"
	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/route"
	"github.com/IZZY-Agency/quartermaster/internal/up"
)

// finishMachine is an upMachine whose project folder is a git repository
// on main, pushed to a bare origin, with worker w1 in a linked worktree on
// branch feature-x one commit ahead. git runs for real; gh answers from gh.
type finishMachine struct {
	*upMachine
	wt, head, origin string
	gh               map[string]string
	ghCalls          []string
}

func newFinishMachine(t *testing.T) *finishMachine {
	t.Helper()
	return newFinishMachineNamed(t, "w1", "")
}

// newFinishMachineNamed is newFinishMachine with the worker named name, spawned
// with the name spawned ("" for a worker spawned before the prefix existed).
func newFinishMachineNamed(t *testing.T, name, spawned string) *finishMachine {
	t.Helper()
	m := &finishMachine{upMachine: newUpMachine(t), gh: map[string]string{}}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	origin := filepath.Join(base, "origin.git")
	m.wt = filepath.Join(base, "wt")
	finishGitT(t, base, "init", "-q", "--bare", "-b", "main", origin)
	finishGitT(t, m.root, "init", "-q", "-b", "main")
	finishGitT(t, m.root, "commit", "-q", "--allow-empty", "-m", "init")
	finishGitT(t, m.root, "remote", "add", "origin", "https://github.com/o/r.git")
	finishGitT(t, m.root, "config", "url."+origin+".insteadOf", "https://github.com/o/r.git")
	finishGitT(t, m.root, "push", "-q", "-u", "origin", "main")
	finishGitT(t, m.root, "worktree", "add", "-q", "-b", "feature-x", m.wt)
	finishGitT(t, m.wt, "commit", "-q", "--allow-empty", "-m", "feature")
	m.head = finishGitT(t, m.wt, "rev-parse", "HEAD")
	finishGitT(t, m.wt, "push", "-q", "origin", "feature-x")
	m.origin = origin
	m.gh["gh pr list --head feature-x --state all --json number,state,headRefOid,isCrossRepository,createdAt,url,headRepository,headRepositoryOwner --limit 20"] = m.prs("MERGED")
	swap(t, &finishRun, func(ctx context.Context, dir, bin string, args ...string) ([]byte, error) {
		if bin == "git" {
			return up.ExecDir(ctx, dir, bin, args...)
		}
		key := bin + " " + strings.Join(args, " ")
		m.ghCalls = append(m.ghCalls, key)
		if out, ok := m.gh[key]; ok {
			return []byte(out), nil
		}
		return nil, errors.New(bin + ": failed")
	})
	swap(t, &finishProcs, func(context.Context) ([]up.ProcCwd, error) { return nil, nil })
	swap(t, &finishSleep, func(time.Duration) {})
	swap(t, &finishLive, func() (map[string]bool, error) { return map[string]bool{}, nil })
	p := m.project(t)
	r := route.Record{
		Time: time.Now().UTC().Add(-time.Hour), Kind: "worker", Name: name, BaseName: spawned, Cwd: m.wt, Branch: "feature-x", Project: m.root,
		UpProject: p.ID, ClaudeSession: upWorkerID,
		Decision: &route.Decision{Kind: "worker", Tier: route.Tier{Model: "sonnet", Effort: "low"}},
	}
	if err := declog.Append(r); err != nil {
		t.Fatal(err)
	}
	m.fixture(t, "coord.jsonl", filepath.Join(m.claude, "projects", "p", upWorkerID+".jsonl"))
	m.tabs(t, p, "t-c", "t-w")
	m.term.showIdle("t-w")
	return m
}

func (m *finishMachine) prs(state string) string {
	return fmt.Sprintf(`[{"number":7,"state":%q,"headRefOid":%q,"isCrossRepository":false,"createdAt":"2026-11-20T10:00:00Z","url":"https://github.com/o/r/pull/7","headRepository":{"name":"r"},"headRepositoryOwner":{"login":"o"}}]`, state, m.head)
}

func finishGitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "GIT_") })
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (m *finishMachine) done(t *testing.T) bool {
	t.Helper()
	p := loadProject(t, m.root)
	_, ok := p.Done[upWorkerID]
	return ok
}

func (m *finishMachine) closedTab() bool {
	return slices.ContainsFunc(m.term.calls, func(c string) bool { return strings.HasPrefix(c, "session close") })
}

func (m *finishMachine) remoteThere(t *testing.T) bool {
	return finishGitT(t, m.origin, "branch", "--list", "feature-x") != ""
}

func (m *finishMachine) branchThere(t *testing.T) bool {
	return finishGitT(t, m.root, "branch", "--list", "feature-x") != ""
}

func TestFinishCleansUpAMergedWorker(t *testing.T) {
	m := newFinishMachine(t)
	code, out, errs := runQM("finish", "w1")
	if code != 0 {
		t.Fatalf("exit %d, out %q, err %q", code, out, errs)
	}
	t.Log(out)
	for _, want := range []string{"proof: pull request #7", "worker w1: marked done", "worker w1: closed agterm t-w", "worktree " + m.wt + ": removed", "branch feature-x: deleted", "remote branch o/r feature-x: deleted", "main checkout " + m.root + ": main already up to date"} {
		if !strings.Contains(out, want) {
			t.Errorf("out lacks %q:\n%s", want, out)
		}
	}
	if !m.done(t) || !m.closedTab() || isDirT(m.wt) || m.branchThere(t) || m.remoteThere(t) {
		t.Errorf("done %v, closed %v, worktree there %v, branch there %v, remote there %v", m.done(t), m.closedTab(), isDirT(m.wt), m.branchThere(t), m.remoteThere(t))
	}
}

func TestFinishWithoutProofChangesNothing(t *testing.T) {
	for _, state := range []string{"OPEN", "CLOSED"} {
		m := newFinishMachine(t)
		m.gh["gh pr list --head feature-x --state all --json number,state,headRefOid,isCrossRepository,createdAt,url,headRepository,headRepositoryOwner --limit 20"] = m.prs(state)
		code, out, errs := runQM("finish", "w1")
		if code != 1 || !strings.Contains(errs, "nothing changed") {
			t.Errorf("%s: exit %d, out %q, err %q", state, code, out, errs)
		}
		if m.done(t) || m.closedTab() || !isDirT(m.wt) || !m.branchThere(t) || !m.remoteThere(t) {
			t.Errorf("%s: something changed: done %v, closed %v, worktree %v, branch %v, gh %q", state, m.done(t), m.closedTab(), isDirT(m.wt), m.branchThere(t), m.ghCalls)
		}
	}
}

func TestFinishDryRunChangesNothing(t *testing.T) {
	m := newFinishMachine(t)
	code, out, errs := runQM("finish", "--dry-run", "w1")
	if code != 0 {
		t.Fatalf("exit %d, out %q, err %q", code, out, errs)
	}
	for _, want := range []string{"proof: ", "would mark done", "would close its agterm t-w", "would remove", "would delete at", "remote branch o/r feature-x: would delete", "would fast-forward main"} {
		if !strings.Contains(out, want) {
			t.Errorf("out lacks %q:\n%s", want, out)
		}
	}
	if m.done(t) || m.closedTab() || !isDirT(m.wt) || !m.branchThere(t) || !m.remoteThere(t) {
		t.Errorf("dry run changed something")
	}
}

func TestFinishLeavesAWorktreeInUseAndItsBranch(t *testing.T) {
	m := newFinishMachine(t)
	swap(t, &finishProcs, func(context.Context) ([]up.ProcCwd, error) {
		return []up.ProcCwd{{PID: 4242, Cmd: "fish", Cwd: m.wt}}, nil
	})
	code, out, errs := runQM("finish", "w1")
	if code != 0 || !strings.Contains(out, "current folder of pid 4242 (fish)") || !strings.Contains(out, "branch feature-x: left: it is checked out in") {
		t.Errorf("exit %d, out %q, err %q", code, out, errs)
	}
	// The steps that do not depend on the worktree still ran.
	if !m.done(t) || !isDirT(m.wt) || !m.branchThere(t) || !strings.Contains(out, "remote branch o/r feature-x: deleted") {
		t.Errorf("done %v, worktree %v, branch %v, out %q", m.done(t), isDirT(m.wt), m.branchThere(t), out)
	}
}

func TestFinishFailedStepExitsOne(t *testing.T) {
	newFinishMachine(t)
	swap(t, &finishProcs, func(context.Context) ([]up.ProcCwd, error) { return nil, errors.New("lsof: failed") })
	code, out, errs := runQM("finish", "w1")
	if code != 1 || !strings.Contains(out, "were not read") || !strings.Contains(out, "remote branch o/r feature-x: deleted") {
		t.Errorf("exit %d, out %q, err %q", code, out, errs)
	}
}

func TestFinishMarksOnlyTheProvenSession(t *testing.T) {
	m := newFinishMachine(t)
	p := loadProject(t, m.root)
	var out strings.Builder
	if _, _, _, err := markDone(p.ID, m.root, "w1", "another-session", &out, &out); err == nil || !strings.Contains(err.Error(), "nothing marked") {
		t.Errorf("markDone error = %v", err)
	}
	if m.done(t) {
		t.Error("marked done although the session was not the proven one")
	}
}

func TestFinishNotMarkedLeavesTheWorktreeAndBranches(t *testing.T) {
	m := newFinishMachine(t)
	swap(t, &finishMark, func(_, _, name, _ string, _, _ io.Writer) (string, int, map[string]launch.SessionRef, error) {
		return "", 0, nil, fmt.Errorf("the newest worker %s is session s2 now, not s1: nothing marked", name)
	})
	code, out, errs := runQM("finish", "w1")
	if code != 1 || !strings.Contains(out, "its worktree and branches are left") || !strings.Contains(out, "main checkout") {
		t.Errorf("exit %d, out %q, err %q", code, out, errs)
	}
	if !isDirT(m.wt) || !m.branchThere(t) || !m.remoteThere(t) || m.closedTab() {
		t.Errorf("worktree %v, branch %v, remote %v, closed %v", isDirT(m.wt), m.branchThere(t), m.remoteThere(t), m.closedTab())
	}
}

func TestFinishTakesOneWorkerName(t *testing.T) {
	for _, args := range [][]string{{"finish"}, {"finish", "-x"}, {"finish", "w1", "w2"}, {"finish", "--dry-run"}} {
		if code, _, errs := runQM(args...); code != 2 || !strings.Contains(errs, "usage:") {
			t.Errorf("%q: exit %d, err %q", args, code, errs)
		}
	}
}

func TestFinishUnknownWorker(t *testing.T) {
	newFinishMachine(t)
	if code, _, errs := runQM("finish", "nobody"); code != 2 || !strings.Contains(errs, `no worker "nobody"`) {
		t.Errorf("exit %d, err %q", code, errs)
	}
}

func isDirT(dir string) bool {
	fi, err := os.Stat(dir)
	return err == nil && fi.IsDir()
}
