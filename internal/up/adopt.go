package up

// This file adopts running sessions on a project's first run (adopt design
// 4.1 to 4.6): it detects the live Claude sessions in the project's folders,
// lets the user (or --adopt) pick a coordinator and workers, plans the
// adoption and records it. Adoption restarts nothing and types nothing: it
// lists, moves (agterm), pins and writes qm's own state and Records.

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/route"
	"github.com/IZZY-Agency/quartermaster/internal/spawn"
	"github.com/IZZY-Agency/quartermaster/internal/transcript"
)

// Errors of adoption.
var (
	// ErrAdoptCoordinator: --adopt found no single coordinator candidate.
	ErrAdoptCoordinator = errors.New("pick the coordinator with --adopt-coordinator ID")
	// ErrCoordinatorGone: the chosen coordinator exited before the
	// confirmation; nothing was adopted.
	ErrCoordinatorGone = errors.New("the chosen coordinator exited before the confirmation: nothing adopted")
	// ErrWorkspaceName: the coordinator's workspace name is not the
	// project's alone.
	ErrWorkspaceName = errors.New("workspace name in use")
	// errAdoptAborted: the questions' input ended; nothing was written.
	errAdoptAborted = errors.New("no answer: nothing adopted")
)

// AdoptCandidate is a live session qm up may adopt. Worker says a kind
// "worker" Record names its id; Self says it is the session running qm up.
// Kept and Dropped are its claude flags (claudecode.KeptFlags). Subfolder
// says it runs inside the root or a worktree but not in that folder itself:
// it is listed, and counts as a live session for the scripted refusal, but
// it cannot be adopted.
type AdoptCandidate struct {
	SessionID, Name, Cwd, Branch string
	PID                          int
	Last                         transcript.Last
	Started                      time.Time
	Kept, Dropped                []string
	Worker, Self, Subfolder      bool
	// Tab is where the session shows in the terminal ("WORKSPACE / TAB"),
	// for the list only: the session's own name is often not the tab's.
	Tab string
}

// LabelTabs fills Tab for each candidate that agterm shows running in a tab:
// the tab its process's environment names (termOf), when that tab's
// foreground is the candidate's own claude (tabRuns). It is for the list the
// user picks from; a candidate with no such tab keeps an empty Tab. It stops
// when ctx ends, and passes ctx to each read.
func LabelTabs(ctx context.Context, cands []AdoptCandidate, wss []launch.Workspace, termOf func(ctx context.Context, pid int) (claudecode.TermEnv, error)) {
	for i := range cands {
		if ctx.Err() != nil {
			return
		}
		env, err := termOf(ctx, cands[i].PID)
		if err != nil || env.AgtermSession == "" {
			continue
		}
		for _, w := range wss {
			for _, tab := range w.Sessions {
				if tab.ID != env.AgtermSession {
					continue
				}
				if _, ok := tabRuns(tab, cands[i]); ok {
					cands[i].Tab = w.Name + " / " + tab.Name
				}
			}
		}
	}
}

// Detected is what Detect found: the candidates, and whether the session
// running qm up (a claude ancestor of qm) is none of them (SelfElsewhere),
// with its folder when known (SelfCwd).
type Detected struct {
	Candidates    []AdoptCandidate
	SelfElsewhere bool
	SelfCwd       string
}

// subfolderNote is how a candidate in a subfolder is shown.
const subfolderNote = "runs in a subfolder: not adoptable"

// DetectEnv is what Detect reads the machine through. Worktrees are the
// project's folders (Worktrees); TranscriptPath returns "" for a missing
// transcript; Run reads a folder's branch (nil: none); SelfPID is the
// process qm up walks up from to find its own claude session. Held are
// session ids the project at root holds already (Held), left out as well: a
// later run adopts only what the project does not hold.
type DetectEnv struct {
	Procs          []claudecode.Proc
	Live           map[string]claudecode.LiveSession
	Worktrees      []string
	TranscriptPath func(id string) (string, error)
	Run            DirRunner
	SelfPID        int
	Held           map[string]bool
	// Ctx bounds the detection and its subprocesses (nil: unbounded); when
	// it ends, the candidates read so far are returned. Quick reads only what
	// says whether a session is a candidate (the restore's note counts them):
	// no branch, no transcript's times or context.
	Ctx   context.Context
	Quick bool
}

// Detect lists the adoption candidates of the project at root (adopt
// design 4.1): the live sessions with a session file (named or not: an
// unusable name is replaced when adopted) whose folder, symlinks
// resolved, is one of env.Worktrees, or inside one of them (Subfolder:
// listed, never adoptable); without a process started with -p or --print,
// a session a review Record names, and a session another recorded project
// holds (its coordinator, past coordinators, adopted or recorded sessions,
// or a worker Record it ties). Ordered by folder (root first, in Worktrees
// order), subfolders after, then name. When qm runs under a claude process
// that is no candidate, SelfElsewhere is set.
func Detect(root string, all []Project, recs []route.Record, env DetectEnv) Detected {
	folder := map[string]int{}
	var dirs []string
	for i, d := range env.Worktrees {
		if _, ok := folder[realPath(d)]; !ok {
			folder[realPath(d)] = i
			dirs = append(dirs, realPath(d))
		}
	}
	// within is the index of the deepest project folder holding cwd.
	within := func(cwd string) (int, bool) {
		best, n := -1, 0
		for _, d := range dirs {
			if strings.HasPrefix(cwd, strings.TrimSuffix(d, string(filepath.Separator))+string(filepath.Separator)) && len(d) > n {
				best, n = folder[d], len(d)
			}
		}
		return best, best >= 0
	}
	self := 0
	if env.SelfPID > 0 {
		if p, ok := claudecode.Ancestor(env.Procs, env.SelfPID); ok {
			self = p.PID
		}
	}
	held := withAliases(heldElsewhere(ProjectID(root), all, recs), env.Live)
	own := withAliases(env.Held, env.Live)
	worker, review := map[string]bool{}, map[string]bool{}
	for _, r := range recs {
		if r.Kind != "worker" || r.ClaudeSession == "" {
			continue
		}
		if r.Type == "review" {
			review[r.ClaudeSession] = true
		} else {
			worker[r.ClaudeSession] = true
		}
	}
	// A reviewer's Record names the id it was launched with; after /clear the
	// same process has another id too.
	review = withAliases(review, env.Live)
	byPID := map[int]claudecode.Proc{}
	for _, p := range env.Procs {
		byPID[p.PID] = p
	}
	var det Detected
	order := map[string]int{}
	ctx := env.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	for id, l := range env.Live {
		if ctx.Err() != nil {
			break
		}
		if self != 0 && l.PID == self {
			det.SelfCwd = l.Cwd
		}
		if l.Cwd == "" || held[id] || own[id] || review[id] {
			continue
		}
		// A session belongs to the folder it was started in (its transcript's
		// first folder), which is also where it is resumed from; its session
		// file names the folder it works in now, which changes with cd.
		cwd, path := realPath(l.Cwd), ""
		if env.TranscriptPath != nil {
			if p, err := env.TranscriptPath(id); err == nil && p != "" {
				path = p
				if start, err := transcript.StartCwd(p); err == nil && start != "" {
					cwd = realPath(start)
				}
			}
		}
		idx, exact := folder[cwd]
		sub := false
		if !exact {
			if idx, sub = within(cwd); !sub {
				continue
			}
		}
		args, _ := claudecode.IsClaude(byPID[l.PID].Cmd)
		if printMode(args) {
			continue
		}
		c := AdoptCandidate{SessionID: id, Name: l.Name, Cwd: cwd, PID: l.PID, Worker: worker[id], Self: self != 0 && l.PID == self, Subfolder: sub}
		c.Kept, c.Dropped = claudecode.KeptFlags(args)
		if !env.Quick {
			if path != "" {
				c.Started, _ = transcript.FirstAt(path)
				c.Last, _ = transcript.LastContext(path)
			}
			c.Branch = currentBranchCtx(ctx, env.Run, cwd)
		}
		order[id] = idx
		det.Candidates = append(det.Candidates, c)
	}
	slices.SortFunc(det.Candidates, func(a, b AdoptCandidate) int {
		return cmp.Or(cmp.Compare(order[a.SessionID], order[b.SessionID]), cmpBool(a.Subfolder, b.Subfolder), cmp.Compare(a.Name, b.Name), cmp.Compare(a.SessionID, b.SessionID))
	})
	det.SelfElsewhere = self != 0 && !slices.ContainsFunc(det.Candidates, func(c AdoptCandidate) bool { return c.Self })
	if !det.SelfElsewhere {
		det.SelfCwd = ""
	}
	return det
}

// cmpBool orders false before true.
func cmpBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}

// realPath is dir, cleaned, with symlinks resolved when it exists.
func realPath(dir string) string {
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		return r
	}
	return filepath.Clean(dir)
}

// printMode says whether claude's arguments hold -p or --print: a one-shot
// run, never adopted. It stops, as claudecode.KeptFlags does, at "--" and at
// the first token holding a newline (ps prints it as \012): a prompt that
// mentions -p is not the flag.
func printMode(args []string) bool {
	for _, a := range args {
		if a == "--" || strings.Contains(a, `\012`) {
			break
		}
		if a == "-p" || a == "--print" || strings.HasPrefix(a, "--print=") {
			return true
		}
	}
	return false
}

// heldElsewhere are the session ids a recorded project other than id holds:
// its coordinator, past coordinators, adopted and recorded sessions, and the
// worker Records it ties.
func heldElsewhere(id string, all []Project, recs []route.Record) map[string]bool {
	held := map[string]bool{}
	for i := range all {
		if all[i].ID != id {
			all[i].addHeld(held, recs)
		}
	}
	delete(held, "")
	return held
}

// withAliases is held with every other id a live process of a held id
// holds: after /clear a coordinator started as A keeps A in its arguments
// while its session file names B, and both ids are that one session. held
// itself is not changed.
func withAliases(held map[string]bool, live map[string]claudecode.LiveSession) map[string]bool {
	out := maps.Clone(held)
	if out == nil {
		out = map[string]bool{}
	}
	pids := map[int]bool{}
	for id := range held {
		if l, ok := live[id]; ok && l.PID > 0 {
			pids[l.PID] = true
		}
	}
	for id, l := range live {
		if pids[l.PID] {
			out[id] = true
		}
	}
	return out
}

// addHeld adds to held the session ids project p holds: its coordinator,
// past coordinators, adopted and recorded sessions, and the worker Records it
// ties.
func (p *Project) addHeld(held map[string]bool, recs []route.Record) {
	held[p.Coordinator.ID] = true
	for _, s := range p.PastCoordinators {
		held[s] = true
	}
	for s := range p.Adopted {
		held[s] = true
	}
	for s := range p.Sessions {
		held[s] = true
	}
	for j := range recs {
		if r := &recs[j]; r.Kind == "worker" && r.ClaudeSession != "" && p.Tied(r) {
			held[r.ClaudeSession] = true
		}
	}
}

// Held are the session ids project p holds, in the sense a restore uses to
// list sessions (Gather): the coordinator and the past ones, the adopted
// sessions, the recorded terminal sessions, and every worker Record p ties
// (finished ones, the sessions a later Record replaced and the ones marked
// done included). A session that is held is restored or left, never offered
// for adoption.
func Held(p Project, recs []route.Record) map[string]bool {
	held := map[string]bool{}
	p.addHeld(held, recs)
	for s := range p.Done {
		held[s] = true
	}
	for j := range recs {
		if r := &recs[j]; r.Kind == "worker" && r.Replaces != "" && p.Tied(r) {
			held[r.Replaces] = true
		}
	}
	delete(held, "")
	return held
}

// Choice is the user's pick: New starts a new coordinator and adopts
// nothing; else Coordinator and Workers are session ids.
type Choice struct {
	New         bool
	Coordinator string
	Workers     []string
}

// ErrSelfElsewhere: --adopt runs inside a Claude session that is not one of
// the project's candidates; adopting another session would be a guess.
var ErrSelfElsewhere = errors.New("nothing adopted")

// ChooseAuto is --adopt's pick (adopt design 4.1): the coordinator is the
// candidate whose id is coordinatorID or starts with it (none or several:
// ErrInput), else the Self candidate, else the one candidate in root that
// no worker Record names (none or several: ErrAdoptCoordinator). When the
// session running qm up is no candidate (det.SelfElsewhere), or runs in a
// subfolder, there is no fallback: ErrSelfElsewhere. The workers are every
// other candidate a worker Record names. A candidate in a subfolder is
// never picked.
func ChooseAuto(det Detected, root, coordinatorID string) (Choice, error) {
	cands := det.Candidates
	var coord []AdoptCandidate
	switch {
	case coordinatorID != "":
		for _, c := range cands {
			if strings.HasPrefix(c.SessionID, coordinatorID) {
				coord = append(coord, c)
			}
		}
		if len(coord) != 1 {
			what := "no candidate"
			if len(coord) > 1 {
				what = "several candidates"
			}
			return Choice{}, fmt.Errorf("%w: --adopt-coordinator %s: %s: %s", ErrInput, coordinatorID, what, describe(cands))
		}
		if coord[0].Subfolder {
			return Choice{}, fmt.Errorf("%w: --adopt-coordinator %s: %s %s", ErrInput, coordinatorID, describe(coord), subfolderNote)
		}
	default:
		for _, c := range cands {
			if c.Self {
				if c.Subfolder {
					return Choice{}, fmt.Errorf("this session runs in %s, not in this project's folders: run qm up there: %w", c.Cwd, ErrSelfElsewhere)
				}
				coord = []AdoptCandidate{c}
				break
			}
		}
		if len(coord) == 0 && det.SelfElsewhere {
			where := "outside this project's folders"
			if det.SelfCwd != "" {
				where = "in " + det.SelfCwd + ", not in this project's folders"
			}
			return Choice{}, fmt.Errorf("this session runs %s: run qm up there: %w", where, ErrSelfElsewhere)
		}
		if len(coord) == 0 {
			for _, c := range cands {
				if c.Cwd == root && !c.Worker && !c.Subfolder {
					coord = append(coord, c)
				}
			}
		}
		switch {
		case len(coord) > 1:
			return Choice{}, fmt.Errorf("several coordinator candidates: %s: %w", describe(coord), ErrAdoptCoordinator)
		case len(coord) == 0:
			return Choice{}, fmt.Errorf("no coordinator candidate (a session in %s that no worker Record names); sessions: %s: %w", root, describe(cands), ErrAdoptCoordinator)
		}
	}
	ch := Choice{Coordinator: coord[0].SessionID}
	for _, c := range cands {
		if c.Worker && !c.Subfolder && c.SessionID != ch.Coordinator {
			ch.Workers = append(ch.Workers, c.SessionID)
		}
	}
	return ch, nil
}

// describe names candidates as NAME (ID prefix, folder), comma separated.
func describe(cands []AdoptCandidate) string {
	if len(cands) == 0 {
		return "none"
	}
	var parts []string
	for _, c := range cands {
		parts = append(parts, fmt.Sprintf("%s (%s, %s)", oneLine(c.Name), prefix(c.SessionID), c.Cwd))
	}
	return strings.Join(parts, ", ")
}

// RenderCandidates prints the candidates as numbered rows from 1.
func RenderCandidates(cands []AdoptCandidate, now time.Time) string {
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	tabs := slices.ContainsFunc(cands, func(c AdoptCandidate) bool { return c.Tab != "" })
	head := "#\tNAME\tSESSION\tFOLDER\tBRANCH\tLAST\tCONTEXT\tKNOWN\t"
	if tabs {
		head = "#\tTAB\tNAME\tSESSION\tFOLDER\tBRANCH\tLAST\tCONTEXT\tKNOWN\t"
	}
	_, _ = fmt.Fprintln(tw, head)
	for i, c := range cands {
		last, ctxk := "-", "-"
		if !c.Last.At.IsZero() {
			last, ctxk = ago(now.Sub(c.Last.At)), kTokens(c.Last.Context)
		}
		known := ""
		switch {
		case c.Subfolder:
			known = subfolderNote
		case c.Worker:
			known = "qm worker"
		}
		self := ""
		if c.Self {
			self = "this session"
		}
		tab := ""
		if tabs {
			tab = orDash(oneLine(c.Tab)) + "\t"
		}
		_, _ = fmt.Fprintf(tw, "%d\t%s%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", i+1, tab, oneLine(c.Name), prefix(c.SessionID), c.Cwd, orDash(c.Branch), last, ctxk, known, self)
	}
	_ = tw.Flush()
	return b.String()
}

// AskAdopt asks for the coordinator (a row number, or n) and the workers
// (row numbers split by spaces or commas, a for all the other rows, q for the qm workers only, empty for
// none). A bad answer is ErrInput; the end of input errAdoptAborted.
func AskAdopt(cands []AdoptCandidate, in *bufio.Reader, out io.Writer) (Choice, error) {
	_, _ = fmt.Fprint(out, "Coordinator: row number, or n to start a new coordinator and adopt nothing: ")
	line, err := readAnswer(in)
	if err != nil {
		return Choice{}, err
	}
	if strings.EqualFold(line, "n") {
		return Choice{New: true}, nil
	}
	row, err := adoptableRow(line, cands)
	if err != nil {
		return Choice{}, err
	}
	ch := Choice{Coordinator: cands[row].SessionID}
	if ch.Workers, err = askWorkers(cands, in, out, "a for all the other rows", row); err != nil {
		return Choice{}, err
	}
	return ch, nil
}

// AskWorkers asks which candidates to adopt as workers on a later run (adopt
// design 4.8): row numbers split by spaces or commas, a for all the rows, q
// for the qm workers only, empty for none. A bad answer is ErrInput; the end
// of input errAdoptAborted.
func AskWorkers(cands []AdoptCandidate, in *bufio.Reader, out io.Writer) ([]string, error) {
	return askWorkers(cands, in, out, "a for all the rows", -1)
}

// askWorkers asks the workers question; allText says what a means. Row
// coordinator (0-based, -1 for none) is the chosen coordinator, no worker.
func askWorkers(cands []AdoptCandidate, in *bufio.Reader, out io.Writer, allText string, coordinator int) ([]string, error) {
	_, _ = fmt.Fprintf(out, "Workers: row numbers, %s, q for the qm workers only, empty for none: ", allText)
	line, err := readAnswer(in)
	if err != nil {
		return nil, err
	}
	var workers []string
	if all, qm := strings.EqualFold(line, "a"), strings.EqualFold(line, "q"); all || qm {
		for i, c := range cands {
			if (all || c.Worker) && !c.Subfolder && i != coordinator {
				workers = append(workers, c.SessionID)
			}
		}
		return workers, nil
	}
	for _, f := range strings.FieldsFunc(line, func(r rune) bool { return r == ' ' || r == ',' || r == '\t' }) {
		i, err := adoptableRow(f, cands)
		if err != nil {
			return nil, err
		}
		if i == coordinator {
			return nil, fmt.Errorf("%w: row %d is the coordinator", ErrInput, i+1)
		}
		if !slices.Contains(workers, cands[i].SessionID) {
			workers = append(workers, cands[i].SessionID)
		}
	}
	return workers, nil
}

// RecordedWorkers are the ids of the adoptable candidates a worker Record
// names: what a script adopts on a later run.
func RecordedWorkers(cands []AdoptCandidate) []string {
	var ids []string
	for _, c := range cands {
		if c.Worker && !c.Subfolder {
			ids = append(ids, c.SessionID)
		}
	}
	return ids
}

// readAnswer reads one line, trimmed; errAdoptAborted when the input ended
// before a full line.
func readAnswer(in *bufio.Reader) (string, error) {
	line, err := in.ReadString('\n')
	if err != nil && (line == "" || !errors.Is(err, io.EOF)) {
		return "", errAdoptAborted
	}
	return strings.TrimSpace(line), nil
}

// adoptableRow is answer s as a 0-based row of cands; a row in a subfolder
// is ErrInput.
func adoptableRow(s string, cands []AdoptCandidate) (int, error) {
	i, err := rowOf(s, len(cands))
	if err == nil && cands[i].Subfolder {
		return 0, fmt.Errorf("%w: row %d %s", ErrInput, i+1, subfolderNote)
	}
	return i, err
}

// rowOf is answer s as a 0-based row index of n rows.
func rowOf(s string, n int) (int, error) {
	i, err := strconv.Atoi(s)
	if err != nil || i < 1 || i > n {
		return 0, fmt.Errorf("%w: %q is not a row number from 1 to %d", ErrInput, s, n)
	}
	return i - 1, nil
}

// AdoptDeps is what adoption reads and changes the machine through. Any
// func may be nil: that terminal or fact is then absent.
type AdoptDeps struct {
	Workspaces func(context.Context) ([]launch.Workspace, error)
	Move       func(ctx context.Context, sessionID, workspaceID string) error
	Pin        func(ctx context.Context, id string) error
	PaneOf     func(ctx context.Context, pane string) (session, windowID, windowName string, err error)
	// WindowPanes is the number of panes of a tmux window: a tmux session is
	// recorded only when its window holds that one pane, since qm down
	// closes the whole window. nil records no tmux window.
	WindowPanes func(ctx context.Context, windowID string) (int, error)
	// PaneRuns says whether the pane of tmux window windowID (a window of
	// one pane) runs process pid in its foreground: a TMUX_PANE a session
	// inherited names a pane another job may hold. nil records no tmux
	// window.
	PaneRuns func(ctx context.Context, windowID string, pid int) (bool, error)
	TermOf   func(pid int) (claudecode.TermEnv, error)
	Live     func() (map[string]claudecode.LiveSession, error)
	Trusted  func(dir string) (bool, error)
	// Running says whether process pid still runs, after its environment
	// could not be read (nil: it does).
	Running func(pid int) bool
	Log     func(route.Record) error
	// Records reads the decision log again under the adoption lock, for the
	// worker Records that tie sessions to other projects. nil reads none.
	Records func() ([]route.Record, error)
	Now     func() time.Time
	// Config is up.claude_args from the config.
	Config []string
}

// AdoptPlan is a planned adoption: the project state to save, the
// sessions adopted and left, the agterm moves and pins, and notes.
type AdoptPlan struct {
	Project     Project
	Coordinator AdoptCandidate
	Workers     []AdoptCandidate
	Left        []AdoptCandidate
	Moves       []AdoptMove
	Pins        []string
	Notes       []string
	// Later says the plan adopts workers into a project that has state
	// (PlanAdoptWorkers): Project holds the new entries only, and Adopt merges
	// them into the saved state.
	Later bool
}

// AdoptMove moves agterm session SessionID (session Name) from workspace
// From into To (id ToID).
type AdoptMove struct {
	Name, SessionID, From, To, ToID string
}

// PlanAdopt plans adopting ch from cands into a new project at root (adopt
// design 4.2 to 4.4, 4.6). It changes nothing on the machine.
func PlanAdopt(ctx context.Context, root string, all []Project, cands []AdoptCandidate, ch Choice, args []string, argsGiven bool, d AdoptDeps) (AdoptPlan, error) {
	var ap AdoptPlan
	if ch.New || ch.Coordinator == "" {
		return ap, fmt.Errorf("%w: no coordinator chosen", ErrInput)
	}
	coord, ok := candidateByID(cands, ch.Coordinator)
	if !ok {
		return ap, fmt.Errorf("%w: coordinator %s is not a candidate", ErrInput, ch.Coordinator)
	}
	if coord.Cwd != realPath(root) {
		return ap, fmt.Errorf("%w: the coordinator %s runs in %s, not in the project root %s: run qm up there, or pick another session", ErrInput, oneLine(coord.Name), coord.Cwd, root)
	}
	ap.Coordinator = coord
	var err error
	if ap.Workers, ap.Left, err = pickWorkers(cands, ch.Workers, map[string]bool{coord.SessionID: true}); err != nil {
		return ap, err
	}

	claudeArgs := args
	switch {
	case argsGiven:
	case len(coord.Kept) > 0:
		claudeArgs = coord.Kept
	default:
		claudeArgs = d.Config
	}
	if err := CheckClaudeArgs(claudeArgs); err != nil {
		return ap, err
	}

	recs, err := adoptRecords(d)
	if err != nil {
		return ap, err
	}
	t := newAdoptTerminal(d, all, recs, cands)
	if err := t.coordinator(ctx, root, all, coord); err != nil {
		return ap, err
	}
	if t.coordRef.ID != "" {
		t.owned[refKey(t.coordRef)] = coord.SessionID
	}
	ap.Notes = append(ap.Notes, t.notes...)
	t.notes = nil

	now := time.Now().UTC()
	if d.Now != nil {
		now = d.Now()
	}
	p := Project{
		ID: ProjectID(root), Root: root, Workspace: t.workspace, Launcher: t.launcher,
		ClaudeArgs:  append([]string{}, claudeArgs...),
		Coordinator: Session{ID: coord.SessionID, Name: coord.Name, Started: coord.Started, Adopted: true},
		Sessions:    map[string]launch.SessionRef{},
		Created:     now,
	}
	if p.Coordinator.Started.IsZero() {
		p.Coordinator.Started = now
	}
	if !spawn.ValidName(coord.Name) {
		p.Coordinator.Name = t.workspace + "-coord"
		if !spawn.ValidName(p.Coordinator.Name) {
			// An agterm workspace name may hold spaces.
			p.Coordinator.Name = cleanName(root) + "-coord"
		}
		ap.Notes = append(ap.Notes, fmt.Sprintf("coordinator named %s in qm's state and from its next restart (its name %q is not a safe session name)", p.Coordinator.Name, coord.Name))
	}
	if t.coordRef.ID != "" {
		p.Sessions[coord.SessionID] = t.coordRef
		if t.coordRef.Launcher == "agterm" {
			ap.Pins = append(ap.Pins, t.coordRef.ID)
		}
	}
	ap.Notes = append(ap.Notes, trustNote(d, p.Coordinator.Name, root)...)

	t.planWorkers(ctx, &p, &ap)
	ap.Project = p
	return ap, nil
}

// candidateByID is the candidate with session id id.
func candidateByID(cands []AdoptCandidate, id string) (AdoptCandidate, bool) {
	i := slices.IndexFunc(cands, func(c AdoptCandidate) bool { return c.SessionID == id })
	if i < 0 {
		return AdoptCandidate{}, false
	}
	return cands[i], true
}

// pickWorkers is the candidates ids name (the ones in skip aside, which are
// picked already), and the candidates left. An id that is no candidate, or a
// candidate in a subfolder, is ErrInput.
func pickWorkers(cands []AdoptCandidate, ids []string, picked map[string]bool) (workers, left []AdoptCandidate, err error) {
	for _, id := range ids {
		c, ok := candidateByID(cands, id)
		if !ok {
			return nil, nil, fmt.Errorf("%w: worker %s is not a candidate", ErrInput, id)
		}
		if picked[id] {
			continue
		}
		if c.Subfolder {
			return nil, nil, fmt.Errorf("%w: worker %s %s", ErrInput, oneLine(c.Name), subfolderNote)
		}
		picked[id] = true
		workers = append(workers, c)
	}
	for _, c := range cands {
		if !picked[c.SessionID] {
			left = append(left, c)
		}
	}
	return workers, left, nil
}

// planWorkers records ap.Workers in p (its Adopted entries and terminal
// sessions) and plans their moves and pins and the trust notes (adopt design
// 4.2 to 4.4), as for a first run's workers and a later run's alike.
func (t *adoptTerminal) planWorkers(ctx context.Context, p *Project, ap *AdoptPlan) {
	d := t.d
	for _, w := range ap.Workers {
		if p.Adopted == nil {
			p.Adopted = map[string]AdoptedSession{}
		}
		if p.Sessions == nil {
			p.Sessions = map[string]launch.SessionRef{}
		}
		name := w.Name
		if w.Worker {
			p.Adopted[w.SessionID] = AdoptedSession{}
		} else {
			if !spawn.ValidName(name) {
				name = fallbackName(w.SessionID)
				ap.Notes = append(ap.Notes, fmt.Sprintf("worker named %s in qm's state and from its next restart (its name %q is not a safe session name)", name, w.Name))
			}
			p.Adopted[w.SessionID] = AdoptedSession{
				Name: name, Cwd: w.Cwd, Model: flagValue(w.Kept, "--model"),
				Args: slices.Clone(w.Kept), Started: w.Started,
			}
		}
		ref, move := t.worker(ctx, w, name)
		ap.Notes = append(ap.Notes, t.notes...)
		t.notes = nil
		if move != nil {
			ap.Moves = append(ap.Moves, *move)
		}
		if ref.ID != "" {
			p.Sessions[w.SessionID] = ref
			if ref.Launcher == "agterm" {
				ap.Pins = append(ap.Pins, ref.ID)
			}
		} else if w.Worker {
			// No terminal session is kept: the session's worker Record must not
			// give its old one back (Gather seeds from Records unless dropped).
			p.drop(w.SessionID)
		}
		ap.Notes = append(ap.Notes, trustNote(d, name, w.Cwd)...)
	}
}

// PlanAdoptWorkers plans adopting the candidates ids name as workers of the
// project p, which has state already (adopt design 4.8): the project's
// workspace and launcher stay, the coordinator is not touched. The returned
// plan's Project holds the new entries only (Adopted and Sessions) beside
// p's identity: Adopt merges them into the saved state. It changes nothing
// on the machine.
func PlanAdoptWorkers(ctx context.Context, p Project, all []Project, cands []AdoptCandidate, ids []string, d AdoptDeps) (AdoptPlan, error) {
	ap := AdoptPlan{Later: true}
	var err error
	if ap.Workers, ap.Left, err = pickWorkers(cands, ids, map[string]bool{}); err != nil {
		return ap, err
	}
	recs, err := adoptRecords(d)
	if err != nil {
		return ap, err
	}
	t := newAdoptTerminal(d, all, recs, cands)
	t.workspace, t.launcher = p.Workspace, p.Launcher
	if p.Launcher == "agterm" {
		// The workspace the project's tabs are in, found by name: a worker in
		// another workspace is moved into it. Without exactly one, no tab is
		// recorded or moved.
		var ids []string
		for _, w := range t.workspaces(ctx) {
			if w.Name == p.Workspace {
				ids = append(ids, w.ID)
			}
		}
		if len(ids) == 1 {
			t.workspaceID = ids[0]
		} else {
			t.launcher = ""
			ap.Notes = append(ap.Notes, fmt.Sprintf("agterm shows %d workspaces named %s: workers adopted with no terminal session; qm down leaves them open", len(ids), p.Workspace))
		}
		ap.Notes = append(ap.Notes, t.notes...)
		t.notes = nil
	}
	proj := Project{ID: p.ID, Root: p.Root, Workspace: p.Workspace, Launcher: p.Launcher, Sessions: map[string]launch.SessionRef{}}
	t.planWorkers(ctx, &proj, &ap)
	if len(proj.Sessions) == 0 {
		proj.Sessions = nil
	}
	ap.Project = proj
	return ap, nil
}

// trustNote is the note on an untrusted (or unreadable) folder.
func trustNote(d AdoptDeps, name, dir string) []string {
	if d.Trusted == nil {
		return nil
	}
	ok, err := d.Trusted(dir)
	if err == nil && ok {
		return nil
	}
	n := fmt.Sprintf("not trusted: %s's folder %s; a later qm up skips it until you run claude there once", name, dir)
	if err != nil {
		n += fmt.Sprintf(" (trust not read: %v)", err)
	}
	return []string{n}
}

// flagValue is the value after flag in kept flags ("" when absent).
func flagValue(kept []string, flag string) string {
	for i := 0; i+1 < len(kept); i++ {
		if kept[i] == flag {
			return kept[i+1]
		}
	}
	return ""
}

// adoptTerminal finds the terminal sessions of the adopted sessions: the
// coordinator's sets the project's workspace and launcher (adopt design
// 4.4), each worker's is recorded when it is in that terminal.
type adoptTerminal struct {
	d                   AdoptDeps
	workspace, launcher string
	workspaceID         string
	coordRef            launch.SessionRef
	wss                 []launch.Workspace
	wssRead             bool
	notes               []string
	failed              bool // a probe failed (ps or tmux), as opposed to answering no
	// agtermOf is the agterm session id each candidate's (and each other
	// running session's) environment names, by Claude session id; cands are
	// all the candidates, others the running sessions that are not.
	agtermOf map[string]string
	cands    []AdoptCandidate
	others   []AdoptCandidate
	// owned are the terminal sessions a project records, by refKey, with
	// the Claude session they are recorded for: never another's to take.
	owned map[string]string
	// unverified says why the running sessions could not be read at all,
	// "" when they were; unread are the running sessions (candidates or
	// not) whose environment could not be read while they still ran. A tab
	// whose claude names no session is not recorded when either leaves its
	// owner unknown: unverified always, unread for a session in the tab's
	// folder (only such a session can own it).
	unverified string
	unread     []unreadTerm
}

// unreadTerm is a running session whose environment could not be read.
type unreadTerm struct{ id, cwd, why string }

// adoptRecords reads the decision log through d.Records (none when nil), for
// the terminal sessions the worker Records give the projects.
func adoptRecords(d AdoptDeps) ([]route.Record, error) {
	if d.Records == nil {
		return nil, nil
	}
	recs, err := d.Records()
	if err != nil {
		return nil, fmt.Errorf("decision log not read, nothing adopted: %w", err)
	}
	return recs, nil
}

// refKey is the key of terminal session ref in adoptTerminal.owned.
func refKey(ref launch.SessionRef) string { return ref.Launcher + "\x00" + ref.ID }

// newAdoptTerminal reads the agterm tab each candidate's environment names
// (picked or not), and each other running session's: a tab is a candidate's
// own only when its foreground matches it and no other running session
// naming it matches it as well (adopt design 4.4). It notes the terminal
// sessions all recorded projects already hold, as Gather reads them (their
// state, and their worker Records in recs: effectiveSessions).
func newAdoptTerminal(d AdoptDeps, all []Project, recs []route.Record, cands []AdoptCandidate) adoptTerminal {
	t := adoptTerminal{d: d, agtermOf: map[string]string{}, cands: cands, owned: map[string]string{}}
	for i := range all {
		for id, ref := range effectiveSessions(all[i], recs) {
			if ref.ID != "" {
				t.owned[refKey(ref)] = id
			}
		}
	}
	if d.TermOf == nil {
		t.unverified = "the terminals of the running sessions are not read"
		return t
	}
	// read records the tab session c's environment names; a read that fails
	// while c still runs leaves the owner of tabs in c's folder unknown, one
	// that fails because c exited is ignored.
	read := func(c AdoptCandidate) {
		if c.PID <= 0 {
			return
		}
		te, err := d.TermOf(c.PID)
		if err != nil {
			if d.Running == nil || d.Running(c.PID) {
				t.unread = append(t.unread, unreadTerm{c.SessionID, c.Cwd, fmt.Sprintf("the terminal of running session %s not read: %v", oneLine(c.Name), err)})
			}
			return
		}
		if te.TmuxPane == "" && te.AgtermSession != "" {
			t.agtermOf[c.SessionID] = te.AgtermSession
		}
	}
	for _, c := range cands {
		read(c)
	}
	if d.Live == nil {
		t.unverified = "the running sessions are not read"
		return t
	}
	live, err := d.Live()
	if err != nil {
		t.unverified = "the running sessions not read: " + err.Error()
		return t
	}
	for id, l := range live {
		if _, ok := candidateByID(cands, id); ok {
			continue
		}
		o := AdoptCandidate{SessionID: id, PID: l.PID, Cwd: realPath(l.Cwd), Name: l.Name}
		read(o)
		if _, ok := t.agtermOf[id]; ok {
			t.others = append(t.others, o)
		}
	}
	return t
}

// unreadOwner says why a tab in folder cwd, whose claude names no session,
// has no verifiable owner for candidate c; "" when it has one.
func (t *adoptTerminal) unreadOwner(c AdoptCandidate) string {
	if t.unverified != "" {
		return t.unverified
	}
	for _, u := range t.unread {
		if u.id != c.SessionID && u.cwd == c.Cwd {
			return u.why
		}
	}
	return ""
}

// ownedElsewhere says why terminal session ref is not candidate c's to
// take: a project records it for another session. "" when it is free.
func (t *adoptTerminal) ownedElsewhere(ref launch.SessionRef, c AdoptCandidate) string {
	if owner, ok := t.owned[refKey(ref)]; ok && owner != c.SessionID {
		return "is recorded for another session"
	}
	return ""
}

func (t *adoptTerminal) termOf(c AdoptCandidate) claudecode.TermEnv {
	if t.d.TermOf == nil || c.PID <= 0 {
		return claudecode.TermEnv{}
	}
	te, err := t.d.TermOf(c.PID)
	if err != nil {
		t.notes = append(t.notes, fmt.Sprintf("%s's terminal not read: %v", c.Name, err))
		return claudecode.TermEnv{}
	}
	return te
}

// workspaces reads agterm's tree once; nil with a note when it fails.
func (t *adoptTerminal) workspaces(ctx context.Context) []launch.Workspace {
	if !t.wssRead && t.d.Workspaces != nil {
		t.wssRead = true
		wss, err := t.d.Workspaces(ctx)
		if err != nil {
			t.notes = append(t.notes, "agterm not read: "+err.Error())
		}
		t.wss = wss
	}
	return t.wss
}

func (t *adoptTerminal) coordinator(ctx context.Context, root string, all []Project, c AdoptCandidate) error {
	te := t.termOf(c)
	if te.TmuxPane != "" && t.d.PaneOf != nil {
		sess, wid, wname, err := t.d.PaneOf(ctx, te.TmuxPane)
		if err == nil {
			t.workspace, t.launcher = sess, "tmux"
			ref := launch.SessionRef{Launcher: "tmux", ID: wid, Name: wname, Adopted: true}
			if why := t.ownedElsewhere(ref, c); why != "" {
				t.notes = append(t.notes, fmt.Sprintf("%s: its tmux window %s %s: no terminal session recorded; qm down leaves it open", c.Name, wid, why))
			} else if t.alonePane(ctx, wid, c.Name) && t.paneRuns(ctx, wid, c, c.Name) {
				t.coordRef = ref
			}
		} else {
			t.notes = append(t.notes, fmt.Sprintf("%s's tmux pane %s not found: %v", c.Name, te.TmuxPane, err))
		}
	}
	if t.launcher == "" && te.TmuxPane == "" && te.AgtermSession != "" {
		wss := t.workspaces(ctx)
		if w, ok := launch.FindSession(wss, te.AgtermSession); ok {
			n := 0
			for _, o := range wss {
				if o.Name == w.Name {
					n++
				}
			}
			if n > 1 {
				return fmt.Errorf("%w: agterm has %d workspaces named %s: rename the workspace %s, then run qm up again", ErrWorkspaceName, n, w.Name, w.Name)
			}
			if ref, ok := t.agtermRef(w, te.AgtermSession, c, c.Name); ok {
				t.workspace, t.workspaceID, t.launcher = w.Name, w.ID, "agterm"
				t.coordRef = ref
			}
		}
	}
	taken := func(name string) bool {
		return slices.ContainsFunc(all, func(o Project) bool { return o.Root != root && o.Workspace == name })
	}
	if t.launcher == "" {
		t.workspace = WorkspaceName(root, taken)
		return nil
	}
	if taken(t.workspace) {
		return fmt.Errorf("%w: another qm up project uses %s: rename the workspace %s, then run qm up again", ErrWorkspaceName, t.workspace, t.workspace)
	}
	return nil
}

// agtermRef is the ref of agterm session id as workspace w lists it, for
// candidate c (named name), on a positive match only (adopt design 4.4):
// the tab's foreground is a claude command that names c's session id, or
// names none while the tab's folder is c's and no other running session
// (candidate or not) whose environment names the tab matches it too, and no
// project records the tab for another session. Otherwise (an idle shell,
// another program, another session, or an AGTERM_SESSION_ID inherited, say,
// by an editor started in that tab) it is false, with a note.
func (t *adoptTerminal) agtermRef(w launch.Workspace, id string, c AdoptCandidate, name string) (launch.SessionRef, bool) {
	i := slices.IndexFunc(w.Sessions, func(s launch.SessionRef) bool { return s.ID == id })
	if i < 0 {
		return launch.SessionRef{}, false
	}
	tab := w.Sessions[i]
	refuse := func(why string) (launch.SessionRef, bool) {
		t.notes = append(t.notes, fmt.Sprintf("%s: its agterm tab %s %s: no terminal session recorded; qm down leaves it open", name, id, why))
		return launch.SessionRef{}, false
	}
	if why := t.ownedElsewhere(launch.SessionRef{Launcher: "agterm", ID: tab.ID}, c); why != "" {
		return refuse(why)
	}
	switch byID, ok := tabRuns(tab, c); {
	case tab.Idle:
		return refuse("is an idle shell, so not its own")
	case !ok:
		return refuse("does not run it (its foreground is not this claude session)")
	case !byID && t.unreadOwner(c) != "":
		return refuse("runs a claude that names no session, and whose it is cannot be verified (" + t.unreadOwner(c) + ")")
	case !byID:
		for _, o := range slices.Concat(t.cands, t.others) {
			if o.SessionID == c.SessionID || t.agtermOf[o.SessionID] != id {
				continue
			}
			if _, ok := tabRuns(tab, o); ok {
				return refuse("is named by another running session too, and its claude names no session")
			}
		}
	}
	return launch.SessionRef{Launcher: "agterm", ID: tab.ID, Name: tab.Name, Adopted: true}, true
}

// tabRuns says whether agterm tab runs candidate c: its foreground is a
// claude command that names c's session id (byID), or names no session id
// while the tab's folder is c's.
func tabRuns(tab launch.SessionRef, c AdoptCandidate) (byID, ok bool) {
	args, isClaude := claudecode.IsClaude(tab.Foreground)
	if !isClaude {
		return false, false
	}
	if sid, _ := claudecode.SessionOfArgs(args); sid != "" {
		return true, sid == c.SessionID
	}
	return false, tab.Cwd != "" && realPath(tab.Cwd) == c.Cwd
}

// paneRuns says whether the pane of tmux window wid runs candidate c's
// process in its foreground, so the window is c's own; else (or unread) a
// note says why no terminal session is recorded for session name.
func (t *adoptTerminal) paneRuns(ctx context.Context, wid string, c AdoptCandidate, name string) bool {
	if t.d.PaneRuns == nil || c.PID <= 0 {
		t.notes = append(t.notes, fmt.Sprintf("%s: the foreground of tmux window %s not read: no terminal session recorded; qm down leaves it open", name, wid))
		return false
	}
	runs, err := t.d.PaneRuns(ctx, wid, c.PID)
	switch {
	case err != nil:
		t.notes = append(t.notes, fmt.Sprintf("%s: the foreground of tmux window %s not read (%v): no terminal session recorded; qm down leaves it open", name, wid, err))
		t.failed = true
		return false
	case !runs:
		t.notes = append(t.notes, fmt.Sprintf("%s: its tmux window %s does not run it in its foreground: no terminal session recorded; qm down leaves it open", name, wid))
		return false
	}
	return true
}

// alonePane says whether tmux window wid holds one pane only, so closing it
// closes session name's pane alone; else (or unread) a note says why no
// terminal session is recorded.
func (t *adoptTerminal) alonePane(ctx context.Context, wid, name string) bool {
	if t.d.WindowPanes == nil {
		t.notes = append(t.notes, fmt.Sprintf("%s: the panes of tmux window %s not read: no terminal session recorded; qm down leaves it open", name, wid))
		return false
	}
	n, err := t.d.WindowPanes(ctx, wid)
	switch {
	case err != nil:
		t.notes = append(t.notes, fmt.Sprintf("%s: the panes of tmux window %s not read (%v): no terminal session recorded; qm down leaves it open", name, wid, err))
		t.failed = true
		return false
	case n != 1:
		t.notes = append(t.notes, fmt.Sprintf("%s: its tmux window %s has %d panes: no terminal session recorded; qm down leaves it open", name, wid, n))
		return false
	}
	return true
}

// worker is the recorded ref of worker c (named name), and its move into
// the project's agterm workspace when it is in another one.
func (t *adoptTerminal) worker(ctx context.Context, c AdoptCandidate, name string) (launch.SessionRef, *AdoptMove) {
	if t.launcher == "" {
		return launch.SessionRef{}, nil
	}
	te := t.termOf(c)
	switch t.launcher {
	case "agterm":
		if te.TmuxPane != "" || te.AgtermSession == "" {
			return launch.SessionRef{}, nil
		}
		w, ok := launch.FindSession(t.workspaces(ctx), te.AgtermSession)
		if !ok {
			return launch.SessionRef{}, nil
		}
		ref, ok := t.agtermRef(w, te.AgtermSession, c, name)
		if !ok {
			return launch.SessionRef{}, nil
		}
		if w.ID != t.workspaceID {
			return ref, &AdoptMove{Name: name, SessionID: te.AgtermSession, From: w.Name, To: t.workspace, ToID: t.workspaceID}
		}
		return ref, nil
	case "tmux":
		if te.TmuxPane == "" || t.d.PaneOf == nil {
			return launch.SessionRef{}, nil
		}
		sess, wid, wname, err := t.d.PaneOf(ctx, te.TmuxPane)
		if err != nil || sess != t.workspace {
			return launch.SessionRef{}, nil
		}
		ref := launch.SessionRef{Launcher: "tmux", ID: wid, Name: wname, Adopted: true}
		if why := t.ownedElsewhere(ref, c); why != "" {
			t.notes = append(t.notes, fmt.Sprintf("%s: its tmux window %s %s: no terminal session recorded; qm down leaves it open", name, wid, why))
			return launch.SessionRef{}, nil
		}
		if !t.alonePane(ctx, wid, name) || !t.paneRuns(ctx, wid, c, name) {
			return launch.SessionRef{}, nil
		}
		return ref, nil
	}
	return launch.SessionRef{}, nil
}

// RenderAdopt prints the planned adoption: one line per adopted session,
// per move and per session left alone, then the notes.
func RenderAdopt(ap AdoptPlan) string {
	var b strings.Builder
	p := ap.Project
	launcher := p.Launcher
	if launcher == "" {
		launcher = "none (no terminal session recorded)"
	}
	fmt.Fprintf(&b, "project %s (%s), workspace %s, launcher %s\n", p.ID, p.Root, p.Workspace, launcher)
	if c := ap.Coordinator; c.SessionID != "" {
		fmt.Fprintf(&b, "adopt coordinator %s (%s) in %s, %s, resumes with: %s%s\n",
			p.Coordinator.Name, prefix(c.SessionID), c.Cwd, modelEffort(c.Kept), flagsText(p.ClaudeArgs), droppedText(c.Dropped))
	}
	for _, w := range ap.Workers {
		a, ok := p.Adopted[w.SessionID]
		if !ok {
			continue
		}
		if w.Worker {
			fmt.Fprintf(&b, "adopt worker %s (%s) in %s, qm worker, %s, resumes with: its launch profile\n",
				w.Name, prefix(w.SessionID), w.Cwd, modelEffort(w.Kept))
			continue
		}
		fmt.Fprintf(&b, "adopt worker %s (%s) in %s, hand-started, %s, resumes with: %s%s\n",
			a.Name, prefix(w.SessionID), w.Cwd, modelEffort(w.Kept), flagsText(w.Kept), droppedText(w.Dropped))
	}
	for _, m := range ap.Moves {
		fmt.Fprintf(&b, "move %s (agterm %s) from workspace %s to %s\n", m.Name, m.SessionID, m.From, m.To)
	}
	for _, l := range ap.Left {
		if l.Subfolder {
			fmt.Fprintf(&b, "not adopted: %s (%s), %s in %s\n", oneLine(l.Name), prefix(l.SessionID), subfolderNote, l.Cwd)
			continue
		}
		fmt.Fprintf(&b, "not adopted: %s (%s)\n", oneLine(l.Name), prefix(l.SessionID))
	}
	for _, n := range ap.Notes {
		fmt.Fprintf(&b, "note: %s\n", n)
	}
	return b.String()
}

func modelEffort(kept []string) string {
	return "model " + cmp.Or(flagValue(kept, "--model"), "default") + ", effort " + cmp.Or(flagValue(kept, "--effort"), "default")
}

func flagsText(flags []string) string {
	if len(flags) == 0 {
		return "no flags"
	}
	return strings.Join(flags, " ")
}

func droppedText(dropped []string) string {
	if len(dropped) == 0 {
		return ""
	}
	return " (dropped: " + strings.Join(dropped, " ") + ")"
}

// Adopt carries out ap (adopt design 4.2, 4.6). It reads the live sessions
// again: a worker that exited, or whose id another process now holds, is
// dropped with a note; such a coordinator is ErrCoordinatorGone, with
// nothing moved, pinned, saved or logged. Then it saves the project (a
// failure moves and pins nothing), moves (a failure leaves that worker with
// no terminal session, saved again), pins (a failure is a note) and logs one
// kind "up" Record per adopted session, action "adopt". ap is updated in
// place.
func Adopt(ctx context.Context, ap *AdoptPlan, d AdoptDeps) error {
	unlock, err := AdoptLock()
	if err != nil {
		return fmt.Errorf("nothing adopted: %w", err)
	}
	saved, err := ap.claim(ctx, d)
	unlock()
	if err != nil || !saved {
		return err
	}
	dropped := false
	for _, m := range slices.Clone(ap.Moves) {
		if d.Move == nil {
			continue
		}
		if err := d.Move(ctx, m.SessionID, m.ToID); err != nil {
			dropped = true
			for id, ref := range ap.Project.Sessions {
				if ref.Launcher == "agterm" && ref.ID == m.SessionID {
					ap.unref(id)
				}
			}
			ap.Notes = append(ap.Notes, fmt.Sprintf("%s not moved into workspace %s (%v): adopted with no terminal session; qm down leaves it open", m.Name, m.To, err))
		}
	}
	for _, id := range ap.Pins {
		if d.Pin == nil {
			break
		}
		if err := d.Pin(ctx, id); err != nil {
			ap.Notes = append(ap.Notes, fmt.Sprintf("agterm session %s not pinned (%v): an agterm restart may re-run its command", id, err))
		}
	}
	if dropped {
		if err := ap.save(); err != nil {
			return fmt.Errorf("%w (the state saved before the moves still records the tabs that were not moved)", err)
		}
	}
	return nil
}

// claim does the part of an adoption that runs under the adoption lock: it
// drops the workers that stopped or that another project took since the
// plan (a first run whose coordinator either happened to fails), writes the
// adoption Records and saves the state. saved is false when nothing was.
func (ap *AdoptPlan) claim(ctx context.Context, d AdoptDeps) (saved bool, err error) {
	var live map[string]claudecode.LiveSession
	if d.Live != nil {
		var err error
		if live, err = d.Live(); err != nil {
			return false, fmt.Errorf("running sessions not read, nothing adopted: %w", err)
		}
		// The same process must still hold the session: an id live under
		// another pid exited and was resumed elsewhere, so the tab read for
		// it is no longer its own.
		same := func(c AdoptCandidate) bool {
			l, ok := live[c.SessionID]
			return ok && (c.PID <= 0 || l.PID == c.PID)
		}
		if !ap.Later && !same(ap.Coordinator) {
			return false, ErrCoordinatorGone
		}
		var kept []AdoptCandidate
		for _, w := range ap.Workers {
			if same(w) {
				kept = append(kept, w)
				continue
			}
			ap.forget(w, "exited before the confirmation: not adopted")
		}
		ap.Workers = kept
	}
	ap.recheckTabs(ctx, d)
	if err := ap.recheckHeld(d, live); err != nil {
		return false, err
	}
	if ap.Later && len(ap.Workers) == 0 {
		return false, nil // every worker stopped or was taken: nothing to record
	}
	// The adoption Records are written before the state: a log that cannot
	// be written leaves nothing saved and no tab touched, so a retry still
	// finds every session to adopt. (A Record is an up/adopt one, not a
	// worker Record: one left by a later failed save holds nothing.)
	if err := ap.logRecords(d); err != nil {
		return false, err
	}
	// The planned state is saved next, before any tab moves: a Save that
	// fails leaves the user's tabs where they were.
	if err := ap.save(); err != nil {
		return false, err
	}
	return true, nil
}

// recheckHeld drops from the plan the sessions another project holds now,
// read again from the saved projects and the decision log under the
// adoption lock: a first run whose coordinator is taken fails, a worker that
// is taken is left with a note. A held id holds every other id of its live
// process too (withAliases, live read under the lock): a worker that is the
// project's own coordinator under another id is left as well.
func (ap *AdoptPlan) recheckHeld(d AdoptDeps, live map[string]claudecode.LiveSession) error {
	all, err := All()
	if err != nil {
		return fmt.Errorf("nothing adopted: %w", err)
	}
	recs, err := adoptRecords(d)
	if err != nil {
		return err
	}
	heldBy := map[string]map[string]bool{}
	for i := range all {
		held := map[string]bool{}
		if all[i].ID == ap.Project.ID {
			held = Held(all[i], recs)
		} else {
			all[i].addHeld(held, recs)
			delete(held, "")
		}
		heldBy[all[i].ID] = withAliases(held, live)
	}
	holder := func(id string) string {
		for i := range all {
			if all[i].ID != ap.Project.ID && heldBy[all[i].ID][id] {
				return all[i].ID
			}
		}
		return ""
	}
	if !ap.Later {
		if h := holder(ap.Coordinator.SessionID); h != "" {
			return fmt.Errorf("coordinator %s not adopted: now held by project %s; nothing adopted", ap.Project.Coordinator.Name, h)
		}
		// The workspace, checked at the plan, may have been taken since.
		for i := range all {
			if o := &all[i]; o.ID != ap.Project.ID && o.Root != ap.Project.Root && ap.Project.Workspace != "" && o.Workspace == ap.Project.Workspace {
				return fmt.Errorf("%w: the workspace %s is now used by project %s: rename the workspace %s, then run qm up again; nothing adopted", ErrWorkspaceName, ap.Project.Workspace, o.ID, ap.Project.Workspace)
			}
		}
	}
	// A terminal session a project recorded since the plan (in its state, or
	// in a worker Record Gather reads it from) stays its: another project's,
	// or this project's for another session.
	type recorded struct{ project, session string }
	owner := map[string]recorded{}
	for i := range all {
		for sid, ref := range effectiveSessions(all[i], recs) {
			if ref.ID != "" {
				owner[refKey(ref)] = recorded{all[i].ID, sid}
			}
		}
	}
	for id, ref := range maps.Clone(ap.Project.Sessions) {
		if o, ok := owner[refKey(ref)]; ok && (o.project != ap.Project.ID || o.session != id) {
			ap.unref(id)
			ap.Notes = append(ap.Notes, fmt.Sprintf("%s: its terminal session %s is now recorded by project %s: no terminal session recorded; qm down leaves it open", ap.sessionName(id), ref.ID, o.project))
		}
	}
	var kept []AdoptCandidate
	for _, w := range ap.Workers {
		if h := holder(w.SessionID); h != "" {
			ap.forget(w, "not adopted: now held by project "+h)
			continue
		}
		if ap.Later && heldBy[ap.Project.ID][w.SessionID] {
			ap.forget(w, "not adopted: its process holds a session the project already has (its coordinator after /clear, say)")
			continue
		}
		kept = append(kept, w)
	}
	ap.Workers = kept
	return nil
}

// recheckTabs repeats, after the confirmation, the checks that gave each
// planned terminal session to its session. An agterm tab: still listed in
// the workspace it was planned in (a planned move's source, else the
// project's workspace), its foreground still that session (tabRuns), and a
// planned move's destination still the one workspace of that name and id. A tmux
// window: still one pane, which runs the session's process in its
// foreground. A terminal session that fails, or every agterm tab when
// agterm cannot be read again, is not recorded: its session is adopted with
// no terminal session, and a tab is neither moved nor pinned.
func (ap *AdoptPlan) recheckTabs(ctx context.Context, d AdoptDeps) {
	var ids, panes []string
	for id, ref := range ap.Project.Sessions {
		switch {
		case ref.ID == "":
		case ref.Launcher == "agterm":
			ids = append(ids, id)
		case ref.Launcher == "tmux":
			panes = append(panes, id)
		}
	}
	slices.Sort(panes)
	for _, id := range panes {
		ref := ap.Project.Sessions[id]
		c, _ := ap.candidate(id)
		t := adoptTerminal{d: d}
		if !t.alonePane(ctx, ref.ID, ap.sessionName(id)) || !t.paneRuns(ctx, ref.ID, c, ap.sessionName(id)) {
			// A probe that failed (ps or tmux) says nothing about the window:
			// no ref is recorded, but the session is not marked Dropped, so
			// a later run may take the window again.
			ap.release(id, !t.failed)
			ap.Notes = append(ap.Notes, t.notes...)
		}
	}
	if len(ids) == 0 {
		return
	}
	slices.Sort(ids)
	var wss []launch.Workspace
	err := errors.New("agterm is not read")
	if d.Workspaces != nil {
		wss, err = d.Workspaces(ctx)
	}
	for _, id := range ids {
		ref := ap.Project.Sessions[id]
		c, known := ap.candidate(id)
		planned := ap.Project.Workspace
		var move *AdoptMove
		for _, m := range ap.Moves {
			if m.SessionID == ref.ID {
				planned, move = m.From, &m
			}
		}
		why := ""
		switch w, listed := launch.FindSession(wss, ref.ID); {
		case err != nil:
			why = "could not be checked again after the confirmation (" + err.Error() + ")"
		case !listed:
			why = "is no longer listed"
		case w.Name != planned:
			why = "is now in workspace " + w.Name
		case move != nil && !resolvesTo(wss, move.To, move.ToID):
			why = "cannot be moved: workspace " + move.To + " no longer resolves to the planned workspace"
		case !known:
			why = "has no planned session to check it against"
		default:
			tab := w.Sessions[slices.IndexFunc(w.Sessions, func(s launch.SessionRef) bool { return s.ID == ref.ID })]
			if _, runs := tabRuns(tab, c); tab.Idle || !runs {
				why = "no longer runs it (its foreground changed since the plan)"
			}
		}
		if why != "" {
			ap.unref(id)
			ap.Notes = append(ap.Notes, fmt.Sprintf("%s: its agterm tab %s %s: no terminal session recorded; qm down leaves it open", ap.sessionName(id), ref.ID, why))
		}
	}
}

// resolvesTo says whether name is, in wss, the name of exactly one workspace,
// the one with id.
func resolvesTo(wss []launch.Workspace, name, id string) bool {
	n, ok := 0, false
	for _, w := range wss {
		if w.Name == name {
			n++
			ok = w.ID == id
		}
	}
	return n == 1 && ok
}

// candidate is the planned coordinator or worker with session id.
func (ap *AdoptPlan) candidate(id string) (AdoptCandidate, bool) {
	if !ap.Later && ap.Coordinator.SessionID == id {
		return ap.Coordinator, true
	}
	return candidateByID(ap.Workers, id)
}

// sessionName is the name session id is adopted under in the plan.
func (ap *AdoptPlan) sessionName(id string) string {
	if id == ap.Project.Coordinator.ID {
		return ap.Project.Coordinator.Name
	}
	if w, ok := candidateByID(ap.Workers, id); ok {
		return adoptedName(ap, w)
	}
	return id
}

// forget drops worker w from the plan, with note why after its name.
func (ap *AdoptPlan) forget(w AdoptCandidate, why string) {
	ap.Notes = append(ap.Notes, adoptedName(ap, w)+" "+why)
	ap.unref(w.SessionID)
	delete(ap.Project.Adopted, w.SessionID)
	delete(ap.Project.Dropped, w.SessionID)
	if len(ap.Project.Adopted) == 0 {
		ap.Project.Adopted = nil
	}
}

// logRecords writes the plan's adoption Records to the decision log: the
// coordinator's on a first run, then each worker's. The first that fails
// fails the adoption.
func (ap *AdoptPlan) logRecords(d AdoptDeps) error {
	if d.Log == nil {
		return nil
	}
	now := ap.Project.Created
	if d.Now != nil {
		now = d.Now()
	}
	log := func(c AdoptCandidate, name, detail string) error {
		r := route.Record{
			Time: now, Kind: "up", Project: ap.Project.Root, UpProject: ap.Project.ID, Action: "adopt",
			ClaudeSession: c.SessionID, Name: name, Cwd: c.Cwd, Branch: c.Branch, Detail: detail,
		}
		if err := d.Log(r); err != nil {
			return fmt.Errorf("qm up Record of %s not logged, nothing adopted: %w", name, err)
		}
		return nil
	}
	if !ap.Later {
		if err := log(ap.Coordinator, ap.Project.Coordinator.Name, "coordinator"); err != nil {
			return err
		}
	}
	for _, w := range ap.Workers {
		name, detail := adoptedName(ap, w), "worker (hand-started)"
		if w.Worker {
			name, detail = w.Name, "worker (qm worker)"
		}
		if err := log(w, name, detail); err != nil {
			return err
		}
	}
	return nil
}

// save writes the plan's state: a first run's project as it is; a later
// run's new workers merged into the project's saved state (loaded again, so
// nothing else in it is rebuilt), the sessions whose tab was not kept without
// one. The caller holds the project's lock.
func (ap *AdoptPlan) save() error {
	if !ap.Later {
		return Save(ap.Project)
	}
	cur, ok, err := LoadID(ap.Project.ID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("save project: %s has no state", ap.Project.ID)
	}
	for id, a := range ap.Project.Adopted {
		if cur.Adopted == nil {
			cur.Adopted = map[string]AdoptedSession{}
		}
		cur.Adopted[id] = a
		if ref, ok := ap.Project.Sessions[id]; ok {
			if cur.Sessions == nil {
				cur.Sessions = map[string]launch.SessionRef{}
			}
			cur.Sessions[id] = ref
			delete(cur.Dropped, id)
		} else {
			delete(cur.Sessions, id)
			if ap.Project.Dropped[id] {
				cur.drop(id)
			}
		}
	}
	return Save(cur)
}

// adoptedName is the name worker w is adopted under.
func adoptedName(ap *AdoptPlan, w AdoptCandidate) string {
	if a, ok := ap.Project.Adopted[w.SessionID]; ok && a.Name != "" {
		return a.Name
	}
	return w.Name
}

// unref forgets session id's terminal session: its ref, pin and move, and
// marks it Dropped.
func (ap *AdoptPlan) unref(id string) { ap.release(id, true) }

// release is unref that marks the session Dropped only when drop is set.
func (ap *AdoptPlan) release(id string, drop bool) {
	ref, ok := ap.Project.Sessions[id]
	if !ok {
		return
	}
	delete(ap.Project.Sessions, id)
	if drop {
		ap.Project.drop(id)
	}
	if ref.Launcher != "agterm" {
		return
	}
	ap.Pins = slices.DeleteFunc(ap.Pins, func(p string) bool { return p == ref.ID })
	ap.Moves = slices.DeleteFunc(ap.Moves, func(m AdoptMove) bool { return m.SessionID == ref.ID })
}
