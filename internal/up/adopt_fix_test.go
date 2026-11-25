package up

import (
	"bufio"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
	"github.com/IZZY-Agency/quartermaster/internal/launch"
)

// inOther puts the hand-started worker's tab s-h in workspace other, so
// adopting it would move it.
func (w *adoptWorld) inOther() {
	w.agterm.ws[0].Sessions = w.agterm.ws[0].Sessions[:2]
	w.agterm.ws[1].Sessions = []launch.SessionRef{{ID: "s-h", Name: "w-hand"}}
}

// touched says whether any agterm call other than the tree read names tab.
func (w *adoptWorld) touched(tab string) bool {
	for _, c := range w.agterm.calls {
		if c != "tree --json" && strings.Contains(c, tab) {
			return true
		}
	}
	return false
}

// An inherited AGTERM_SESSION_ID names a tab that runs something else: the
// tab is not recorded, moved or pinned.
func TestAdoptTabRunsSomethingElse(t *testing.T) {
	w := newAdoptWorld(t)
	w.inOther()
	w.agterm.fg["s-h"] = []string{"npm", "run", "dev"}
	ap := w.adopt(t, allThree)
	if _, ok := ap.Project.Sessions[adHand]; ok || len(ap.Moves) != 0 || w.touched("s-h") {
		t.Fatalf("a tab running something else was taken: sessions %+v, moves %+v, calls %q", ap.Project.Sessions, ap.Moves, w.agterm.calls)
	}
	if _, ok := ap.Project.Adopted[adHand]; !ok || !notesHold(ap.Notes, "does not run it") {
		t.Fatalf("not adopted, or no note: %q", ap.Notes)
	}
}

// A tab whose claude resumes the candidate's session id is its own, wherever
// its folder; one that resumes another id is not.
func TestAdoptTabMatchesBySessionID(t *testing.T) {
	w := newAdoptWorld(t)
	w.inOther()
	w.procs[2].Cmd = "claude --resume " + adHand
	w.agterm.fg["s-h"] = strings.Fields(w.procs[2].Cmd)
	w.agterm.cwd["s-h"] = filepath.Join(w.root, "elsewhere")
	ap := w.adopt(t, allThree)
	if got := ap.Project.Sessions[adHand]; got.ID != "s-h" || len(ap.Moves) != 1 {
		t.Fatalf("by id: ref %+v, moves %+v", got, ap.Moves)
	}

	w = newAdoptWorld(t)
	w.inOther()
	w.agterm.fg["s-h"] = []string{"claude", "-r", adQM}
	ap = w.adopt(t, allThree)
	if _, ok := ap.Project.Sessions[adHand]; ok || len(ap.Moves) != 0 || w.touched("s-h") {
		t.Fatalf("other id: sessions %+v, moves %+v", ap.Project.Sessions, ap.Moves)
	}
}

// A tab whose claude names no session matches only in the candidate's own
// folder.
func TestAdoptTabMatchesByFolder(t *testing.T) {
	w := newAdoptWorld(t)
	w.inOther()
	w.agterm.cwd["s-h"] = w.repo
	ap := w.adopt(t, allThree)
	if _, ok := ap.Project.Sessions[adHand]; ok || len(ap.Moves) != 0 || w.touched("s-h") {
		t.Fatalf("other folder: sessions %+v, moves %+v", ap.Project.Sessions, ap.Moves)
	}
}

// Every ref adoption records is marked Adopted: the user's own tab.
func TestAdoptRefsMarkedAdopted(t *testing.T) {
	w := newAdoptWorld(t)
	w.adopt(t, allThree)
	p, _, _ := Load(w.repo)
	if len(p.Sessions) != 3 {
		t.Fatalf("sessions %+v", p.Sessions)
	}
	for id, ref := range p.Sessions {
		if !ref.Adopted {
			t.Fatalf("%s: ref %+v not marked adopted", id, ref)
		}
	}
}

// The coordinator's id is live again, but held by another process (it exited
// and was resumed in another tab): it is gone, nothing is adopted. A worker
// in the same case is dropped.
func TestAdoptSameIDOtherProcess(t *testing.T) {
	w := newAdoptWorld(t)
	ap, err := w.plan(t, allThree, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	w.procs[2].PID = 513
	w.session(t, 513, adHand, w.wtHand, "w-hand")
	if err := Adopt(context.Background(), &ap, w.deps()); err != nil {
		t.Fatal(err)
	}
	p, _, _ := Load(w.repo)
	if _, ok := p.Adopted[adHand]; ok || w.agterm.has("session restore --none --target s-h --json") {
		t.Fatalf("worker held by another process adopted: %+v", p.Adopted)
	}
	if !notesHold(ap.Notes, "w-hand exited before the confirmation") {
		t.Fatalf("notes %q", ap.Notes)
	}

	w = newAdoptWorld(t)
	ap, err = w.plan(t, allThree, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	w.procs[0].PID = 511
	w.session(t, 511, adCoord, w.repo, "proj-coord")
	if err := Adopt(context.Background(), &ap, w.deps()); !errors.Is(err, ErrCoordinatorGone) {
		t.Fatalf("err %v, want ErrCoordinatorGone", err)
	}
	if _, ok, _ := Load(w.repo); ok {
		t.Fatal("state written")
	}
}

// A Save that fails leaves the user's tabs where they were: nothing moved
// or pinned.
func TestAdoptSaveFailsMovesNothing(t *testing.T) {
	w := newAdoptWorld(t)
	w.inOther()
	ap, err := w.plan(t, allThree, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(ap.Moves) != 1 {
		t.Fatalf("moves %+v", ap.Moves)
	}
	// The state file alone is blocked: the adoption lock beside it works, so
	// the run fails at the save itself.
	blockSave(t, ProjectID(w.repo))
	err = Adopt(context.Background(), &ap, w.deps())
	if err == nil || strings.Contains(err.Error(), "lock") {
		t.Fatalf("Adopt err %v, want the save's", err)
	}
	for _, v := range w.agterm.verbs() {
		if v != "tree" {
			t.Fatalf("acted before saving: %q", w.agterm.calls)
		}
	}
}

// A move that fails after the first save is saved again without that ref.
func TestAdoptMoveFailsSavedAgain(t *testing.T) {
	w := newAdoptWorld(t)
	w.inOther()
	w.agterm.fail = map[string]bool{"session move": true}
	w.adopt(t, allThree)
	p, _, _ := Load(w.repo)
	if _, ok := p.Sessions[adHand]; ok || p.Sessions[adCoord].ID != "s-c" {
		t.Fatalf("sessions %+v", p.Sessions)
	}
}

// A session inside the root or a worktree, but not in that folder itself,
// is listed as in a subfolder, after the folder's own sessions.
func TestDetectSubfolder(t *testing.T) {
	w := newAdoptWorld(t)
	app := filepath.Join(w.repo, "app")
	mkdir(t, app)
	w.side(t)
	w.session(t, 414, adSide, app, "side")
	got := w.detect(t)
	if len(got) != 4 || got[1].SessionID != adSide || !got[1].Subfolder || got[0].Subfolder {
		t.Fatalf("candidates %+v", got)
	}
	if !strings.Contains(RenderCandidates(got, now), "runs in a subfolder: not adoptable") {
		t.Fatalf("rows:\n%s", RenderCandidates(got, now))
	}
	ap, err := w.plan(t, allThree, nil, false)
	if err != nil || !strings.Contains(RenderAdopt(ap), "not adopted: side (00000000), runs in a subfolder") {
		t.Fatalf("plan %v:\n%s", err, RenderAdopt(ap))
	}
	if _, err := w.plan(t, Choice{Coordinator: adCoord, Workers: []string{adSide}}, nil, false); !errors.Is(err, ErrInput) {
		t.Fatalf("subfolder worker: err %v, want ErrInput", err)
	}
}

func TestChooseAutoSubfolder(t *testing.T) {
	cs := []AdoptCandidate{{SessionID: "a", Name: "a", Cwd: "/r/app", Subfolder: true}, {SessionID: "b", Name: "b", Cwd: "/r/app", Worker: true, Subfolder: true}}
	if _, err := ChooseAuto(Detected{Candidates: cs}, "/r", ""); !errors.Is(err, ErrAdoptCoordinator) {
		t.Fatalf("err %v, want ErrAdoptCoordinator", err)
	}
	if _, err := ChooseAuto(Detected{Candidates: cs}, "/r", "a"); !errors.Is(err, ErrInput) {
		t.Fatalf("by id: err %v, want ErrInput", err)
	}
	cs = append(cs, AdoptCandidate{SessionID: "c", Name: "c", Cwd: "/r"})
	ch, err := ChooseAuto(Detected{Candidates: cs}, "/r", "")
	if err != nil || ch.Coordinator != "c" || len(ch.Workers) != 0 {
		t.Fatalf("ChooseAuto = %+v, %v", ch, err)
	}
	cs[0].Self = true
	if _, err := ChooseAuto(Detected{Candidates: cs}, "/r", ""); !errors.Is(err, ErrSelfElsewhere) || !strings.Contains(err.Error(), "runs in /r/app") {
		t.Fatalf("self in a subfolder: err %v", err)
	}
}

func TestAskAdoptRefusesSubfolder(t *testing.T) {
	cs := []AdoptCandidate{{SessionID: "a", Name: "a"}, {SessionID: "b", Name: "b", Worker: true, Subfolder: true}}
	for _, in := range []string{"2\n", "1\n2\n"} {
		if _, err := AskAdopt(cs, bufio.NewReader(strings.NewReader(in)), io.Discard); !errors.Is(err, ErrInput) {
			t.Fatalf("%q: err %v, want ErrInput", in, err)
		}
	}
	ch, err := AskAdopt(cs, bufio.NewReader(strings.NewReader("1\na\n")), io.Discard)
	if err != nil || len(ch.Workers) != 0 {
		t.Fatalf("a: %+v, %v", ch, err)
	}
}

// qm up --adopt run by a session outside the project's folders (a
// coordinator whose Bash sits in a worker's worktree) adopts nothing: no
// fallback to another session.
func TestChooseAutoSelfElsewhere(t *testing.T) {
	w := newAdoptWorld(t)
	w.procs[0].Cmd = "claude -n proj-coord"
	w.session(t, 411, adCoord, filepath.Join(w.root, "elsewhere"), "proj-coord")
	w.procs = append(w.procs, claudecode.Proc{PID: 900, PPID: 411, Cmd: "/bin/zsh -c qm up"}, claudecode.Proc{PID: 901, PPID: 900, Cmd: "qm up"})
	w.selfPID = 901
	det := w.detected(t)
	if !det.SelfElsewhere || det.SelfCwd != filepath.Join(w.root, "elsewhere") {
		t.Fatalf("detected %+v", det)
	}
	_, err := ChooseAuto(det, w.repo, "")
	if !errors.Is(err, ErrSelfElsewhere) || !strings.Contains(err.Error(), "this session runs in "+filepath.Join(w.root, "elsewhere")+", not in this project's folders: run qm up there") {
		t.Fatalf("err %v", err)
	}
	if ch, err := ChooseAuto(det, w.repo, adHand); err != nil || ch.Coordinator != adHand {
		t.Fatalf("explicit: %+v, %v", ch, err)
	}
}
