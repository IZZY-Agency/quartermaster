package up

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/route"
)

// laterWorld is the adoptWorld of a project that has state already: the
// running session adCoord is its coordinator, in tab s-c of workspace proj.
func laterWorld(t *testing.T) (*adoptWorld, Project) {
	t.Helper()
	w := newAdoptWorld(t)
	p := Project{
		ID: ProjectID(w.repo), Root: w.repo, Workspace: "proj", Launcher: "agterm",
		ClaudeArgs:  []string{"--model", "opus"},
		Coordinator: Session{ID: adCoord, Name: "proj-coord", Started: now.Add(-6 * time.Hour), Adopted: true},
		Sessions:    map[string]launch.SessionRef{adCoord: {Launcher: "agterm", ID: "s-c", Name: "proj-coord"}},
		Done:        map[string]time.Time{"00000000-0000-4000-8000-0000000000d1": now},
		Created:     now.Add(-6 * time.Hour),
	}
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	w.all = []Project{p}
	return w, p
}

func (w *adoptWorld) detectHeld(t *testing.T, held map[string]bool) []AdoptCandidate {
	t.Helper()
	return Detect(w.repo, w.all, w.recs, DetectEnv{
		Procs: w.procs, Live: w.live(t), Worktrees: []string{w.repo, w.wtQM, w.wtHand},
		TranscriptPath: w.transcriptPath, SelfPID: w.selfPID, Held: held,
	}).Candidates
}

func TestHeldCoversEverythingARestoreRestores(t *testing.T) {
	const (
		past, adopted, rec, done, replaced, sess = "id-past", "id-adopted", "id-rec", "id-done", "id-replaced", "id-sess"
		other, foreign                           = "id-other", "id-foreign"
	)
	p := Project{
		ID: "proj-1", Coordinator: Session{ID: "id-coord"}, PastCoordinators: []string{past},
		Adopted:  map[string]AdoptedSession{adopted: {}},
		Sessions: map[string]launch.SessionRef{sess: {}},
		Done:     map[string]time.Time{done: now},
	}
	recs := []route.Record{
		{Kind: "worker", UpProject: "proj-1", ClaudeSession: rec},
		{Kind: "worker", UpProject: "proj-1", ClaudeSession: other, Replaces: replaced},
		{Kind: "worker", UpProject: "elsewhere", ClaudeSession: foreign},
		{Kind: "up", UpProject: "proj-1", ClaudeSession: "id-up-record"},
	}
	held := Held(p, recs)
	for _, id := range []string{"id-coord", past, adopted, rec, done, replaced, sess, other} {
		if !held[id] {
			t.Errorf("%s not held: %v", id, held)
		}
	}
	for _, id := range []string{foreign, "id-up-record", ""} {
		if held[id] {
			t.Errorf("%s held", id)
		}
	}
}

func TestDetectHeldByTheProjectItself(t *testing.T) {
	w, p := laterWorld(t)
	// Without the project's own held set, the coordinator is a candidate.
	if got := candIDs(w.detectHeld(t, nil)); !slices.Contains(got, adCoord) {
		t.Fatalf("candidates %v", got)
	}
	got := candIDs(w.detectHeld(t, Held(p, w.recs)))
	if slices.Contains(got, adCoord) || !slices.Contains(got, adHand) || !slices.Contains(got, adQM) {
		t.Fatalf("candidates %v", got)
	}
	p.Adopted = map[string]AdoptedSession{adHand: {Name: "w-hand", Cwd: w.wtHand}}
	if got := candIDs(w.detectHeld(t, Held(p, w.recs))); slices.Contains(got, adHand) {
		t.Fatalf("an adopted session is a candidate: %v", got)
	}
}

func TestAskWorkersAsksNoCoordinator(t *testing.T) {
	w, _ := laterWorld(t)
	cands := w.detectHeld(t, map[string]bool{adCoord: true}) // w-hand (413), w-qm (412)
	ask := func(answer string) ([]string, string, error) {
		var out bytes.Buffer
		ids, err := AskWorkers(cands, bufio.NewReader(strings.NewReader(answer)), &out)
		return ids, out.String(), err
	}
	ids, out, err := ask("a\n")
	if err != nil || len(ids) != len(cands) || strings.Contains(out, "Coordinator") || !strings.Contains(out, "a for all the rows") {
		t.Fatalf("a: %v %v %q", ids, err, out)
	}
	if ids, _, err = ask("q\n"); err != nil || !slices.Equal(ids, []string{adQM}) {
		t.Fatalf("q: %v %v", ids, err)
	}
	if ids, _, err = ask("\n"); err != nil || len(ids) != 0 {
		t.Fatalf("empty: %v %v", ids, err)
	}
	if ids, _, err = ask("1, 2\n"); err != nil || len(ids) != 2 {
		t.Fatalf("rows: %v %v", ids, err)
	}
	if _, _, err = ask("3\n"); !errors.Is(err, ErrInput) {
		t.Fatalf("bad row: %v", err)
	}
	if _, _, err = ask(""); err == nil {
		t.Fatal("ended input answered")
	}
}

func TestAskWorkersRefusesSubfolder(t *testing.T) {
	cands := []AdoptCandidate{{SessionID: "a", Name: "a", Subfolder: true}}
	if _, err := AskWorkers(cands, bufio.NewReader(strings.NewReader("1\n")), &bytes.Buffer{}); !errors.Is(err, ErrInput) {
		t.Fatalf("err %v", err)
	}
}

func (w *adoptWorld) laterPlan(t *testing.T, p Project, ids ...string) AdoptPlan {
	t.Helper()
	held := Held(p, w.recs)
	ap, err := PlanAdoptWorkers(context.Background(), p, w.all, w.detectHeld(t, held), ids, w.deps())
	if err != nil {
		t.Fatal(err)
	}
	return ap
}

func TestAdoptWorkersMergesIntoState(t *testing.T) {
	w, p := laterWorld(t)
	ap := w.laterPlan(t, p, adQM, adHand)
	if ap.Coordinator.SessionID != "" || len(ap.Workers) != 2 {
		t.Fatalf("plan %+v", ap)
	}
	if out := RenderAdopt(ap); strings.Contains(out, "adopt coordinator") || !strings.Contains(out, "adopt worker w-hand") || !strings.Contains(out, "adopt worker w-qm") {
		t.Fatalf("render:\n%s", out)
	}
	// The project changes on disk after the plan was made (a qm up done, a qm
	// down, new coordinator arguments, a terminal session of another run): the
	// adoption merges into that state, not into the planning snapshot.
	cur := p
	cur.ClaudeArgs = []string{"--model", "sonnet", "--effort", "high"}
	cur.Done = map[string]time.Time{"00000000-0000-4000-8000-0000000000d1": now, "00000000-0000-4000-8000-0000000000d2": now.Add(time.Minute)}
	cur.DownAt = now.Add(2 * time.Minute)
	cur.Sessions = map[string]launch.SessionRef{
		adCoord:     {Launcher: "agterm", ID: "s-c2", Name: "proj-coord"},
		"unrelated": {Launcher: "tmux", ID: "@7", Name: "elsewhere"},
	}
	if err := Save(cur); err != nil {
		t.Fatal(err)
	}
	if err := Adopt(context.Background(), &ap, w.deps()); err != nil {
		t.Fatal(err)
	}
	got, ok, err := Load(w.repo)
	if err != nil || !ok {
		t.Fatalf("load: %v %v", ok, err)
	}
	if got.Coordinator != p.Coordinator || got.Workspace != "proj" || got.Launcher != "agterm" || !got.Created.Equal(p.Created) {
		t.Fatalf("project rebuilt: %+v", got)
	}
	if !slices.Equal(got.ClaudeArgs, cur.ClaudeArgs) || !got.DownAt.Equal(cur.DownAt) || len(got.Done) != 2 {
		t.Fatalf("saved state overwritten by the plan's snapshot: %+v", got)
	}
	if got.Sessions[adCoord].ID != "s-c2" || got.Sessions["unrelated"].ID != "@7" || len(got.Sessions) != 4 {
		t.Fatalf("existing terminal state lost: %+v", got.Sessions)
	}
	if got.Adopted[adHand].Name != "w-hand" || got.Adopted[adHand].Cwd != w.wtHand || got.Adopted[adQM].Name != "" {
		t.Fatalf("adopted %+v", got.Adopted)
	}
	if got.Sessions[adHand].ID != "s-h" || got.Sessions[adQM].ID != "s-q" || !got.Sessions[adHand].Adopted {
		t.Fatalf("sessions %+v", got.Sessions)
	}
	var names []string
	for _, r := range w.logged {
		if r.Kind != "up" || r.Action != "adopt" || r.UpProject != p.ID {
			t.Fatalf("record %+v", r)
		}
		names = append(names, r.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"w-hand", "w-qm"}) {
		t.Fatalf("logged %v", names)
	}
	// It restarts nothing, types nothing and closes nothing; w-qm is in
	// the project's workspace already, w-hand too.
	for _, v := range w.agterm.verbs() {
		if v != "tree" && v != "tree --json" && v != "session restore" {
			t.Fatalf("agterm acted: %q", w.agterm.calls)
		}
	}
}

func TestAdoptWorkersMovesIntoProjectWorkspace(t *testing.T) {
	w, p := laterWorld(t)
	w.side(t) // 414 sits in workspace other (ws-2)
	ap := w.laterPlan(t, p, adSide)
	if len(ap.Moves) != 1 || ap.Moves[0].To != "proj" || ap.Moves[0].ToID != "ws-1" {
		t.Fatalf("moves %+v", ap.Moves)
	}
	if err := Adopt(context.Background(), &ap, w.deps()); err != nil {
		t.Fatal(err)
	}
	if !w.agterm.has("session move --target s-x --workspace ws-1") && !slices.ContainsFunc(w.agterm.calls, func(c string) bool { return strings.HasPrefix(c, "session move") && strings.Contains(c, "s-x") }) {
		t.Fatalf("no move: %q", w.agterm.calls)
	}
}

func TestAdoptWorkersKeepsProjectsWorkspaceWhenAgtermLacksIt(t *testing.T) {
	w, p := laterWorld(t)
	p.Workspace = "gone"
	ap := w.laterPlan(t, p, adHand)
	if len(ap.Moves) != 0 || len(ap.Project.Sessions) != 0 || !notesHold(ap.Notes, "gone") {
		t.Fatalf("plan %+v", ap)
	}
	if _, ok := ap.Project.Adopted[adHand]; !ok {
		t.Fatalf("not adopted: %+v", ap.Project)
	}
}

func TestAdoptWorkersDropsASessionThatStopped(t *testing.T) {
	w, p := laterWorld(t)
	ap := w.laterPlan(t, p, adQM, adHand)
	// adHand exits between the list and the confirmation.
	w.procs = slices.DeleteFunc(w.procs, func(pr claudecode.Proc) bool { return pr.PID == 413 })
	if err := Adopt(context.Background(), &ap, w.deps()); err != nil {
		t.Fatal(err)
	}
	got, _, _ := Load(w.repo)
	if _, ok := got.Adopted[adHand]; ok || len(got.Adopted) != 1 {
		t.Fatalf("adopted %+v", got.Adopted)
	}
	if _, ok := got.Sessions[adHand]; ok {
		t.Fatalf("sessions %+v", got.Sessions)
	}
	if len(w.logged) != 1 || w.logged[0].Name != "w-qm" || !notesHold(ap.Notes, "exited before the confirmation") {
		t.Fatalf("logged %+v, notes %q", w.logged, ap.Notes)
	}
}

func TestAdoptWorkersAllStoppedWritesNothing(t *testing.T) {
	w, p := laterWorld(t)
	ap := w.laterPlan(t, p, adHand)
	w.procs = slices.DeleteFunc(w.procs, func(pr claudecode.Proc) bool { return pr.PID == 413 })
	if err := Adopt(context.Background(), &ap, w.deps()); err != nil {
		t.Fatal(err)
	}
	got, _, _ := Load(w.repo)
	if len(got.Adopted) != 0 || len(w.logged) != 0 || len(ap.Workers) != 0 {
		t.Fatalf("state %+v, logged %+v", got, w.logged)
	}
}

func TestAdoptWorkersMoveFailureLeavesNoRef(t *testing.T) {
	w, p := laterWorld(t)
	w.side(t)
	w.agterm.fail = map[string]bool{"session move": true}
	ap := w.laterPlan(t, p, adSide)
	if err := Adopt(context.Background(), &ap, w.deps()); err != nil {
		t.Fatal(err)
	}
	got, _, _ := Load(w.repo)
	if _, ok := got.Adopted[adSide]; !ok {
		t.Fatalf("not adopted: %+v", got.Adopted)
	}
	if _, ok := got.Sessions[adSide]; ok {
		t.Fatalf("tab recorded though not moved: %+v", got.Sessions)
	}
}

func TestPlanAdoptWorkersRefusals(t *testing.T) {
	w, p := laterWorld(t)
	cases := map[string][]string{"not a candidate": {adCoord}, "unknown": {"nope"}}
	for name, ids := range cases {
		if _, err := PlanAdoptWorkers(context.Background(), p, w.all, w.detectHeld(t, Held(p, w.recs)), ids, w.deps()); !errors.Is(err, ErrInput) {
			t.Errorf("%s: %v", name, err)
		}
	}
	cands := []AdoptCandidate{{SessionID: "sub", Name: "sub", Subfolder: true}}
	if _, err := PlanAdoptWorkers(context.Background(), p, w.all, cands, []string{"sub"}, w.deps()); !errors.Is(err, ErrInput) {
		t.Errorf("subfolder: %v", err)
	}
}

func TestRecordedWorkers(t *testing.T) {
	cands := []AdoptCandidate{
		{SessionID: "a", Worker: true}, {SessionID: "b"}, {SessionID: "c", Worker: true, Subfolder: true},
	}
	if got := RecordedWorkers(cands); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("got %v", got)
	}
}

// A recorded worker whose terminal ref adoption does not keep (it left the
// recorded window; its tab is not verified) must not get the old ref back
// from its worker Record on the next Gather: qm down would close a window
// that runs something else now.
func TestAdoptWorkersDroppedRefIsNotSeededFromTheRecord(t *testing.T) {
	w, p := laterWorld(t)
	w.recs[0].TermLauncher, w.recs[0].TermID = "tmux", "@3"
	w.term[412] = claudecode.TermEnv{} // w-qm runs outside any known terminal now
	ap := w.laterPlan(t, p, adQM)
	if _, ok := ap.Project.Sessions[adQM]; ok {
		t.Fatalf("a ref was planned: %+v", ap.Project.Sessions)
	}
	if err := Adopt(context.Background(), &ap, w.deps()); err != nil {
		t.Fatal(err)
	}
	got, _, _ := Load(w.repo)
	if !got.Dropped[adQM] {
		t.Fatalf("not marked dropped: %+v", got.Dropped)
	}
	if _, err := Gather(&got, w.recs, Env{Live: w.live(t), TranscriptPath: w.transcriptPath, Now: now}); err != nil {
		t.Fatal(err)
	}
	if ref, ok := got.Sessions[adQM]; ok {
		t.Fatalf("the Record's old ref came back for qm down to close: %+v", ref)
	}
	// A later adoption with a verified ref clears the mark.
	w.term[412] = claudecode.TermEnv{AgtermSession: "s-q"}
	got.Adopted = nil
	if err := Save(got); err != nil {
		t.Fatal(err)
	}
	ap = w.laterPlan(t, got, adQM)
	if err := Adopt(context.Background(), &ap, w.deps()); err != nil {
		t.Fatal(err)
	}
	got, _, _ = Load(w.repo)
	if got.Dropped[adQM] || got.Sessions[adQM].ID != "s-q" {
		t.Fatalf("a verified ref did not replace the drop: %+v %+v", got.Dropped, got.Sessions)
	}
}
