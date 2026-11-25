package up

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/route"
)

// failLog is a decision log that cannot be written.
func failLog(route.Record) error { return errors.New("read-only file system") }

// A first run whose decision log cannot be written saves no state and
// touches no tab: a retry still finds every session to adopt.
func TestAdoptLogUnwritableSavesNothing(t *testing.T) {
	w := newAdoptWorld(t)
	w.inOther()
	ap, err := w.plan(t, allThree, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	d := w.deps()
	d.Log = failLog
	if err := Adopt(context.Background(), &ap, d); err == nil {
		t.Fatal("Adopt succeeded with an unwritable decision log")
	}
	if _, ok, err := Load(w.repo); err != nil || ok {
		t.Fatalf("state saved: ok=%v err=%v", ok, err)
	}
	for _, v := range w.agterm.verbs() {
		if v != "tree" {
			t.Fatalf("acted with no Record: %q", w.agterm.calls)
		}
	}
}

// A later run whose decision log cannot be written leaves the saved state as
// it was: the workers stay candidates for a retry.
func TestAdoptWorkersLogUnwritableSavesNothing(t *testing.T) {
	w, p := laterWorld(t)
	ap := w.laterPlan(t, p, adQM, adHand)
	d := w.deps()
	d.Log = failLog
	if err := Adopt(context.Background(), &ap, d); err == nil {
		t.Fatal("Adopt succeeded with an unwritable decision log")
	}
	cur, _, err := LoadID(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cur.Adopted) != 0 || len(cur.Sessions) != 1 {
		t.Fatalf("state changed: adopted %v sessions %v", cur.Adopted, cur.Sessions)
	}
	if got := candIDs(w.detectHeld(t, Held(cur, w.recs))); len(got) != 2 {
		t.Fatalf("candidates after the failed run %v", got)
	}
}

// otherProject saves a project rooted at the linked worktree w.wtHand, with a
// coordinator that is not running, and plans adopting adHand into it.
func (w *adoptWorld) otherProject(t *testing.T) (Project, AdoptPlan) {
	t.Helper()
	o := Project{
		ID: ProjectID(w.wtHand), Root: w.wtHand, Workspace: "hand", Launcher: "agterm",
		Coordinator: Session{ID: "00000000-0000-4000-8000-0000000000c9", Name: "hand-coord"},
		Created:     now,
	}
	if err := Save(o); err != nil {
		t.Fatal(err)
	}
	cands := Detect(w.wtHand, []Project{o}, w.recs, DetectEnv{
		Procs: w.procs, Live: w.live(t), Worktrees: []string{w.wtHand},
		TranscriptPath: w.transcriptPath, SelfPID: w.selfPID, Held: Held(o, w.recs),
	}).Candidates
	ap, err := PlanAdoptWorkers(context.Background(), o, []Project{o}, cands, []string{adHand}, w.deps())
	if err != nil {
		t.Fatal(err)
	}
	return o, ap
}

// Two later runs planned at once for one session: the second to confirm
// drops it, as now held by the first project.
func TestAdoptWorkersSessionClaimedMeanwhile(t *testing.T) {
	w, p := laterWorld(t)
	apA := w.laterPlan(t, p, adHand)
	o, apB := w.otherProject(t)
	if err := Adopt(context.Background(), &apA, w.deps()); err != nil {
		t.Fatal(err)
	}
	if err := Adopt(context.Background(), &apB, w.deps()); err != nil {
		t.Fatal(err)
	}
	if !notesHold(apB.Notes, "not adopted: now held by project "+p.ID) {
		t.Fatalf("notes %q", apB.Notes)
	}
	cur, _, err := LoadID(o.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cur.Adopted[adHand]; ok || len(apB.Workers) != 0 {
		t.Fatalf("adopted by both: %v, workers %v", cur.Adopted, candIDs(apB.Workers))
	}
	for _, r := range w.logged {
		if r.UpProject == o.ID {
			t.Fatalf("Record for the dropped worker: %+v", r)
		}
	}
}

// A first run confirmed after another project took one of its workers adopts
// the rest and drops that one.
func TestAdoptSessionClaimedMeanwhile(t *testing.T) {
	w := newAdoptWorld(t)
	ap, err := w.plan(t, allThree, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	o, apB := w.otherProject(t)
	if err := Adopt(context.Background(), &apB, w.deps()); err != nil {
		t.Fatal(err)
	}
	if err := Adopt(context.Background(), &ap, w.deps()); err != nil {
		t.Fatal(err)
	}
	if !notesHold(ap.Notes, "not adopted: now held by project "+o.ID) {
		t.Fatalf("notes %q", ap.Notes)
	}
	p, _, err := Load(w.repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Adopted[adHand]; ok {
		t.Fatalf("adopted by both: %v", p.Adopted)
	}
	if _, ok := p.Sessions[adHand]; ok || p.Coordinator.ID != adCoord {
		t.Fatalf("sessions %v coordinator %v", p.Sessions, p.Coordinator)
	}
}

// A first run whose coordinator another project took meanwhile fails with
// nothing saved: here a first run in the linked worktree w.wtHand picks
// adHand as its coordinator, and the main project adopts adHand first.
func TestAdoptCoordinatorClaimedMeanwhile(t *testing.T) {
	w, p := laterWorld(t)
	w.inOther()
	cands := Detect(w.wtHand, w.all, w.recs, DetectEnv{
		Procs: w.procs, Live: w.live(t), Worktrees: []string{w.wtHand},
		TranscriptPath: w.transcriptPath, SelfPID: w.selfPID,
	}).Candidates
	first, err := PlanAdopt(context.Background(), w.wtHand, w.all, cands, Choice{Coordinator: adHand}, nil, false, w.deps())
	if err != nil {
		t.Fatal(err)
	}
	later := w.laterPlan(t, p, adHand)
	if err := Adopt(context.Background(), &later, w.deps()); err != nil {
		t.Fatal(err)
	}
	if err := Adopt(context.Background(), &first, w.deps()); err == nil || !strings.Contains(err.Error(), "now held by project "+p.ID) {
		t.Fatalf("err %v", err)
	}
	if _, ok, _ := Load(w.wtHand); ok {
		t.Fatal("state saved")
	}
}

// A second adoption waits for the adoption lock, and gives up with
// ErrAdoptBusy after adoptLockWait.
func TestAdoptLockWaitsThenBusy(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	unlock, err := AdoptLock()
	if err != nil {
		t.Fatal(err)
	}
	defer func(old time.Duration) { adoptLockWait = old }(adoptLockWait)
	adoptLockWait = 100 * time.Millisecond
	if _, err := AdoptLock(); !errors.Is(err, ErrAdoptBusy) {
		t.Fatalf("err %v", err)
	}
	unlock()
	again, err := AdoptLock()
	if err != nil {
		t.Fatal(err)
	}
	again()
	if all, err := All(); err != nil || len(all) != 0 {
		t.Fatalf("the lock file read as a project: %v %v", all, err)
	}
}

// sideInCoordTab adds the side session adSide in the root, its environment
// inheriting the coordinator's tab s-c, whose claude names no session id.
func (w *adoptWorld) sideInCoordTab(t *testing.T) {
	t.Helper()
	w.side(t)
	w.term[414] = claudecode.TermEnv{AgtermSession: "s-c"}
}

// sideRef plans adopting adSide into p and returns its recorded ref.
func (w *adoptWorld) sideRef(t *testing.T, p Project) (launch.SessionRef, AdoptPlan) {
	t.Helper()
	ap := w.laterPlan(t, p, adSide)
	return ap.Project.Sessions[adSide], ap
}

// A side session that inherited the coordinator's tab is not given that tab:
// the project records it for its coordinator.
func TestAdoptWorkersCoordinatorTabRecordedIsNotTaken(t *testing.T) {
	w, p := laterWorld(t)
	w.sideInCoordTab(t)
	if ref, ap := w.sideRef(t, p); ref.ID != "" || slices.Contains(ap.Pins, "s-c") {
		t.Fatalf("the coordinator's tab given to the worker: %+v, pins %v", ref, ap.Pins)
	}
}

// With no ref recorded for the coordinator, the live coordinator whose
// environment names the tab and whose folder is the tab's makes it
// ambiguous: no ref.
func TestAdoptWorkersCoordinatorTabLiveIsAmbiguous(t *testing.T) {
	w, p := laterWorld(t)
	p.Sessions = nil
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	w.all = []Project{p}
	w.sideInCoordTab(t)
	ref, ap := w.sideRef(t, p)
	if ref.ID != "" {
		t.Fatalf("the coordinator's tab given to the worker: %+v", ref)
	}
	if !notesHold(ap.Notes, "named by another running session too") {
		t.Fatalf("notes %q", ap.Notes)
	}
}

// When a running session's terminal cannot be read, a tab whose claude names
// no session cannot be told apart: no ref.
func TestAdoptWorkersTabOwnerUnverified(t *testing.T) {
	w, p := laterWorld(t)
	p.Sessions = nil
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	w.all = []Project{p}
	w.sideInCoordTab(t)
	d := w.deps()
	d.TermOf = func(pid int) (claudecode.TermEnv, error) {
		if pid == 411 {
			return claudecode.TermEnv{}, errors.New("operation not permitted")
		}
		return w.term[pid], nil
	}
	cands := w.detectHeld(t, Held(p, w.recs))
	ap, err := PlanAdoptWorkers(context.Background(), p, w.all, cands, []string{adSide}, d)
	if err != nil {
		t.Fatal(err)
	}
	if ref := ap.Project.Sessions[adSide]; ref.ID != "" {
		t.Fatalf("ref recorded with its owner unverified: %+v", ref)
	}
	if !notesHold(ap.Notes, "cannot be verified") {
		t.Fatalf("notes %q", ap.Notes)
	}
}

// blockSave makes the state file of project id a folder, so Save fails while
// the adoption lock, in the same folder, still works; unblock undoes it.
func blockSave(t *testing.T, id string) (unblock func()) {
	t.Helper()
	dir, err := projectsDir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, id+".json")
	mkdir(t, path)
	if err := os.WriteFile(filepath.Join(path, "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := os.RemoveAll(path); err != nil {
			t.Fatal(err)
		}
	}
}

// A log that fails after the first Record leaves nothing saved; a retry
// adopts everything. The coordinator's Record is then logged twice: an
// up/adopt Record is an audit entry and holds nothing.
func TestAdoptLogFailsAfterFirstRecordThenRetry(t *testing.T) {
	w := newAdoptWorld(t)
	ap, err := w.plan(t, allThree, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	retry := ap
	retry.Workers, retry.Notes = slices.Clone(ap.Workers), slices.Clone(ap.Notes)
	d := w.deps()
	n := 0
	d.Log = func(r route.Record) error {
		if n++; n > 1 {
			return errors.New("disk full")
		}
		w.logged = append(w.logged, r)
		return nil
	}
	if err := Adopt(context.Background(), &ap, d); err == nil {
		t.Fatal("Adopt succeeded with a failing log")
	}
	if _, ok, _ := Load(w.repo); ok {
		t.Fatal("state saved")
	}
	if err := Adopt(context.Background(), &retry, w.deps()); err != nil {
		t.Fatal(err)
	}
	p, ok, err := Load(w.repo)
	if err != nil || !ok || len(p.Adopted) != 2 || p.Coordinator.ID != adCoord {
		t.Fatalf("retry state %+v ok=%v err=%v", p, ok, err)
	}
	if len(w.logged) != 4 {
		t.Fatalf("logged %d Records, want 1 then 3", len(w.logged))
	}
}

// A failed state save after the Records were logged; a retry adopts.
func TestAdoptSaveFailsThenRetry(t *testing.T) {
	w := newAdoptWorld(t)
	ap, err := w.plan(t, allThree, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	retry := ap
	retry.Workers, retry.Notes = slices.Clone(ap.Workers), slices.Clone(ap.Notes)
	unblock := blockSave(t, ProjectID(w.repo))
	if err := Adopt(context.Background(), &ap, w.deps()); err == nil {
		t.Fatal("Adopt succeeded with an unwritable state")
	}
	unblock()
	if _, ok, _ := Load(w.repo); ok {
		t.Fatal("state saved")
	}
	if err := Adopt(context.Background(), &retry, w.deps()); err != nil {
		t.Fatal(err)
	}
	if p, ok, _ := Load(w.repo); !ok || len(p.Adopted) != 2 {
		t.Fatalf("retry state %+v", p)
	}
}

// Two first runs planned at once, distinct coordinators in one agterm
// workspace: the second to confirm fails, with nothing saved.
func TestAdoptWorkspaceClaimedMeanwhile(t *testing.T) {
	w := newAdoptWorld(t)
	a, err := w.plan(t, Choice{Coordinator: adCoord}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	cands := Detect(w.wtHand, nil, w.recs, DetectEnv{
		Procs: w.procs, Live: w.live(t), Worktrees: []string{w.wtHand},
		TranscriptPath: w.transcriptPath, SelfPID: w.selfPID,
	}).Candidates
	b, err := PlanAdopt(context.Background(), w.wtHand, nil, cands, Choice{Coordinator: adHand}, nil, false, w.deps())
	if err != nil {
		t.Fatal(err)
	}
	if a.Project.Workspace != "proj" || b.Project.Workspace != "proj" {
		t.Fatalf("workspaces %q %q", a.Project.Workspace, b.Project.Workspace)
	}
	if err := Adopt(context.Background(), &a, w.deps()); err != nil {
		t.Fatal(err)
	}
	err = Adopt(context.Background(), &b, w.deps())
	if err == nil || !strings.Contains(err.Error(), "workspace proj is now used by project "+a.Project.ID) {
		t.Fatalf("err %v", err)
	}
	if _, ok, _ := Load(w.wtHand); ok {
		t.Fatal("state saved")
	}
}

// A tab another project recorded since the plan is not recorded again: the
// worker is adopted with no terminal session.
func TestAdoptWorkersTabClaimedMeanwhile(t *testing.T) {
	w, p := laterWorld(t)
	ap := w.laterPlan(t, p, adHand)
	if ap.Project.Sessions[adHand].ID != "s-h" {
		t.Fatalf("planned ref %+v", ap.Project.Sessions)
	}
	o := Project{
		ID: ProjectID(w.wtQM), Root: w.wtQM, Workspace: "elsewhere", Launcher: "agterm",
		Coordinator: Session{ID: "00000000-0000-4000-8000-0000000000c8", Name: "o-coord"},
		Sessions:    map[string]launch.SessionRef{"00000000-0000-4000-8000-0000000000c8": {Launcher: "agterm", ID: "s-h"}},
	}
	if err := Save(o); err != nil {
		t.Fatal(err)
	}
	if err := Adopt(context.Background(), &ap, w.deps()); err != nil {
		t.Fatal(err)
	}
	cur, _, _ := LoadID(p.ID)
	if _, ok := cur.Adopted[adHand]; !ok {
		t.Fatalf("worker not adopted: %v", cur.Adopted)
	}
	if _, ok := cur.Sessions[adHand]; ok || slices.Contains(ap.Pins, "s-h") {
		t.Fatalf("tab recorded twice: %v pins %v", cur.Sessions, ap.Pins)
	}
	if !notesHold(ap.Notes, "now recorded by project "+o.ID) {
		t.Fatalf("notes %q", ap.Notes)
	}
}

const adTwin = "00000000-0000-4000-8000-0000000000e5"

// An unpicked candidate owns tab s-h (its claude names no session) and its
// environment cannot be read while it runs; a picked session in the same
// folder inherited the tab: whose the tab is cannot be verified, no ref.
func TestAdoptWorkersTabOwnerUnreadable(t *testing.T) {
	w, p := laterWorld(t)
	w.session(t, 415, adTwin, w.wtHand, "w-twin")
	w.transcript(t, adTwin, now.Add(-time.Hour))
	w.procs = append(w.procs, claudecode.Proc{PID: 415, PPID: 1, Cmd: "claude"})
	w.term[415] = claudecode.TermEnv{AgtermSession: "s-h"}
	d := w.deps()
	d.TermOf = func(pid int) (claudecode.TermEnv, error) {
		if pid == 413 {
			return claudecode.TermEnv{}, errors.New("operation not permitted")
		}
		return w.term[pid], nil
	}
	d.Running = func(int) bool { return true }
	ap, err := PlanAdoptWorkers(context.Background(), p, w.all, w.detectHeld(t, Held(p, w.recs)), []string{adTwin}, d)
	if err != nil {
		t.Fatal(err)
	}
	if ref := ap.Project.Sessions[adTwin]; ref.ID != "" {
		t.Fatalf("the unreadable owner's tab given away: %+v", ref)
	}
	if !notesHold(ap.Notes, "cannot be verified") {
		t.Fatalf("notes %q", ap.Notes)
	}
}

// A failed read of a session that has exited, or that runs in another
// folder (so cannot own the tab), does not block an ordinary worker's tab.
func TestAdoptWorkersUnrelatedFailedReadIgnored(t *testing.T) {
	for _, running := range []bool{false, true} {
		w, p := laterWorld(t)
		d := w.deps()
		d.TermOf = func(pid int) (claudecode.TermEnv, error) {
			if pid == 411 {
				return claudecode.TermEnv{}, errors.New("no such process")
			}
			return w.term[pid], nil
		}
		d.Running = func(int) bool { return running }
		ap, err := PlanAdoptWorkers(context.Background(), p, w.all, w.detectHeld(t, Held(p, w.recs)), []string{adHand}, d)
		if err != nil {
			t.Fatal(err)
		}
		if ref := ap.Project.Sessions[adHand]; ref.ID != "s-h" {
			t.Fatalf("running=%v: ref %+v, notes %q", running, ref, ap.Notes)
		}
	}
}
