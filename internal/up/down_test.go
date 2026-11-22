package up

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/declog"
	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/route"
	"github.com/IZZY-Agency/quartermaster/internal/transcript"
)

// fakeCloser records the ids it closes; ids in fail fail with the message.
// It lists every session recorded in p but those in unlisted; with listErr
// set, listing fails.
type fakeCloser struct {
	p        *Project
	closed   []string
	fail     map[string]string
	gone     map[string]bool // fail with launch.ErrGone
	unlisted map[string]bool
	renamed  map[string]string // id: the name the listing shows instead
	busy     map[string]bool   // listed as running something (not Idle)
	other    map[string]bool   // its foreground is not the session's claude
	notFg    map[string]bool   // a tmux window whose pane's foreground is another job
	listErr  error
	listed   []string // launcher/workspace of each listing
	// later changes a listed window from the launcher's second listing on,
	// the one Down makes right before it closes the window; laterErr fails
	// those listings.
	later    map[string]func(*launch.SessionRef)
	laterErr error
	count    map[string]int
}

func (f *fakeCloser) Sessions(_ context.Context, launcher, ws string) ([]launch.SessionRef, error) {
	f.listed = append(f.listed, launcher+"/"+ws)
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.count == nil {
		f.count = map[string]int{}
	}
	f.count[launcher]++
	again := f.count[launcher] > 1
	if again && f.laterErr != nil {
		return nil, f.laterErr
	}
	var out []launch.SessionRef
	for sid, ref := range f.p.Sessions {
		if ref.Launcher == launcher && !f.unlisted[ref.ID] {
			ref.Workspace, ref.Idle, ref.Panes = ws, !f.busy[ref.ID], 1
			ref.Foreground = "claude --resume " + sid
			if f.other[ref.ID] {
				ref.Foreground = "npm run dev"
			}
			if n, ok := f.renamed[ref.ID]; ok {
				ref.Name = n
			}
			if change := f.later[ref.ID]; again && change != nil {
				change(&ref)
			}
			out = append(out, ref)
		}
	}
	return out, nil
}

func (f *fakeCloser) PaneRuns(_ context.Context, ref launch.SessionRef, _ string) (bool, error) {
	return !f.notFg[ref.ID], nil
}

func (f *fakeCloser) Close(_ context.Context, ref launch.SessionRef) error {
	f.closed = append(f.closed, ref.ID)
	if f.gone[ref.ID] {
		return fmt.Errorf("agterm: %w", launch.ErrGone)
	}
	if msg, ok := f.fail[ref.ID]; ok {
		return errors.New(msg)
	}
	return nil
}

// downFixture is a saved project with a coordinator and two workers, each
// with a terminal session, and the steps that describe them (the coordinator
// listed between the workers, to show the order does not come from the list).
func downFixture(t *testing.T) (*Project, []Step) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	at := time.Now().Add(-10 * time.Minute)
	p := &Project{
		ID: "proj-12345678", Root: "/work/proj", Workspace: "proj", Launcher: "agterm",
		Sessions: map[string]launch.SessionRef{
			"c1": {Launcher: "agterm", ID: "t-c"},
			"w1": {Launcher: "agterm", ID: "t-w1"},
			"w2": {Launcher: "agterm", ID: "t-w2"},
		},
	}
	mk := func(role Role, id, name string, a Action) Step {
		return Step{Candidate: Candidate{Role: role, SessionID: id, Name: name, Cwd: "/work/proj", Branch: "b-" + name,
			Last: transcript.Last{Context: 123456, At: at}}, Action: a, Why: "live"}
	}
	steps := []Step{mk(RoleWorker, "w1", "w1", Leave), mk(RoleCoordinator, "c1", "coord", Leave), mk(RoleWorker, "w2", "w2", Resume)}
	if err := Save(*p); err != nil {
		t.Fatal(err)
	}
	return p, steps
}

func TestDownClosesWorkersThenCoordinator(t *testing.T) {
	p, steps := downFixture(t)
	c := &fakeCloser{p: p}
	outs := Down(context.Background(), p, steps, c, false)
	if got := strings.Join(c.closed, ","); got != "t-w1,t-w2,t-c" {
		t.Errorf("closed %s, want workers then coordinator", got)
	}
	for _, o := range outs {
		if o.Err != nil {
			t.Errorf("%s: %v", o.Step.Name, o.Err)
		}
	}
	got, err := readProject(mustPath(t, p.ID))
	if err != nil {
		t.Fatal(err)
	}
	if got.DownAt.IsZero() || len(got.Sessions) != 0 {
		t.Errorf("saved DownAt %v, sessions %v: want DownAt set and the closed sessions dropped", got.DownAt, got.Sessions)
	}
}

func mustPath(t *testing.T, id string) string {
	t.Helper()
	dir, err := projectsDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, id+".json")
}

func downRecords(t *testing.T) map[string]string {
	t.Helper()
	recs, err := declog.Read(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, r := range recs {
		if r.Kind == "up" && r.Action == "down" {
			if r.UpProject != "proj-12345678" {
				t.Errorf("record of %s: up_project %q", r.Name, r.UpProject)
			}
			out[r.Name] = r.Detail
		}
	}
	return out
}

func TestDownKeepOpen(t *testing.T) {
	p, steps := downFixture(t)
	c := &fakeCloser{p: p}
	Down(context.Background(), p, steps, c, true)
	if len(c.closed) != 0 {
		t.Errorf("closed %v with keepOpen", c.closed)
	}
	got, err := readProject(mustPath(t, p.ID))
	if err != nil || got.DownAt.IsZero() || len(got.Sessions) != 3 {
		t.Errorf("saved %+v, %v: want DownAt set, sessions kept", got, err)
	}
	recs := downRecords(t)
	if len(recs) != 3 || !strings.Contains(recs["w1"], "context 123k, last activity 10 min ago") {
		t.Errorf("records %v", recs)
	}
}

func TestDownMissingRefIsFine(t *testing.T) {
	p, steps := downFixture(t)
	delete(p.Sessions, "w1")
	c := &fakeCloser{p: p, gone: map[string]bool{"t-w2": true}}
	outs := Down(context.Background(), p, steps, c, false)
	for _, o := range outs {
		if o.Err != nil {
			t.Errorf("%s: %v", o.Step.Name, o.Err)
		}
	}
	if got := strings.Join(c.closed, ","); got != "t-w2,t-c" {
		t.Errorf("closed %s", got)
	}
}

func TestDownFailedCloseGoesOn(t *testing.T) {
	p, steps := downFixture(t)
	c := &fakeCloser{p: p, fail: map[string]string{"t-w1": "agterm: boom"}}
	outs := Down(context.Background(), p, steps, c, false)
	failed := 0
	for _, o := range outs {
		if o.Err != nil {
			failed++
		}
	}
	if failed != 1 || outs[0].Err == nil || len(c.closed) != 3 {
		t.Errorf("failed %d, closed %v, outs[0].Err %v", failed, c.closed, outs[0].Err)
	}
	if _, ok := p.Sessions["w1"]; !ok {
		t.Errorf("the ref of the session that failed to close was dropped")
	}
}

func TestDownMessageNotGone(t *testing.T) {
	// A failure that only reads like "not found" is a failure.
	p, steps := downFixture(t)
	c := &fakeCloser{p: p, fail: map[string]string{"t-w1": "agtermctl: command not found"}}
	outs := Down(context.Background(), p, steps, c, false)
	if outs[0].Err == nil {
		t.Errorf("a command-not-found failure was read as gone")
	}
}

func TestDownClosesSkippedWithoutRecord(t *testing.T) {
	p, steps := downFixture(t)
	steps[0].Action = Skip // w1 finished, its shell still listed
	c := &fakeCloser{p: p}
	outs := Down(context.Background(), p, steps, c, false)
	if got := strings.Join(c.closed, ","); got != "t-w2,t-w1,t-c" && got != "t-w1,t-w2,t-c" {
		t.Errorf("closed %s", got)
	}
	if recs := downRecords(t); len(recs) != 2 || recs["w1"] != "" {
		t.Errorf("records %v, want w2 and coord only", recs)
	}
	if len(outs) != 3 || len(p.Sessions) != 0 {
		t.Errorf("outcomes %d, sessions %v", len(outs), p.Sessions)
	}
	// Without a recorded terminal session a skipped step is not an outcome.
	p, steps = downFixture(t)
	steps[0].Action = Skip
	delete(p.Sessions, "w1")
	if outs := Down(context.Background(), p, steps, &fakeCloser{p: p}, false); len(outs) != 2 {
		t.Errorf("outcomes %d, want 2", len(outs))
	}
}

func TestDownSaveFailure(t *testing.T) {
	p, steps := downFixture(t)
	p.ID = "" // Save refuses a project with no id
	c := &fakeCloser{p: p}
	outs := Down(context.Background(), p, steps, c, false)
	for _, o := range outs {
		if o.Err == nil || strings.Count(o.Err.Error(), "the project was not saved") != 2 {
			t.Errorf("%s: err %v, want the failed save reported after both saves", o.Step.Name, o.Err)
		}
	}
	if len(c.closed) != 3 {
		t.Errorf("closed %v: a failed save must not stop the closing", c.closed)
	}
	// With keepOpen there is one save.
	p, steps = downFixture(t)
	p.ID = ""
	for _, o := range Down(context.Background(), p, steps, &fakeCloser{p: p}, true) {
		if o.Err == nil {
			t.Errorf("%s: no error for a failed save", o.Step.Name)
		}
	}
}

func TestDownClosesOnlyListedSessions(t *testing.T) {
	// A recorded session the project's workspace no longer shows (a tmux
	// window id from before a reboot) is never closed, and its ref is dropped.
	p, steps := downFixture(t)
	p.Sessions["w1"] = launch.SessionRef{Launcher: "tmux", ID: "@4"}
	p.Sessions["old"] = launch.SessionRef{Launcher: "agterm", ID: "t-old"} // a session no step names
	c := &fakeCloser{p: p, unlisted: map[string]bool{"@4": true, "t-old": true}}
	outs := Down(context.Background(), p, steps, c, false)
	if got := strings.Join(c.closed, ","); got != "t-w2,t-c" {
		t.Errorf("closed %s, want only the listed sessions", got)
	}
	if slices.Sort(c.listed); strings.Join(slices.Compact(c.listed), ",") != "agterm/proj,tmux/proj" {
		t.Errorf("listings %v, want the launchers' workspaces", c.listed)
	}
	for _, o := range outs {
		if o.Err != nil || o.Step.Name == "w1" && o.Ref.ID != "" {
			t.Errorf("%s: ref %v, err %v", o.Step.Name, o.Ref, o.Err)
		}
	}
	got, err := readProject(mustPath(t, p.ID))
	if err != nil || len(got.Sessions) != 0 {
		t.Errorf("saved sessions %v, %v: want the unlisted refs dropped", got.Sessions, err)
	}
}

func TestDownClosesThroughTheListedRef(t *testing.T) {
	// The ref closed is the listed one, which names the tmux session.
	p, steps := downFixture(t)
	for id, ref := range p.Sessions {
		ref.Launcher = "tmux"
		p.Sessions[id] = ref
	}
	var got []launch.SessionRef
	c := &refSpy{fakeCloser: fakeCloser{p: p}, got: &got}
	Down(context.Background(), p, steps, c, false)
	if len(got) != 3 {
		t.Fatalf("closed %v", got)
	}
	for _, r := range got {
		if r.Workspace != "proj" {
			t.Errorf("closed %+v without the project's tmux session", r)
		}
	}
}

type refSpy struct {
	fakeCloser
	got *[]launch.SessionRef
}

func (s *refSpy) Close(ctx context.Context, ref launch.SessionRef) error {
	*s.got = append(*s.got, ref)
	return s.fakeCloser.Close(ctx, ref)
}

func TestDownListingFailsClosesNothing(t *testing.T) {
	p, steps := downFixture(t)
	c := &fakeCloser{p: p, listErr: errors.New("agtermctl: not running")}
	outs := Down(context.Background(), p, steps, c, false)
	if len(c.closed) != 0 {
		t.Errorf("closed %v without a listing", c.closed)
	}
	for _, o := range outs {
		if o.Err == nil || !strings.Contains(o.Err.Error(), "not listed") {
			t.Errorf("%s: err %v", o.Step.Name, o.Err)
		}
	}
	if len(p.Sessions) != 3 {
		t.Errorf("sessions %v: want every ref kept", p.Sessions)
	}
}

func TestDownClosesSpawnedWorker(t *testing.T) {
	// A worker qm spawn worker started is known only from its Record:
	// Gather loads its terminal session, and Down closes it.
	w := newWorld(t)
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	w.project.Coordinator = Session{}
	r := profiled(w, smallID, "w-small", "small", now.Add(-time.Hour))
	r.TermLauncher, r.TermID = "agterm", "t-small"
	cs, err := Gather(&w.project, []route.Record{r}, w.env(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	steps := Plan(cs, false, now)
	c := &fakeCloser{p: &w.project}
	outs := Down(context.Background(), &w.project, steps, c, false)
	if strings.Join(c.closed, ",") != "t-small" || len(outs) != 1 || outs[0].Ref.ID != "t-small" {
		t.Errorf("closed %v, outcomes %+v", c.closed, outs)
	}
}

func TestDownTmuxReusedIDOtherNameNotClosed(t *testing.T) {
	// tmux window ids restart with the server: a recorded @3 of worker w1
	// that the project's tmux session now shows under another name is
	// another window, never closed, and its ref is dropped.
	p, steps := downFixture(t)
	p.Sessions["w1"] = launch.SessionRef{Launcher: "tmux", ID: "@3", Name: "w1"}
	c := &fakeCloser{p: p, renamed: map[string]string{"@3": "proj-coord"}}
	Down(context.Background(), p, steps, c, false)
	if slices.Contains(c.closed, "@3") {
		t.Errorf("closed the reused window id: %v", c.closed)
	}
	if _, ok := p.Sessions["w1"]; ok || !p.Dropped["w1"] {
		t.Errorf("sessions %v, dropped %v: want w1's ref dropped and remembered", p.Sessions, p.Dropped)
	}
	// The same name: the same window, closed.
	p, steps = downFixture(t)
	p.Sessions["w1"] = launch.SessionRef{Launcher: "tmux", ID: "@3", Name: "w1"}
	c = &fakeCloser{p: p}
	Down(context.Background(), p, steps, c, false)
	if !slices.Contains(c.closed, "@3") {
		t.Errorf("the listed window with its name was not closed: %v", c.closed)
	}
}

func TestDroppedRefNotSeededAgain(t *testing.T) {
	// A worker's ref from its Record, closed by qm down, is not loaded from
	// the Record again: by then its id may name another window.
	w := newWorld(t)
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	w.project.Coordinator = Session{}
	r := profiled(w, smallID, "w-small", "small", now.Add(-time.Hour))
	r.TermLauncher, r.TermID = "tmux", "@3"
	recs := []route.Record{r}
	cs, err := Gather(&w.project, recs, w.env(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	c := &fakeCloser{p: &w.project}
	Down(context.Background(), &w.project, Plan(cs, false, now), c, false)
	if !slices.Equal(c.closed, []string{"@3"}) {
		t.Fatalf("closed %v", c.closed)
	}
	saved, err := readProject(mustPath(t, w.project.ID))
	if err != nil || !saved.Dropped[smallID] {
		t.Fatalf("saved dropped %v, %v", saved.Dropped, err)
	}
	if _, err := Gather(&saved, recs, w.env(t, nil)); err != nil {
		t.Fatal(err)
	}
	if _, ok := saved.Sessions[smallID]; ok {
		t.Errorf("the closed ref was seeded again: %v", saved.Sessions)
	}
}

func TestDownClosesAdoptedSessions(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	p := &Project{
		ID: "proj-12345678", Root: "/work/proj", Workspace: "proj", Launcher: "agterm",
		Coordinator: Session{ID: "c1", Name: "proj-coord", Adopted: true},
		Adopted:     map[string]AdoptedSession{"w1": {Name: "w-hand", Cwd: "/work/proj-wt"}},
		Sessions: map[string]launch.SessionRef{
			"c1": {Launcher: "agterm", ID: "t-c"},
			"w1": {Launcher: "agterm", ID: "t-w1"},
		},
	}
	if err := Save(*p); err != nil {
		t.Fatal(err)
	}
	hand := p.Adopted["w1"]
	steps := []Step{
		{Candidate: Candidate{Role: RoleCoordinator, SessionID: "c1", Name: "proj-coord", Cwd: "/work/proj"}, Action: Leave, Why: "live"},
		{Candidate: Candidate{Role: RoleWorker, SessionID: "w1", Name: "w-hand", Cwd: "/work/proj-wt", Adopted: &hand}, Action: Leave, Why: "live"},
	}
	c := &fakeCloser{p: p}
	for _, o := range Down(context.Background(), p, steps, c, false) {
		if o.Err != nil {
			t.Errorf("%s: %v", o.Step.Name, o.Err)
		}
	}
	if got := strings.Join(c.closed, ","); got != "t-w1,t-c" {
		t.Errorf("closed %s, want the worker then the coordinator", got)
	}
}

func TestDownLeavesBusyTabOfEndedSession(t *testing.T) {
	// Finding 3: w2 (resume: its session does not run) has a tab that now
	// runs something else: left open, its ref dropped. w1 and the
	// coordinator are running (leave): closed as before.
	p, steps := downFixture(t)
	c := &fakeCloser{p: p, busy: map[string]bool{"t-w2": true, "t-w1": true, "t-c": true}}
	outs := Down(context.Background(), p, steps, c, false)
	if !slices.Equal(c.closed, []string{"t-w1", "t-c"}) {
		t.Errorf("closed %v, want the running sessions' tabs only", c.closed)
	}
	var left bool
	for _, o := range outs {
		if o.Step.SessionID == "w2" {
			left = o.Err == nil && o.Ref.ID == "t-w2" && o.Ref.Warning == LeftOpen
		}
	}
	if !left {
		t.Errorf("outcomes %+v, want w2 left open", outs)
	}
	if _, ok := p.Sessions["w2"]; ok {
		t.Errorf("w2's ref kept")
	}
	small := &Project{Workspace: "proj", Sessions: map[string]launch.SessionRef{"w2": {Launcher: "agterm", ID: "t-w2"}}}
	listed := DownListing(context.Background(), small, &fakeCloser{p: small, busy: map[string]bool{"t-w2": true}})
	if ok, why := WillClose(steps[2], launch.SessionRef{Launcher: "agterm", ID: "t-w2"}, listed); ok || why != LeftOpen {
		t.Errorf("WillClose = %v %q", ok, why)
	}
}

func TestDownLeaveTabRunsSomethingElse(t *testing.T) {
	// Finding 3 (re-review): w1 is running, but its recorded tab now runs
	// something else (it was resumed by hand in another tab): qm down
	// leaves that tab open. A tmux window of two panes is left open too.
	p, steps := downFixture(t)
	c := &fakeCloser{p: p, other: map[string]bool{"t-w1": true}}
	outs := Down(context.Background(), p, steps, c, false)
	if slices.Contains(c.closed, "t-w1") || !slices.Contains(c.closed, "t-c") {
		t.Errorf("closed %v, want t-c and not t-w1", c.closed)
	}
	for _, o := range outs {
		if o.Step.SessionID == "w1" && o.Ref.Warning != LeftOpen {
			t.Errorf("w1 outcome %+v, want LeftOpen", o)
		}
	}
	leave := Step{Candidate: Candidate{SessionID: "c1"}, Action: Leave}
	if closable(leave, launch.SessionRef{Launcher: "tmux", ID: "@1", Panes: 2}) || !closable(leave, launch.SessionRef{Launcher: "tmux", ID: "@1", Panes: 1}) {
		t.Error("a running session's tmux window: closable only with one pane")
	}
}

// A running worker's tmux window of one pane is closed only when that pane
// runs the worker in its foreground: a job the user started there (the
// worker suspended, or the window inherited) is left open.
func TestDownTmuxWindowForegroundElsewhereLeftOpen(t *testing.T) {
	p, steps := downFixture(t)
	p.Sessions["w1"] = launch.SessionRef{Launcher: "tmux", ID: "@7", Name: "w1"}
	c := &fakeCloser{p: p, notFg: map[string]bool{"@7": true}}
	outs := Down(context.Background(), p, steps, c, false)
	if slices.Contains(c.closed, "@7") {
		t.Fatalf("closed %v: the window running another job was closed", c.closed)
	}
	for _, o := range outs {
		if o.Step.SessionID == "w1" && o.Ref.Warning != LeftOpen {
			t.Errorf("w1 outcome %+v, want LeftOpen", o)
		}
	}
	p, steps = downFixture(t)
	p.Sessions["w1"] = launch.SessionRef{Launcher: "tmux", ID: "@7", Name: "w1"}
	c = &fakeCloser{p: p}
	Down(context.Background(), p, steps, c, false)
	if !slices.Contains(c.closed, "@7") {
		t.Fatalf("closed %v: the window running the worker was left", c.closed)
	}
}

// Down lists a window again right before it closes it (as qm up does a stale
// one): a pane split or a job started there while earlier windows closed is
// not closed with the window.
func TestDownRelistsEachWindowBeforeClosingIt(t *testing.T) {
	tmux := func(t *testing.T) (*Project, []Step) {
		p, steps := downFixture(t)
		p.Sessions["w1"] = launch.SessionRef{Launcher: "tmux", ID: "@7", Name: "w1"}
		p.Sessions["w2"] = launch.SessionRef{Launcher: "tmux", ID: "@8", Name: "w2"}
		return p, steps
	}
	left := func(t *testing.T, outs []Outcome, sid string) {
		t.Helper()
		for _, o := range outs {
			if o.Step.SessionID == sid && (o.Err != nil || o.Ref.Warning != LeftOpen) {
				t.Errorf("%s outcome %+v, want LeftOpen", sid, o)
			}
		}
	}
	t.Run("a running session's window gained a pane", func(t *testing.T) {
		p, steps := tmux(t)
		c := &fakeCloser{p: p, later: map[string]func(*launch.SessionRef){"@7": func(r *launch.SessionRef) { r.Panes = 2 }}}
		outs := Down(context.Background(), p, steps, c, false)
		if slices.Contains(c.closed, "@7") || !slices.Contains(c.closed, "@8") {
			t.Errorf("closed %v, want @8 and not @7", c.closed)
		}
		left(t, outs, "w1")
	})
	t.Run("a stopped session's window runs a job now", func(t *testing.T) {
		p, steps := tmux(t)
		c := &fakeCloser{p: p, later: map[string]func(*launch.SessionRef){"@8": func(r *launch.SessionRef) { r.Idle = false }}}
		outs := Down(context.Background(), p, steps, c, false)
		if slices.Contains(c.closed, "@8") || !slices.Contains(c.closed, "@7") {
			t.Errorf("closed %v, want @7 and not @8", c.closed)
		}
		left(t, outs, "w2")
	})
	t.Run("an agterm tab's foreground changed", func(t *testing.T) {
		p, steps := downFixture(t)
		c := &fakeCloser{p: p, later: map[string]func(*launch.SessionRef){"t-w1": func(r *launch.SessionRef) { r.Foreground = "npm run dev" }}}
		outs := Down(context.Background(), p, steps, c, false)
		if slices.Contains(c.closed, "t-w1") || !slices.Contains(c.closed, "t-c") {
			t.Errorf("closed %v, want t-c and not t-w1", c.closed)
		}
		left(t, outs, "w1")
	})
	t.Run("a window is another one now", func(t *testing.T) {
		p, steps := tmux(t)
		c := &fakeCloser{p: p, later: map[string]func(*launch.SessionRef){"@7": func(r *launch.SessionRef) { r.Name = "other" }}}
		outs := Down(context.Background(), p, steps, c, false)
		if slices.Contains(c.closed, "@7") {
			t.Errorf("closed %v: a window with another name was closed", c.closed)
		}
		if _, ok := p.Sessions["w1"]; ok {
			t.Error("the ref of a window that is gone was kept")
		}
		for _, o := range outs {
			if o.Step.SessionID == "w1" && o.Err != nil {
				t.Errorf("w1: %v", o.Err)
			}
		}
	})
	t.Run("the listing fails", func(t *testing.T) {
		p, steps := downFixture(t)
		c := &fakeCloser{p: p, laterErr: errors.New("agtermctl: not running")}
		outs := Down(context.Background(), p, steps, c, false)
		if len(c.closed) != 0 || len(p.Sessions) != 3 {
			t.Errorf("closed %v, kept %v: want nothing closed, every ref kept", c.closed, p.Sessions)
		}
		for _, o := range outs {
			if o.Err == nil || !strings.Contains(o.Err.Error(), "not listed again") {
				t.Errorf("%s: err %v", o.Step.Name, o.Err)
			}
		}
	})
}
