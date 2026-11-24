package up

import (
	"context"
	"fmt"
	"slices"

	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/route"
	"github.com/IZZY-Agency/quartermaster/internal/spawn"
)

// HereTerm is the terminal qm up runs in, as a coordinator started in it
// (qm up --here) records it: the agterm session (AGTERM_SESSION_ID) or tmux
// pane (TMUX_PANE) of qm up's own environment, checked against what agterm
// or tmux list, never taken from the environment alone. Prefer is the
// launcher qm would pick (agterm, tmux, "" for agterm first) when both are
// set. Argv is qm's own command line (its arguments joined by spaces): an
// agterm tab is this one only when its foreground is that command, since a
// process started from the tab (an editor's terminal, say) inherits its
// AGTERM_SESSION_ID. The functions read and act on the terminal; PaneRuns
// says whether tmux pane's foreground is qm itself, and RenameWindow names a
// tmux window (which also stops tmux renaming it); All reads every saved
// project and Records the decision log, whose worker Records hold terminal
// sessions too.
type HereTerm struct {
	AgtermSession, TmuxPane, Prefer string
	// Argv and Cwd are qm's command line and working folder.
	Argv, Cwd string

	Workspaces   func(ctx context.Context) ([]launch.Workspace, error)
	NewWorkspace func(ctx context.Context, name string) (id string, err error)
	Move         func(ctx context.Context, sessionID, workspaceID string) error
	Pin          func(ctx context.Context, id string) error
	PaneOf       func(ctx context.Context, pane string) (session, windowID, windowName string, err error)
	WindowPanes  func(ctx context.Context, windowID string) (int, error)
	PaneRuns     func(ctx context.Context, pane string) (bool, error)
	RenameWindow func(ctx context.Context, windowID, name string) error
	All          func() ([]Project, error)
	Records      func() ([]route.Record, error)
}

// noRef ends every note of a tab not recorded.
const noRef = "the coordinator starts here with no terminal session recorded; qm down leaves this tab open"

// Resolve is the terminal session of the tab qm up runs in, for p's
// coordinator about to start in it: own are the coordinator's session ids
// (a tab recorded for one of them is its own), first says a first run. It
// never fails: a tab it cannot record gives a ref with no ID and a note
// saying why, and the coordinator still starts here.
//
// Under agterm, the tab must be listed, run qm itself (Argv, in its folder
// Cwd when agterm reports the tab's folder) and not be
// split into several panes (qm down closes the whole tab). A tab recorded in
// any project for another session (its state or its worker Records), or in
// the workspace of another qm up project, is never taken. A tab outside the project's workspace is moved into it (created
// when missing, refused when several have its name), then pinned, as
// adoption does; a failed pin keeps the ref with a note. Under tmux, the
// pane must run qm itself, its window must have that one pane and be in the
// project's tmux session; on a first run the window's tmux session becomes
// the project's workspace unless another project has it (the coordinator is
// then named after it when that makes a usable name). The window is renamed
// to the coordinator's name, so its recorded name stays what tmux lists. In
// any other terminal there is no ref, and that is not an error.
func (h HereTerm) Resolve(ctx context.Context, p *Project, own []string, first bool) (launch.SessionRef, string) {
	kind := ""
	switch {
	case h.AgtermSession != "" && h.TmuxPane != "":
		kind = "agterm"
		if h.Prefer == "tmux" {
			kind = "tmux"
		}
	case h.AgtermSession != "":
		kind = "agterm"
	case h.TmuxPane != "":
		kind = "tmux"
	default:
		return launch.SessionRef{}, "this terminal is neither agterm nor tmux: " + noRef
	}
	var all []Project
	if h.All != nil {
		var err error
		if all, err = h.All(); err != nil {
			return launch.SessionRef{}, fmt.Sprintf("the saved projects were not read (%v): %s", err, noRef)
		}
	}
	if h.Records != nil {
		recs, err := h.Records()
		if err != nil {
			return launch.SessionRef{}, fmt.Sprintf("the decision log was not read (%v): %s", err, noRef)
		}
		// The terminal sessions each project holds, its worker Records'
		// included, as Gather and adoption read them.
		for i := range all {
			all[i].Sessions = effectiveSessions(all[i], recs)
		}
	}
	if kind == "tmux" {
		return h.tmux(ctx, p, own, first, all)
	}
	return h.agterm(ctx, p, own, all)
}

// ownedElsewhere says why terminal session ref is not the coordinator's to
// take: p or a saved project records it for a session not in own.
func ownedElsewhere(ref launch.SessionRef, p *Project, own []string, all []Project) string {
	for _, o := range append([]Project{*p}, all...) {
		for sid, r := range o.Sessions {
			if r.Launcher == ref.Launcher && r.ID == ref.ID && !slices.Contains(own, sid) {
				return "is recorded for another session"
			}
		}
	}
	return ""
}

// otherProject says whether workspace name is another qm up project's.
func otherProject(name string, p *Project, all []Project) bool {
	return slices.ContainsFunc(all, func(o Project) bool { return o.Root != p.Root && o.Workspace == name })
}

func (h HereTerm) agterm(ctx context.Context, p *Project, own []string, all []Project) (launch.SessionRef, string) {
	id := h.AgtermSession
	refuse := func(why string) (launch.SessionRef, string) {
		return launch.SessionRef{}, fmt.Sprintf("this tab (agterm %s) %s: %s", id, why, noRef)
	}
	if h.Workspaces == nil {
		return refuse("cannot be listed")
	}
	wss, err := h.Workspaces(ctx)
	if err != nil {
		return refuse(fmt.Sprintf("was not listed (%v)", err))
	}
	cur, ok := launch.FindSession(wss, id)
	if !ok {
		return refuse("is not a tab agterm lists")
	}
	var tab launch.SessionRef
	for _, s := range cur.Sessions {
		if s.ID == id {
			tab = s
		}
	}
	ref := launch.SessionRef{Launcher: "agterm", ID: id, Name: tab.Name, Adopted: true}
	if h.Argv == "" || tab.Foreground != h.Argv || (tab.Cwd != "" && h.Cwd != "" && realPath(tab.Cwd) != realPath(h.Cwd)) {
		return refuse("does not run this qm in its foreground (a terminal started from it inherits its id)")
	}
	if tab.Split {
		return refuse("is split into several panes, and qm down closes the whole tab")
	}
	if why := ownedElsewhere(ref, p, own, all); why != "" {
		return refuse(why)
	}
	if cur.Name != p.Workspace && otherProject(cur.Name, p, all) {
		return refuse(fmt.Sprintf("is in workspace %s of another qm up project", cur.Name))
	}
	if cur.Name != p.Workspace {
		var targets []launch.Workspace
		for _, w := range wss {
			if w.Name == p.Workspace {
				targets = append(targets, w)
			}
		}
		var target string
		switch len(targets) {
		case 0:
			if h.NewWorkspace == nil {
				return refuse(fmt.Sprintf("is not in workspace %s, which cannot be created", p.Workspace))
			}
			if target, err = h.NewWorkspace(ctx, p.Workspace); err != nil {
				return refuse(fmt.Sprintf("is not in workspace %s, which was not created (%v)", p.Workspace, err))
			}
		case 1:
			target = targets[0].ID
		default:
			return refuse(fmt.Sprintf("was not moved: %d workspaces named %s", len(targets), p.Workspace))
		}
		if h.Move == nil {
			return refuse(fmt.Sprintf("cannot be moved into workspace %s", p.Workspace))
		}
		if err := h.Move(ctx, id, target); err != nil {
			return refuse(fmt.Sprintf("was not moved into workspace %s (%v)", p.Workspace, err))
		}
	}
	if h.Pin != nil {
		if err := h.Pin(ctx, id); err != nil {
			return ref, fmt.Sprintf("this tab (agterm %s) was not pinned: %v", id, err)
		}
	}
	return ref, ""
}

func (h HereTerm) tmux(ctx context.Context, p *Project, own []string, first bool, all []Project) (launch.SessionRef, string) {
	refuse := func(why string) (launch.SessionRef, string) {
		return launch.SessionRef{}, fmt.Sprintf("this tmux pane (%s) %s: %s", h.TmuxPane, why, noRef)
	}
	if h.PaneOf == nil {
		return refuse("cannot be read")
	}
	sess, wid, wname, err := h.PaneOf(ctx, h.TmuxPane)
	if err != nil {
		return refuse(fmt.Sprintf("was not read (%v)", err))
	}
	if h.PaneRuns == nil {
		return refuse("cannot be checked to run this qm")
	}
	switch ok, err := h.PaneRuns(ctx, h.TmuxPane); {
	case err != nil:
		return refuse(fmt.Sprintf("was not checked to run this qm (%v)", err))
	case !ok:
		return refuse("does not run this qm in its foreground")
	}
	ws := p.Workspace
	if sess != p.Workspace {
		switch {
		case !first:
			return refuse(fmt.Sprintf("is in tmux session %s, not the project's %s", sess, p.Workspace))
		case otherProject(sess, p, all):
			return refuse(fmt.Sprintf("is in tmux session %s of another qm up project", sess))
		}
		ws = sess
	}
	ref := launch.SessionRef{Launcher: "tmux", ID: wid, Name: wname, Adopted: true}
	if why := ownedElsewhere(ref, p, own, all); why != "" {
		return refuse(fmt.Sprintf("is in window %s, which %s", wid, why))
	}
	if h.WindowPanes == nil {
		return refuse(fmt.Sprintf("is in window %s, whose panes cannot be read", wid))
	}
	switch n, err := h.WindowPanes(ctx, wid); {
	case err != nil:
		return refuse(fmt.Sprintf("is in window %s, whose panes were not read (%v)", wid, err))
	case n != 1:
		return refuse(fmt.Sprintf("is in window %s, which has %d panes", wid, n))
	}
	name := p.Coordinator.Name
	if first && ws != p.Workspace && spawn.ValidName(ws+"-coord") {
		name = ws + "-coord"
	}
	if h.RenameWindow == nil {
		return refuse(fmt.Sprintf("is in window %s, which cannot be renamed", wid))
	}
	if err := h.RenameWindow(ctx, wid, name); err != nil {
		return refuse(fmt.Sprintf("is in window %s, which was not renamed (%v)", wid, err))
	}
	ref.Name = name
	p.Workspace, p.Coordinator.Name = ws, name
	return ref, ""
}
