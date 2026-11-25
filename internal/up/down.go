package up

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
	"github.com/IZZY-Agency/quartermaster/internal/declog"
	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/route"
)

// Terminals lists and closes terminal sessions by launcher (agterm, tmux),
// whichever launcher qm would pick now.
type Terminals interface {
	// Sessions lists the sessions of launcher's workspace (agterm) or tmux
	// session called workspaceName.
	Sessions(ctx context.Context, launcher, workspaceName string) ([]launch.SessionRef, error)
	// PaneRuns says whether listed tmux window ref (of one pane) runs
	// session sessionID's process in its foreground.
	PaneRuns(ctx context.Context, ref launch.SessionRef, sessionID string) (bool, error)
	Close(ctx context.Context, ref launch.SessionRef) error
}

// Untinter is a Terminals that takes qm's tint off agterm tabs, by their ids.
// A failure is its own to tell: it never stops Down.
type Untinter interface {
	Untint(ctx context.Context, ids []string)
}

// untint takes qm's tint off the agterm tabs among refs, when terms can. A
// tab that is closed next is cleared too: which ones stay open is not known
// before their close, and the close of the coordinator's tab may end qm.
func untint(ctx context.Context, terms Terminals, refs map[string]launch.SessionRef) {
	u, ok := terms.(Untinter)
	if !ok {
		return
	}
	var ids []string
	for _, r := range refs {
		if r.Launcher == "agterm" && r.ID != "" && !slices.Contains(ids, r.ID) {
			ids = append(ids, r.ID)
		}
	}
	if len(ids) == 0 {
		return
	}
	slices.Sort(ids)
	u.Untint(ctx, ids)
}

// Down ends the project's run (spec 5.7, qm down): it sets p.DownAt and saves
// the state, logs a kind "up", action "down" Record for every session in
// steps that is not skipped (with its context and last activity), then,
// unless keepOpen, closes the workers' terminal sessions and the
// coordinator's last, so a coordinator that runs qm down itself ends it only
// after the rest. The state is saved before anything is closed. A skipped
// session (finished, untrusted) that still has a recorded terminal session
// is closed too, with no Record.
//
// Before closing, Down lists the project's workspace (agterm) or tmux
// session once per launcher and again right before each close, and closes only a recorded session still in
// it, through the listed ref: a tmux window id restarts with the server, so
// an id alone may name a window of another tmux session. A recorded session
// the listing no longer shows is dropped from p.Sessions, whatever session
// it belongs to. A listing that fails closes nothing of that launcher (the
// Outcome's Err). A session the launcher says is gone (launch.ErrGone) is
// not an error; a failed close is the Outcome's Err and the rest go on. A
// closed session is dropped from p.Sessions. A nil terms closes nothing.
//
// A running session's terminal session is closed only when it runs that
// session (closable): an agterm tab whose foreground is it, a tmux window
// of one pane whose foreground is its process (Terminals.PaneRuns, read
// right before the close). A recorded session whose Claude session is not running (any
// step but a leave) is closed only when the listing marks it Idle: an idle shell
// (agterm), or a dead or shell pane alone in its window (tmux). Since a
// reused or adopted tab outlives its session, it may run something else by
// now: then it is left open, its ref dropped, and the Outcome's Ref carries
// the Warning LeftOpen. So is a tmux window that was moved, renamed or linked
// into another tmux session by hand (IsLeftOpen, with why).
// p.Closed holds the sessions closed, and those known not to run; a running
// one left open is not in it.
//
// The recorded agterm tabs lose qm's tint (Untinter) once DownAt is saved,
// with keepOpen too, and a tab left open again once it is no longer recorded,
// so a qm watch that still saw it recorded leaves no tint on it.
// The caller holds the project's Lock. Outcome.Ref is the terminal session
// closed ("" ID when there was none); a skipped step has an Outcome only
// when it was closed or failed to close.
func Down(ctx context.Context, p *Project, steps []Step, terms Terminals, keepOpen bool) []Outcome {
	now := time.Now().UTC()
	p.markClosed(steps, now, !keepOpen && terms != nil)
	p.DownAt = now
	var outs []Outcome
	for _, s := range steps {
		if s.Action == Skip {
			continue
		}
		o := Outcome{Step: s}
		if err := declog.Append(downRecord(p, s, now)); err != nil {
			o.Err = fmt.Errorf("%s: down Record not logged: %w", s.Name, err)
		}
		outs = append(outs, o)
	}
	saveFailed := func(err error) {
		for i := range outs {
			outs[i].Err = errors.Join(outs[i].Err, fmt.Errorf("the project was not saved: %w", err))
		}
	}
	if err := Save(*p); err != nil {
		saveFailed(err)
	}
	if terms == nil {
		return outs
	}
	untint(ctx, terms, p.Sessions)
	if keepOpen {
		return outs
	}
	listed, listErr := listRecorded(ctx, p, terms)
	// A tmux window moved to another session or renamed by hand is no longer
	// listed as the recorded one: it is left open, and said so.
	moved := map[string]launch.SessionRef{}
	for id, ref := range p.Sessions {
		byID, ok := listed[ref.Launcher]
		if l, there := byID[ref.ID]; !ok || there && sameWindow(ref, l) {
			continue
		}
		if why := movedWhy(ctx, terms, p.Workspace, ref); why != "" {
			moved[id] = leftRef(ref, why)
		}
	}
	changed := pruneUnlisted(p, listed, nil)
	// Skipped sessions that still have a terminal session join the closing.
	for _, s := range steps {
		if ref, ok := p.Sessions[s.SessionID]; s.Action == Skip && ok && ref.ID != "" {
			outs = append(outs, Outcome{Step: s})
		} else if ref, ok := moved[s.SessionID]; s.Action == Skip && ok {
			outs = append(outs, Outcome{Step: s, Ref: ref})
		}
	}
	// Workers first, the coordinator last.
	slices.SortStableFunc(outs, func(a, b Outcome) int {
		return cmp.Compare(rank(b.Step.Role), rank(a.Step.Role))
	})
	for i := range outs {
		id := outs[i].Step.SessionID
		ref, ok := p.Sessions[id]
		if !ok || ref.ID == "" {
			if left, was := moved[id]; was {
				outs[i].Ref = left
			}
			continue
		}
		if err := listErr[ref.Launcher]; err != nil {
			outs[i].Err = errors.Join(outs[i].Err, fmt.Errorf("%s: %s %s not closed: the workspace %s was not listed: %w", outs[i].Step.Name, ref.Launcher, ref.ID, p.Workspace, err))
			continue
		}
		// The workspace is listed again right before this close (as qm up
		// does a stale window): the first listing was read before the earlier
		// closes, and a pane split or a job started meanwhile would die with
		// the window. A window that is no longer the recorded one is gone.
		l, found, err := relisted(ctx, p, ref, terms)
		if err != nil {
			outs[i].Err = errors.Join(outs[i].Err, fmt.Errorf("%s: %s %s not closed: the workspace %s was not listed again: %w", outs[i].Step.Name, ref.Launcher, ref.ID, p.Workspace, err))
			continue
		}
		if !found {
			if why := movedWhy(ctx, terms, p.Workspace, ref); why != "" {
				outs[i].Ref = leftRef(ref, why)
			}
			p.drop(id)
			changed = true
			continue
		}
		leaves := outs[i].Step.Action == Leave && closable(outs[i].Step, l)
		if leaves {
			// Saved before its close: a coordinator that runs qm down itself
			// ends with its own tab, closed last, and saves nothing after.
			// A failed save is reported by the one after the closes.
			p.Closed[id] = now
			_ = Save(*p)
		}
		// Not closed after all: it is not recorded as closed.
		reopen := func() {
			if leaves {
				delete(p.Closed, id)
				changed = true
			}
		}
		runs := true
		if closable(outs[i].Step, l) && outs[i].Step.Action == Leave && l.Launcher == "tmux" {
			// Right before the close: the window's pane still runs the
			// session in its foreground, not a job started there since.
			runs, err = terms.PaneRuns(ctx, l, id)
			if errors.Is(err, launch.ErrGone) {
				// Closed or moved by hand meanwhile.
				reopen()
				if why := movedWhy(ctx, terms, p.Workspace, ref); why != "" {
					outs[i].Ref = leftRef(ref, why)
				}
				p.drop(id)
				changed = true
				continue
			}
			if err != nil {
				reopen()
				outs[i].Err = errors.Join(outs[i].Err, fmt.Errorf("%s: tmux %s not closed: the foreground of its pane not read: %w", outs[i].Step.Name, ref.ID, err))
				continue
			}
		}
		if !runs || !closable(outs[i].Step, l) {
			reopen()
			outs[i].Ref = stored(ref)
			outs[i].Ref.Warning = LeftOpen
			if l.Linked {
				outs[i].Ref = leftRef(ref, launch.LinkedWhy)
			}
			p.drop(id)
			changed = true
			if ref.Launcher == "agterm" {
				_ = Save(*p) // saved again, with its error, at the end
				untint(ctx, terms, map[string]launch.SessionRef{id: ref})
			}
			continue
		}
		err = terms.Close(ctx, l)
		if c := (*launch.ChangedError)(nil); errors.As(err, &c) {
			reopen()
			outs[i].Ref = leftRef(ref, c.Why)
			p.drop(id)
			changed = true
			continue
		}
		if err != nil && !errors.Is(err, launch.ErrGone) {
			reopen()
			outs[i].Err = errors.Join(outs[i].Err, fmt.Errorf("%s: close %s %s: %w", outs[i].Step.Name, ref.Launcher, ref.ID, err))
			continue
		}
		outs[i].Ref = stored(ref)
		p.Closed[id] = now
		p.drop(id)
		changed = true
	}
	if changed {
		if err := Save(*p); err != nil {
			saveFailed(err)
		}
	}
	return outs
}

// markClosed sets p.Closed as qm down saves it before it closes anything: no
// running session is in it yet (Down adds each right before its close), and,
// when qm down closes (not --keep-open), each one known not to run is. What
// an earlier qm down closed stays, also one of an older qm, which recorded
// only when it ran (p.DownAt, still the earlier one here).
func (p *Project) markClosed(steps []Step, now time.Time, closes bool) {
	older := p.Closed == nil && !p.DownAt.IsZero()
	if p.Closed == nil {
		p.Closed = map[string]time.Time{}
	}
	for _, s := range steps {
		switch {
		case s.Action == Leave:
			delete(p.Closed, s.SessionID)
		case closes && !s.MaybeLive:
			p.Closed[s.SessionID] = now
		case older && !s.Started.After(p.DownAt):
			p.Closed[s.SessionID] = p.DownAt
		}
	}
}

// ClearDown forgets what qm down recorded, for a qm up that restores p. It
// reports whether there was anything to forget, which the caller then saves.
func (p *Project) ClearDown() bool {
	had := !p.DownAt.IsZero() || p.Closed != nil
	p.DownAt, p.Closed = time.Time{}, nil
	return had
}

// relisted lists recorded session rec's workspace again and returns the
// listed window that is rec (same id, and for tmux the same name), found false
// when none is.
func relisted(ctx context.Context, p *Project, rec launch.SessionRef, terms Terminals) (l launch.SessionRef, found bool, err error) {
	refs, err := terms.Sessions(ctx, rec.Launcher, p.Workspace)
	if err != nil {
		return l, false, err
	}
	for _, r := range refs {
		if r.ID == rec.ID && (r.Launcher == "" || r.Launcher == rec.Launcher) && sameWindow(rec, r) {
			r.Launcher = rec.Launcher
			return r, true, nil
		}
	}
	return l, false, nil
}

// LeftOpen is the warning on a terminal session qm down leaves open.
const LeftOpen = leftOpen + "it now runs something else"

const leftOpen = "left open: "

// IsLeftOpen says whether warning, an Outcome's Ref.Warning, says that Down
// left the terminal session open, and why.
func IsLeftOpen(warning string) bool { return strings.HasPrefix(warning, leftOpen) }

// leftRef is ref with the warning that it was left open, and why.
func leftRef(ref launch.SessionRef, why string) launch.SessionRef {
	ref = stored(ref)
	ref.Warning = leftOpen + why
	return ref
}

// movedWhy says where tmux window rec, which tmux session workspace no
// longer shows as recorded, is now: moved to another tmux session, or under
// another name. "" when it is gone, or finder cannot tell (launch.Locator).
func movedWhy(ctx context.Context, finder any, workspace string, rec launch.SessionRef) string {
	f, ok := finder.(launch.Locator)
	if !ok || rec.Launcher != "tmux" || rec.ID == "" {
		return ""
	}
	rec.Workspace = workspace
	places, err := f.Locate(ctx, rec)
	if err != nil {
		return fmt.Sprintf("it is not in tmux session %s as recorded, and was not found (%v)", workspace, err)
	}
	return launch.Moved(rec, places)
}

// DownListing lists the project's workspace once per launcher its recorded
// sessions ran in, as Down does, for showing what Down will do: the listed
// sessions by launcher and id (a launcher whose listing failed is absent).
func DownListing(ctx context.Context, p *Project, terms Terminals) map[string]map[string]launch.SessionRef {
	listed, _ := listRecorded(ctx, p, terms)
	return listed
}

// WillClose says what Down does with session ref of step s, given listed
// from DownListing: true when it closes it; false with why when it does not.
func WillClose(s Step, ref launch.SessionRef, listed map[string]map[string]launch.SessionRef) (bool, string) {
	byID, ok := listed[ref.Launcher]
	if !ok {
		return true, ""
	}
	l, there := byID[ref.ID]
	if there && !sameWindow(ref, l) {
		return false, leftOpen + launch.Renamed(ref, l)
	}
	if !there {
		return false, "no longer listed"
	}
	if l.Linked {
		return false, leftOpen + launch.LinkedWhy
	}
	if !closable(s, l) {
		return false, LeftOpen
	}
	return true, ""
}

// closable says whether Down closes listed terminal session l of step s.
// A session that is not running: only an idle shell (Idle). A running one
// (a leave): an agterm tab only when its foreground is that session
// (runsSession), a tmux window only when it has one pane, so closing it
// ends that session alone. An agterm tab split into several panes is never
// closed: closing it would end what runs beside the session. Nor is a tmux
// window linked into another tmux session too, which would lose it.
func closable(s Step, l launch.SessionRef) bool {
	if l.Launcher == "agterm" && l.Split || l.Linked {
		return false
	}
	if s.Action != Leave {
		return l.Idle
	}
	if l.Launcher == "tmux" {
		return l.Panes == 1
	}
	return runsSession(l, s.SessionID, s.Cwd)
}

// runsSession says whether listed tab l's foreground is a claude running
// session id: its arguments name id, or name no session and the tab's folder
// is cwd.
func runsSession(l launch.SessionRef, id, cwd string) bool {
	args, ok := claudecode.IsClaude(l.Foreground)
	if !ok {
		return false
	}
	if sid, _ := claudecode.SessionOfArgs(args); sid != "" {
		return sid == id
	}
	return cwd != "" && l.Cwd != "" && realPath(l.Cwd) == realPath(cwd)
}

// RunsOther says whether listed tab l's foreground is a Claude session other
// than the one a worker is known by (ids, in folder cwd). For a worker that is
// not live every Claude session is another: one that names the worker's own
// session was resumed since the worker was read. A claude that names no
// session is a live worker's when the tab's folder is cwd.
func RunsOther(l launch.SessionRef, ids []string, cwd string, live bool) bool {
	args, ok := claudecode.ClaudeArgs(l.Foreground)
	if !ok {
		return false
	}
	if !live {
		return true
	}
	if sid, _ := claudecode.SessionOfArgs(args); sid != "" {
		return !slices.Contains(ids, sid)
	}
	return cwd == "" || l.Cwd == "" || realPath(l.Cwd) != realPath(cwd)
}

// listRecorded lists the project's workspace once for each launcher a
// recorded session of p ran in: the listed sessions by launcher and id, and
// the listings that failed.
func listRecorded(ctx context.Context, p *Project, terms Terminals) (map[string]map[string]launch.SessionRef, map[string]error) {
	listed := map[string]map[string]launch.SessionRef{}
	failed := map[string]error{}
	for _, ref := range p.Sessions {
		l := ref.Launcher
		if ref.ID == "" || listed[l] != nil || failed[l] != nil {
			continue
		}
		refs, err := terms.Sessions(ctx, l, p.Workspace)
		if err != nil {
			failed[l] = err
			continue
		}
		byID := map[string]launch.SessionRef{}
		for _, r := range refs {
			if r.ID != "" && (r.Launcher == "" || r.Launcher == l) {
				r.Launcher = l
				byID[r.ID] = r
			}
		}
		listed[l] = byID
	}
	return listed, failed
}

// pruneUnlisted drops from p.Sessions (remembering it in p.Dropped) every
// recorded session of a launcher listed that the listing does not show,
// except the sessions in keep. It reports whether it dropped any.
func pruneUnlisted(p *Project, listed map[string]map[string]launch.SessionRef, keep map[string]bool) bool {
	dropped := false
	for id, ref := range p.Sessions {
		byID, ok := listed[ref.Launcher]
		if !ok || keep[id] {
			continue
		}
		if l, there := byID[ref.ID]; !there || !sameWindow(ref, l) {
			p.drop(id)
			dropped = true
		}
	}
	return dropped
}

// sameWindow says whether listed session l is the recorded session rec. A
// tmux window id restarts with the tmux server, so an id may name another
// window by now: a tmux window is the same only with the same name too.
func sameWindow(rec, l launch.SessionRef) bool {
	if rec.ID != l.ID {
		return false
	}
	if rec.Launcher == "tmux" {
		return l.Name == rec.Name
	}
	return true
}

// downRecord is the decision-log Record of step s at qm down: the session's
// context size and last activity as Detail.
func downRecord(p *Project, s Step, now time.Time) route.Record {
	detail := "no assistant activity read"
	if !s.Last.At.IsZero() {
		detail = fmt.Sprintf("context %s, last activity %s", kTokens(s.Last.Context), ago(now.Sub(s.Last.At)))
	}
	return route.Record{
		Time: now, Kind: "up", Project: p.Root, UpProject: p.ID, Action: "down",
		ClaudeSession: s.SessionID, Name: s.Name, Detail: detail, Cwd: s.Cwd, Branch: s.Branch,
	}
}
