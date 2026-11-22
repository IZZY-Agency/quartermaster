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
// user picks from; a candidate with no such tab keeps an empty Tab.
func LabelTabs(cands []AdoptCandidate, wss []launch.Workspace, termOf func(pid int) (claudecode.TermEnv, error)) {
	for i := range cands {
		env, err := termOf(cands[i].PID)
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
// process qm up walks up from to find its own claude session.
type DetectEnv struct {
	Procs          []claudecode.Proc
	Live           map[string]claudecode.LiveSession
	Worktrees      []string
	TranscriptPath func(id string) (string, error)
	Run            DirRunner
	SelfPID        int
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
	held := heldElsewhere(ProjectID(root), all, recs)
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
	byPID := map[int]claudecode.Proc{}
	for _, p := range env.Procs {
		byPID[p.PID] = p
	}
	var det Detected
	order := map[string]int{}
	for id, l := range env.Live {
		if self != 0 && l.PID == self {
			det.SelfCwd = l.Cwd
		}
		if l.Cwd == "" || held[id] || review[id] {
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
		if path != "" {
			c.Started, _ = transcript.FirstAt(path)
			c.Last, _ = transcript.LastContext(path)
		}
		c.Branch = currentBranch(env.Run, cwd)
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
		p := &all[i]
		if p.ID == id {
			continue
		}
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
	_, _ = fmt.Fprint(out, "Workers: row numbers, a for all the other rows, q for the qm workers only, empty for none: ")
	if line, err = readAnswer(in); err != nil {
		return Choice{}, err
	}
	if all, qm := strings.EqualFold(line, "a"), strings.EqualFold(line, "q"); all || qm {
		for _, c := range cands {
			if (all || c.Worker) && !c.Subfolder && c.SessionID != ch.Coordinator {
				ch.Workers = append(ch.Workers, c.SessionID)
			}
		}
		return ch, nil
	}
	for _, f := range strings.FieldsFunc(line, func(r rune) bool { return r == ' ' || r == ',' || r == '\t' }) {
		i, err := adoptableRow(f, cands)
		if err != nil {
			return Choice{}, err
		}
		if i == row {
			return Choice{}, fmt.Errorf("%w: row %d is the coordinator", ErrInput, i+1)
		}
		if !slices.Contains(ch.Workers, cands[i].SessionID) {
			ch.Workers = append(ch.Workers, cands[i].SessionID)
		}
	}
	return ch, nil
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
	TermOf      func(pid int) (claudecode.TermEnv, error)
	Live        func() (map[string]claudecode.LiveSession, error)
	Trusted     func(dir string) (bool, error)
	Log         func(route.Record) error
	Now         func() time.Time
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
	byID := map[string]AdoptCandidate{}
	for _, c := range cands {
		byID[c.SessionID] = c
	}
	coord, ok := byID[ch.Coordinator]
	if !ok {
		return ap, fmt.Errorf("%w: coordinator %s is not a candidate", ErrInput, ch.Coordinator)
	}
	if coord.Cwd != realPath(root) {
		return ap, fmt.Errorf("%w: the coordinator %s runs in %s, not in the project root %s: run qm up there, or pick another session", ErrInput, oneLine(coord.Name), coord.Cwd, root)
	}
	ap.Coordinator = coord
	picked := map[string]bool{coord.SessionID: true}
	for _, id := range ch.Workers {
		c, ok := byID[id]
		if !ok {
			return ap, fmt.Errorf("%w: worker %s is not a candidate", ErrInput, id)
		}
		if picked[id] {
			continue
		}
		if c.Subfolder {
			return ap, fmt.Errorf("%w: worker %s %s", ErrInput, oneLine(c.Name), subfolderNote)
		}
		picked[id] = true
		ap.Workers = append(ap.Workers, c)
	}
	for _, c := range cands {
		if !picked[c.SessionID] {
			ap.Left = append(ap.Left, c)
		}
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

	t := adoptTerminal{d: d, agtermOf: map[string]string{}, cands: cands}
	// The agterm tab every candidate's environment names, picked or not: a
	// tab is a candidate's own only when its foreground matches it, and no
	// other candidate naming it matches it as well (adopt design 4.4).
	for _, c := range cands {
		if te := t.termOf(c); te.TmuxPane == "" && te.AgtermSession != "" {
			t.agtermOf[c.SessionID] = te.AgtermSession
		}
	}
	t.notes = nil
	if err := t.coordinator(ctx, root, all, coord); err != nil {
		return ap, err
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

	for _, w := range ap.Workers {
		if p.Adopted == nil {
			p.Adopted = map[string]AdoptedSession{}
		}
		name := w.Name
		if w.Worker {
			p.Adopted[w.SessionID] = AdoptedSession{}
		} else {
			if !spawn.ValidName(name) {
				name = "w-" + w.SessionID[:min(8, len(w.SessionID))]
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
		}
		ap.Notes = append(ap.Notes, trustNote(d, name, w.Cwd)...)
	}
	ap.Project = p
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
	// agtermOf is the agterm session id each candidate's environment names,
	// by Claude session id; cands are all the candidates.
	agtermOf map[string]string
	cands    []AdoptCandidate
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
			if t.alonePane(ctx, wid, c.Name) {
				t.coordRef = launch.SessionRef{Launcher: "tmux", ID: wid, Name: wname, Adopted: true}
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
// names none while the tab's folder is c's and no other candidate whose
// environment names the tab matches it too. Otherwise (an idle shell,
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
	switch byID, ok := tabRuns(tab, c); {
	case tab.Idle:
		return refuse("is an idle shell, so not its own")
	case !ok:
		return refuse("does not run it (its foreground is not this claude session)")
	case !byID:
		for _, o := range t.cands {
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
		if err != nil || sess != t.workspace || !t.alonePane(ctx, wid, name) {
			return launch.SessionRef{}, nil
		}
		return launch.SessionRef{Launcher: "tmux", ID: wid, Name: wname, Adopted: true}, nil
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
	c := ap.Coordinator
	fmt.Fprintf(&b, "adopt coordinator %s (%s) in %s, %s, resumes with: %s%s\n",
		p.Coordinator.Name, prefix(c.SessionID), c.Cwd, modelEffort(c.Kept), flagsText(p.ClaudeArgs), droppedText(c.Dropped))
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
	if d.Live != nil {
		live, err := d.Live()
		if err != nil {
			return fmt.Errorf("running sessions not read, nothing adopted: %w", err)
		}
		// The same process must still hold the session: an id live under
		// another pid exited and was resumed elsewhere, so the tab read for
		// it is no longer its own.
		same := func(c AdoptCandidate) bool {
			l, ok := live[c.SessionID]
			return ok && (c.PID <= 0 || l.PID == c.PID)
		}
		if !same(ap.Coordinator) {
			return ErrCoordinatorGone
		}
		var kept []AdoptCandidate
		for _, w := range ap.Workers {
			if same(w) {
				kept = append(kept, w)
				continue
			}
			ap.Notes = append(ap.Notes, fmt.Sprintf("%s exited before the confirmation: not adopted", adoptedName(ap, w)))
			ap.unref(w.SessionID)
			delete(ap.Project.Adopted, w.SessionID)
		}
		ap.Workers = kept
		if len(ap.Project.Adopted) == 0 {
			ap.Project.Adopted = nil
		}
	}
	// The planned state is saved first: a Save that fails leaves the user's
	// tabs where they were.
	if err := Save(ap.Project); err != nil {
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
		if err := Save(ap.Project); err != nil {
			return fmt.Errorf("%w (the state saved before the moves still records the tabs that were not moved)", err)
		}
	}
	if d.Log == nil {
		return nil
	}
	now := ap.Project.Created
	if d.Now != nil {
		now = d.Now()
	}
	log := func(c AdoptCandidate, name, detail string) {
		r := route.Record{
			Time: now, Kind: "up", Project: ap.Project.Root, UpProject: ap.Project.ID, Action: "adopt",
			ClaudeSession: c.SessionID, Name: name, Cwd: c.Cwd, Branch: c.Branch, Detail: detail,
		}
		if err := d.Log(r); err != nil {
			ap.Notes = append(ap.Notes, fmt.Sprintf("qm up Record of %s not logged: %v", name, err))
		}
	}
	log(ap.Coordinator, ap.Project.Coordinator.Name, "coordinator")
	for _, w := range ap.Workers {
		if w.Worker {
			log(w, w.Name, "worker (qm worker)")
		} else {
			log(w, adoptedName(ap, w), "worker (hand-started)")
		}
	}
	return nil
}

// adoptedName is the name worker w is adopted under.
func adoptedName(ap *AdoptPlan, w AdoptCandidate) string {
	if a, ok := ap.Project.Adopted[w.SessionID]; ok && a.Name != "" {
		return a.Name
	}
	return w.Name
}

// unref forgets session id's terminal session: its ref, pin and move.
func (ap *AdoptPlan) unref(id string) {
	ref, ok := ap.Project.Sessions[id]
	if !ok {
		return
	}
	delete(ap.Project.Sessions, id)
	if ref.Launcher != "agterm" {
		return
	}
	ap.Pins = slices.DeleteFunc(ap.Pins, func(p string) bool { return p == ref.ID })
	ap.Moves = slices.DeleteFunc(ap.Moves, func(m AdoptMove) bool { return m.SessionID == ref.ID })
}
