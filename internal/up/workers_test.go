package up

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/route"
)

func TestUpDone(t *testing.T) {
	w := newWorld(t)
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	recs := []route.Record{profiled(w, warmID, "w-warm", "warm", now.Add(-time.Hour))}
	cands, err := Gather(&w.project, recs, w.env(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	marked, err := MarkDone(&w.project, cands, "w-warm", now)
	if err != nil || marked != warmID {
		t.Fatalf("MarkDone = %v, %v", marked, err)
	}
	if _, err := MarkDone(&w.project, cands, "w-none", now); !errors.Is(err, ErrInput) {
		t.Errorf("unknown worker: err %v, want ErrInput", err)
	}
	env := w.env(t, nil)
	env.Finished = Finished(w.project, nil)
	cs, err := Gather(&w.project, recs, env)
	if err != nil {
		t.Fatal(err)
	}
	if s := Plan(cs, true, now)[1]; s.Action != Skip || s.Why != "finished: marked done" {
		t.Errorf("after done: %+v", s)
	}
}

func TestUpDoneMarksNewestOfAName(t *testing.T) {
	// A reused worker name: only the newest session is marked, so the one
	// still running is not, and an older one is left as it was.
	w := newWorld(t)
	recs := []route.Record{
		profiled(w, warmID, "w1", "warm", now.Add(-3*time.Hour)),
		profiled(w, smallID, "w1", "small", now.Add(-time.Hour)),
	}
	cands, err := Gather(&w.project, recs, w.env(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	marked, err := MarkDone(&w.project, cands, "w1", now)
	if err != nil || marked != smallID {
		t.Fatalf("MarkDone = %v, %v; want the newest, %s", marked, err, smallID)
	}
	if _, ok := w.project.Done[warmID]; ok || len(w.project.Done) != 1 {
		t.Errorf("done %v: want only the newest session", w.project.Done)
	}
}

func TestUpDoneFollowsNeverRunRestart(t *testing.T) {
	// The worker's fresh restart never ran (no transcript, no process):
	// Gather restores the session it replaced, and qm up done marks that
	// one, so the next plan skips it.
	w := newWorld(t)
	old := profiled(w, largeID, "w1", "large", now.Add(-3*time.Hour))
	never := profiled(w, lostID, "w1", "large", now.Add(-time.Hour))
	never.Replaces = largeID
	recs := []route.Record{old, never}
	cands, err := Gather(&w.project, recs, w.env(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	marked, err := MarkDone(&w.project, cands, "w1", now)
	if err != nil || marked != largeID {
		t.Fatalf("MarkDone = %v, %v; want the restored session %s", marked, err, largeID)
	}
	if _, ok := w.project.Done[lostID]; !ok {
		t.Errorf("the never-run session was not marked too: %v", w.project.Done)
	}
	env := w.env(t, nil)
	env.Finished = Finished(w.project, nil)
	cs, err := Gather(&w.project, recs, env)
	if err != nil {
		t.Fatal(err)
	}
	if s := Plan(cs, false, now)[1]; s.Action != Skip || s.Why != "finished: marked done" {
		t.Errorf("after done: %s %q", s.Action, s.Why)
	}
}

func TestUpRefusesWorkerWorktree(t *testing.T) {
	w := newWorld(t)
	wt, err := filepath.EvalSymlinks(filepath.Join(w.root, "worktrees", "warm"))
	if err != nil {
		t.Fatal(err)
	}
	recs := []route.Record{profiled(w, warmID, "w-warm", "warm", now.Add(-time.Hour))}
	name, root, ok := WorkerOf(wt, []Project{w.project}, recs)
	if !ok || name != "w-warm" || root != w.project.Root {
		t.Errorf("WorkerOf = %q, %q, %v", name, root, ok)
	}
	if _, _, ok := WorkerOf(w.project.Root, []Project{w.project}, recs); ok {
		t.Errorf("the project root read as a worker's worktree")
	}
}

func TestUpDoneHandStarted(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	w := newWorld(t)
	w.project.Adopted = map[string]AdoptedSession{
		smallID: {Name: "w-hand", Cwd: filepath.Join(w.root, "worktrees", "small"), Started: now.Add(-4 * time.Hour)},
	}
	cands, err := Gather(&w.project, nil, w.env(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	marked, err := MarkDone(&w.project, cands, "w-hand", now)
	if err != nil || marked != smallID {
		t.Fatalf("MarkDone = %v, %v", marked, err)
	}
	if _, ok := w.project.Done[smallID]; !ok {
		t.Errorf("Done = %v, want %s", w.project.Done, smallID)
	}
}

func TestUpRefusesAdoptedWorkerWorktree(t *testing.T) {
	w := newWorld(t)
	real := func(dir string) string {
		t.Helper()
		r, err := filepath.EvalSymlinks(filepath.Join(w.root, "worktrees", dir))
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	w.project.Adopted = map[string]AdoptedSession{
		largeID: {Name: "w-hand", Cwd: filepath.Join(w.root, "worktrees", "large")},
		warmID:  {},
	}
	adopted := rec("", warmID, "w-adopted", now.Add(-time.Hour))
	adopted.Cwd = filepath.Join(w.root, "worktrees", "warm")
	other := rec("", smallID, "w-other", now.Add(-time.Hour))
	other.Cwd = filepath.Join(w.root, "worktrees", "small")
	recs := []route.Record{adopted, other}
	projects := []Project{w.project}
	if name, root, ok := WorkerOf(real("large"), projects, recs); !ok || name != "w-hand" || root != w.project.Root {
		t.Errorf("hand-started: WorkerOf = %q, %q, %v", name, root, ok)
	}
	if name, root, ok := WorkerOf(real("warm"), projects, recs); !ok || name != "w-adopted" || root != w.project.Root {
		t.Errorf("adopted with a Record: WorkerOf = %q, %q, %v", name, root, ok)
	}
	if _, _, ok := WorkerOf(real("small"), projects, recs); ok {
		t.Errorf("a Record tied to no project read as a worker's worktree")
	}
}

// A hand-started worker adopted later under the name of a recorded one is
// older than it: qm up done marks the newest by start, not the last listed.
func TestUpDoneNewestAcrossRecordedAndHandStarted(t *testing.T) {
	w := newWorld(t)
	w.project.Adopted = map[string]AdoptedSession{
		smallID: {Name: "w1", Cwd: filepath.Join(w.root, "worktrees", "small"), Started: now.Add(-4 * time.Hour)},
	}
	recs := []route.Record{profiled(w, warmID, "w1", "warm", now.Add(-time.Hour))}
	cands, err := Gather(&w.project, recs, w.env(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	marked, err := MarkDone(&w.project, cands, "w1", now)
	if err != nil || marked != warmID {
		t.Fatalf("MarkDone = %v, %v; want the newer recorded worker %s", marked, err, warmID)
	}
	if _, ok := w.project.Done[smallID]; ok {
		t.Errorf("the older hand-started worker was marked too: %v", w.project.Done)
	}
}
