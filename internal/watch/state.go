// Package watch gives each unfinished worker of a qm up project a state
// (gone, stopped, silent, working) and waits for one to stop (qm watch).
package watch

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
	"github.com/IZZY-Agency/quartermaster/internal/route"
	"github.com/IZZY-Agency/quartermaster/internal/up"
)

// State is what a worker is doing, as qm can tell from outside.
type State string

// The worker states, in the order they are decided.
const (
	Gone    State = "gone"
	Stopped State = "stopped"
	Silent  State = "silent"
	Working State = "working"
	// Unknown: the process list could not be read.
	Unknown State = "unknown"
)

const (
	// DefaultSilent is the silence threshold when the config sets none.
	DefaultSilent = 10 * time.Minute
	// Grace is how long after its spawn a worker without a live process
	// counts as starting (working), not gone.
	Grace = 2 * time.Minute
	// startSlack is how far a turn record may predate its process's start,
	// ps's start time having whole seconds.
	startSlack = 2 * time.Second
)

// Turn is a session's turn as its turn hooks recorded it; Known is false
// when no turn hook ran (no plugin, or an older one). PID is the claude
// process that recorded it.
type Turn struct {
	Known bool
	Open  bool
	At    time.Time
	PID   int
}

// Machine is what rows are decided from. Detect is the process list's
// error: non-nil makes every state Unknown. Live is the live sessions
// (claudecode.Live), Starts the claude processes' start times by pid. Turn
// reads a session's turn, Transcript a session's transcript path ("" when
// missing), Activity a transcript's newest write (zero when unknown),
// Context a transcript's context size (0 when unknown).
type Machine struct {
	Now    time.Time
	Detect error
	Silent time.Duration
	Live   map[string]claudecode.LiveSession
	// Files holds the live sessions a PID file names (session id to pid),
	// the others being known from a process's arguments only. Reserved
	// holds the session ids of the project's coordinators, whose processes
	// no worker is matched to.
	Files      map[string]int
	Reserved   []string
	Starts     map[int]time.Time
	Turn       func(sessionID string) Turn
	Transcript func(sessionID string) string
	Activity   func(transcript string) time.Time
	Context    func(transcript string) int
}

// Row is one worker's line of the workers table.
type Row struct {
	Name string `json:"name"`
	// SessionID is the worker's session as qm recorded it; Current the
	// session its process runs now (another after /clear or a compaction).
	SessionID string `json:"session_id"`
	Current   string `json:"current_session,omitempty"`
	State     State  `json:"state"`
	// Since is the time the SINCE column shows; HasSince is false for gone
	// and unknown.
	Since    time.Duration `json:"since_ns"`
	HasSince bool          `json:"has_since"`
	// Activity is the newest write to the session's transcripts, or its
	// start when there is none; StopAt is when the turn ended (stopped).
	Activity time.Time `json:"activity"`
	StopAt   time.Time `json:"stop_at,omitzero"`
	Context  int       `json:"context,omitempty"`
	Model    string    `json:"model,omitempty"`
	Folder   string    `json:"folder,omitempty"`
	Handoff  string    `json:"handoff,omitempty"`
}

// Workers is the unfinished workers of project p (design 3.1): the workers
// Gather lists (it already leaves out replaced sessions), minus the ones
// marked done. It reads no process list and no transcript: handoffDir
// (<state>/handoff, "" for none) is the only thing it looks at.
func Workers(p up.Project, recs []route.Record, handoffDir string) ([]up.Candidate, error) {
	p.Sessions = maps.Clone(p.Sessions) // Gather seeds it; p is not saved
	cands, err := up.Gather(&p, recs, up.Env{HandoffDir: handoffDir})
	if err != nil {
		return nil, err
	}
	var out []up.Candidate
	for _, c := range cands {
		if _, done := p.Done[c.SessionID]; c.Role != up.RoleWorker || done {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// proc is a live process and the session id it runs now.
type proc struct {
	id  string
	pid int
}

// Rows decides each worker's state and row; root is the project's folder.
func Rows(root string, cands []up.Candidate, m Machine) []Row {
	follow := folderMatches(cands, m)
	out := make([]Row, 0, len(cands))
	for _, c := range cands {
		out = append(out, rowOf(root, c, m, follow))
	}
	return out
}

// folderMatches pairs the hand-started workers (adopted, no decision record,
// so no launch id in their arguments) whose recorded session no process runs
// with the live session in their folder, by candidate session id. A match
// needs exactly one such worker and exactly one free process in a folder.
// The processes that run a worker's recorded session, and the coordinators'
// (m.Reserved), are not free; a worker with a decision record is never
// matched: its launch id is on its process's command line, so another
// process in its folder is not its.
func folderMatches(cands []up.Candidate, m Machine) map[string]proc {
	taken := map[int]bool{}
	for _, c := range cands {
		if ls, ok := m.Live[c.SessionID]; ok {
			taken[ls.PID] = true
		}
	}
	for _, id := range m.Reserved {
		if ls, ok := m.Live[id]; ok {
			taken[ls.PID] = true
		}
	}
	seeking := map[string][]string{} // folder to candidate session ids
	for _, c := range cands {
		if _, live := m.Live[c.SessionID]; !live && c.Record == nil && c.Cwd != "" {
			seeking[c.Cwd] = append(seeking[c.Cwd], c.SessionID)
		}
	}
	free := map[string]map[int]string{} // folder to pid to its session id
	for sid, o := range m.Live {
		if o.Cwd == "" || taken[o.PID] {
			continue
		}
		if free[o.Cwd] == nil {
			free[o.Cwd] = map[int]string{}
		}
		if old, ok := free[o.Cwd][o.PID]; !ok || old > sid {
			free[o.Cwd][o.PID] = sid
		}
	}
	out := map[string]proc{}
	for dir, ids := range seeking {
		if len(ids) != 1 || len(free[dir]) != 1 {
			continue
		}
		for pid, sid := range free[dir] {
			out[ids[0]] = proc{sid, pid}
		}
	}
	return out
}

// current is the session worker c's process runs now and that process:
// the recorded session's process, under the id its PID file names (a new
// one after /clear or a compaction, even when c already records it); else,
// for a hand-started worker, the folder match. ok is false when no process
// runs it.
func current(c up.Candidate, m Machine, follow map[string]proc) (id string, pid int, ok bool) {
	if ls, found := m.Live[c.SessionID]; found {
		id, pid = c.SessionID, ls.PID
		if m.Files[id] == pid {
			return id, pid, true
		}
		for sid, o := range m.Live {
			if o.PID == pid && sid != c.SessionID && m.Files[sid] == pid {
				id = sid
			}
		}
		return id, pid, true
	}
	if f, found := follow[c.SessionID]; found {
		return f.id, f.pid, true
	}
	return "", 0, false
}

func rowOf(root string, c up.Candidate, m Machine, follow map[string]proc) Row {
	r := Row{
		Name:      c.Name,
		SessionID: c.SessionID,
		Model:     model(c),
		Folder:    folder(root, c.Cwd),
		Handoff:   c.Handoff,
		Activity:  c.Started,
	}
	id, pid, live := current(c, m, follow)
	if !live {
		id = c.SessionID
	}
	r.Current = id
	var path string
	if m.Transcript != nil {
		path = m.Transcript(id)
	}
	if path != "" && m.Activity != nil {
		if at := m.Activity(path); at.After(r.Activity) {
			r.Activity = at
		}
	}
	if path != "" && m.Context != nil {
		r.Context = m.Context(path)
	}
	var turn Turn
	if m.Turn != nil && live {
		turn = m.Turn(id)
	}
	// A turn another process recorded (the session was resumed since, or
	// the pid was reused) says nothing about this one.
	if turn.Known && turn.PID != pid {
		turn = Turn{}
	}
	if start, ok := m.Starts[pid]; ok && turn.Known && turn.At.Before(start.Add(-startSlack)) {
		turn = Turn{}
	}
	switch {
	case m.Detect != nil:
		r.State = Unknown
	case !live && m.Now.Sub(c.Started) >= Grace:
		r.State = Gone
	case live && turn.Known && !turn.Open:
		r.State = Stopped
		r.StopAt = turn.At
		r.Since, r.HasSince = m.Now.Sub(turn.At), true
	default:
		// Working, silent, or a worker just spawned whose process is not
		// visible yet (within Grace).
		r.Since, r.HasSince = m.Now.Sub(r.Activity), !r.Activity.IsZero()
		r.State = Working
		if live && r.HasSince && r.Since >= m.Silent {
			r.State = Silent
		}
	}
	if r.Since < 0 {
		r.Since = 0
	}
	return r
}

// model is the worker's model/effort from its Record; "" without one.
func model(c up.Candidate) string {
	if c.Record == nil || c.Record.Decision == nil || c.Record.Decision.Tier.Model == "" {
		return ""
	}
	t := c.Record.Decision.Tier
	if t.Effort == "" {
		return t.Model
	}
	return t.Model + "/" + t.Effort
}

// folder is dir relative to root: "." for root itself, with ".." steps
// when dir is outside it; dir as it is when it has no relative form.
func folder(root, dir string) string {
	if dir == "" {
		return ""
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return dir
	}
	return rel
}

// Activity is the newest modification time among a session's transcript and
// its subagents' transcripts, nested ones included (Claude Code keeps them
// under <dir>/<session id>/subagents/); zero when none can be read. It
// reads only modification times, never the content.
func Activity(transcript string) time.Time {
	var newest time.Time
	if fi, err := os.Stat(transcript); err == nil {
		newest = fi.ModTime()
	}
	subs := filepath.Join(strings.TrimSuffix(transcript, ".jsonl"), "subagents")
	_ = filepath.WalkDir(subs, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		if fi, err := d.Info(); err == nil && fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
		return nil
	})
	return newest
}

// SinceText is the SINCE column: 45s, 12m, 3h; "" without one.
func (r Row) SinceText() string {
	if !r.HasSince {
		return ""
	}
	return Duration(r.Since)
}

// Duration writes d as whole seconds under a minute, whole minutes under an
// hour, else whole hours.
func Duration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return fmt.Sprintf("%dh", int(d/time.Hour))
}

// ContextText is the CONTEXT column: 180k; "" when unknown.
func (r Row) ContextText() string {
	if r.Context <= 0 {
		return ""
	}
	return fmt.Sprintf("%dk", (r.Context+500)/1000)
}

// Table writes rows as the workers table, indented two spaces; labels adds
// "(label)" to a row's state, by session id. A stopped or gone row with a
// handoff note gets a second line naming it, home written as ~.
func Table(rows []Row, labels map[string]string, home string) string {
	lines := [][]string{{"NAME", "STATE", "SINCE", "CONTEXT", "MODEL", "FOLDER"}}
	for _, r := range rows {
		state := string(r.State)
		if l := labels[r.SessionID]; l != "" {
			state += " (" + l + ")"
		}
		lines = append(lines, []string{r.Name, state, r.SinceText(), r.ContextText(), r.Model, r.Folder})
	}
	width := make([]int, len(lines[0]))
	for _, l := range lines {
		for i, cell := range l {
			width[i] = max(width[i], len(cell))
		}
	}
	var b strings.Builder
	for n, l := range lines {
		row := "  "
		for i, cell := range l {
			if i < len(l)-1 {
				cell += strings.Repeat(" ", width[i]-len(cell)+2)
			}
			row += cell
		}
		b.WriteString(strings.TrimRight(row, " ") + "\n")
		if n == 0 {
			continue
		}
		if r := rows[n-1]; r.Handoff != "" && (r.State == Stopped || r.State == Gone) {
			fmt.Fprintf(&b, "    handoff note: %s\n", Tilde(r.Handoff, home))
		}
	}
	return b.String()
}

// Tilde writes path with the home folder as ~.
func Tilde(path, home string) string {
	if home == "" || home == "/" {
		return path
	}
	if path == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(path, home+"/"); ok {
		return "~/" + rest
	}
	return path
}

// PIDFileSessions is the live sessions a PID file (<claude dir>/sessions/
// <pid>.json) names, by session id to pid: the entries of live that came
// from a process's arguments alone are left out. A file whose pid is not the
// live session's, or that cannot be read, is skipped.
func PIDFileSessions(claudeDir string, live map[string]claudecode.LiveSession) map[string]int {
	out := map[string]int{}
	files, _ := filepath.Glob(filepath.Join(claudeDir, "sessions", "*.json"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var s struct {
			PID       int    `json:"pid"`
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(b, &s) != nil || s.SessionID == "" {
			continue
		}
		if ls, ok := live[s.SessionID]; ok && ls.PID == s.PID {
			out[s.SessionID] = s.PID
		}
	}
	return out
}
