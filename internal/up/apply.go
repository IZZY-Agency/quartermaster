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
	Log         func(route.Record) error
	Now         func() time.Time
	Self        []string
	Trusted     func(dir string) (bool, error)
	Run         DirRunner
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
	// waitFor is the session id typed into a reused tab, which Apply waits
	// to see running once every step has started.
	waitFor string
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
// knows, and the duplicate guard covers it.
func Apply(ctx context.Context, p *Project, steps []Step, d Deps) []Outcome {
	if p.Sessions == nil {
		p.Sessions = map[string]launch.SessionRef{}
	}
	order := slices.Clone(steps)
	slices.SortStableFunc(order, func(a, b Step) int {
		return cmp.Compare(rank(a.Role), rank(b.Role))
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
	for _, s := range order {
		o := Outcome{Step: s}
		switch s.Action {
		case Leave:
			if starting {
				o.Ref = repin(ctx, p, s, d)
			}
		case Resume, Fresh:
			start(ctx, p, order, s, d, sp, &o)
			if w := warnings[s.SessionID]; len(w) > 0 {
				o.Ref.Warning = strings.Join(append(w, nonEmpty(o.Ref.Warning)...), "; ")
			}
		}
		if starting && o.Err == nil {
			id := o.Step.SessionID
			if o.NewSessionID != "" {
				id = o.NewSessionID
			}
			logUp(d, p, o.Step, id, d.now(), &o)
		}
		outs = append(outs, o)
	}
	waitStarted(ctx, d, outs)
	return outs
}

func rank(r Role) int {
	if r == RoleCoordinator {
		return 0
	}
	return 1
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
func closeStale(ctx context.Context, sp *StalePlan, d Deps) map[string][]string {
	warn := map[string][]string{}
	for id, w := range sp.Warn {
		warn[id] = append(warn[id], w...)
	}
	if d.Closer == nil {
		return warn
	}
	for _, s := range sp.Close {
		if err := d.Closer.Close(ctx, s.Ref); err != nil && !errors.Is(err, launch.ErrGone) {
			warn[s.Step.SessionID] = append(warn[s.Step.SessionID], fmt.Sprintf("stale session %s not closed: %v", s.Ref.ID, err))
		}
	}
	return warn
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
func start(ctx context.Context, p *Project, steps []Step, s Step, d Deps, sp *StalePlan, o *Outcome) {
	if d.Launcher != nil && d.Live != nil {
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
		env := map[string]string{"QM_PROJECT_ID": p.ID, "QM_WORKSPACE": p.Workspace}
		script = spawn.PlainResumeScript(adoptedArgs(s.Adopted.Args, p.ClaudeArgs), env, s.Name, sid, s.Cwd, qmDirOf(d.Exe))
	default:
		var prof route.Profile
		if prof, err = workerProfile(p, s, d.Exe); err != nil {
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
	if d.Launcher != nil {
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
		if d.Launcher == nil {
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
