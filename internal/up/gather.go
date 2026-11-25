package up

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/route"
	"github.com/IZZY-Agency/quartermaster/internal/spawn"
	"github.com/IZZY-Agency/quartermaster/internal/transcript"
)

// Role is what a session is to its project.
type Role string

// The roles of a project's sessions.
const (
	RoleCoordinator Role = "coordinator"
	RoleWorker      Role = "worker"
)

// Candidate is one session qm up considers restoring, with the facts the
// restore decision reads. Record is the worker's newest decision Record
// (nil for the coordinator). Transcript is "" when the transcript is
// missing. Branch is the branch checked out in Cwd, else the Record's.
// Finished says why a worker is finished ("" when it is not). Handoff is the
// path of the session's handoff note when one was written after Started.
type Candidate struct {
	Role       Role
	SessionID  string
	Name       string
	Cwd        string
	Branch     string
	Transcript string
	Record     *route.Record
	Last       transcript.Last
	Live       bool
	Trusted    bool
	Finished   string
	Handoff    string
	Started    time.Time
	// Instead is the session this candidate stands in for: a fresh start
	// that never ran (no transcript, no process), whose own session is
	// restored in its place. A fresh start from it replaces Instead.
	Instead string
	// Note says what could not be read about the session (an unreadable
	// transcript, its trust, its handoff note); the plan shows it.
	Note string
	// Adopted is the adopted entry of a hand-started worker (no Record):
	// its name, folder and kept claude flags (adopt design 4.3); nil for
	// every other session.
	Adopted *AdoptedSession
	// LineStarted is when the first session of this worker's line started:
	// its Record's time, followed back through the Records it replaces. A
	// pull request the line's first session opened belongs to it too.
	LineStarted time.Time
}

// lineStart is when c's line of sessions started (LineStarted, else
// Started).
func (c Candidate) lineStart() time.Time {
	if !c.LineStarted.IsZero() && c.LineStarted.Before(c.Started) {
		return c.LineStarted
	}
	return c.Started
}

// Env is what Gather reads the machine through. Live holds the live
// sessions (claudecode.Live). TranscriptPath returns "" for a missing
// transcript. Trusted reads Claude Code's folder trust. Finished says why a
// worker is finished (see Finished); nil reads every worker as unfinished.
// HandoffDir is <state>/handoff. Run, when set, reads the branch checked
// out in a worker's folder; nil keeps the Record's branch. Now is when the
// decision is made: Gather does not read it, and the caller passes the same
// value to Decide.
type Env struct {
	Now            time.Time
	Live           map[string]claudecode.LiveSession
	TranscriptPath func(id string) (string, error)
	Trusted        func(dir string) (bool, error)
	Finished       func(c Candidate) string
	HandoffDir     string
	Run            DirRunner
	// Shallow reads no transcript: one that exists counts as readable, and
	// its file's time as its last activity (Last.At), with no context size.
	Shallow bool
}

// Gather lists the sessions of project p: its coordinator from the state,
// then its workers from the decision Records (kind "worker", tied to p:
// up_project p.ID or a session p adopted), the newest Record per session id,
// the reserved ones of p.Starting included, minus the ids another Record
// replaces, oldest start first, then its
// hand-started workers (adopted, no Record), oldest start first. It also
// loads into p.Sessions the terminal session each worker Record names
// (term_launcher and term_id, or the Detail of an older Record) when
// p.Sessions has none for that session, so qm down closes a worker qm spawn
// worker started and qm up pins it again. A fact that cannot be read is the
// candidate's Note, never an error: an unreadable transcript counts as
// missing.
func Gather(p *Project, recs []route.Record, env Env) ([]Candidate, error) {
	recs = p.withStarting(recs)
	seedSessions(p, recs)
	var out []Candidate
	if p.Coordinator.ID != "" {
		c := Candidate{
			Role:      RoleCoordinator,
			SessionID: p.Coordinator.ID,
			Name:      p.Coordinator.Name,
			Cwd:       p.Root,
			Started:   p.Coordinator.Started,
		}
		c.Branch = currentBranch(env.Run, c.Cwd)
		fill(&c, p.ID, env)
		// A coordinator started fresh that never ran (qm up had no launcher
		// and its printed line was not run) has no transcript and no process:
		// the newest past coordinator with a transcript is the one to
		// restore. Its start is its transcript's first timestamp, so an
		// older handoff note is not used.
		if env.TranscriptPath != nil && c.Transcript == "" && !c.Live {
			for i := len(p.PastCoordinators) - 1; i >= 0; i-- {
				id := p.PastCoordinators[i]
				path, err := env.TranscriptPath(id)
				if err != nil || path == "" {
					continue // unreadable counts as missing
				}
				var started time.Time
				if !env.Shallow {
					if started, err = transcript.FirstAt(path); err != nil {
						continue
					}
				}
				old := Candidate{Role: RoleCoordinator, SessionID: id, Name: c.Name, Cwd: c.Cwd, Branch: c.Branch, Started: started, Instead: c.SessionID}
				fill(&old, p.ID, env)
				if old.Transcript == "" {
					continue
				}
				c = old
				break
			}
		}
		out = append(out, c)
	}
	newest := newestWorkers(p, recs)
	latest := latestReplacers(p, recs)
	starts := lineStarts(p, recs)
	seen := map[string]bool{}
	for _, c := range out {
		seen[c.SessionID] = true
	}
	for _, r := range workerRecords(p, recs) {
		c := workerCandidate(r, p.ID, starts, env)
		// A session started fresh that never ran (qm up had no launcher and
		// its printed line was not run) has no transcript and no process:
		// the session it replaces, with its transcript and handoff note, is
		// the one to restore, and a fresh start from it replaces the never-run
		// one (Instead). Only the newest Record replacing a session may fall
		// back to it, so the chain reaches each old session once.
		instead := c.SessionID
		for walked := map[string]bool{c.SessionID: true}; env.TranscriptPath != nil && c.Record != nil && c.Transcript == "" && !c.Live && c.Record.Replaces != ""; {
			old, ok := newest[c.Record.Replaces]
			if !ok || walked[old.ClaudeSession] || latest[c.Record.Replaces] != c.Record {
				break
			}
			walked[old.ClaudeSession] = true
			c = workerCandidate(old, p.ID, starts, env)
			c.Instead = instead
		}
		if seen[c.SessionID] {
			continue // one candidate per session
		}
		seen[c.SessionID] = true
		out = append(out, c)
	}
	out = append(out, handStarted(p, newest, seen, env)...)
	dropOthersNotes(out)
	return out, nil
}

// Ran tells whether the session runs or has a transcript. A session that did
// not may only have been launched.
func (c Candidate) Ran() bool { return c.Live || c.Transcript != "" }

// dropOthersNotes takes the handoff note from each worker whose launch nothing
// confirmed and that never ran, when another session of its name ran or
// started later (a retry): a note is known by the worker's name, and that
// session wrote it, or may have.
func dropOthersNotes(cands []Candidate) {
	for i := range cands {
		c := &cands[i]
		if c.Handoff == "" || c.Ran() || c.Record == nil || !spawn.Unconfirmed(c.Record) {
			continue
		}
		if slices.ContainsFunc(cands, func(o Candidate) bool {
			return o.Role == RoleWorker && o.SessionID != c.SessionID && o.Name == c.Name && (o.Ran() || o.Started.After(c.Started))
		}) {
			c.Handoff = ""
			c.addNote("its launch was not confirmed, and the handoff note of its name is another session's")
		}
	}
}

// handStarted is the candidates of p's hand-started workers (adopt design
// 4.3): one per p.Adopted entry with a folder and no tied worker Record,
// built from the entry, ordered by start, then name.
func handStarted(p *Project, newest map[string]*route.Record, seen map[string]bool, env Env) []Candidate {
	var out []Candidate
	for sid, a := range p.Adopted {
		if a.Cwd == "" || newest[sid] != nil || seen[sid] {
			continue
		}
		c := Candidate{
			Role:        RoleWorker,
			SessionID:   sid,
			Name:        a.Name,
			Cwd:         a.Cwd,
			Started:     a.Started,
			LineStarted: a.Started,
		}
		c.Adopted = &a
		fill(&c, p.ID, env)
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b Candidate) int {
		return cmp.Or(a.Started.Compare(b.Started), cmp.Compare(a.Name, b.Name), cmp.Compare(a.SessionID, b.SessionID))
	})
	return out
}

// latestReplacers maps each session id to the newest worker Record tied to
// project p that replaces it.
func latestReplacers(p *Project, recs []route.Record) map[string]*route.Record {
	out := map[string]*route.Record{}
	for i := range recs {
		r := &recs[i]
		if r.Kind != "worker" || !p.Tied(r) || r.ClaudeSession == "" || r.Replaces == "" {
			continue
		}
		if old, ok := out[r.Replaces]; !ok || !r.Time.Before(old.Time) {
			out[r.Replaces] = r
		}
	}
	return out
}

// lineStarts maps each worker session tied to project p to when its line
// started: the earliest Record of the session, followed back through the
// sessions its Records replace.
func lineStarts(p *Project, recs []route.Record) map[string]time.Time {
	first := map[string]time.Time{}
	replaces := map[string]string{}
	for i := range recs {
		r := &recs[i]
		if r.Kind != "worker" || !p.Tied(r) || r.ClaudeSession == "" {
			continue
		}
		if t, ok := first[r.ClaudeSession]; !ok || r.Time.Before(t) {
			first[r.ClaudeSession] = r.Time
		}
		if r.Replaces != "" && r.Replaces != r.ClaudeSession {
			replaces[r.ClaudeSession] = r.Replaces
		}
	}
	out := make(map[string]time.Time, len(first))
	for sid, t := range first {
		start := t
		for cur, walked := sid, map[string]bool{sid: true}; ; {
			prev, ok := replaces[cur]
			if !ok || walked[prev] {
				break
			}
			walked[prev] = true
			if pt, ok := first[prev]; ok && pt.Before(start) {
				start = pt
			}
			cur = prev
		}
		out[sid] = start
	}
	return out
}

// RecordedTabs are the terminal sessions p records, by session id, its worker
// Records' included (effectiveSessions).
func RecordedTabs(p Project, recs []route.Record) map[string]launch.SessionRef {
	return effectiveSessions(p, recs)
}

// effectiveSessions are the terminal sessions p records as Gather reads
// them: p.Sessions, and for a session with none there that p never dropped,
// its newest worker Record's (seedSessions). p is not changed.
func effectiveSessions(p Project, recs []route.Record) map[string]launch.SessionRef {
	p.Sessions = maps.Clone(p.Sessions)
	seedSessions(&p, recs)
	return p.Sessions
}

// seedSessions loads into p.Sessions the terminal session of each worker
// Record of p (the newest per session id) when p.Sessions has none for it
// (a reservation's empty one counts as none, reserve)
// and the session's ref was never closed or dropped (p.Dropped): a tmux
// window id from a Record may name another window by now.
func seedSessions(p *Project, recs []route.Record) {
	for sid, r := range newestWorkers(p, recs) {
		if p.Sessions[sid].ID != "" || p.Dropped[sid] {
			continue
		}
		ref, ok := termRef(r)
		if !ok {
			continue
		}
		if p.Sessions == nil {
			p.Sessions = map[string]launch.SessionRef{}
		}
		p.Sessions[sid] = ref
	}
}

// termRef is the terminal session worker Record r names: its term_launcher
// and term_id, else, for a Record of an older qm, its Detail "LAUNCHER ID".
// A Record with no name (UsableRecords cleared it) names none: a tmux window
// is known by its id and name together.
func termRef(r *route.Record) (launch.SessionRef, bool) {
	if r.Name == "" {
		return launch.SessionRef{}, false
	}
	l, id := r.TermLauncher, r.TermID
	if l == "" && id == "" {
		if f := strings.Fields(r.Detail); len(f) == 2 && (f[0] == "agterm" || f[0] == "tmux") {
			l, id = f[0], f[1]
		}
	}
	if l == "" || id == "" {
		return launch.SessionRef{}, false
	}
	return launch.SessionRef{Launcher: l, ID: id, Name: r.Name}, true
}

// workerCandidate is the candidate of worker Record r, its facts filled.
func workerCandidate(r *route.Record, projectID string, starts map[string]time.Time, env Env) Candidate {
	c := Candidate{
		Role:        RoleWorker,
		SessionID:   r.ClaudeSession,
		Name:        r.Name,
		Cwd:         r.Cwd,
		Branch:      r.Branch,
		Record:      r,
		Started:     r.Time,
		LineStarted: starts[r.ClaudeSession],
	}
	fill(&c, projectID, env)
	return c
}

// newestWorkers is the newest worker Record of each session tied to project
// p (Tied), by session id.
func newestWorkers(p *Project, recs []route.Record) map[string]*route.Record {
	newest := map[string]*route.Record{}
	for i := range recs {
		r := &recs[i]
		if r.Kind != "worker" || !p.Tied(r) || r.ClaudeSession == "" {
			continue
		}
		if old, ok := newest[r.ClaudeSession]; !ok || !r.Time.Before(old.Time) {
			newest[r.ClaudeSession] = r
		}
	}
	return newest
}

// workerRecords returns the newest worker Record of each session tied to
// project p, without the sessions a later one of those Records replaces,
// ordered by start time, then name.
func workerRecords(p *Project, recs []route.Record) []*route.Record {
	newest := newestWorkers(p, recs)
	var replacers []*route.Record
	for i := range recs {
		r := &recs[i]
		if r.Kind == "worker" && p.Tied(r) && r.ClaudeSession != "" && r.Replaces != "" {
			replacers = append(replacers, r)
		}
	}
	// A Record replaces a session only when it is later than that session's
	// newest Record.
	replaced := map[string]bool{}
	for _, r := range replacers {
		if old, ok := newest[r.Replaces]; ok && r.Time.After(old.Time) {
			replaced[r.Replaces] = true
		}
	}
	var out []*route.Record
	for sid, r := range newest {
		if !replaced[sid] {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b *route.Record) int {
		return cmp.Or(a.Time.Compare(b.Time), cmp.Compare(a.Name, b.Name), cmp.Compare(a.ClaudeSession, b.ClaudeSession))
	})
	return out
}

// fill sets the facts of c read from the machine: live, transcript and its
// last context, folder (from the transcript when the Record names none) and
// branch, trust, finished, handoff note. What cannot be read is added to
// c.Note: an unreadable transcript counts as missing, an unreadable trust as
// not trusted, an unreadable handoff note as none.
func fill(c *Candidate, projectID string, env Env) {
	_, c.Live = env.Live[c.SessionID]
	if env.TranscriptPath != nil {
		path, err := env.TranscriptPath(c.SessionID)
		if err != nil {
			c.addNote("transcript not read, counted as missing: " + err.Error())
		} else {
			c.Transcript = path
		}
	}
	if c.Transcript != "" && env.Shallow {
		fi, err := os.Stat(c.Transcript)
		if err != nil {
			c.addNote("transcript not read, counted as missing: " + err.Error())
			c.Transcript = ""
		} else {
			c.Last.At = fi.ModTime()
		}
	} else if c.Transcript != "" {
		last, err := transcript.LastContext(c.Transcript)
		if err == nil && c.Cwd == "" {
			// The folder to resume from is the one the session started in.
			c.Cwd, err = transcript.StartCwd(c.Transcript)
		}
		if err != nil {
			c.addNote("transcript not read, counted as missing: " + err.Error())
			c.Transcript = ""
		} else {
			c.Last = last
		}
	}
	if c.Role == RoleWorker {
		if b := currentBranch(env.Run, c.Cwd); b != "" {
			c.Branch = b
		}
	}
	if c.Cwd != "" && !filepath.IsAbs(c.Cwd) {
		c.addNote(fmt.Sprintf("folder %q is not an absolute path, counted as not trusted", c.Cwd))
	} else if c.Cwd != "" && env.Trusted != nil {
		ok, err := env.Trusted(c.Cwd)
		if err != nil {
			c.addNote(fmt.Sprintf("trust of %s not read, counted as not trusted: %v", c.Cwd, err))
		}
		c.Trusted = ok && err == nil
	}
	// A live worker is left whatever its pull request says: no gh call.
	if c.Role == RoleWorker && env.Finished != nil && !c.Live {
		c.Finished = env.Finished(*c)
	}
	note, err := handoff(env.HandoffDir, projectID, c.Name, c.Started)
	if err != nil {
		c.addNote(err.Error())
	}
	c.Handoff = note
}

func (c *Candidate) addNote(s string) {
	if c.Note != "" {
		c.Note += "; "
	}
	c.Note += s
}

// handoff is the session's handoff note when it was modified after started:
// <dir>/<projectID>/<name>.md, else, as written by an older qm,
// <dir>/<name>.md; "" when neither is (a note left by an earlier session of
// the same name is never used).
func handoff(dir, projectID, name string, started time.Time) (string, error) {
	if dir == "" || !safeName(name) {
		return "", nil
	}
	var paths []string
	if safeName(projectID) {
		paths = append(paths, filepath.Join(dir, projectID, name+".md"))
	}
	paths = append(paths, filepath.Join(dir, name+".md"))
	for _, path := range paths {
		fi, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("handoff note not read: %w", err)
		}
		if !fi.IsDir() && fi.ModTime().After(started) {
			return path, nil
		}
	}
	return "", nil
}

// safeName says whether s can be one path element: not empty, ".", ".." and
// without a separator.
func safeName(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\\\x00")
}
