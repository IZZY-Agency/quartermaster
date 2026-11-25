package up

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
	"github.com/IZZY-Agency/quartermaster/internal/launch"
)

// finder is qm down's terminals that find a window wherever it is now.
type finder struct {
	*fakeCloser
	places  []launch.SessionRef
	err     error
	refused map[string]string // why Close left a window open, by id
}

func (f finder) Locate(context.Context, launch.SessionRef) ([]launch.SessionRef, error) {
	return f.places, f.err
}

func (f finder) Close(ctx context.Context, ref launch.SessionRef) error {
	if why, ok := f.refused[ref.ID]; ok {
		return &launch.ChangedError{ID: ref.ID, Why: why}
	}
	return f.fakeCloser.Close(ctx, ref)
}

// A recorded tmux window moved to another session, renamed, linked or
// changed right before its close is left open, its ref dropped, and Down says
// why; one that is gone is not spoken of.
func TestDownTmuxWindowNotAsRecorded(t *testing.T) {
	for _, c := range []struct {
		name string
		set  func(*finder)
		want string // "" for no word of a window
	}{
		{"moved", func(f *finder) {
			f.unlisted = map[string]bool{"@7": true}
			f.places = []launch.SessionRef{{ID: "@7", Name: "w1", Workspace: "mine"}}
		}, "left open: it is now in tmux session mine"},
		{"not found", func(f *finder) {
			f.unlisted = map[string]bool{"@7": true}
			f.err = errors.New("lost server")
		}, "left open: it is not in tmux session proj as recorded, and was not found (lost server)"},
		{"gone", func(f *finder) { f.unlisted = map[string]bool{"@7": true} }, ""},
		{"moved and renamed, or its id another's", func(f *finder) {
			f.unlisted = map[string]bool{"@7": true}
			f.places = []launch.SessionRef{{ID: "@7", Name: "vim", Workspace: "mine"}}
		}, `left open: a window of its id is now "vim" in tmux session mine`},
		{"renamed", func(f *finder) {
			f.renamed = map[string]string{"@7": "vim"}
			f.places = []launch.SessionRef{{ID: "@7", Name: "vim", Workspace: "proj"}}
		}, `left open: that window is named "vim" now, not "w1"`},
		{"renamed while others closed", func(f *finder) {
			f.later = map[string]func(*launch.SessionRef){"@7": func(r *launch.SessionRef) { r.Name = "vim" }}
			f.places = []launch.SessionRef{{ID: "@7", Name: "vim", Workspace: "proj"}}
		}, `left open: that window is named "vim" now, not "w1"`},
		{"linked", func(f *finder) {
			f.later = map[string]func(*launch.SessionRef){"@7": func(r *launch.SessionRef) { r.Linked = true }}
		}, "left open: it is linked into more than one tmux session"},
		{"changed at the close", func(f *finder) { f.refused = map[string]string{"@7": "it has 2 panes"} }, "left open: it has 2 panes"},
	} {
		for _, action := range []Action{Leave, Resume, Skip} {
			t.Run(c.name+" "+string(action), func(t *testing.T) {
				p, steps := downFixture(t)
				p.Sessions["w1"] = launch.SessionRef{Launcher: "tmux", ID: "@7", Name: "w1"}
				for i := range steps {
					if steps[i].SessionID == "w1" {
						steps[i].Action = action
					}
				}
				f := finder{fakeCloser: &fakeCloser{p: p}}
				c.set(&f)
				outs := Down(context.Background(), p, steps, f, false)
				if slices.Contains(f.closed, "@7") {
					t.Fatalf("closed %v", f.closed)
				}
				got, n := "", 0
				for _, o := range outs {
					if o.Step.SessionID == "w1" {
						got, n = o.Ref.Warning, n+1
						if o.Err != nil {
							t.Errorf("w1: %v", o.Err)
						}
					}
				}
				if got != c.want || n > 1 || c.want != "" && (n != 1 || !IsLeftOpen(got)) {
					t.Errorf("w1: %d outcomes, %q, want %q", n, got, c.want)
				}
				if _, ok := p.Sessions["w1"]; ok || !p.Dropped["w1"] {
					t.Errorf("sessions %v, dropped %v", p.Sessions, p.Dropped)
				}
				if _, closed := p.Closed["w1"]; action == Leave && closed {
					t.Error("a running session left open is recorded as closed")
				}
			})
		}
	}
}

// The plan shown before qm down asks says which windows stay, and why.
func TestWillCloseTmuxWindow(t *testing.T) {
	rec := launch.SessionRef{Launcher: "tmux", ID: "@7", Name: "w1"}
	listed := func(change func(*launch.SessionRef)) map[string]map[string]launch.SessionRef {
		l := launch.SessionRef{Launcher: "tmux", ID: "@7", Name: "w1", Panes: 1}
		change(&l)
		return map[string]map[string]launch.SessionRef{"tmux": {"@7": l}}
	}
	for want, change := range map[string]func(*launch.SessionRef){
		"": func(*launch.SessionRef) {},
		`left open: that window is named "vim" now, not "w1"`: func(r *launch.SessionRef) { r.Name = "vim" },
		"left open: " + launch.LinkedWhy:                      func(r *launch.SessionRef) { r.Linked = true },
		LeftOpen:                                              func(r *launch.SessionRef) { r.Panes = 2 },
	} {
		ok, why := WillClose(Step{Action: Leave}, rec, listed(change))
		if ok != (want == "") || why != want {
			t.Errorf("WillClose = %v, %q, want %q", ok, why, want)
		}
	}
	// A window recorded with no name is not known by its id alone.
	if sameWindow(launch.SessionRef{Launcher: "tmux", ID: "@7"}, launch.SessionRef{Launcher: "tmux", ID: "@7", Name: "mine"}) {
		t.Error("a window id alone names a window")
	}
}

// finderLister lists windows, and finds one wherever it is now.
type finderLister struct {
	fakeLister
	places []launch.SessionRef
}

func (f finderLister) Locate(context.Context, launch.SessionRef) ([]launch.SessionRef, error) {
	return f.places, nil
}

// qm up says where a window it would have closed is now, and closes none
// that was moved, renamed or linked by hand.
func TestUpStaleTmuxWindowNotAsRecorded(t *testing.T) {
	rec := launch.SessionRef{Launcher: "tmux", ID: "@4", Name: "repo-coord"}
	at := func(name, ws string) launch.SessionRef {
		return launch.SessionRef{Launcher: "tmux", ID: "@4", Name: name, Workspace: ws, Idle: true, Panes: 1}
	}
	linked := at("repo-coord", "repo")
	linked.Linked = true
	for _, c := range []struct {
		name   string
		listed fakeLister
		places []launch.SessionRef
		note   string
	}{
		{"moved", nil, []launch.SessionRef{at("repo-coord", "mine")}, "now in tmux session mine"},
		{"renamed", fakeLister{at("vim", "repo")}, []launch.SessionRef{at("vim", "repo")}, `named "vim" now`},
		{"linked", fakeLister{linked}, []launch.SessionRef{linked, at("repo-coord", "mine")}, launch.LinkedWhy},
		{"gone", nil, nil, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			w.project.Sessions = map[string]launch.SessionRef{coordID: rec}
			steps := []Step{{Candidate: Candidate{Role: RoleCoordinator, SessionID: coordID, Name: "repo-coord"}, Action: Resume}}
			closer := &fakeCloser{p: &w.project}
			l := finderLister{c.listed, c.places}
			sp := PlanStale(context.Background(), &w.project, steps, Deps{Lister: l, Closer: closer})
			notes := strings.Join(sp.Notes, "\n")
			if len(sp.Close) != 0 || len(sp.Reuse) != 0 || (c.note == "") != (notes == "") || !strings.Contains(notes, c.note) {
				t.Errorf("close %v, reuse %v, notes %q, want %q", sp.Close, sp.Reuse, notes, c.note)
			}
			// Planned while it was an idle window of the project's session,
			// and changed while qm up asked.
			planned := PlanStale(context.Background(), &w.project, steps, Deps{Lister: fakeLister{at("repo-coord", "repo")}, Closer: closer})
			if len(planned.Close) != 1 {
				t.Fatalf("planned closes %v", planned.Close)
			}
			warn := strings.Join(closeStale(context.Background(), planned, Deps{Lister: l, Closer: closer})[coordID], "\n")
			if len(closer.closed) != 0 || (c.note == "") != (warn == "") || !strings.Contains(warn, c.note) {
				t.Errorf("closed %v, warned %q, want %q", closer.closed, warn, c.note)
			}
		})
	}
}

// A worker's window in another tmux session, or in several, is not recorded,
// and the plan says so.
func TestAdoptTmuxWindowElsewhereSaidSo(t *testing.T) {
	for want, set := range map[string]func(*adoptWorld){
		"its tmux window @9 is in tmux session else, not the project's tmx": func(w *adoptWorld) { w.panes["%2"] = [3]string{"else", "@9", "w-qm"} },
		"w-qm's tmux pane %2 not used: no such pane":                        func(w *adoptWorld) { delete(w.panes, "%2") },
	} {
		w := newAdoptWorld(t)
		w.term[411] = claudecode.TermEnv{TmuxPane: "%1"}
		w.term[412] = claudecode.TermEnv{TmuxPane: "%2"}
		w.panes["%1"] = [3]string{"tmx", "@1", "proj-coord"}
		set(w)
		ap := w.adopt(t, allThree)
		p, _, _ := Load(w.repo)
		if _, ok := p.Sessions[adQM]; ok || !notesHold(ap.Notes, want) {
			t.Errorf("ref %+v, notes %q, want %q", p.Sessions[adQM], ap.Notes, want)
		}
	}
}

// A window moved, renamed or replaced between the plan and the confirmation
// is not recorded. One that cannot be read again is not marked Dropped.
func TestAdoptTmuxWindowMovedBeforeConfirmation(t *testing.T) {
	for name, c := range map[string]struct {
		now     [3]string
		dropped bool
	}{
		"moved":          {[3]string{"else", "@2", "w-qm"}, true},
		"renamed":        {[3]string{"tmx", "@2", "vim"}, true},
		"another window": {[3]string{"tmx", "@5", "w-qm"}, true},
		"not read":       {[3]string{}, false},
	} {
		t.Run(name, func(t *testing.T) {
			w := newAdoptWorld(t)
			w.term[411] = claudecode.TermEnv{TmuxPane: "%1"}
			w.term[412] = claudecode.TermEnv{TmuxPane: "%2"}
			w.panes["%1"] = [3]string{"tmx", "@1", "proj-coord"}
			w.panes["%2"] = [3]string{"tmx", "@2", "w-qm"}
			ap, err := w.plan(t, allThree, nil, false)
			if err != nil || ap.Project.Sessions[adQM].ID != "@2" {
				t.Fatalf("plan %+v, %v", ap.Project.Sessions, err)
			}
			w.panes["%2"] = c.now
			if c.now[0] == "" {
				delete(w.panes, "%2")
			}
			if err := Adopt(context.Background(), &ap, w.deps()); err != nil {
				t.Fatal(err)
			}
			p, _, _ := Load(w.repo)
			if _, ok := p.Sessions[adQM]; ok || p.Dropped[adQM] != c.dropped || !notesHold(ap.Notes, "no terminal session recorded") {
				t.Errorf("ref %+v, dropped %v, notes %q", p.Sessions[adQM], p.Dropped, ap.Notes)
			}
			if p.Sessions[adCoord].ID != "@1" {
				t.Errorf("coordinator ref %+v", p.Sessions[adCoord])
			}
		})
	}
}

// qm up --here in a window of several tmux sessions records none, and says
// why.
func TestHereTmuxLinkedWindow(t *testing.T) {
	f := &hereFake{paneSess: "repo", panes: 1}
	h := f.term("", "%3", "tmux")
	h.PaneOf = func(context.Context, string) (string, string, string, error) {
		return "", "", "", launch.ErrLinked
	}
	ref, note := h.Resolve(context.Background(), hereProject(), []string{"c1"}, false)
	if ref.ID != "" || !strings.Contains(note, "linked into more than one tmux session") || len(f.calls) != 0 {
		t.Errorf("ref %+v, note %q, calls %q", ref, note, f.calls)
	}
}

// paneGone is a qm down whose window is closed by hand while the running
// sessions are read.
type paneGone struct{ finder }

func (p paneGone) PaneRuns(context.Context, launch.SessionRef, string) (bool, error) {
	return false, fmt.Errorf("tmux: %w: can't find window: @7", launch.ErrGone)
}

func TestDownTmuxWindowGoneBeforeItsPaneIsRead(t *testing.T) {
	p, steps := downFixture(t)
	p.Sessions["w1"] = launch.SessionRef{Launcher: "tmux", ID: "@7", Name: "w1"}
	f := paneGone{finder{fakeCloser: &fakeCloser{p: p}}}
	for _, o := range Down(context.Background(), p, steps, f, false) {
		if o.Step.SessionID == "w1" && (o.Err != nil || o.Ref.Warning != "") {
			t.Errorf("outcome %+v, want no word of a window that is gone", o)
		}
	}
	_, there := p.Sessions["w1"]
	if _, closed := p.Closed["w1"]; there || closed || slices.Contains(f.closed, "@7") {
		t.Errorf("sessions %v, closed %v, %v", p.Sessions, p.Closed, f.closed)
	}
}

// A TMUX_PANE that names a pane of another process (inherited, or of another
// tmux server) does not make that pane's session the project's.
func TestAdoptTmuxPaneOfAnotherProcess(t *testing.T) {
	w := newAdoptWorld(t)
	w.term[411] = claudecode.TermEnv{TmuxPane: "%1"}
	w.panes["%1"] = [3]string{"mine", "@1", "vim"}
	w.notFg = map[string]bool{"%1": true, "@1": true}
	ap := w.adopt(t, allThree)
	p, _, _ := Load(w.repo)
	if p.Workspace == "mine" || p.Launcher == "tmux" || len(p.Sessions) != 0 || !notesHold(ap.Notes, "pane %1 does not run it") {
		t.Errorf("project %+v, notes %q", p, ap.Notes)
	}
}
