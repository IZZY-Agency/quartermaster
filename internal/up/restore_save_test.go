package up

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/route"
)

// Issue #69: a session a restore starts is held by the saved state before
// its process can be seen.

// errCrash is what a test panics with to end qm in the middle of a launch.
var errCrash = errors.New("qm ended here")

// onNew wires d to a fake agterm whose session new first calls hook, with
// nothing of the launch done yet: what hook reads on disk is what another
// project's qm would read while the session starts. after, when set, is
// called once the session was opened.
func onNew(d Deps, f *fakeAgterm, hook, after func()) Deps {
	a := launch.Agterm{Bin: "agtermctl", Run: func(ctx context.Context, bin string, args ...string) ([]byte, error) {
		opens := len(args) >= 2 && args[0] == "session" && args[1] == "new"
		if opens && hook != nil {
			hook()
		}
		b, err := f.run(ctx, bin, args...)
		if opens && after != nil {
			after()
		}
		return b, err
	}}
	d.Launcher, d.Lister, d.Closer, d.Pin = a, a, a, a.Pin
	return d
}

// crashes runs Apply and expects it to end in errCrash.
func crashes(t *testing.T, p *Project, steps []Step, d Deps) {
	t.Helper()
	defer func() {
		if r := recover(); r != errCrash {
			t.Fatalf("recovered %v, want the crash", r)
		}
	}()
	Apply(context.Background(), p, steps, d)
	t.Fatal("Apply did not crash")
}

// startedID is the session id the newest launch script starts.
func startedID(t *testing.T) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(os.Getenv("XDG_STATE_HOME"), "quartermaster", "launch", "*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if _, rest, ok := strings.Cut(readFile(t, f), " --session-id "); ok {
			id, _, _ := strings.Cut(rest, " ")
			return strings.TrimSpace(id)
		}
	}
	t.Fatal("no launch script starts a new session")
	return ""
}

// notAdoptable checks that session id, running in the folder cwd, is neither
// offered to nor claimed by another project's adoption.
func notAdoptable(t *testing.T, id, cwd string, recs []route.Record) {
	t.Helper()
	main := t.TempDir()
	other := Project{ID: ProjectID(main), Root: main, Workspace: "main", Coordinator: Session{ID: "00000000-0000-4000-8000-0000000000d1", Name: "main-coord"}, Created: now}
	if err := Save(other); err != nil {
		t.Fatal(err)
	}
	live := map[string]claudecode.LiveSession{id: {PID: 901, Cwd: cwd, Name: "seen"}}
	all, err := All()
	if err != nil {
		t.Fatal(err)
	}
	if det := Detect(main, all, recs, DetectEnv{Live: live, Worktrees: []string{main, cwd}}); len(det.Candidates) != 0 {
		t.Errorf("another project is offered %v", candIDs(det.Candidates))
	}
	ap := AdoptPlan{Later: true, Project: other, Workers: []AdoptCandidate{{SessionID: id, Name: "seen", Cwd: cwd, PID: 901}}}
	ap.Project.Adopted = map[string]AdoptedSession{id: {Name: "seen", Cwd: cwd}}
	var logged []route.Record
	err = Adopt(context.Background(), &ap, AdoptDeps{
		Live:    func() (map[string]claudecode.LiveSession, error) { return live, nil },
		Records: func() ([]route.Record, error) { return recs, nil },
		Log:     func(r route.Record) error { logged = append(logged, r); return nil },
		Now:     func() time.Time { return now },
	})
	if err != nil {
		t.Errorf("adoption: %v", err)
	}
	if len(ap.Workers) != 0 || len(logged) != 0 || len(savedProject(t, other).Adopted) != 0 {
		t.Errorf("another project claimed it: workers %v, logged %v, saved %v", ap.Workers, logged, savedProject(t, other).Adopted)
	}
}

// coordFresh is a project whose stopped coordinator is started fresh.
func coordFresh(t *testing.T) (*world, *fakeAgterm, Deps, []Step, *[]route.Record) {
	t.Helper()
	logged := new([]route.Record)
	f := &fakeAgterm{ws: "repo"}
	d := upDeps(t, f, logged)
	w := newWorld(t)
	fixture(t, "cold-large.jsonl", w.root, w.transcripts[coordID])
	w.project.Sessions = map[string]launch.SessionRef{coordID: {Launcher: "tmux", ID: "@7", Name: "repo-coord"}}
	if err := Save(w.project); err != nil {
		t.Fatal(err)
	}
	return w, f, d, w.plan(t, nil, nil), logged
}

// workerFresh is a project with a running coordinator and a stopped worker
// that is started fresh from its handoff note.
func workerFresh(t *testing.T) (*world, *fakeAgterm, Deps, []Step, *[]route.Record) {
	t.Helper()
	logged := new([]route.Record)
	f := &fakeAgterm{ws: "repo"}
	d := upDeps(t, f, logged)
	w := newWorld(t)
	start := now.Add(-4 * time.Hour)
	w.note(t, "w-large", start.Add(time.Hour))
	if err := Save(w.project); err != nil {
		t.Fatal(err)
	}
	return w, f, d, w.plan(t, w.largeRecs(start), coordRuns), logged
}

var coordRuns = []claudecode.Proc{{PID: 301, Cmd: "claude -n repo-coord"}}

func (w *world) largeRecs(start time.Time) []route.Record {
	return []route.Record{profiled(w, largeID, "w-large", "large", start)}
}

// reload is the saved project, as the next qm up reads it.
func (w *world) reload(t *testing.T) {
	t.Helper()
	w.project = savedProject(t, w.project)
}

func sameState(t *testing.T, got, want Project) {
	t.Helper()
	got.Updated, want.Updated = time.Time{}, time.Time{}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("saved state =\n%+v\nwant\n%+v", got, want)
	}
}

func TestRestoreFreshCoordinatorHeldBeforeItStarts(t *testing.T) {
	w, f, d, steps, _ := coordFresh(t)
	ran := false
	d = onNew(d, f, func() {
		ran = true
		id := startedID(t)
		saved := savedProject(t, w.project)
		if saved.Coordinator.ID != id || saved.Coordinator.Name != "repo-coord" || !slices.Equal(saved.PastCoordinators, []string{coordID}) {
			t.Errorf("saved before the start: coordinator %+v, past %v, want %s and the previous one past", saved.Coordinator, saved.PastCoordinators, id)
		}
		notAdoptable(t, id, w.project.Root, nil)
	}, nil)
	o := outcomeOf(t, Apply(context.Background(), &w.project, steps, d), "repo-coord")
	if !ran || o.Err != nil || o.NewSessionID == "" {
		t.Fatalf("ran %v, outcome = %+v", ran, o)
	}
	saved := savedProject(t, w.project)
	if saved.Coordinator.ID != o.NewSessionID || !slices.Equal(saved.PastCoordinators, []string{coordID}) || saved.Sessions[o.NewSessionID].ID != o.Ref.ID {
		t.Errorf("saved after the start = %+v", saved)
	}
}

func TestRestoreFreshWorkerHeldBeforeItStarts(t *testing.T) {
	w, f, d, steps, logged := workerFresh(t)
	ran := false
	d = onNew(d, f, func() {
		ran = true
		if len(*logged) != 1 { // the coordinator's leave
			t.Errorf("logged before the start: %+v", *logged)
		}
		notAdoptable(t, startedID(t), filepath.Join(w.root, "worktrees", "large"), w.largeRecs(now.Add(-4*time.Hour)))
	}, nil)
	o := outcomeOf(t, Apply(context.Background(), &w.project, steps, d), "w-large")
	if !ran || o.Err != nil || o.NewSessionID == "" {
		t.Fatalf("ran %v, outcome = %+v", ran, o)
	}
	saved := savedProject(t, w.project)
	if saved.Sessions[o.NewSessionID].ID != o.Ref.ID || len(saved.Starting) != 0 {
		t.Errorf("saved after the start = %+v", saved)
	}
}

// The current coordinator never ran and a past one is resumed in its place:
// the project's coordinator id changes, and both ids stay held (the printed
// line of the one that never ran may still be run by hand).
func TestRestoreResumedCoordinatorIDChangesStaysHeld(t *testing.T) {
	var logged []route.Record
	f := &fakeAgterm{ws: "repo"}
	d := upDeps(t, f, &logged)
	w := newWorld(t)
	const never = "00000000-0000-4000-8000-0000000000c5"
	w.project.Coordinator = Session{ID: never, Name: "repo-coord", Started: now.Add(-time.Hour)}
	w.project.PastCoordinators = []string{coordID}
	if err := Save(w.project); err != nil {
		t.Fatal(err)
	}
	steps := w.plan(t, nil, nil)
	if s := steps[0]; s.Action != Resume || s.SessionID != coordID || s.Instead != never {
		t.Fatalf("step = %+v, want the past coordinator resumed", s)
	}
	check := func() {
		notAdoptable(t, coordID, w.project.Root, nil)
		notAdoptable(t, never, w.project.Root, nil)
	}
	o := outcomeOf(t, Apply(context.Background(), &w.project, steps, onNew(d, f, check, check)), "repo-coord")
	if o.Err != nil {
		t.Fatal(o.Err)
	}
	check()
	if saved := savedProject(t, w.project); saved.Coordinator.ID != coordID || !slices.Equal(saved.PastCoordinators, []string{never}) {
		t.Errorf("saved coordinator %+v, past %v", saved.Coordinator, saved.PastCoordinators)
	}
}

// A resumed coordinator cleared with /clear runs under another id: that id
// is held through its process.
func TestRestoreResumedCoordinatorClearedStaysHeld(t *testing.T) {
	var logged []route.Record
	f := &fakeAgterm{ws: "repo"}
	d := upDeps(t, f, &logged)
	w := newWorld(t)
	if err := Save(w.project); err != nil {
		t.Fatal(err)
	}
	const cleared = "00000000-0000-4000-8000-0000000000c6"
	live := map[string]claudecode.LiveSession{
		coordID: {PID: 901, Cwd: w.project.Root}, cleared: {PID: 901, Cwd: w.project.Root, Name: "repo-coord"},
	}
	check := func() {
		main := t.TempDir()
		all, err := All()
		if err != nil {
			t.Fatal(err)
		}
		if det := Detect(main, all, nil, DetectEnv{Live: live, Worktrees: []string{main, w.project.Root}}); len(det.Candidates) != 0 {
			t.Errorf("another project is offered %v", candIDs(det.Candidates))
		}
	}
	o := outcomeOf(t, Apply(context.Background(), &w.project, w.plan(t, nil, nil), onNew(d, f, check, check)), "repo-coord")
	if o.Err != nil || o.Step.Action != Resume {
		t.Fatalf("outcome = %+v", o)
	}
}

// A coordinator started fresh in place of one that never ran: the never-run
// id stays held too.
func TestRestoreFreshCoordinatorInPlaceOfNeverRunHoldsAll(t *testing.T) {
	w, f, d, _, _ := coordFresh(t)
	const never = "00000000-0000-4000-8000-0000000000c5"
	w.project.Coordinator.ID = never
	w.project.PastCoordinators = []string{coordID}
	if err := Save(w.project); err != nil {
		t.Fatal(err)
	}
	before := savedProject(t, w.project)
	steps := w.plan(t, nil, nil)
	if s := steps[0]; s.Action != Fresh || s.SessionID != coordID || s.Instead != never {
		t.Fatalf("step = %+v, want a fresh start from the past coordinator", s)
	}
	f.fail = map[string]bool{"repo-coord": true}
	if o := outcomeOf(t, Apply(context.Background(), &w.project, steps, d), "repo-coord"); o.Err == nil {
		t.Fatal("the launch did not fail")
	}
	sameState(t, savedProject(t, w.project), before)
	f.fail = nil
	o := outcomeOf(t, Apply(context.Background(), &w.project, steps, d), "repo-coord")
	if o.Err != nil {
		t.Fatal(o.Err)
	}
	if saved := savedProject(t, w.project); saved.Coordinator.ID != o.NewSessionID || !slices.Equal(saved.PastCoordinators, []string{never, coordID}) {
		t.Errorf("saved coordinator %+v, past %v", saved.Coordinator, saved.PastCoordinators)
	}
}

func TestRestoreFailedLaunchWithdrawsCoordinator(t *testing.T) {
	w, f, d, steps, logged := coordFresh(t)
	before := savedProject(t, w.project)
	f.fail = map[string]bool{"repo-coord": true}
	reserved := false
	d = onNew(d, f, func() { reserved = savedProject(t, w.project).Coordinator.ID != coordID }, nil)
	o := outcomeOf(t, Apply(context.Background(), &w.project, steps, d), "repo-coord")
	if o.Err == nil || !reserved {
		t.Fatalf("reserved %v, outcome = %+v", reserved, o)
	}
	sameState(t, savedProject(t, w.project), before)
	if len(*logged) != 0 || len(w.project.Done) != 0 {
		t.Errorf("logged %+v, done %v", *logged, w.project.Done)
	}
	// The next qm up plans the same start again.
	w.reload(t)
	if s := w.plan(t, nil, nil)[0]; s.Action != Fresh || s.SessionID != coordID || s.Instead != "" {
		t.Errorf("next step = %+v", s)
	}
}

func TestRestoreFailedLaunchWithdrawsWorker(t *testing.T) {
	w, f, d, steps, logged := workerFresh(t)
	before := savedProject(t, w.project)
	f.fail = map[string]bool{"w-large": true}
	reserved := false
	d = onNew(d, f, func() { reserved = len(savedProject(t, w.project).Starting) == 1 }, nil)
	o := outcomeOf(t, Apply(context.Background(), &w.project, steps, d), "w-large")
	if o.Err == nil || !reserved {
		t.Fatalf("reserved %v, outcome = %+v", reserved, o)
	}
	sameState(t, savedProject(t, w.project), before)
	for _, r := range *logged {
		if r.Kind == "worker" {
			t.Errorf("worker Record logged for a failed start: %+v", r)
		}
	}
}

// A state that cannot be saved starts nothing: the session would run unheld.
func TestRestoreUnsavedReservationStartsNothing(t *testing.T) {
	for name, setup := range map[string]func(*testing.T) (*world, *fakeAgterm, Deps, []Step, *[]route.Record){"repo-coord": coordFresh, "w-large": workerFresh} {
		w, f, d, steps, _ := setup(t)
		want := w.project.Coordinator
		cannotSave(t, w.project)
		o := outcomeOf(t, Apply(context.Background(), &w.project, steps, d), name)
		if o.Err == nil || !strings.Contains(o.Err.Error(), "not started") {
			t.Errorf("%s: err = %v, want one that says it was not started", name, o.Err)
		}
		if slices.Contains(f.verbs(), "session new "+name) {
			t.Errorf("%s: agtermctl calls = %v, want no start", name, f.verbs())
		}
		if w.project.Coordinator != want || len(w.project.PastCoordinators) != 0 || len(w.project.Starting) != 0 {
			t.Errorf("%s: project = %+v, want the reservation undone", name, w.project)
		}
	}
}

// A reservation that cannot be withdrawn on disk is, in memory, and the error
// says so: what the next qm up reads is what a crash would have left.
func TestRestoreWithdrawNotSaved(t *testing.T) {
	w, f, d, steps, _ := coordFresh(t)
	f.fail = map[string]bool{"repo-coord": true}
	d = onNew(d, f, func() { cannotSave(t, w.project) }, nil)
	o := outcomeOf(t, Apply(context.Background(), &w.project, steps, d), "repo-coord")
	if o.Err == nil || !strings.Contains(o.Err.Error(), "session new failed") || !strings.Contains(o.Err.Error(), "its reservation was not withdrawn") {
		t.Errorf("err = %v", o.Err)
	}
	if w.project.Coordinator.ID != coordID || len(w.project.PastCoordinators) != 0 {
		t.Errorf("coordinator %+v, past %v", w.project.Coordinator, w.project.PastCoordinators)
	}
}

// cannotSave replaces p's state file with a folder (blockSave).
func cannotSave(t *testing.T, p Project) {
	t.Helper()
	dir, err := projectsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, p.ID+".json")); err != nil {
		t.Fatal(err)
	}
	blockSave(t, p.ID)
}

// A launch that reports an error but may have started the session keeps the
// coordinator's reservation: the next qm up leaves it when it runs, else
// restores the previous coordinator.
func TestRestoreUnclearLaunchKeepsCoordinator(t *testing.T) {
	w, f, d, steps, _ := coordFresh(t)
	a := launch.Agterm{Bin: "agtermctl", Run: func(ctx context.Context, bin string, args ...string) ([]byte, error) {
		if args[0] == "session" && args[1] == "new" {
			return []byte(`{"ok":true,"result":{}}`), nil // opened, no id printed
		}
		return f.run(ctx, bin, args...)
	}}
	d.Launcher = a
	o := outcomeOf(t, Apply(context.Background(), &w.project, steps, d), "repo-coord")
	if !errors.Is(o.Err, launch.ErrMaybeStarted) {
		t.Fatalf("err = %v", o.Err)
	}
	id := startedID(t)
	notAdoptable(t, id, w.project.Root, nil)
	w.reload(t)
	if w.project.Coordinator.ID != id || !slices.Equal(w.project.PastCoordinators, []string{coordID}) {
		t.Errorf("saved coordinator %+v, past %v", w.project.Coordinator, w.project.PastCoordinators)
	}
	if s := w.plan(t, nil, nil)[0]; s.Action != Fresh || s.SessionID != coordID || s.Instead != id {
		t.Errorf("next step = %+v, want the previous coordinator", s)
	}
}

// The same for a fresh worker: its Record is logged, as that of a line
// printed for the user to run.
func TestRestoreUnclearLaunchKeepsWorker(t *testing.T) {
	w, f, d, _, logged := workerFresh(t)
	f.tree = []launch.SessionRef{{ID: "s-old", Name: "w-large"}}
	f.idle = map[string]bool{"s-old": true}
	f.typeErr = errors.New("agtermctl: signal: killed")
	w.project.Sessions = map[string]launch.SessionRef{largeID: {Launcher: "agterm", ID: "s-old", Name: "w-large"}}
	recs := w.largeRecs(now.Add(-4 * time.Hour))
	steps := w.plan(t, recs, coordRuns)
	o := outcomeOf(t, Apply(context.Background(), &w.project, steps, d), "w-large")
	if !errors.Is(o.Err, launch.ErrMaybeStarted) || !strings.Contains(o.Err.Error(), "may have received the launch line") {
		t.Fatalf("err = %v", o.Err)
	}
	id := startedID(t)
	i := slices.IndexFunc(*logged, func(r route.Record) bool { return r.Kind == "worker" })
	if i < 0 || (*logged)[i].ClaudeSession != id || (*logged)[i].TermID != "" || (*logged)[i].Replaces != largeID {
		t.Fatalf("logged = %+v, want the new session's Record", *logged)
	}
	recs = append(recs, *logged...)
	notAdoptable(t, id, filepath.Join(w.root, "worktrees", "large"), recs)
	w.reload(t)
	if _, ok := w.project.Sessions[id]; ok || len(w.project.Starting) != 0 {
		t.Errorf("saved = %+v, want the Record alone to hold it", w.project)
	}
	if steps := w.plan(t, recs, coordRuns); len(steps) != 2 || steps[1].SessionID != largeID || steps[1].Action != Fresh || steps[1].Instead != id {
		t.Errorf("next plan = %+v, want the old session", steps)
	}
}

// qm ends after the coordinator's id is saved, before the launch: the next
// qm up restores the previous coordinator, as for one that never ran. When
// the launch did happen, it finds the new coordinator running and leaves it.
func TestRestoreCrashAroundCoordinatorLaunch(t *testing.T) {
	for _, launched := range []bool{false, true} {
		w, f, d, steps, _ := coordFresh(t)
		crash := func() { panic(errCrash) }
		if launched {
			crashes(t, &w.project, steps, onNew(d, f, nil, crash))
		} else {
			crashes(t, &w.project, steps, onNew(d, f, crash, nil))
		}
		w.reload(t)
		id := w.project.Coordinator.ID
		if id != startedID(t) {
			t.Fatalf("launched %v: saved coordinator %+v", launched, w.project.Coordinator)
		}
		notAdoptable(t, id, w.project.Root, nil)
		if _, notes, err := RecoverStarts(&w.project, nil, d); len(notes) != 0 || err != nil {
			t.Errorf("launched %v: notes %v, err %v", launched, notes, err)
		}
		if launched {
			steps := w.plan(t, nil, []claudecode.Proc{{PID: 402, Cmd: "claude -n repo-coord --session-id " + id}})
			if len(steps) != 1 || steps[0].SessionID != id || steps[0].Action != Leave {
				t.Errorf("next plan = %+v, want the new coordinator left", steps)
			}
			continue
		}
		steps = w.plan(t, nil, nil)
		if s := steps[0]; s.Action != Fresh || s.SessionID != coordID || s.Instead != id {
			t.Fatalf("next step = %+v, want the previous coordinator", s)
		}
		o := outcomeOf(t, Apply(context.Background(), &w.project, steps, d), "repo-coord")
		if saved := savedProject(t, w.project); o.Err != nil || saved.Coordinator.ID != o.NewSessionID || !slices.Equal(saved.PastCoordinators, []string{id, coordID}) {
			t.Errorf("outcome %+v, saved %+v", o, saved)
		}
	}
}

// The same for a fresh worker: the next qm up logs its Record, so it is
// left when it runs and the session it replaces restored when it does not.
func TestRestoreCrashAroundWorkerLaunch(t *testing.T) {
	for _, launched := range []bool{false, true} {
		w, f, d, steps, logged := workerFresh(t)
		crash := func() { panic(errCrash) }
		if launched {
			crashes(t, &w.project, steps, onNew(d, f, nil, crash))
		} else {
			crashes(t, &w.project, steps, onNew(d, f, crash, nil))
		}
		w.reload(t)
		id := startedID(t)
		recs := w.largeRecs(now.Add(-4 * time.Hour))
		cwd := filepath.Join(w.root, "worktrees", "large")
		notAdoptable(t, id, cwd, recs)

		// What only reads the project (a dry run, qm up done, qm down) sees
		// the Record already, and changes nothing.
		before, n := savedProject(t, w.project), len(*logged)
		procs := coordRuns
		if launched {
			procs = append(slices.Clone(coordRuns), claudecode.Proc{PID: 402, Cmd: "claude -n w-large --session-id " + id})
		}
		check := func() {
			t.Helper()
			next := w.plan(t, recs, procs)
			switch {
			case len(next) != 2:
				t.Errorf("launched %v: next plan %+v", launched, next)
			case launched && (next[1].SessionID != id || next[1].Action != Leave):
				t.Errorf("next step = %+v, want the new session left", next[1])
			case !launched && (next[1].SessionID != largeID || next[1].Action != Fresh || next[1].Instead != id):
				t.Errorf("next step = %+v, want the old session", next[1])
			}
		}
		check()
		sameState(t, savedProject(t, w.project), before)
		if len(*logged) != n {
			t.Errorf("logged %+v", *logged)
		}

		var notes []string
		var err error
		recs, notes, err = RecoverStarts(&w.project, recs, d)
		if err != nil || len(notes) != 1 || !strings.Contains(notes[0], "w-large") {
			t.Errorf("launched %v: notes %q", launched, notes)
		}
		if r := (*logged)[len(*logged)-1]; r.Kind != "worker" || r.ClaudeSession != id || r.Replaces != largeID {
			t.Fatalf("launched %v: logged %+v", launched, *logged)
		}
		w.reload(t)
		if _, ok := w.project.Sessions[id]; ok || len(w.project.Starting) != 0 {
			t.Errorf("launched %v: saved %+v", launched, w.project)
		}
		notAdoptable(t, id, cwd, recs)
		check()
	}
}

// A worker Record that could not be logged after the start is logged by the
// next qm up, once.
func TestRestoreUnloggedWorkerRecordLoggedLater(t *testing.T) {
	w, _, d, steps, logged := workerFresh(t)
	write := d.Log
	d.Log = func(r route.Record) error {
		if r.Kind == "worker" {
			return errors.New("read-only file system")
		}
		return write(r)
	}
	o := outcomeOf(t, Apply(context.Background(), &w.project, steps, d), "w-large")
	if o.Err == nil || !strings.Contains(o.Err.Error(), "its worker Record was not logged") {
		t.Fatalf("err = %v", o.Err)
	}
	w.reload(t)
	recs := w.largeRecs(now.Add(-4 * time.Hour))
	if _, notes, err := RecoverStarts(&w.project, recs, d); err == nil || len(notes) != 0 || len(w.project.Starting) != 1 {
		t.Errorf("log still unwritable: notes %q, err %v, starting %v", notes, err, w.project.Starting)
	}
	d.Log = write
	got, _, err := RecoverStarts(&w.project, recs, d)
	r := (*logged)[len(*logged)-1]
	if err != nil || r.ClaudeSession != o.NewSessionID || r.TermID != o.Ref.ID || len(got) != 2 || len(w.project.Starting) != 0 {
		t.Errorf("recovered %+v, starting %v", r, w.project.Starting)
	}
	if w.project.Sessions[o.NewSessionID].ID != o.Ref.ID {
		t.Errorf("its terminal session was dropped: %+v", w.project.Sessions)
	}
}

// The terminal session of a worker whose Record is not logged yet is no
// longer listed (its tab moved) or closed: the worker stays held.
func TestRestoreUnloggedWorkerTabGoneStaysHeld(t *testing.T) {
	w, _, d, steps, logged := workerFresh(t)
	write := d.Log
	d.Log = func(r route.Record) error { return errors.New("read-only file system") }
	o := outcomeOf(t, Apply(context.Background(), &w.project, steps, d), "w-large")
	w.reload(t)
	if !pruneUnlisted(&w.project, map[string]map[string]launch.SessionRef{"agterm": {}}, nil) || w.project.Sessions[o.NewSessionID].ID != "" {
		t.Fatalf("sessions = %+v, want the tab forgotten", w.project.Sessions)
	}
	if err := Save(w.project); err != nil {
		t.Fatal(err)
	}
	recs := w.largeRecs(now.Add(-4 * time.Hour))
	cwd := filepath.Join(w.root, "worktrees", "large")
	notAdoptable(t, o.NewSessionID, cwd, recs)
	d.Log = write
	recs, _, err := RecoverStarts(&w.project, recs, d)
	if _, ok := w.project.Sessions[o.NewSessionID]; err != nil || ok || len(*logged) != 1 {
		t.Errorf("err %v, sessions %+v, logged %+v", err, w.project.Sessions, *logged)
	}
	notAdoptable(t, o.NewSessionID, cwd, recs)
}

// The previous coordinator is resumed by hand after a reserved one never
// ran: it is the project's coordinator again, and the other id stays held.
func TestRestoreLeftPastCoordinatorIsCurrentAgain(t *testing.T) {
	const never = "00000000-0000-4000-8000-0000000000c5"
	runs := map[string]claudecode.LiveSession{coordID: {PID: 301, Cwd: "/x", Name: "repo-coord"}}
	for name, late := range map[string]bool{"before the plan": false, "while qm up asked": true} {
		w, f, d, _, _ := coordFresh(t)
		w.project.Coordinator.ID, w.project.PastCoordinators = never, []string{coordID}
		if err := Save(w.project); err != nil {
			t.Fatal(err)
		}
		procs := coordRuns
		if late {
			procs = nil
			d.Live = func() (map[string]claudecode.LiveSession, error) { return runs, nil }
		}
		steps := w.plan(t, nil, procs)
		o := outcomeOf(t, Apply(context.Background(), &w.project, steps, d), "repo-coord")
		if o.Err != nil || o.Step.Action != Leave || slices.Contains(f.verbs(), "session new repo-coord") {
			t.Fatalf("%s: outcome %+v", name, o)
		}
		if saved := savedProject(t, w.project); saved.Coordinator.ID != coordID || !slices.Equal(saved.PastCoordinators, []string{never}) {
			t.Errorf("%s: saved coordinator %+v, past %v", name, saved.Coordinator, saved.PastCoordinators)
		}
	}
}

// A reservation whose Record was logged (qm ended before the save after it)
// is cleared without logging it twice, and its terminal session read from
// the Record.
func TestRestoreRecoverLogsOnce(t *testing.T) {
	w, f, d, steps, logged := workerFresh(t)
	crashes(t, &w.project, steps, onNew(d, f, func() { panic(errCrash) }, nil))
	w.reload(t)
	id := startedID(t)
	rec := w.project.Starting[id]
	rec.TermLauncher, rec.TermID = "agterm", "s-9"
	recs := append(w.largeRecs(now.Add(-4*time.Hour)), rec)
	if got := effectiveSessions(w.project, recs)[id]; got.ID != "s-9" {
		t.Errorf("the Record's terminal session is hidden by the reservation: %+v", got)
	}
	n := len(*logged)
	got, notes, err := RecoverStarts(&w.project, recs, d)
	if err != nil || len(got) != len(recs) || len(notes) != 0 || len(*logged) != n || len(w.project.Starting) != 0 {
		t.Errorf("records %+v, notes %q, starting %v", got, notes, w.project.Starting)
	}
	w.reload(t)
	if len(w.project.Starting) != 0 {
		t.Errorf("saved starting %v", w.project.Starting)
	}
}

// A first run and qm up --here save before the process is visible.
func TestFirstRunHeldBeforeItStarts(t *testing.T) {
	var logged []route.Record
	f := &fakeAgterm{ws: "repo"}
	d := upDeps(t, f, &logged)
	root := t.TempDir()
	ran := false
	d = onNew(d, f, func() {
		ran = true
		p, ok, err := Load(root)
		if err != nil || !ok || p.Coordinator.ID != startedID(t) {
			t.Errorf("saved before the start: %+v, %v, %v", p.Coordinator, ok, err)
		}
	}, nil)
	if _, _, err := Start(context.Background(), root, nil, d); err != nil || !ran {
		t.Fatalf("ran %v, err %v", ran, err)
	}
}

func TestRestoreHereHeldBeforeItRuns(t *testing.T) {
	w, _, d, steps, _ := coordFresh(t)
	d.Here = func(context.Context, *Project, []string, bool) (launch.SessionRef, string) {
		return launch.SessionRef{Launcher: "agterm", ID: "s-here", Name: "repo-coord"}, ""
	}
	o := outcomeOf(t, Apply(context.Background(), &w.project, steps, d), "repo-coord")
	if o.Err != nil || !o.Here {
		t.Fatalf("outcome = %+v", o)
	}
	// Apply returned: the caller runs the script only now.
	if saved := savedProject(t, w.project); saved.Coordinator.ID != startedID(t) || saved.Sessions[o.NewSessionID].ID != "s-here" {
		t.Errorf("saved = %+v", saved)
	}
	notAdoptable(t, o.NewSessionID, w.project.Root, nil)
}
