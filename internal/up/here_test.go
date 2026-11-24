package up

import (
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

// hereFake is the terminal as ResolveHere reads it: agterm workspaces, tmux
// panes, and a record of what it did.
type hereFake struct {
	wss      []launch.Workspace
	all      []Project
	panes    int
	paneSess string
	calls    []string
	failMove bool
	failPin  bool
	notQM    bool // the tmux pane's foreground is not qm
	recs     []route.Record
}

// hereArgv is qm's own command line in these tests: the foreground of the
// tabs tab lists.
const hereArgv = "qm up --here"

func (f *hereFake) term(agtermID, tmuxPane, prefer string) HereTerm {
	return HereTerm{
		AgtermSession: agtermID, TmuxPane: tmuxPane, Prefer: prefer,
		Workspaces: func(context.Context) ([]launch.Workspace, error) { return f.wss, nil },
		NewWorkspace: func(_ context.Context, name string) (string, error) {
			f.calls = append(f.calls, "workspace new "+name)
			return "ws-new", nil
		},
		Move: func(_ context.Context, id, ws string) error {
			f.calls = append(f.calls, "move "+id+" "+ws)
			if f.failMove {
				return errors.New("move refused")
			}
			return nil
		},
		Pin: func(_ context.Context, id string) error {
			f.calls = append(f.calls, "pin "+id)
			if f.failPin {
				return errors.New("pin refused")
			}
			return nil
		},
		PaneOf: func(_ context.Context, pane string) (string, string, string, error) {
			f.calls = append(f.calls, "paneof "+pane)
			return f.paneSess, "@7", "shell", nil
		},
		WindowPanes: func(context.Context, string) (int, error) { return f.panes, nil },
		PaneRuns:    func(context.Context, string) (bool, error) { return !f.notQM, nil },
		RenameWindow: func(_ context.Context, wid, name string) error {
			f.calls = append(f.calls, "rename "+wid+" "+name)
			return nil
		},
		All:     func() ([]Project, error) { return f.all, nil },
		Records: func() ([]route.Record, error) { return f.recs, nil },
		Argv:    hereArgv,
		Cwd:     "/r/repo",
	}
}

func hereProject() *Project {
	return &Project{ID: "repo-1", Root: "/r/repo", Workspace: "repo", Coordinator: Session{ID: "c1", Name: "repo-coord"}, Sessions: map[string]launch.SessionRef{}}
}

func tab(ids ...string) []launch.SessionRef {
	var out []launch.SessionRef
	for _, id := range ids {
		out = append(out, launch.SessionRef{Launcher: "agterm", ID: id, Name: "tab " + id, Foreground: hereArgv})
	}
	return out
}

func TestHereAgtermTabInProjectWorkspaceIsRecordedAndPinned(t *testing.T) {
	f := &hereFake{wss: []launch.Workspace{{ID: "w1", Name: "repo", Sessions: tab("t1")}}}
	ref, note := f.term("t1", "", "agterm").Resolve(context.Background(), hereProject(), []string{"c1"}, false)
	want := launch.SessionRef{Launcher: "agterm", ID: "t1", Name: "tab t1", Adopted: true}
	if ref != want || note != "" {
		t.Errorf("ref %+v note %q, want %+v", ref, note, want)
	}
	if !slices.Equal(f.calls, []string{"pin t1"}) {
		t.Errorf("calls = %v, want only the pin: the tab is in the workspace already", f.calls)
	}
}

func TestHereAgtermTabElsewhereIsMovedIntoTheWorkspace(t *testing.T) {
	f := &hereFake{wss: []launch.Workspace{
		{ID: "w0", Name: "scratch", Sessions: tab("t1")},
		{ID: "w1", Name: "repo", Sessions: tab("t9")},
	}}
	ref, note := f.term("t1", "", "agterm").Resolve(context.Background(), hereProject(), []string{"c1"}, false)
	if ref.ID != "t1" || note != "" {
		t.Errorf("ref %+v note %q", ref, note)
	}
	if !slices.Equal(f.calls, []string{"move t1 w1", "pin t1"}) {
		t.Errorf("calls = %v, want the move into w1 then the pin", f.calls)
	}
}

func TestHereAgtermCreatesTheWorkspaceWhenMissing(t *testing.T) {
	f := &hereFake{wss: []launch.Workspace{{ID: "w0", Name: "scratch", Sessions: tab("t1")}}}
	ref, _ := f.term("t1", "", "agterm").Resolve(context.Background(), hereProject(), []string{"c1"}, true)
	if ref.ID != "t1" || !slices.Equal(f.calls, []string{"workspace new repo", "move t1 ws-new", "pin t1"}) {
		t.Errorf("ref %+v calls %v", ref, f.calls)
	}
}

func TestHereAgtermRefusals(t *testing.T) {
	other := Project{ID: "other-1", Root: "/r/other", Workspace: "theirs", Sessions: map[string]launch.SessionRef{"x": {Launcher: "agterm", ID: "tX"}}}
	mine := Project{ID: "repo-1", Root: "/r/repo", Workspace: "repo", Sessions: map[string]launch.SessionRef{"w1": {Launcher: "agterm", ID: "tW"}}}
	for name, tc := range map[string]struct {
		wss  []launch.Workspace
		all  []Project
		tab  string
		want string
	}{
		"another project's workspace": {
			wss: []launch.Workspace{{ID: "w9", Name: "theirs", Sessions: tab("t1")}, {ID: "w1", Name: "repo"}},
			all: []Project{other}, tab: "t1", want: "another qm up project",
		},
		"another project's tab": {
			wss: []launch.Workspace{{ID: "w0", Name: "scratch", Sessions: tab("tX")}, {ID: "w1", Name: "repo"}},
			all: []Project{other}, tab: "tX", want: "recorded for another session",
		},
		"another session of this project": {
			wss: []launch.Workspace{{ID: "w0", Name: "scratch", Sessions: tab("tW")}, {ID: "w1", Name: "repo"}},
			all: []Project{mine}, tab: "tW", want: "recorded for another session",
		},
		"several workspaces of the name": {
			wss: []launch.Workspace{{ID: "w0", Name: "scratch", Sessions: tab("t1")}, {ID: "w1", Name: "repo"}, {ID: "w2", Name: "repo"}},
			tab: "t1", want: "2 workspaces named repo",
		},
		"tab not listed": {
			wss: []launch.Workspace{{ID: "w1", Name: "repo", Sessions: tab("t9")}},
			tab: "t1", want: "not a tab agterm lists",
		},
		"tab that does not run qm": {
			wss: []launch.Workspace{{ID: "w0", Name: "scratch", Sessions: []launch.SessionRef{{Launcher: "agterm", ID: "t1", Foreground: "vim notes"}}}, {ID: "w1", Name: "repo"}},
			tab: "t1", want: "does not run this qm",
		},
		"tab of another folder": {
			wss: []launch.Workspace{{ID: "w0", Name: "scratch", Sessions: []launch.SessionRef{{Launcher: "agterm", ID: "t1", Foreground: hereArgv, Cwd: "/r/elsewhere"}}}, {ID: "w1", Name: "repo"}},
			tab: "t1", want: "does not run this qm",
		},
		"split tab": {
			wss: []launch.Workspace{{ID: "w0", Name: "scratch", Sessions: []launch.SessionRef{{Launcher: "agterm", ID: "t1", Foreground: hereArgv, Split: true}}}, {ID: "w1", Name: "repo"}},
			tab: "t1", want: "split into several panes",
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := &hereFake{wss: tc.wss, all: tc.all}
			ref, note := f.term(tc.tab, "", "agterm").Resolve(context.Background(), hereProject(), []string{"c1"}, false)
			if ref.ID != "" || !strings.Contains(note, tc.want) {
				t.Errorf("ref %+v note %q, want no ref and a note with %q", ref, note, tc.want)
			}
			for _, c := range f.calls {
				if strings.HasPrefix(c, "move") || strings.HasPrefix(c, "workspace new") {
					t.Errorf("refused tab still acted on: %v", f.calls)
				}
			}
		})
	}
}

// A worker's tab known only from its worker Record is that worker's.
func TestHereAgtermTabOfAWorkerRecordIsRefused(t *testing.T) {
	other := Project{ID: "other-1", Root: "/r/other", Workspace: "theirs"}
	rec := route.Record{Kind: "worker", Name: "w", UpProject: "other-1", ClaudeSession: "wk", TermLauncher: "agterm", TermID: "t1", Time: now}
	f := &hereFake{wss: []launch.Workspace{{ID: "w0", Name: "scratch", Sessions: tab("t1")}, {ID: "w1", Name: "repo"}}, all: []Project{other}, recs: []route.Record{rec}}
	ref, note := f.term("t1", "", "agterm").Resolve(context.Background(), hereProject(), []string{"c1"}, false)
	if ref.ID != "" || !strings.Contains(note, "recorded for another session") {
		t.Errorf("ref %+v note %q", ref, note)
	}
}

func TestHereAgtermTabRecordedForTheCoordinatorsOwnSessionIsFine(t *testing.T) {
	mine := Project{ID: "repo-1", Root: "/r/repo", Workspace: "repo", Sessions: map[string]launch.SessionRef{"c0": {Launcher: "agterm", ID: "t1"}}}
	f := &hereFake{wss: []launch.Workspace{{ID: "w1", Name: "repo", Sessions: tab("t1")}}, all: []Project{mine}}
	ref, note := f.term("t1", "", "agterm").Resolve(context.Background(), hereProject(), []string{"c1", "c0"}, false)
	if ref.ID != "t1" || note != "" {
		t.Errorf("ref %+v note %q: the coordinator's earlier session's tab is the coordinator's", ref, note)
	}
}

func TestHereAgtermMoveFailureRecordsNothing(t *testing.T) {
	f := &hereFake{wss: []launch.Workspace{{ID: "w0", Name: "scratch", Sessions: tab("t1")}, {ID: "w1", Name: "repo"}}, failMove: true}
	ref, note := f.term("t1", "", "agterm").Resolve(context.Background(), hereProject(), []string{"c1"}, false)
	if ref.ID != "" || !strings.Contains(note, "move refused") {
		t.Errorf("ref %+v note %q", ref, note)
	}
}

func TestHereAgtermPinFailureKeepsTheRefWithANote(t *testing.T) {
	f := &hereFake{wss: []launch.Workspace{{ID: "w1", Name: "repo", Sessions: tab("t1")}}, failPin: true}
	ref, note := f.term("t1", "", "agterm").Resolve(context.Background(), hereProject(), []string{"c1"}, false)
	if ref.ID != "t1" || !strings.Contains(note, "pin") {
		t.Errorf("ref %+v note %q", ref, note)
	}
}

func TestHereTmuxWindowOfTheProjectsSession(t *testing.T) {
	f := &hereFake{paneSess: "repo", panes: 1}
	ref, note := f.term("", "%3", "tmux").Resolve(context.Background(), hereProject(), []string{"c1"}, false)
	want := launch.SessionRef{Launcher: "tmux", ID: "@7", Name: "repo-coord", Adopted: true}
	if ref != want || note != "" {
		t.Errorf("ref %+v note %q, want %+v", ref, note, want)
	}
	if !slices.Contains(f.calls, "rename @7 repo-coord") {
		t.Errorf("calls = %v, want the window renamed to the coordinator's name", f.calls)
	}
}

func TestHereTmuxFirstRunTakesTheSessionAsTheWorkspace(t *testing.T) {
	f := &hereFake{paneSess: "mysession", panes: 1}
	p := hereProject()
	ref, note := f.term("", "%3", "tmux").Resolve(context.Background(), p, []string{"c1"}, true)
	if ref.ID != "@7" || note != "" || p.Workspace != "mysession" || p.Coordinator.Name != "mysession-coord" || ref.Name != "mysession-coord" {
		t.Errorf("ref %+v note %q project %+v: the window's session becomes the workspace", ref, note, p)
	}
}

// A tmux session whose name makes no usable session name keeps the
// coordinator's own name.
func TestHereTmuxFirstRunKeepsAUsableCoordinatorName(t *testing.T) {
	f := &hereFake{paneSess: "release team", panes: 1}
	p := hereProject()
	ref, note := f.term("", "%3", "tmux").Resolve(context.Background(), p, []string{"c1"}, true)
	if ref.ID != "@7" || note != "" || p.Workspace != "release team" || p.Coordinator.Name != "repo-coord" {
		t.Errorf("ref %+v note %q project %+v", ref, note, p)
	}
}

func TestHereTmuxRefusals(t *testing.T) {
	other := Project{ID: "other-1", Root: "/r/other", Workspace: "theirs"}
	for name, tc := range map[string]struct {
		sess  string
		panes int
		first bool
		all   []Project
		want  string
	}{
		"restore in another session":           {sess: "elsewhere", panes: 1, want: "tmux session elsewhere"},
		"several panes":                        {sess: "repo", panes: 2, want: "2 panes"},
		"pane runs something else":             {sess: "repo", panes: 1, want: "does not run this qm"},
		"first run, another project's session": {sess: "theirs", panes: 1, first: true, all: []Project{other}, want: "another qm up project"},
	} {
		t.Run(name, func(t *testing.T) {
			f := &hereFake{paneSess: tc.sess, panes: tc.panes, all: tc.all, notQM: tc.want == "does not run this qm"}
			p := hereProject()
			ref, note := f.term("", "%3", "tmux").Resolve(context.Background(), p, []string{"c1"}, tc.first)
			if ref.ID != "" || !strings.Contains(note, tc.want) || p.Workspace != "repo" {
				t.Errorf("ref %+v note %q workspace %q, want no ref and a note with %q", ref, note, p.Workspace, tc.want)
			}
		})
	}
}

func TestHereOtherTerminalHasNoReferenceAndIsNotAnError(t *testing.T) {
	f := &hereFake{}
	ref, note := f.term("", "", "").Resolve(context.Background(), hereProject(), []string{"c1"}, false)
	if ref.ID != "" || !strings.Contains(note, "neither agterm nor tmux") {
		t.Errorf("ref %+v note %q", ref, note)
	}
}

func TestHereBothEnvironmentsFollowTheLauncherPicked(t *testing.T) {
	f := &hereFake{wss: []launch.Workspace{{ID: "w1", Name: "repo", Sessions: tab("t1")}}, paneSess: "repo", panes: 1}
	if ref, _ := f.term("t1", "%3", "tmux").Resolve(context.Background(), hereProject(), []string{"c1"}, false); ref.Launcher != "tmux" {
		t.Errorf("tmux picked: ref %+v", ref)
	}
	if ref, _ := f.term("t1", "%3", "agterm").Resolve(context.Background(), hereProject(), []string{"c1"}, false); ref.Launcher != "agterm" {
		t.Errorf("agterm picked: ref %+v", ref)
	}
	if ref, _ := f.term("t1", "%3", "").Resolve(context.Background(), hereProject(), []string{"c1"}, false); ref.Launcher != "agterm" {
		t.Errorf("none picked: ref %+v, want agterm first like the launcher's auto pick", ref)
	}
}

// A fresh coordinator started here: one save with the tab, one Record, the
// script written, nothing launched.
func TestStartHereRecordsTheTabAndLaunchesNothing(t *testing.T) {
	var logged []route.Record
	f := &fakeAgterm{}
	d := upDeps(t, f, &logged)
	d.Here = func(_ context.Context, p *Project, own []string, first bool) (launch.SessionRef, string) {
		if !first || len(own) != 1 || own[0] != p.Coordinator.ID {
			t.Errorf("Here(first %v, own %v), want the first run and the new coordinator id", first, own)
		}
		return launch.SessionRef{Launcher: "agterm", ID: "t1", Name: "tab", Adopted: true}, "pin failed"
	}
	root := t.TempDir()
	p, o, err := Start(context.Background(), root, nil, d)
	if err != nil {
		t.Fatal(err)
	}
	if !o.Here || o.Script == "" || o.Ref.ID != "t1" || !strings.Contains(o.Ref.Warning, "pin failed") {
		t.Errorf("outcome = %+v", o)
	}
	if len(f.calls) != 0 {
		t.Errorf("agtermctl called for a coordinator started here: %v", f.calls)
	}
	saved, ok, err := Load(root)
	if err != nil || !ok || saved.Sessions[p.Coordinator.ID].ID != "t1" || !saved.Sessions[p.Coordinator.ID].Adopted {
		t.Errorf("saved = %+v, %v, %v", saved.Sessions, ok, err)
	}
	if r := upRecords(logged)[p.Coordinator.Name]; r.Action != "fresh" || r.ClaudeSession != p.Coordinator.ID {
		t.Errorf("up Record = %+v", r)
	}
}

func TestStartHereWithoutALauncherOrATabStillStarts(t *testing.T) {
	var logged []route.Record
	f := &fakeAgterm{}
	d := upDeps(t, f, &logged)
	d.Launcher, d.Lister, d.Closer, d.Pin = nil, nil, nil, nil
	d.Here = func(context.Context, *Project, []string, bool) (launch.SessionRef, string) {
		return launch.SessionRef{}, "neither agterm nor tmux"
	}
	root := t.TempDir()
	p, o, err := Start(context.Background(), root, nil, d)
	if err != nil || !o.Here || o.Script == "" {
		t.Fatalf("err %v, outcome %+v", err, o)
	}
	saved, ok, err := Load(root)
	if err != nil || !ok || saved.Coordinator.ID != p.Coordinator.ID || len(saved.Sessions) != 0 || saved.Launcher != "" {
		t.Errorf("saved = %+v, %v, %v", saved, ok, err)
	}
	if len(logged) != 1 {
		t.Errorf("%d Records logged, want the coordinator's", len(logged))
	}
}

func TestStartHereTmuxFirstRunNamesTheCoordinatorAfterTheWorkspace(t *testing.T) {
	var logged []route.Record
	d := upDeps(t, &fakeAgterm{}, &logged)
	d.Here = func(_ context.Context, p *Project, _ []string, _ bool) (launch.SessionRef, string) {
		p.Workspace, p.Coordinator.Name = "mysession", "mysession-coord"
		return launch.SessionRef{Launcher: "tmux", ID: "@7", Name: "shell", Adopted: true}, ""
	}
	p, o, err := Start(context.Background(), t.TempDir(), nil, d)
	if err != nil {
		t.Fatal(err)
	}
	script := readFile(t, o.Script)
	if p.Workspace != "mysession" || p.Coordinator.Name != "mysession-coord" || !strings.Contains(script, "QM_WORKSPACE=mysession") || !strings.Contains(script, "-n mysession-coord") {
		t.Errorf("project %+v, script:\n%s", p, script)
	}
}

// A restore with a worker to start and a coordinator to start here: the
// worker starts through the launcher, the coordinator is only prepared.
func TestApplyHereStartsWorkersFirstAndOnlyPreparesTheCoordinator(t *testing.T) {
	var logged []route.Record
	f := &fakeAgterm{ws: "repo"}
	d := upDeps(t, f, &logged)
	var order []string
	d.Here = func(_ context.Context, p *Project, own []string, first bool) (launch.SessionRef, string) {
		order = append(order, "here")
		if first || !slices.Contains(own, coordID) {
			t.Errorf("Here(first %v, own %v)", first, own)
		}
		return launch.SessionRef{Launcher: "agterm", ID: "t1", Name: "tab", Adopted: true}, ""
	}
	w := newWorld(t)
	w.project.ClaudeArgs = []string{"--permission-mode", "acceptEdits"}
	recs := []route.Record{profiled(w, warmID, "w-warm", "warm", now.Add(-time.Hour))}
	steps := w.plan(t, recs, nil)
	outs := Apply(context.Background(), &w.project, steps, d)
	if len(outs) != 2 || outs[0].Step.Role != RoleWorker || outs[1].Step.Role != RoleCoordinator {
		t.Fatalf("outcomes = %+v, want the worker first, the coordinator last", outs)
	}
	c := outs[1]
	if c.Err != nil || !c.Here || c.Script == "" || (c.Step.Action != Resume && c.Step.Action != Fresh) {
		t.Fatalf("coordinator outcome = %+v", c)
	}
	if outs[0].Err != nil || outs[0].Ref.ID != "s-1" {
		t.Errorf("worker outcome = %+v", outs[0])
	}
	for _, v := range f.verbs() {
		if strings.Contains(v, "repo-coord") {
			t.Errorf("the coordinator was launched: %v", f.verbs())
		}
	}
	sid := c.Step.SessionID
	if c.NewSessionID != "" {
		sid = c.NewSessionID
	}
	saved := savedProject(t, w.project)
	if saved.Sessions[sid].ID != "t1" || saved.Sessions[warmID].ID != "s-1" || saved.Coordinator.ID != sid {
		t.Errorf("saved state = %+v, coordinator %+v", saved.Sessions, saved.Coordinator)
	}
	recsOf := upRecords(logged)
	if recsOf["repo-coord"].ClaudeSession != sid || recsOf["w-warm"].Action != "resume" {
		t.Errorf("up Records = %+v", recsOf)
	}
	if !slices.Equal(order, []string{"here"}) {
		t.Errorf("order = %v", order)
	}
}

// The workers' permission cap is the saved coordinator arguments: no
// coordinator runs yet, so there is no live read to lower them.
func TestApplyHereCapsWorkersAtTheSavedCoordinatorArguments(t *testing.T) {
	var logged []route.Record
	f := &fakeAgterm{ws: "repo"}
	d := upDeps(t, f, &logged)
	d.Here = func(context.Context, *Project, []string, bool) (launch.SessionRef, string) {
		return launch.SessionRef{}, ""
	}
	var asked []string
	d.CoordinatorArgs = func(id string) ([]string, bool, error) {
		asked = append(asked, id)
		return nil, false, nil
	}
	w := newWorld(t)
	w.project.ClaudeArgs = []string{"--permission-mode", "acceptEdits"}
	recs := []route.Record{profiled(w, warmID, "w-warm", "warm", now.Add(-time.Hour), "--dangerously-skip-permissions")}
	steps := w.plan(t, recs, nil)
	outs := Apply(context.Background(), &w.project, steps, d)
	o := outcomeOf(t, outs, "w-warm")
	script := readFile(t, o.Script)
	if o.Err != nil || strings.Contains(script, "--dangerously-skip-permissions") || !strings.Contains(script, "--permission-mode acceptEdits") {
		t.Errorf("worker not capped at the saved arguments: %+v\n%s", o.Err, script)
	}
	if !slices.Contains(asked, coordID) {
		t.Errorf("the coordinator's sessions asked about = %v, want %s among them", asked, coordID)
	}
}

// A coordinator already running is left whatever the tab setting: nothing is
// prepared, and nothing is to be started here.
func TestApplyHereLeavesALiveCoordinator(t *testing.T) {
	var logged []route.Record
	f := &fakeAgterm{ws: "repo", tree: []launch.SessionRef{{ID: "s-old", Name: "repo-coord"}}}
	d := upDeps(t, f, &logged)
	d.Here = func(context.Context, *Project, []string, bool) (launch.SessionRef, string) {
		t.Error("Here called for a coordinator that is left")
		return launch.SessionRef{}, ""
	}
	w := newWorld(t)
	w.project.Sessions = map[string]launch.SessionRef{coordID: {Launcher: "agterm", ID: "s-old", Name: "repo-coord"}}
	steps := w.plan(t, nil, []claudecode.Proc{{PID: 301, Cmd: "claude -n repo-coord"}})
	outs := Apply(context.Background(), &w.project, steps, d)
	if len(outs) != 1 || outs[0].Here || outs[0].Step.Action != Leave {
		t.Errorf("outcomes = %+v", outs)
	}
}

// A coordinator that started running while qm up asked is left, not exec'd.
func TestApplyHereCoordinatorStartedMeanwhileIsLeft(t *testing.T) {
	var logged []route.Record
	f := &fakeAgterm{ws: "repo"}
	d := upDeps(t, f, &logged)
	d.Here = func(context.Context, *Project, []string, bool) (launch.SessionRef, string) {
		t.Error("Here called for a coordinator that started meanwhile")
		return launch.SessionRef{}, ""
	}
	d.Live = func() (map[string]claudecode.LiveSession, error) {
		return map[string]claudecode.LiveSession{coordID: {}}, nil
	}
	w := newWorld(t)
	steps := w.plan(t, nil, nil)
	outs := Apply(context.Background(), &w.project, steps, d)
	c := outcomeOf(t, outs, "repo-coord")
	if c.Here || c.Step.Action != Leave {
		t.Errorf("outcome = %+v, want a leave and nothing to start here", c)
	}
}

// A coordinator whose state cannot be saved is not started here.
func TestApplyHereUnsavedStateReportsAnError(t *testing.T) {
	var logged []route.Record
	f := &fakeAgterm{ws: "repo"}
	d := upDeps(t, f, &logged)
	d.Here = func(context.Context, *Project, []string, bool) (launch.SessionRef, string) {
		return launch.SessionRef{Launcher: "agterm", ID: "t1"}, ""
	}
	w := newWorld(t)
	w.project.ID = "../escape"
	steps := w.plan(t, nil, nil)
	outs := Apply(context.Background(), &w.project, steps, d)
	c := outcomeOf(t, outs, "repo-coord")
	if c.Err == nil {
		t.Errorf("outcome = %+v, want an error: the state was not saved", c)
	}
}

// qm down needs nothing new: the coordinator's tab, recorded by a start here,
// is closed because its foreground is that session.
func TestDownClosesTheTabRecordedByAStartHere(t *testing.T) {
	var logged []route.Record
	d := upDeps(t, &fakeAgterm{}, &logged)
	d.Here = func(context.Context, *Project, []string, bool) (launch.SessionRef, string) {
		return launch.SessionRef{Launcher: "agterm", ID: "t1", Name: "tab", Adopted: true}, ""
	}
	root := t.TempDir()
	p, _, err := Start(context.Background(), root, nil, d)
	if err != nil {
		t.Fatal(err)
	}
	step := Step{Candidate: Candidate{Role: RoleCoordinator, SessionID: p.Coordinator.ID, Name: p.Coordinator.Name, Cwd: root}, Action: Leave}
	terms := &fakeCloser{p: &p}
	outs := Down(context.Background(), &p, []Step{step}, terms, false)
	if len(outs) != 1 || outs[0].Err != nil || outs[0].Ref.Warning != "" || !slices.Equal(terms.closed, []string{"t1"}) {
		t.Errorf("outcomes %+v, closed %v, want the tab t1 closed", outs, terms.closed)
	}
}

// A coordinator started here with no terminal reference is reported as it
// is today: no terminal session to close.
func TestDownReportsACoordinatorStartedHereWithoutATab(t *testing.T) {
	var logged []route.Record
	d := upDeps(t, &fakeAgterm{}, &logged)
	d.Here = func(context.Context, *Project, []string, bool) (launch.SessionRef, string) {
		return launch.SessionRef{}, "neither agterm nor tmux"
	}
	root := t.TempDir()
	p, _, err := Start(context.Background(), root, nil, d)
	if err != nil {
		t.Fatal(err)
	}
	step := Step{Candidate: Candidate{Role: RoleCoordinator, SessionID: p.Coordinator.ID, Name: p.Coordinator.Name, Cwd: root}, Action: Leave}
	terms := &fakeCloser{p: &p}
	outs := Down(context.Background(), &p, []Step{step}, terms, false)
	if len(outs) != 1 || outs[0].Ref.ID != "" || len(terms.closed) != 0 {
		t.Errorf("outcomes %+v, closed %v", outs, terms.closed)
	}
}

// The coordinator's earlier idle tab is neither reused nor closed when it
// starts here, and its record is dropped: the coordinator runs in qm up's tab.
func TestApplyHereLeavesTheCoordinatorsEarlierTabAlone(t *testing.T) {
	var logged []route.Record
	f := &fakeAgterm{ws: "repo", tree: []launch.SessionRef{{ID: "c-old", Name: "repo-coord"}}, idle: map[string]bool{"c-old": true}}
	d := upDeps(t, f, &logged)
	d.Here = func(context.Context, *Project, []string, bool) (launch.SessionRef, string) {
		return launch.SessionRef{}, "neither agterm nor tmux"
	}
	w := newWorld(t)
	w.project.Sessions = map[string]launch.SessionRef{coordID: {Launcher: "agterm", ID: "c-old", Name: "repo-coord"}}
	steps := w.plan(t, nil, nil)
	d.Stale = PlanStale(context.Background(), &w.project, steps, d)
	if len(d.Stale.Reuse) != 0 || len(d.Stale.Close) != 0 {
		t.Fatalf("plan = %+v, want the coordinator's tab neither reused nor closed", d.Stale)
	}
	outs := Apply(context.Background(), &w.project, steps, d)
	c := outcomeOf(t, outs, "repo-coord")
	if c.Err != nil || !c.Here || !strings.Contains(c.Ref.Warning, "neither agterm nor tmux") {
		t.Errorf("outcome = %+v", c)
	}
	for _, v := range f.verbs() {
		if strings.Contains(v, "c-old") || strings.HasPrefix(v, "session new") {
			t.Errorf("acted on a tab: %v", f.verbs())
		}
	}
	if len(f.typed) != 0 {
		t.Errorf("typed %q", f.typed)
	}
	if saved := savedProject(t, w.project); len(saved.Sessions) != 0 {
		t.Errorf("saved sessions = %+v, want the earlier tab's record dropped", saved.Sessions)
	}
}

// Without a launcher, a coordinator resumed here is still prepared, saved
// and logged.
func TestApplyHereWithoutALauncher(t *testing.T) {
	var logged []route.Record
	d := upDeps(t, &fakeAgterm{}, &logged)
	d.Launcher, d.Lister, d.Closer, d.Pin = nil, nil, nil, nil
	d.Here = func(context.Context, *Project, []string, bool) (launch.SessionRef, string) {
		return launch.SessionRef{Launcher: "tmux", ID: "@7", Name: "shell", Adopted: true}, ""
	}
	w := newWorld(t)
	outs := Apply(context.Background(), &w.project, w.plan(t, nil, nil), d)
	c := outcomeOf(t, outs, "repo-coord")
	sid := c.Step.SessionID
	if c.NewSessionID != "" {
		sid = c.NewSessionID
	}
	if c.Err != nil || !c.Here || c.Script == "" {
		t.Fatalf("outcome = %+v", c)
	}
	if saved := savedProject(t, w.project); saved.Sessions[sid].ID != "@7" || saved.Coordinator.ID != sid {
		t.Errorf("saved = %+v", saved)
	}
	if upRecords(logged)["repo-coord"].ClaudeSession != sid {
		t.Errorf("Records = %+v", logged)
	}
}

// Reused tabs are waited for before the coordinator's step: a coordinator
// resumed by hand during that wait is left, and nothing is to start here.
func TestApplyHereWaitsForReusedTabsBeforeTheCoordinator(t *testing.T) {
	var logged []route.Record
	f := &fakeAgterm{ws: "repo", tree: []launch.SessionRef{{ID: "s-old", Name: "w-warm"}}, idle: map[string]bool{"s-old": true}}
	d := upDeps(t, f, &logged)
	d.Here = func(context.Context, *Project, []string, bool) (launch.SessionRef, string) {
		t.Error("Here called for a coordinator resumed by hand during the wait")
		return launch.SessionRef{}, ""
	}
	w := newWorld(t)
	w.project.Sessions = map[string]launch.SessionRef{warmID: {Launcher: "agterm", ID: "s-old", Name: "w-warm"}}
	recs := []route.Record{profiled(w, warmID, "w-warm", "warm", now.Add(-time.Hour))}
	steps := w.plan(t, recs, nil)
	// Read 1, the worker's start: nothing running. Read 2: the worker,
	// typed into its tab. Read 3 on: the coordinator too, resumed by hand.
	// Waiting first, the coordinator's own check is read 3 and sees it;
	// checked before the wait, it would be read 2 and miss it.
	d.Live, _ = liveSeq(map[string]claudecode.LiveSession{}, map[string]claudecode.LiveSession{warmID: {}}, map[string]claudecode.LiveSession{warmID: {}, coordID: {}})
	d.Sleep = func(time.Duration) {}
	outs := Apply(context.Background(), &w.project, steps, d)
	if o := outcomeOf(t, outs, "w-warm"); o.Err != nil || o.Ref.ID != "s-old" {
		t.Errorf("worker outcome = %+v", o)
	}
	if c := outcomeOf(t, outs, "repo-coord"); c.Here || c.Step.Action != Leave {
		t.Errorf("coordinator outcome = %+v, want a leave", c)
	}
}

// A past coordinator resumed here in place of a never-run one: the never-run
// session's tab is dropped, so it holds no tab nobody runs in.
func TestApplyHereDropsTheTabOfTheSessionItStandsFor(t *testing.T) {
	var logged []route.Record
	// The never-run session's tab is still listed: only the start drops it.
	d := upDeps(t, &fakeAgterm{ws: "repo", tree: []launch.SessionRef{{ID: "t-never", Name: "repo-coord"}}}, &logged)
	d.Here = func(context.Context, *Project, []string, bool) (launch.SessionRef, string) {
		return launch.SessionRef{Launcher: "agterm", ID: "t1", Name: "tab", Adopted: true}, ""
	}
	w := newWorld(t)
	const past = "00000000-0000-4000-8000-0000000000c0"
	w.project.PastCoordinators = []string{past}
	w.project.Sessions = map[string]launch.SessionRef{coordID: {Launcher: "agterm", ID: "t-never", Name: "repo-coord"}}
	step := Step{Candidate: Candidate{Role: RoleCoordinator, SessionID: past, Instead: coordID, Name: "repo-coord", Cwd: w.project.Root, Trusted: true}, Action: Resume, Why: "fallback"}
	outs := Apply(context.Background(), &w.project, []Step{step}, d)
	if len(outs) != 1 || outs[0].Err != nil || !outs[0].Here {
		t.Fatalf("outcomes = %+v", outs)
	}
	saved := savedProject(t, w.project)
	if _, ok := saved.Sessions[coordID]; ok || saved.Sessions[past].ID != "t1" || saved.Coordinator.ID != past {
		t.Errorf("saved sessions %+v, coordinator %+v", saved.Sessions, saved.Coordinator)
	}
}

// qm down never closes a split agterm tab: what runs beside the coordinator
// would end with it.
func TestDownLeavesASplitTabOpen(t *testing.T) {
	p := &Project{ID: "repo-1", Root: "/r/repo", Workspace: "repo", Sessions: map[string]launch.SessionRef{"c1": {Launcher: "agterm", ID: "t1"}}}
	step := Step{Candidate: Candidate{Role: RoleCoordinator, SessionID: "c1", Name: "repo-coord", Cwd: "/r/repo"}, Action: Leave}
	terms := &fakeCloser{p: p, split: map[string]bool{"t1": true}}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	Down(context.Background(), p, []Step{step}, terms, false)
	if len(terms.closed) != 0 {
		t.Errorf("closed %v, want the split tab left open", terms.closed)
	}
}
