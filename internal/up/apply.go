package up

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/route"
	"github.com/IZZY-Agency/quartermaster/internal/spawn"
)

// ErrInput marks bad input: a coordinator argument qm sets itself, or a
// worker name the project does not have.
var ErrInput = errors.New("bad input")

// Deps is what Start and Apply act through. Launcher nil means no launcher:
// the scripts are written and nothing starts or is recorded. Lister and
// Closer, when set, close stale shells (spec 5.6); Pin re-pins the live agterm
// sessions of the project (a session the launcher starts is pinned by the
// launcher itself). Coordinator is the config's up.claude_args, a new
// project's coordinator arguments when none are given. Log writes a decision
// Record. Self holds the terminal sessions qm up runs in (its agterm
// session, its tmux window), never closed. Trusted
// reads Claude Code's folder trust of a new project's root (nil: no check).
// Run reads git and gh for a fresh coordinator's summary (nil: no facts).
type Deps struct {
	Launcher    launch.Launcher
	Lister      launch.Lister
	Closer      launch.Closer
	Pin         func(ctx context.Context, id string) error
	Exe         string
	Coordinator []string
	// MCPConfigs are the config's project_mcp files: a restored worker
	// keeps its recorded MCP config only when it is one of them.
	MCPConfigs []string
	Log        func(route.Record) error
	Now        func() time.Time
	Self       []string
	Trusted    func(dir string) (bool, error)
	Run        DirRunner
	// Stale is the reuse and stale-shell plan shown before asking
	// (PlanStale); nil makes Apply list the workspace itself.
	Stale *StalePlan
	// Live reads the running Claude sessions again right before each start,
	// so a session the user resumed by hand while qm up asked is left, not
	// started twice, and a reused tab is checked to have started claude
	// (nil: neither check).
	Live func() (map[string]claudecode.LiveSession, error)
	// Sleep waits between those checks after a reuse (nil: time.Sleep);
	// ReuseWait is how long to wait for claude to show (0: 15 seconds).
	Sleep     func(time.Duration)
	ReuseWait time.Duration
	// Progress prints a line while Apply waits for claude to start in the
	// reused tabs (nil: silent).
	Progress func(line string)
	// CoordinatorArgs reads the running coordinator's claude arguments off
	// its process (LiveArgs), right before a worker starts: live is false
	// when the session does not run. A worker's permission is capped at
	// them while the coordinator runs, at the saved ClaudeArgs when it does
	// not (nil: the saved ones always).
	CoordinatorArgs func(sessionID string) (args []string, live bool, err error)
	// Here, when set, starts the coordinator in the tab qm up runs in (qm
	// up --here): a coordinator that starts is prepared last, its script
	// written and its state saved, but not launched; the caller runs its
	// script in place of qm (Outcome.Here). Here returns the tab's terminal
	// session (HereTerm.Resolve), "" ID when none is recorded, and a note.
	Here func(ctx context.Context, p *Project, own []string, first bool) (launch.SessionRef, string)
}

// coordinatorCap is the claude arguments a worker's permission flags are
// capped at: the project's saved ones, lowered to the running coordinator's
// own when it runs stricter (spawn.LowerPermission of both). A live read can
// only lower what the saved arguments allow, so prompt text misread as a
// flag cannot raise the cap, and neither can a coordinator not seen running
// (its session file unreadable, say): the saved arguments are then the cap,
// as before live reads. The coordinator is any of ids, the sessions this
// restore's coordinator step stands for (coordinatorIDs), and the saved
// coordinator; when several run, the weakest of their flags. When a running
// coordinator's flags cannot be read it fails rather than fall back on the
// saved ones, which could allow more than the coordinator has.
func coordinatorCap(p *Project, ids []string, d Deps) ([]string, error) {
	if d.CoordinatorArgs == nil {
		return p.ClaudeArgs, nil
	}
	var capArgs []string
	running := false
	for _, id := range ids {
		args, live, err := d.CoordinatorArgs(id)
		switch {
		case err != nil:
			return nil, fmt.Errorf("the coordinator %s's permission flags were not read, so no worker is started (the saved ones could allow it more than the coordinator has): %w", p.Coordinator.Name, err)
		case !live:
			continue
		case running:
			if capArgs, err = spawn.LowerPermission(capArgs, args); err != nil {
				return nil, fmt.Errorf("the running sessions of the coordinator %s: %w, so no worker is started", p.Coordinator.Name, err)
			}
		default:
			capArgs, running = args, true
		}
	}
	if !running {
		return p.ClaudeArgs, nil
	}
	lowered, err := spawn.LowerPermission(capArgs, p.ClaudeArgs)
	if err != nil {
		return nil, fmt.Errorf("the running coordinator %s's permission flags against its saved ones: %w, so no worker is started", p.Coordinator.Name, err)
	}
	return lowered, nil
}

// coordinatorIDs are the sessions the coordinator may run as once its step
// o is done: the session the step left or resumed, the one it stands in
// for, a fresh start's new id, and the saved coordinator's id.
func coordinatorIDs(p *Project, o *Outcome) []string {
	var ids []string
	add := func(id string) {
		if id != "" && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	if o != nil {
		add(o.Step.SessionID)
		add(o.Step.Instead)
		add(o.NewSessionID)
	}
	add(p.Coordinator.ID)
	return ids
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now().UTC()
}

// Outcome is what Apply did with one step. Ref is the terminal session the
// step's session runs in (started, or left live); its Warning says what went
// wrong around it (a failed pin, a stale shell not closed). NewSessionID is
// the id of a session started fresh. Script is the launch script written
// ("" for a leave or a skip). Err is set when the session did not start, or
// started but was not recorded.
type Outcome struct {
	Step         Step
	Ref          launch.SessionRef
	NewSessionID string
	Script       string
	Err          error
	// Here says the coordinator is to start in the tab qm up runs in: its
	// script is ready, and the caller runs it in place of qm when Err is nil.
	Here bool
	// waitFor is the session id typed into a reused tab, which Apply waits
	// to see running once every step has started.
	waitFor string
	// warn is a warning of the start itself, joined into Ref.Warning.
	warn string
}

// Apply carries out the plan (spec 6): stale tmux windows closed, then the
// coordinator, then the workers; each session started is recorded in
// p.Sessions, the state saved after it, and a kind "up" Record logged for
// every step, leave and skip included. Live agterm sessions of the project
// are pinned again. A failure on one session is its Outcome's Err; the rest go
// on. The plan carried out is d.Stale, the one shown, when set: an idle
// agterm tab recorded for a session is reused, not closed. Without a launcher, Apply only writes the scripts and starts nothing:
// no terminal session is recorded and no kind "up" Record logged, but a
// fresh session's new id is (the new coordinator in the state, a fresh
// worker's Record), so a printed line once run is a session the next qm up
// knows, and the duplicate guard covers it. With d.Here, a coordinator that
// starts comes last: its script is written, the tab qm up runs in recorded
// (d.Here), the state saved and its Record logged, launcher or not, but it is
// not launched (Outcome.Here): the caller runs its script in place of qm once
// Apply is done. Its earlier terminal session is neither reused nor closed.
func Apply(ctx context.Context, p *Project, steps []Step, d Deps) []Outcome {
	if p.Sessions == nil {
		p.Sessions = map[string]launch.SessionRef{}
	}
	order := slices.Clone(steps)
	here := d.Here != nil
	slices.SortStableFunc(order, func(a, b Step) int {
		return cmp.Compare(applyRank(a, here), applyRank(b, here))
	})
	starting := d.Launcher != nil
	var warnings map[string][]string
	sp := d.Stale
	if starting {
		if sp == nil {
			sp = PlanStale(ctx, p, order, d)
		}
		warnings = closeStale(ctx, sp, d)
		pruneStale(p, order, sp, launcherKind(d.Launcher))
	}
	outs := make([]Outcome, 0, len(order))
	// The coordinator's step comes first (applyRank): the workers' permission
	// cap reads the sessions it stands for. A coordinator started here comes
	// last, so the cap reads the sessions its step stands for already: none
	// runs yet, so the cap is the saved arguments its script starts with.
	coordIDs := coordinatorIDs(p, nil)
	for _, s := range order {
		if here && s.Role == RoleCoordinator {
			coordIDs = coordinatorIDs(p, &Outcome{Step: s})
		}
	}
	waited := false
	for _, s := range order {
		if here && s.Role == RoleCoordinator && (s.Action == Resume || s.Action == Fresh) && !waited {
			// Reused tabs are waited for before the coordinator's step, so
			// its last check for a hand-resumed copy comes right before qm
			// is replaced by it.
			waitStarted(ctx, d, outs)
			waited = true
		}
		o := Outcome{Step: s}
		switch s.Action {
		case Leave:
			if starting {
				o.Ref = repin(ctx, p, s, d)
			}
		case Resume, Fresh:
			start(ctx, p, order, s, d, sp, coordIDs, &o)
			if w := append(slices.Clone(warnings[s.SessionID]), nonEmpty(o.warn)...); len(w) > 0 {
				o.Ref.Warning = strings.Join(append(w, nonEmpty(o.Ref.Warning)...), "; ")
			}
		}
		if (starting || o.Here) && o.Err == nil {
			id := o.Step.SessionID
			if o.NewSessionID != "" {
				id = o.NewSessionID
			}
			logUp(d, p, o.Step, id, d.now(), &o)
		}
		if s.Role == RoleCoordinator {
			coordIDs = coordinatorIDs(p, &o)
		}
		outs = append(outs, o)
	}
	if !waited {
		waitStarted(ctx, d, outs)
	}
	return outs
}

func rank(r Role) int {
	if r == RoleCoordinator {
		return 0
	}
	return 1
}

// applyRank orders Apply's steps: the coordinator first (rank), but last
// when it starts in the tab qm up runs in (here): qm is replaced by it once
// every other step is done.
func applyRank(s Step, here bool) int {
	if here && s.Role == RoleCoordinator && (s.Action == Resume || s.Action == Fresh) {
		return 2
	}
	return rank(s.Role)
}

func nonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}

// Stale is a terminal session of the project's workspace that Apply closes
// before starting Step's session: the tmux window recorded for it (a tmux
// window id restarts with the server, so it is also checked by name).
type Stale struct {
	Step Step
	Ref  launch.SessionRef
}

// StalePlan is what Apply does about earlier terminal sessions before
// starting anything: Close (tmux windows), in order; Reuse, the idle agterm
// tab recorded for a session, by the step's session id, which the session is
// started in; Notes, a recorded tab left open; Warn, warnings by session id
// (a listing that failed). Listed is set when the workspace was listed;
// Shown holds the sessions listed, by id.
type StalePlan struct {
	Close  []Stale
	Reuse  map[string]launch.SessionRef
	Notes  []string
	Warn   map[string][]string
	Listed bool
	Shown  map[string]launch.SessionRef
	reused []reuse // Reuse in step order, for RenderStale
}

type reuse struct {
	name string
	ref  launch.SessionRef
}

// PlanStale lists the project's workspace and says what Apply does about the
// terminal session recorded for each session about to start: an agterm tab
// that is listed by id and idle is reused (Reuse); one that is busy is left
// open with a note and a new tab is opened; one not listed is a new session.
// A tmux window is closed when its id and name match the record, it has one
// pane, that pane is dead or runs a shell, and qm opened it (Close): a window
// recorded when its session was adopted is the user's own, never closed.
// Nothing is ever matched by name alone: two sessions of one name are
// different sessions. A live session's recorded terminal session and d.Self
// are never reused or closed. It acts on nothing; the plan shows it before
// asking. Without a Lister or Closer, or with nothing to start, it lists
// nothing.
func PlanStale(ctx context.Context, p *Project, steps []Step, d Deps) *StalePlan {
	sp := &StalePlan{Warn: map[string][]string{}, Reuse: map[string]launch.SessionRef{}}
	if d.Lister == nil || d.Closer == nil || Starts(steps) == 0 {
		return sp
	}
	listed, err := d.Lister.Sessions(ctx, p.Workspace)
	if err != nil {
		for _, s := range steps {
			if s.Action == Resume || s.Action == Fresh {
				sp.Warn[s.SessionID] = append(sp.Warn[s.SessionID], "stale sessions not closed: "+err.Error())
			}
		}
		return sp
	}
	if d.Here != nil {
		// A coordinator started here runs in qm up's own tab: its recorded
		// terminal session is neither reused nor closed.
		steps = slices.DeleteFunc(slices.Clone(steps), func(s Step) bool { return s.Role == RoleCoordinator })
	}
	sp.Listed, sp.Shown = true, map[string]launch.SessionRef{}
	for _, l := range listed {
		sp.Shown[l.ID] = l
	}
	keepID := map[string]bool{}
	for _, id := range d.Self {
		keepID[id] = true
	}
	for _, s := range steps {
		if s.Action == Leave {
			if ref, ok := p.Sessions[s.SessionID]; ok {
				keepID[ref.ID] = true
			}
		}
	}
	taken := map[string]bool{}
	for _, s := range steps {
		if s.Action != Resume && s.Action != Fresh {
			continue
		}
		rec, ok := p.Sessions[s.SessionID]
		if !ok || rec.ID == "" {
			continue
		}
		for _, l := range listed {
			if l.ID == "" || keepID[l.ID] || taken[l.ID] || (rec.Launcher != "" && rec.Launcher != l.Launcher) || !sameWindow(rec, l) {
				continue
			}
			taken[l.ID] = true
			switch {
			case l.Launcher != "agterm" && (rec.Adopted || !l.Idle):
				// The user's own window (adopted), or one that runs
				// something other than a shell now: never closed.
				why := "runs something else now"
				if rec.Adopted {
					why = "is your own (adopted), not one qm opened"
				}
				sp.Notes = append(sp.Notes, fmt.Sprintf("%s: its window (%s %s) %s: left open, a new window will be opened", oneLine(s.Name), l.Launcher, l.ID, why))
			case l.Launcher != "agterm":
				sp.Close = append(sp.Close, Stale{Step: s, Ref: l})
			case l.Idle:
				sp.Reuse[s.SessionID] = l
				sp.reused = append(sp.reused, reuse{s.Name, l})
			default:
				sp.Notes = append(sp.Notes, fmt.Sprintf("%s: its tab (agterm %s) is busy: left open, a new tab will be opened", oneLine(s.Name), l.ID))
			}
		}
	}
	return sp
}

// RenderStale says what PlanStale found: a line per tab reused, a line per
// tmux window to close, the notes, and the listings that failed.
func RenderStale(sp *StalePlan) string {
	if sp == nil {
		return ""
	}
	var b strings.Builder
	for _, r := range sp.reused {
		fmt.Fprintf(&b, "reuse %s (%s %s)\n", orDash(oneLine(r.name)), r.ref.Launcher, r.ref.ID)
	}
	for _, s := range sp.Close {
		fmt.Fprintf(&b, "will close: %s (%s %s), a stale shell of %s\n", orDash(oneLine(s.Ref.Name)), s.Ref.Launcher, s.Ref.ID, oneLine(s.Step.Name))
	}
	for _, n := range sp.Notes {
		fmt.Fprintln(&b, n)
	}
	seen := map[string]bool{}
	for _, ws := range sp.Warn {
		for _, w := range ws {
			if !seen[w] {
				seen[w] = true
				fmt.Fprintf(&b, "warning: %s\n", w)
			}
		}
	}
	return b.String()
}

// closeStale closes the stale tmux windows of sp. It returns warnings by session
// id: sp's own, and a close that failed.
// Each window is listed again right before it is closed (staleNow): the plan
// was shown before asking, and a window that changed meanwhile is left.
func closeStale(ctx context.Context, sp *StalePlan, d Deps) map[string][]string {
	warn := map[string][]string{}
	for id, w := range sp.Warn {
		warn[id] = append(warn[id], w...)
	}
	if d.Closer == nil {
		return warn
	}
	for _, s := range sp.Close {
		gone, why := staleNow(ctx, s.Ref, d)
		if gone {
			continue
		}
		if why != "" {
			warn[s.Step.SessionID] = append(warn[s.Step.SessionID], fmt.Sprintf("stale session %s left open: %s", s.Ref.ID, why))
			continue
		}
		if err := d.Closer.Close(ctx, s.Ref); err != nil && !errors.Is(err, launch.ErrGone) {
			warn[s.Step.SessionID] = append(warn[s.Step.SessionID], fmt.Sprintf("stale session %s not closed: %v", s.Ref.ID, err))
		}
	}
	return warn
}

// staleNow lists stale window ref's workspace again: gone when the window
// is no longer listed as the same window (its id and name), so there is
// nothing to close; else why it is left open now when it is no longer an
// idle window of the panes it was planned with ("" to close it).
func staleNow(ctx context.Context, ref launch.SessionRef, d Deps) (gone bool, why string) {
	if d.Lister == nil {
		return false, "its workspace cannot be listed again"
	}
	listed, err := d.Lister.Sessions(ctx, ref.Workspace)
	if err != nil {
		return false, "its workspace was not listed again: " + err.Error()
	}
	for _, l := range listed {
		if l.Launcher != ref.Launcher || !sameWindow(ref, l) {
			continue
		}
		if !l.Idle || l.Panes != ref.Panes {
			return false, "it runs something now, or has another pane"
		}
		return false, ""
	}
	return true, ""
}

// pruneStale drops from p.Sessions the refs of launcher kind that the
// workspace listing of sp no longer shows, except those of live sessions
// (a live session's ref is pinned again, wherever it now is).
func pruneStale(p *Project, steps []Step, sp *StalePlan, kind string) {
	if !sp.Listed || kind == "" {
		return
	}
	keep := map[string]bool{}
	for _, s := range steps {
		if s.Action == Leave {
			keep[s.SessionID] = true
		}
	}
	pruneUnlisted(p, map[string]map[string]launch.SessionRef{kind: sp.Shown}, keep)
}

// repin pins again the recorded agterm session of a live session; a failed
// pin is the returned ref's Warning.
func repin(ctx context.Context, p *Project, s Step, d Deps) launch.SessionRef {
	ref, ok := p.Sessions[s.SessionID]
	if !ok || ref.Launcher != "agterm" || d.Pin == nil {
		return ref
	}
	if err := d.Pin(ctx, ref.ID); err != nil {
		ref.Warning = "agterm restore pin failed: " + err.Error()
	}
	return ref
}

// start writes the script of a resume or fresh step and, with a launcher,
// starts it in the project's workspace, records it and saves the state. With
// d.Live it first reads the running sessions again: a session that started
// running while qm up asked (resumed by hand) is left, and o.Step becomes a
// leave with a warning; a failed read starts nothing.
func start(ctx context.Context, p *Project, steps []Step, s Step, d Deps, sp *StalePlan, coordIDs []string, o *Outcome) {
	here := d.Here != nil && s.Role == RoleCoordinator
	// A coordinator started here holds the adoption lock from its last check
	// for a copy resumed by hand, through the read of what other projects
	// hold, to the save, as a first run and an adoption hold it.
	var lockErr error
	if here {
		unlock, err := AdoptLock()
		if err != nil {
			lockErr = err
		} else {
			defer unlock()
		}
	}
	if (d.Launcher != nil || here) && d.Live != nil {
		live, err := d.Live()
		if err != nil {
			o.Err = fmt.Errorf("the running sessions were not read again, so %s was not started (it may have been resumed by hand meanwhile): %w", s.Name, err)
			return
		}
		for _, id := range []string{s.SessionID, s.Instead} {
			if _, ok := live[id]; id == "" || !ok {
				continue
			}
			o.Step.Action, o.Step.Why = Leave, "it started running while qm up asked"
			o.Ref.Warning = "it started running while qm up asked: left as it is, not started again"
			keepRunningRef(ctx, p, s, id, d)
			return
		}
	}
	var (
		script string
		newRec *route.Record
		err    error
	)
	sid := s.SessionID
	switch {
	case s.Role == RoleCoordinator && s.Action == Resume:
		script, err = coordinatorScript(p, sid, "", true, d.Exe)
	case s.Role == RoleCoordinator:
		var summary string
		if summary, err = writeSummary(p, steps, s, d); err == nil {
			if sid, err = spawn.NewSessionID(); err == nil {
				script, err = coordinatorScript(p, sid, summary, false, d.Exe)
			}
		}
	case s.Adopted != nil:
		// A hand-started worker: resumed with its kept flags, never started
		// fresh, no worker Record (adopt design 4.3).
		if s.Action != Resume {
			err = fmt.Errorf("%s: a hand-started worker is only resumed", s.Name)
			break
		}
		if !spawn.ValidName(s.Name) {
			err = fmt.Errorf("hand-started worker %s: name %q is not a usable session name: resume it by hand: cd %s && claude --resume %s", s.SessionID, s.Name, launch.Quote(s.Cwd), s.SessionID)
			break
		}
		if !filepath.IsAbs(s.Cwd) {
			err = fmt.Errorf("hand-started worker %s: folder %q is not an absolute path: not started", s.SessionID, s.Cwd)
			break
		}
		var capArgs []string
		if capArgs, err = coordinatorCap(p, coordIDs, d); err != nil {
			break
		}
		env := map[string]string{"QM_PROJECT_ID": p.ID, "QM_WORKSPACE": p.Workspace}
		var args []string
		if args, err = adoptedArgs(s.Adopted.Args, capArgs); err != nil {
			err = fmt.Errorf("hand-started worker %s: not started, %w", s.Name, err)
			break
		}
		script = spawn.PlainResumeScript(args, env, s.Name, sid, s.Cwd, qmDirOf(d.Exe))
	default:
		if !spawn.ValidName(s.Name) {
			err = fmt.Errorf("worker %s: name %q is not a usable session name: not started", s.SessionID, s.Name)
			break
		}
		if !filepath.IsAbs(s.Cwd) {
			err = fmt.Errorf("worker %s: folder %q is not an absolute path: not started", s.SessionID, s.Cwd)
			break
		}
		var capArgs []string
		if capArgs, err = coordinatorCap(p, coordIDs, d); err != nil {
			break
		}
		var prof route.Profile
		if prof, o.warn, err = workerProfile(p, s, d.Exe, capArgs, d.MCPConfigs); err != nil {
			break
		}
		tier, qmDir := s.Record.Decision.Tier, qmDirOf(d.Exe)
		if s.Action == Resume {
			script = spawn.ResumeScript(prof, tier, s.Name, sid, s.Cwd, qmDir)
			break
		}
		var note string
		if note, err = writeNote(p, s); err != nil {
			break
		}
		if sid, err = spawn.NewSessionID(); err != nil {
			break
		}
		script = spawn.LaunchScript(prof, tier, s.Name, sid, s.Cwd, note, qmDir)
		// The fields of the worker's start (spec 5.3), not a copy: the old
		// Record's coordinator session or override no longer apply.
		old := s.Record
		dec := *old.Decision
		dec.Profile = &prof
		newRec = &route.Record{
			Kind: "worker", Project: old.Project, Branch: s.Branch, Name: s.Name,
			Type: old.Type, Size: old.Size, Decision: &dec,
			ClaudeSession: sid, Replaces: replaces(s), Cwd: s.Cwd, UpProject: p.ID,
		}
	}
	if err != nil {
		o.Err = err
		return
	}
	if sid != s.SessionID {
		o.NewSessionID = sid
	}
	if o.Script, err = spawn.WriteScript(s.Name, script); err != nil {
		o.Err = err
		return
	}
	done := "script " + o.Script + " written"
	switch {
	case here:
		// Started by the caller in place of qm: the tab qm up runs in is
		// recorded, or none, never the session's earlier tab (left open).
		own := slices.Concat([]string{p.Coordinator.ID, s.SessionID, s.Instead, sid}, p.PastCoordinators)
		var ref launch.SessionRef
		if lockErr != nil {
			o.warn = "this tab not recorded (" + lockErr.Error() + "): " + noRef
		} else {
			ref, o.warn = d.Here(ctx, p, own, false)
		}
		o.Here, o.Ref = true, ref
		if ref.ID != "" {
			p.Sessions[sid] = stored(ref)
			delete(p.Dropped, sid)
			done = fmt.Sprintf("to start in this tab (%s %s)", ref.Launcher, ref.ID)
		} else {
			if _, ok := p.Sessions[sid]; ok {
				p.drop(sid)
			}
			done = "to start in this tab"
		}
		if _, ok := p.Sessions[s.Instead]; ok && s.Instead != "" && s.Instead != sid {
			// The never-run session this one stands in for: its tab is
			// not the coordinator's.
			p.drop(s.Instead)
		}
	case d.Launcher != nil:
		var reused bool
		if o.Ref, reused, err = launchStep(ctx, p, s, d, sp, o.Script); err != nil {
			o.Err = err
			return
		}
		if reused {
			// Checked once every step has started (waitStarted). A fresh
			// start's worker Record is logged either way: should the line
			// never run, the next plan falls back to the session it
			// replaces (Instead), and should the user run it later, qm
			// knows the session.
			o.waitFor = sid
		}
		p.Sessions[sid] = stored(o.Ref)
		delete(p.Dropped, sid)
		done = fmt.Sprintf("started (%s %s)", o.Ref.Launcher, o.Ref.ID)
	}
	if sid == s.SessionID {
		if d.Launcher == nil && !here {
			return // a resume without a launcher changes nothing qm records
		}
	} else if _, ok := p.Sessions[s.SessionID]; ok {
		p.drop(s.SessionID)
	}
	now := d.now()
	var problems []string
	if s.Role == RoleCoordinator && s.SessionID != p.Coordinator.ID {
		// Gather fell back to a past coordinator (the current one never
		// ran): it is the project's coordinator again.
		p.PastCoordinators = slices.DeleteFunc(p.PastCoordinators, func(id string) bool { return id == s.SessionID })
		p.Coordinator = Session{ID: s.SessionID, Name: s.Name, Started: s.Started}
	}
	switch {
	case s.Role == RoleCoordinator && sid != s.SessionID:
		p.PastCoordinators = append(p.PastCoordinators, s.SessionID)
		p.Coordinator = Session{ID: sid, Name: s.Name, Started: now}
	case newRec != nil:
		newRec.Time = now
		newRec.Detail = o.Ref.Launcher + " " + o.Ref.ID
		newRec.TermLauncher, newRec.TermID = o.Ref.Launcher, o.Ref.ID
		if d.Launcher == nil {
			newRec.Detail = "no launcher: " + o.Script
			newRec.TermLauncher, newRec.TermID = "", ""
		}
		if d.Log != nil {
			if err := d.Log(*newRec); err != nil {
				problems = append(problems, "its worker Record was not logged: "+err.Error())
			}
		}
	}
	if err := Save(*p); err != nil {
		problems = append(problems, "the project was not saved: "+err.Error())
	}
	if len(problems) > 0 {
		o.Err = fmt.Errorf("%s %s, but %s", s.Name, done, strings.Join(problems, "; "))
	}
}

// launchStep starts the script of step s: in its recorded idle agterm tab
// when sp plans to reuse it, else in a new session; reused says which. Right
// before a reuse it lists the workspace again: a tab no longer listed, or no
// longer an idle shell, is left alone (a note) and a new session opened.
// Only agterm's definite refusal (launch.ErrRefused) falls back to a new
// session and then closes the old tab, whose failure is a Warning on the
// returned ref; any other failure is the error, and the tab may have
// received the line, so nothing else is started.
func launchStep(ctx context.Context, p *Project, s Step, d Deps, sp *StalePlan, script string) (ref launch.SessionRef, reused bool, err error) {
	spec := launch.LaunchSpec{Name: s.Name, Cwd: s.Cwd, Script: script, WorkspaceName: p.Workspace}
	var old launch.SessionRef
	var planned bool
	if sp != nil {
		old, planned = sp.Reuse[s.SessionID]
	}
	r, can := d.Launcher.(launch.Reuser)
	if !planned || !can || launcherKind(d.Launcher) != "agterm" {
		ref, err = d.Launcher.Spawn(ctx, spec)
		return ref, false, err
	}
	if note := stillIdle(ctx, p, d, old); note != "" {
		ref, err = d.Launcher.Spawn(ctx, spec)
		ref.Warning = strings.Join(append([]string{note}, nonEmpty(ref.Warning)...), "; ")
		return ref, false, err
	}
	if ref, err = r.Reuse(ctx, old, script); err == nil {
		return ref, true, nil
	}
	if !errors.Is(err, launch.ErrRefused) {
		return launch.SessionRef{}, false, fmt.Errorf("%w; the tab (agterm %s) may have received the launch line: look at it before running qm up again", err, old.ID)
	}
	if ref, err = d.Launcher.Spawn(ctx, spec); err != nil {
		return ref, false, err
	}
	if d.Closer != nil {
		if err := d.Closer.Close(ctx, old); err != nil && !errors.Is(err, launch.ErrGone) {
			ref.Warning = strings.Join(append(nonEmpty(ref.Warning), fmt.Sprintf("stale session %s not closed: %v", old.ID, err)), "; ")
		}
	}
	return ref, false, nil
}

// stillIdle lists the project's workspace again and says why tab old may not
// be typed into now ("" when it may): it is no longer listed, or no longer
// an idle shell, or the listing failed. Without a Lister it trusts the plan.
func stillIdle(ctx context.Context, p *Project, d Deps, old launch.SessionRef) string {
	if d.Lister == nil {
		return ""
	}
	listed, err := d.Lister.Sessions(ctx, p.Workspace)
	if err != nil {
		return fmt.Sprintf("the workspace was not listed again (%v): its tab (agterm %s) left alone, a new tab opened", err, old.ID)
	}
	for _, l := range listed {
		if l.ID == old.ID {
			if l.Idle {
				return ""
			}
			return fmt.Sprintf("its tab (agterm %s) is no longer an idle shell: left open, a new tab opened", old.ID)
		}
	}
	return fmt.Sprintf("its tab (agterm %s) is no longer listed: a new tab opened", old.ID)
}

// reuseStep is how often waitStarted reads the running sessions.
const reuseStep = 500 * time.Millisecond

// waitStarted waits, once every step has started, for the sessions typed
// into reused tabs (Outcome.waitFor) to show among the running ones: one
// deadline for all, d.ReuseWait (15 seconds by default), read every
// reuseStep, with a progress line, and no longer than ctx. A session that
// never shows gets the warning "typed into agterm ID but claude did not
// start". Without d.Live it does not check.
func waitStarted(ctx context.Context, d Deps, outs []Outcome) {
	if d.Live == nil {
		return
	}
	pending := map[string]int{}
	for i := range outs {
		if id := outs[i].waitFor; id != "" && outs[i].Err == nil {
			pending[id] = i
		}
	}
	if len(pending) == 0 {
		return
	}
	wait, sleep := d.ReuseWait, d.Sleep
	if wait <= 0 {
		wait = 15 * time.Second
	}
	if sleep == nil {
		sleep = time.Sleep
	}
	if d.Progress != nil {
		d.Progress(fmt.Sprintf("waiting up to %s for claude to start in %d reused tab(s)", wait, len(pending)))
	}
	why := "claude did not start"
	for i := 0; len(pending) > 0; i++ {
		if live, err := d.Live(); err == nil {
			for id := range pending {
				if _, ok := live[id]; ok {
					delete(pending, id)
				}
			}
		}
		if len(pending) == 0 {
			return
		}
		if ctx.Err() != nil {
			why = "qm up stopped before claude was seen to start"
			break
		}
		if time.Duration(i)*reuseStep >= wait {
			break
		}
		sleep(reuseStep)
	}
	for _, i := range pending {
		o := &outs[i]
		o.Ref.Warning = strings.Join(append(nonEmpty(o.Ref.Warning), fmt.Sprintf("typed into agterm %s but %s", o.Ref.ID, why)), "; ")
	}
}

// keepRunningRef keeps the recorded terminal session of step s, whose
// session id now runs (resumed by hand while qm up asked), only when the
// listing shows that tab running it (runsSession; a tmux window: listed and
// not an idle shell). Otherwise the ref is dropped and the state saved, so
// qm down never closes a tab that runs something else. A failed listing
// keeps it.
func keepRunningRef(ctx context.Context, p *Project, s Step, id string, d Deps) {
	ref, ok := p.Sessions[s.SessionID]
	if !ok || d.Lister == nil {
		return
	}
	listed, err := d.Lister.Sessions(ctx, p.Workspace)
	if err != nil {
		return
	}
	for _, l := range listed {
		if l.ID != ref.ID || !sameWindow(ref, l) {
			continue
		}
		if (l.Launcher == "tmux" && !l.Idle) || runsSession(l, id, s.Cwd) {
			return
		}
	}
	p.drop(s.SessionID)
	_ = Save(*p)
}

// replaces is the session a fresh start from s replaces: the never-run
// session s stands in for, else s's own.
func replaces(s Step) string {
	if s.Instead != "" {
		return s.Instead
	}
	return s.SessionID
}

// stored is ref as the state keeps it, without a warning.
func stored(ref launch.SessionRef) launch.SessionRef {
	ref.Warning = ""
	return ref
}

// logUp logs the kind "up" Record of step s (spec 5.3): action, the session
// id it ran under, the reason. A failure is a warning on o.
func logUp(d Deps, p *Project, s Step, id string, now time.Time, o *Outcome) {
	if d.Log == nil {
		return
	}
	r := route.Record{
		Time: now, Kind: "up", Project: p.Root, UpProject: p.ID, Action: string(s.Action),
		ClaudeSession: id, Name: s.Name, Detail: s.Why, Cwd: s.Cwd, Branch: s.Branch,
	}
	if id != s.SessionID {
		r.Replaces = s.SessionID
	}
	if err := d.Log(r); err != nil {
		o.Ref.Warning = strings.Join(append(nonEmpty(o.Ref.Warning), "qm up Record not logged: "+err.Error()), "; ")
	}
}
