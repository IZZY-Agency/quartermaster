package up

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
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
	Close(ctx context.Context, ref launch.SessionRef) error
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
// session once per launcher, and closes only a recorded session still in
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
// of one pane. A recorded session whose Claude session is not running (any
// step but a leave) is closed only when the listing marks it Idle: an idle shell
// (agterm), or a dead or shell pane alone in its window (tmux). Since a
// reused or adopted tab outlives its session, it may run something else by
// now: then it is left open, its ref dropped, and the Outcome's Ref carries
// the Warning LeftOpen.
// The caller holds the project's Lock. Outcome.Ref is the terminal session
// closed ("" ID when there was none); a skipped step has an Outcome only
// when it was closed or failed to close.
func Down(ctx context.Context, p *Project, steps []Step, terms Terminals, keepOpen bool) []Outcome {
	now := time.Now().UTC()
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
	if keepOpen || terms == nil {
		return outs
	}
	listed, listErr := listRecorded(ctx, p, terms)
	changed := pruneUnlisted(p, listed, nil)
	// Skipped sessions that still have a terminal session join the closing.
	for _, s := range steps {
		if ref, ok := p.Sessions[s.SessionID]; s.Action == Skip && ok && ref.ID != "" {
			outs = append(outs, Outcome{Step: s})
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
			continue
		}
		if err := listErr[ref.Launcher]; err != nil {
			outs[i].Err = errors.Join(outs[i].Err, fmt.Errorf("%s: %s %s not closed: the workspace %s was not listed: %w", outs[i].Step.Name, ref.Launcher, ref.ID, p.Workspace, err))
			continue
		}
		l := listed[ref.Launcher][ref.ID]
		if !closable(outs[i].Step, l) {
			outs[i].Ref = stored(ref)
			outs[i].Ref.Warning = LeftOpen
			p.drop(id)
			changed = true
			continue
		}
		err := terms.Close(ctx, l)
		if err != nil && !errors.Is(err, launch.ErrGone) {
			outs[i].Err = errors.Join(outs[i].Err, fmt.Errorf("%s: close %s %s: %w", outs[i].Step.Name, ref.Launcher, ref.ID, err))
			continue
		}
		outs[i].Ref = stored(ref)
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

// LeftOpen is the warning on a terminal session qm down leaves open.
const LeftOpen = "left open: it now runs something else"

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
	if !there || !sameWindow(ref, l) {
		return false, "no longer listed"
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
// ends that session alone.
func closable(s Step, l launch.SessionRef) bool {
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
	if rec.Launcher == "tmux" && rec.Name != "" {
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
