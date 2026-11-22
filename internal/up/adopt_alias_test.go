package up

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/route"
)

// adClear is the id the coordinator (pid 411) has after /clear.
const adClear = "00000000-0000-4000-8000-0000000004c1"

// cleared makes the coordinator's process run /clear: its arguments keep
// adCoord, its session file names adClear. Both ids are the one process.
func (w *adoptWorld) cleared(t *testing.T) {
	t.Helper()
	w.procs[0].Cmd = "claude --resume " + adCoord + " -n proj-coord --model opus"
	fixture(t, filepath.Join("sessions-cleared", "411.json"), w.root, filepath.Join(w.claude, "sessions", "411.json"))
	live := w.live(t)
	if live[adCoord].PID != 411 || live[adClear].PID != 411 {
		t.Fatalf("live %v: want both ids on pid 411", live)
	}
}

// After /clear, the coordinator's new id is held with its old one: a later
// run never offers the coordinator as a worker.
func TestDetectHeldCoversTheCoordinatorAfterClear(t *testing.T) {
	w, p := laterWorld(t)
	w.cleared(t)
	if got := candIDs(w.detectHeld(t, Held(p, w.recs))); slices.Contains(got, adClear) || slices.Contains(got, adCoord) {
		t.Fatalf("the coordinator offered as a worker: %v", got)
	}
}

// Another project's coordinator after /clear is held under both ids too.
func TestDetectHeldElsewhereCoversBothIDs(t *testing.T) {
	w := newAdoptWorld(t)
	w.cleared(t)
	o := Project{ID: "other-1", Root: w.wtQM, Coordinator: Session{ID: adCoord, Name: "o-coord"}}
	got := candIDs(Detect(w.repo, []Project{o}, w.recs, DetectEnv{
		Procs: w.procs, Live: w.live(t), Worktrees: []string{w.repo, w.wtQM, w.wtHand},
		TranscriptPath: w.transcriptPath, SelfPID: w.selfPID,
	}).Candidates)
	if slices.Contains(got, adClear) {
		t.Fatalf("another project's coordinator offered under its new id: %v", got)
	}
}

// The recheck under the adoption lock drops a worker that is the project's
// own coordinator under another id.
func TestAdoptWorkersCoordinatorAliasDroppedUnderTheLock(t *testing.T) {
	w, p := laterWorld(t)
	w.cleared(t)
	ap, err := PlanAdoptWorkers(context.Background(), p, w.all, w.detectHeld(t, nil), []string{adClear}, w.deps())
	if err != nil {
		t.Fatal(err)
	}
	if err := Adopt(context.Background(), &ap, w.deps()); err != nil {
		t.Fatal(err)
	}
	cur, _, _ := LoadID(p.ID)
	if _, ok := cur.Adopted[adClear]; ok || len(ap.Workers) != 0 {
		t.Fatalf("the coordinator adopted as a worker: %v", cur.Adopted)
	}
	if !notesHold(ap.Notes, "not adopted") {
		t.Fatalf("notes %q", ap.Notes)
	}
}

// The recheck under the adoption lock drops a worker that another project
// holds under another id of the same process.
func TestAdoptWorkersOtherProjectAliasDroppedUnderTheLock(t *testing.T) {
	w, p := laterWorld(t)
	w.cleared(t)
	o := Project{
		ID: ProjectID(w.wtHand), Root: w.wtHand, Workspace: "hand", Launcher: "agterm",
		Coordinator: Session{ID: "00000000-0000-4000-8000-0000000000c9", Name: "hand-coord"},
		Created:     now,
	}
	if err := Save(o); err != nil {
		t.Fatal(err)
	}
	ap, err := PlanAdoptWorkers(context.Background(), o, []Project{o}, w.detectHeld(t, nil), []string{adClear}, w.deps())
	if err != nil {
		t.Fatal(err)
	}
	if err := Adopt(context.Background(), &ap, w.deps()); err != nil {
		t.Fatal(err)
	}
	cur, _, _ := LoadID(o.ID)
	if _, ok := cur.Adopted[adClear]; ok || !notesHold(ap.Notes, "now held by project "+p.ID) {
		t.Fatalf("adopted %v, notes %q", cur.Adopted, ap.Notes)
	}
}

// otherWithRecordedTab saves a project whose worker's tab s-h is recorded
// only in its worker Record (a fresh spawn's), and returns the records.
func (w *adoptWorld) otherWithRecordedTab(t *testing.T) (Project, []route.Record) {
	t.Helper()
	o := Project{
		ID: "other-1", Root: w.wtQM, Workspace: "elsewhere", Launcher: "agterm",
		Coordinator: Session{ID: "00000000-0000-4000-8000-0000000000c8", Name: "o-coord"},
		Created:     now,
	}
	if err := Save(o); err != nil {
		t.Fatal(err)
	}
	recs := append(slices.Clone(w.recs), route.Record{
		Time: now.Add(-time.Hour), Kind: "worker", ClaudeSession: "00000000-0000-4000-8000-0000000000e1", Name: "w-gone",
		UpProject: o.ID, Cwd: w.wtQM, TermLauncher: "agterm", TermID: "s-h", Decision: &route.Decision{},
	})
	return o, recs
}

// A tab another project records only in a worker Record (Gather restores
// and closes it from there) is not taken at the plan.
func TestAdoptWorkersTabInAnotherProjectsRecordIsNotTaken(t *testing.T) {
	w, p := laterWorld(t)
	o, recs := w.otherWithRecordedTab(t)
	d := w.deps()
	d.Records = func() ([]route.Record, error) { return recs, nil }
	ap, err := PlanAdoptWorkers(context.Background(), p, append(w.all, o), w.detectHeld(t, Held(p, w.recs)), []string{adHand}, d)
	if err != nil {
		t.Fatal(err)
	}
	if ref := ap.Project.Sessions[adHand]; ref.ID != "" || slices.Contains(ap.Pins, "s-h") {
		t.Fatalf("a tab another project's Record holds was taken: %+v, pins %v", ref, ap.Pins)
	}
}

// A tab another project recorded in a worker Record since the plan is not
// recorded: the recheck under the lock reads the Records too.
func TestAdoptWorkersTabRecordedMeanwhileInARecord(t *testing.T) {
	w, p := laterWorld(t)
	ap := w.laterPlan(t, p, adHand)
	if ap.Project.Sessions[adHand].ID != "s-h" {
		t.Fatalf("planned ref %+v", ap.Project.Sessions)
	}
	_, recs := w.otherWithRecordedTab(t)
	d := w.deps()
	d.Records = func() ([]route.Record, error) { return recs, nil }
	if err := Adopt(context.Background(), &ap, d); err != nil {
		t.Fatal(err)
	}
	cur, _, _ := LoadID(p.ID)
	if _, ok := cur.Sessions[adHand]; ok || slices.Contains(ap.Pins, "s-h") {
		t.Fatalf("tab recorded twice: %v pins %v", cur.Sessions, ap.Pins)
	}
	if !notesHold(ap.Notes, "now recorded by project other-1") {
		t.Fatalf("notes %q", ap.Notes)
	}
}

// A ref the other project dropped (qm down left it open) is not its any
// more: the tab is taken.
func TestAdoptWorkersTabDroppedByAnotherProjectIsTaken(t *testing.T) {
	w, p := laterWorld(t)
	o, recs := w.otherWithRecordedTab(t)
	o.Dropped = map[string]bool{"00000000-0000-4000-8000-0000000000e1": true}
	if err := Save(o); err != nil {
		t.Fatal(err)
	}
	d := w.deps()
	d.Records = func() ([]route.Record, error) { return recs, nil }
	ap, err := PlanAdoptWorkers(context.Background(), p, append(w.all, o), w.detectHeld(t, Held(p, w.recs)), []string{adHand}, d)
	if err != nil {
		t.Fatal(err)
	}
	if err := Adopt(context.Background(), &ap, d); err != nil {
		t.Fatal(err)
	}
	cur, _, _ := LoadID(p.ID)
	if cur.Sessions[adHand].ID != "s-h" {
		t.Fatalf("a dropped ref still held the tab: %v, notes %q", cur.Sessions, ap.Notes)
	}
}

// A planned tab that runs another program by the confirmation (the worker
// suspended, its pid unchanged) is not recorded, moved or pinned: the
// worker is adopted with no terminal session.
func TestAdoptWorkersTabRepurposedBeforeConfirmation(t *testing.T) {
	w, p := laterWorld(t)
	ap := w.laterPlan(t, p, adHand)
	if ap.Project.Sessions[adHand].ID != "s-h" {
		t.Fatalf("planned ref %+v", ap.Project.Sessions)
	}
	w.agterm.fg["s-h"] = []string{"vim", "notes.md"}
	if err := Adopt(context.Background(), &ap, w.deps()); err != nil {
		t.Fatal(err)
	}
	cur, _, _ := LoadID(p.ID)
	if _, ok := cur.Adopted[adHand]; !ok {
		t.Fatalf("worker not adopted: %v", cur.Adopted)
	}
	if _, ok := cur.Sessions[adHand]; ok || slices.Contains(ap.Pins, "s-h") {
		t.Fatalf("the repurposed tab was recorded: %v pins %v", cur.Sessions, ap.Pins)
	}
	if !notesHold(ap.Notes, "its agterm tab s-h no longer runs it") {
		t.Fatalf("notes %q", ap.Notes)
	}
}

// A planned tab moved to another workspace by the confirmation is not
// recorded either.
func TestAdoptWorkersTabMovedBeforeConfirmation(t *testing.T) {
	w, p := laterWorld(t)
	ap := w.laterPlan(t, p, adHand)
	if ap.Project.Sessions[adHand].ID != "s-h" {
		t.Fatalf("planned ref %+v", ap.Project.Sessions)
	}
	ws := w.agterm.ws
	ws[0].Sessions = slices.DeleteFunc(ws[0].Sessions, func(s launch.SessionRef) bool { return s.ID == "s-h" })
	ws[1].Sessions = append(ws[1].Sessions, launch.SessionRef{ID: "s-h", Name: "w-hand"})
	if err := Adopt(context.Background(), &ap, w.deps()); err != nil {
		t.Fatal(err)
	}
	cur, _, _ := LoadID(p.ID)
	if _, ok := cur.Sessions[adHand]; ok || slices.Contains(w.agterm.calls, "session move s-h") {
		t.Fatalf("the moved tab was recorded: %v calls %v", cur.Sessions, w.agterm.calls)
	}
	if !notesHold(ap.Notes, "its agterm tab s-h is now in workspace other") {
		t.Fatalf("notes %q", ap.Notes)
	}
}

// A tmux window whose pane runs another job in its foreground (TMUX_PANE
// inherited, say) is not recorded for the worker.
func TestAdoptTmuxWindowRunningAnotherJobNotRecorded(t *testing.T) {
	w := newAdoptWorld(t)
	w.term[411] = claudecode.TermEnv{TmuxPane: "%1"}
	w.term[412] = claudecode.TermEnv{TmuxPane: "%2"}
	w.panes["%1"] = [3]string{"tmx", "@1", "proj-coord"}
	w.panes["%2"] = [3]string{"tmx", "@2", "w-qm"}
	w.notFg = map[string]bool{"@2": true}
	ap := w.adopt(t, allThree)
	p, _, _ := Load(w.repo)
	if p.Sessions[adCoord].ID != "@1" {
		t.Fatalf("coordinator ref %+v", p.Sessions[adCoord])
	}
	if _, ok := p.Sessions[adQM]; ok || !notesHold(ap.Notes, "its tmux window @2 does not run it in its foreground") {
		t.Fatalf("worker ref %+v, notes %q", p.Sessions[adQM], ap.Notes)
	}
}

// A tmux window whose pane's foreground changed between the plan and the
// confirmation is not recorded: the check is repeated under the lock.
func TestAdoptTmuxWindowForegroundChangedBeforeConfirmation(t *testing.T) {
	w := newAdoptWorld(t)
	w.term[411] = claudecode.TermEnv{TmuxPane: "%1"}
	w.term[412] = claudecode.TermEnv{TmuxPane: "%2"}
	w.panes["%1"] = [3]string{"tmx", "@1", "proj-coord"}
	w.panes["%2"] = [3]string{"tmx", "@2", "w-qm"}
	ap, err := w.plan(t, allThree, nil, false)
	if err != nil || ap.Project.Sessions[adQM].ID != "@2" {
		t.Fatalf("plan %+v, %v", ap.Project.Sessions, err)
	}
	w.notFg = map[string]bool{"@2": true}
	w.windowPanes = map[string]int{"@1": 2}
	if err := Adopt(context.Background(), &ap, w.deps()); err != nil {
		t.Fatal(err)
	}
	p, _, _ := Load(w.repo)
	if _, ok := p.Sessions[adQM]; ok {
		t.Fatalf("worker ref %+v recorded", p.Sessions[adQM])
	}
	if _, ok := p.Sessions[adCoord]; ok || !notesHold(ap.Notes, "has 2 panes") {
		t.Fatalf("coordinator ref %+v, notes %q", p.Sessions[adCoord], ap.Notes)
	}
}

// A reviewer launched as one id and cleared to another is left out under both
// ids: its Record names the id it was launched with.
func TestDetectReviewClearedToAnotherIDLeftOut(t *testing.T) {
	w := newAdoptWorld(t)
	w.recs = append(w.recs, route.Record{Time: now, Kind: "worker", Type: "review", ClaudeSession: adCoord})
	w.cleared(t) // pid 411 is adCoord by its arguments and adClear by its session file
	got := candIDs(w.detect(t))
	if slices.Contains(got, adClear) || slices.Contains(got, adCoord) {
		t.Fatalf("the reviewer offered as a worker: %v", got)
	}
}

// A pane that cannot be probed under the lock (ps or tmux failed) records no
// terminal session, but is not marked Dropped as a pane that runs another job
// is: the failure may be transient, and the window is read again next time.
func TestAdoptTmuxWindowProbeFailureIsNotDropped(t *testing.T) {
	adopt := func(t *testing.T, mod func(w *adoptWorld)) Project {
		w := newAdoptWorld(t)
		w.term[411] = claudecode.TermEnv{TmuxPane: "%1"}
		w.term[412] = claudecode.TermEnv{TmuxPane: "%2"}
		w.panes["%1"] = [3]string{"tmx", "@1", "proj-coord"}
		w.panes["%2"] = [3]string{"tmx", "@2", "w-qm"}
		ap, err := w.plan(t, allThree, nil, false)
		if err != nil || ap.Project.Sessions[adQM].ID != "@2" {
			t.Fatalf("plan %+v, %v", ap.Project.Sessions, err)
		}
		mod(w)
		if err := Adopt(context.Background(), &ap, w.deps()); err != nil {
			t.Fatal(err)
		}
		p, _, _ := Load(w.repo)
		if _, ok := p.Sessions[adQM]; ok {
			t.Fatalf("worker ref %+v recorded", p.Sessions[adQM])
		}
		if p.Sessions[adCoord].ID != "@1" {
			t.Fatalf("coordinator ref %+v", p.Sessions[adCoord])
		}
		return p
	}
	failed := adopt(t, func(w *adoptWorld) { w.paneErr = map[string]error{"@2": errors.New("tmux: server exited")} })
	if failed.Dropped[adQM] {
		t.Errorf("a failed probe marked the session dropped: %v", failed.Dropped)
	}
	other := adopt(t, func(w *adoptWorld) { w.notFg = map[string]bool{"@2": true} })
	if !other.Dropped[adQM] {
		t.Errorf("a window that runs another job was not marked dropped: %v", other.Dropped)
	}
}

// A planned move whose destination no longer resolves to the planned
// workspace id by the confirmation (the workspace was renamed or closed, or
// another one took its name) is dropped with its ref: nothing is moved or
// pinned, and the worker is adopted with no terminal session.
func TestAdoptWorkersMoveDestinationChangedBeforeConfirmation(t *testing.T) {
	for name, change := range map[string]func(ws []launch.Workspace){
		"renamed":    func(ws []launch.Workspace) { ws[0].Name = "renamed" },
		"another id": func(ws []launch.Workspace) { ws[0].ID = "ws-9" },
		"closed":     func(ws []launch.Workspace) { ws[0].Name, ws[0].ID = "", "" },
	} {
		t.Run(name, func(t *testing.T) {
			w, p := laterWorld(t)
			w.side(t) // 414 sits in workspace other (ws-2)
			ap := w.laterPlan(t, p, adSide)
			if len(ap.Moves) != 1 || ap.Moves[0].ToID != "ws-1" || ap.Project.Sessions[adSide].ID != "s-x" {
				t.Fatalf("plan: moves %+v, refs %+v", ap.Moves, ap.Project.Sessions)
			}
			if w.agterm.ws[0].ID != "ws-1" || w.agterm.ws[0].Name != "proj" {
				t.Fatalf("workspace 0 is %q %q, not the project's", w.agterm.ws[0].ID, w.agterm.ws[0].Name)
			}
			change(w.agterm.ws)
			if err := Adopt(context.Background(), &ap, w.deps()); err != nil {
				t.Fatal(err)
			}
			cur, _, _ := LoadID(p.ID)
			if _, ok := cur.Sessions[adSide]; ok || len(ap.Moves) != 0 || slices.Contains(ap.Pins, "s-x") {
				t.Fatalf("refs %v, moves %+v, pins %v: want the move and the ref dropped", cur.Sessions, ap.Moves, ap.Pins)
			}
			if slices.ContainsFunc(w.agterm.calls, func(c string) bool { return strings.HasPrefix(c, "session move") }) {
				t.Fatalf("a tab was moved: %q", w.agterm.calls)
			}
			if _, ok := cur.Adopted[adSide]; !ok || !notesHold(ap.Notes, "no longer resolves to the planned workspace") {
				t.Fatalf("adopted %v, notes %q", cur.Adopted, ap.Notes)
			}
		})
	}
}
