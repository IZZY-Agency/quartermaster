package watch

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
	"github.com/IZZY-Agency/quartermaster/internal/route"
	"github.com/IZZY-Agency/quartermaster/internal/up"
)

var t0 = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

const pid = 4242

// cand is a worker of a project at /p, started an hour before t0.
func cand(name string) up.Candidate {
	return up.Candidate{
		Role: up.RoleWorker, SessionID: "s-" + name, Name: name, Cwd: "/p/.claude/worktrees/" + name,
		Started: t0.Add(-time.Hour),
		Record:  &route.Record{Decision: &route.Decision{Tier: route.Tier{Model: "opus", Effort: "high"}}},
	}
}

// machine sees the worker s-w live under its launch id (process pid,
// started two hours before t0), reads every turn as turn and every
// transcript as written at act.
func machine(turn Turn, act time.Time) Machine {
	return Machine{
		Now:        t0,
		Silent:     10 * time.Minute,
		Live:       map[string]claudecode.LiveSession{"s-w": {PID: pid, SessionID: "s-w"}},
		Starts:     map[int]time.Time{pid: t0.Add(-2 * time.Hour)},
		Turn:       func(string) Turn { return turn },
		Transcript: func(id string) string { return "/t/" + id + ".jsonl" },
		Activity:   func(string) time.Time { return act },
		Context:    func(string) int { return 180_000 },
	}
}

func one(t *testing.T, c up.Candidate, m Machine) Row {
	t.Helper()
	rows := Rows("/p", []up.Candidate{c}, m)
	if len(rows) != 1 {
		t.Fatalf("rows %v", rows)
	}
	return rows[0]
}

func TestStates(t *testing.T) {
	open := Turn{Known: true, Open: true, At: t0.Add(-time.Hour), PID: pid}
	ended := Turn{Known: true, At: t0.Add(-12 * time.Minute), PID: pid}
	gone := machine(open, t0)
	gone.Live = nil
	goneEnded := machine(ended, t0)
	goneEnded.Live = nil
	cases := []struct {
		name  string
		m     Machine
		state State
		since string
	}{
		{"working", machine(open, t0.Add(-20*time.Second)), Working, "20s"},
		{"stopped", machine(ended, t0.Add(-11*time.Minute)), Stopped, "12m"},
		{"silent", machine(open, t0.Add(-10*time.Minute)), Silent, "10m"},
		{"gone", gone, Gone, ""},
		// The order: gone before stopped, stopped before silent.
		{"gone wins over stopped", goneEnded, Gone, ""},
		{"stopped wins over silent", machine(ended, t0.Add(-3*time.Hour)), Stopped, "12m"},
		// No turn record (adopted, older plugin): never stopped.
		{"no turn working", machine(Turn{}, t0.Add(-time.Minute)), Working, "1m"},
		{"no turn silent", machine(Turn{}, t0.Add(-3*time.Hour)), Silent, "1h"}, // never before its start
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := one(t, cand("w"), tc.m)
			if r.State != tc.state || r.SinceText() != tc.since {
				t.Fatalf("got %s %q, want %s %q", r.State, r.SinceText(), tc.state, tc.since)
			}
		})
	}
}

func TestTurnOfAnotherProcessIsUnknown(t *testing.T) {
	// A stop recorded by an earlier process of the session (resumed since).
	other := Turn{Known: true, At: t0.Add(-time.Minute), PID: pid + 1}
	if r := one(t, cand("w"), machine(other, t0)); r.State != Working {
		t.Fatalf("other pid: %s, want working", r.State)
	}
	// The same pid, reused: the record predates the process.
	reused := Turn{Known: true, At: t0.Add(-3 * time.Hour), PID: pid}
	if r := one(t, cand("w"), machine(reused, t0)); r.State != Working {
		t.Fatalf("reused pid: %s, want working", r.State)
	}
}

const wdir = "/p/.claude/worktrees/w"

// handStarted is a worker adopted without a decision record (no launch id).
func handStarted(name string) up.Candidate {
	c := cand(name)
	c.Record = nil
	return c
}

func TestCurrentSessionAfterClear(t *testing.T) {
	// /clear: the launch arguments still name s-w, the PID file names s-new.
	m := machine(Turn{}, time.Time{})
	m.Live["s-new"] = claudecode.LiveSession{PID: pid, SessionID: "s-new", Cwd: wdir}
	m.Files = map[string]int{"s-new": pid}
	var turnOf, transcriptOf []string
	m.Turn = func(id string) Turn {
		turnOf = append(turnOf, id)
		return Turn{Known: true, At: t0.Add(-time.Minute), PID: pid}
	}
	m.Activity = func(path string) time.Time { transcriptOf = append(transcriptOf, path); return t0 }
	r := one(t, cand("w"), m)
	if r.State != Stopped || r.Current != "s-new" || strings.Join(turnOf, ",") != "s-new" || strings.Join(transcriptOf, ",") != "/t/s-new.jsonl" {
		t.Fatalf("row %+v, turns read %v, transcripts read %v", r, turnOf, transcriptOf)
	}
}

func TestPIDFileIDWinsWhenAlreadyRecorded(t *testing.T) {
	// A worker adopted after /clear is recorded under its PID-file id s-new;
	// its old launch id s-w is still in the arguments, with a name (-n).
	c := handStarted("w")
	c.SessionID = "s-new"
	m := machine(Turn{}, t0)
	m.Live = map[string]claudecode.LiveSession{
		"s-new": {PID: pid, SessionID: "s-new", Cwd: wdir},
		"s-w":   {PID: pid, SessionID: "s-w", Name: "w"},
	}
	m.Files = map[string]int{"s-new": pid}
	if r := one(t, c, m); r.Current != "s-new" {
		t.Fatalf("current %q, want the PID-file id s-new", r.Current)
	}
}

func TestFolderFallbackOnlyForHandStartedWorkers(t *testing.T) {
	// A recorded worker whose process is gone is never matched to another
	// session in its folder.
	m := machine(Turn{}, t0)
	m.Live = map[string]claudecode.LiveSession{"s-other": {PID: pid, SessionID: "s-other", Cwd: wdir}}
	m.Files = map[string]int{"s-other": pid}
	if r := one(t, cand("w"), m); r.State != Gone {
		t.Fatalf("recorded worker borrowed a session: %+v", r)
	}
	// The same for a replacement that never started, beside its running
	// predecessor (the predecessor is no candidate; its process lives on).
	m.Live = map[string]claudecode.LiveSession{
		"s-old": {PID: pid, SessionID: "s-old", Cwd: wdir},
	}
	m.Files = map[string]int{"s-old": pid}
	if r := one(t, cand("w"), m); r.State != Gone {
		t.Fatalf("replacement borrowed its predecessor: %+v", r)
	}
	// A hand-started worker is followed.
	if r := one(t, handStarted("w"), m); r.State != Working || r.Current != "s-old" {
		t.Fatalf("hand-started worker not followed: %+v", r)
	}
}

func TestFolderFallbackNeedsOneToOne(t *testing.T) {
	m := machine(Turn{}, t0)
	m.Live = map[string]claudecode.LiveSession{"s-new": {PID: pid, SessionID: "s-new", Cwd: wdir}}
	m.Files = map[string]int{"s-new": pid}
	// Two live sessions in the folder: no guess.
	m2 := m
	m2.Live = map[string]claudecode.LiveSession{
		"s-new": {PID: pid, SessionID: "s-new", Cwd: wdir},
		"s-two": {PID: pid + 1, SessionID: "s-two", Cwd: wdir},
	}
	m2.Files = map[string]int{"s-new": pid, "s-two": pid + 1}
	if r := one(t, handStarted("w"), m2); r.State != Gone {
		t.Fatalf("ambiguous sessions: %+v", r)
	}
	// Two hand-started workers, one live session: neither takes it.
	a, b := handStarted("a"), handStarted("b")
	a.Cwd, b.Cwd = wdir, wdir
	for _, r := range Rows("/p", []up.Candidate{a, b}, m) {
		if r.State != Gone {
			t.Fatalf("shared session taken by %+v", r)
		}
	}
}

func TestFolderFallbackInProjectRoot(t *testing.T) {
	w := handStarted("w")
	w.Cwd = "/p"
	m := machine(Turn{}, t0)
	m.Live = map[string]claudecode.LiveSession{"s-new": {PID: pid, SessionID: "s-new", Cwd: "/p"}}
	m.Files = map[string]int{"s-new": pid}
	if r := one(t, w, m); r.State != Working || r.Current != "s-new" {
		t.Fatalf("root worker not followed: %+v", r)
	}
	// The coordinator's process runs in the root too: reserved, never taken.
	m.Live["s-coord"] = claudecode.LiveSession{PID: pid + 1, SessionID: "s-coord", Cwd: "/p"}
	m.Files["s-coord"] = pid + 1
	m.Reserved = []string{"s-coord"}
	if r := one(t, w, m); r.State != Working || r.Current != "s-new" {
		t.Fatalf("with the coordinator reserved: %+v", r)
	}
	delete(m.Live, "s-new")
	delete(m.Files, "s-new")
	if r := one(t, w, m); r.State != Gone {
		t.Fatalf("took the coordinator's session: %+v", r)
	}
}

func TestNewWorkerWithoutProcessIsWorking(t *testing.T) {
	c := cand("w")
	c.Started = t0.Add(-30 * time.Second)
	m := machine(Turn{}, time.Time{})
	m.Live = nil
	if r := one(t, c, m); r.State != Working || r.SinceText() != "30s" {
		t.Fatalf("within the grace: %s %q", r.State, r.SinceText())
	}
	c.Started = t0.Add(-Grace)
	if r := one(t, c, m); r.State != Gone {
		t.Fatalf("after the grace: %s", r.State)
	}
}

func TestDetectionFailedIsUnknown(t *testing.T) {
	m := machine(Turn{Known: true, PID: pid}, t0)
	m.Detect = errors.New("ps: exit 1")
	m.Live = nil
	if r := one(t, cand("w"), m); r.State != Unknown {
		t.Fatalf("state %s, want unknown", r.State)
	}
}

func TestRowColumns(t *testing.T) {
	r := one(t, cand("w"), machine(Turn{}, t0))
	if r.Model != "opus/high" || r.Folder != ".claude/worktrees/w" || r.ContextText() != "180k" {
		t.Fatalf("row %+v", r)
	}
	adopted := cand("w")
	adopted.Cwd = "/p"
	adopted.Record = nil
	adopted.Adopted = &up.AdoptedSession{Name: "w", Cwd: "/p", Model: "sonnet", Args: []string{"--effort", "low"}}
	m := machine(Turn{}, t0)
	m.Context = func(string) int { return 0 }
	r = one(t, adopted, m)
	if r.Model != "" || r.Folder != "." || r.ContextText() != "" {
		t.Fatalf("adopted row %+v", r)
	}
	outside := cand("w")
	outside.Cwd = "/q/other"
	if r = one(t, outside, machine(Turn{}, t0)); r.Folder != "../q/other" {
		t.Fatalf("outside folder %q", r.Folder)
	}
}

func TestHandoffLineOnlyStoppedOrGone(t *testing.T) {
	c := cand("w")
	c.Handoff = "/hd/me/.local/state/quartermaster/handoff/pid/w.md"
	stopped := one(t, c, machine(Turn{Known: true, At: t0, PID: pid}, t0))
	working := one(t, c, machine(Turn{Known: true, Open: true, At: t0, PID: pid}, t0))
	out := Table([]Row{stopped}, nil, "/hd/me")
	if !strings.Contains(out, "    handoff note: ~/.local/state/quartermaster/handoff/pid/w.md") {
		t.Fatalf("table:\n%s", out)
	}
	if out := Table([]Row{working}, nil, "/hd/me"); strings.Contains(out, "handoff") {
		t.Fatalf("working row shows a handoff line:\n%s", out)
	}
}

func TestTableLabels(t *testing.T) {
	r := one(t, cand("w"), machine(Turn{Known: true, At: t0, PID: pid}, t0))
	out := Table([]Row{r}, map[string]string{r.SessionID: "reported"}, "")
	if !strings.Contains(out, "stopped (reported)") || !strings.Contains(out, "NAME") {
		t.Fatalf("table:\n%s", out)
	}
}

// layout copies testdata/transcript.jsonl to each path, written at its time.
func layout(t *testing.T, files map[string]time.Time) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "transcript.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for p, at := range files {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
	}
}

func TestActivityReadsNestedSubagents(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "sid.jsonl")
	direct := filepath.Join(dir, "sid", "subagents", "agent-a1.jsonl")
	nested := filepath.Join(dir, "sid", "subagents", "agent-a1", "subagents", "agent-b2.jsonl")
	recent := t0.Add(-30 * time.Second)
	layout(t, map[string]time.Time{main: t0.Add(-time.Hour), direct: t0.Add(-50 * time.Minute), nested: recent})
	if got := Activity(main); !got.Equal(recent) {
		t.Fatalf("activity %v, want the nested subagent's %v", got, recent)
	}
	// A subagent writing keeps the worker working.
	m := machine(Turn{}, time.Time{})
	m.Transcript = func(string) string { return main }
	m.Activity = Activity
	if r := one(t, cand("w"), m); r.State != Working {
		t.Fatalf("state %s, want working", r.State)
	}
	if got := Activity(filepath.Join(dir, "missing.jsonl")); !got.IsZero() {
		t.Fatalf("missing transcript: %v", got)
	}
}

func TestWorkersDropsDoneAndReplaced(t *testing.T) {
	p := up.Project{ID: "pid", Root: "/p", Done: map[string]time.Time{"s-done": t0}}
	rec := func(name, sid, replaces string, at time.Time) route.Record {
		return route.Record{Time: at, Kind: "worker", Name: name, ClaudeSession: sid, Cwd: "/p", UpProject: "pid", Replaces: replaces}
	}
	recs := []route.Record{
		rec("a", "s-a", "", t0.Add(-3*time.Hour)),
		rec("done", "s-done", "", t0.Add(-3*time.Hour)),
		rec("b", "s-old", "", t0.Add(-3*time.Hour)),
		rec("b", "s-new", "s-old", t0.Add(-time.Hour)),
	}
	cands, err := Workers(p, recs, "")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range cands {
		got = append(got, c.SessionID)
	}
	if strings.Join(got, ",") != "s-a,s-new" {
		t.Fatalf("workers %v", got)
	}
}

func TestHandoffOnlyWhenNewerThanSpawn(t *testing.T) {
	dir := t.TempDir()
	note := filepath.Join(dir, "pid", "w.md")
	spawned := time.Now().Add(-time.Hour)
	p := up.Project{ID: "pid", Root: "/p"}
	recs := []route.Record{{Time: spawned, Kind: "worker", Name: "w", ClaudeSession: "s-w", Cwd: "/p", UpProject: "pid"}}
	for _, tc := range []struct {
		mtime time.Time
		want  bool
	}{{spawned.Add(-time.Minute), false}, {spawned.Add(time.Minute), true}} {
		layout(t, map[string]time.Time{note: tc.mtime})
		cands, err := Workers(p, recs, dir)
		if err != nil || len(cands) != 1 {
			t.Fatalf("workers %v %v", cands, err)
		}
		m := machine(Turn{Known: true, At: t0, PID: pid}, t0)
		out := Table(Rows("/p", cands, m), nil, "")
		if got := strings.Contains(out, "handoff note: "+note); got != tc.want {
			t.Fatalf("note at %v: shown %v, want %v\n%s", tc.mtime, got, tc.want, out)
		}
	}
}

func TestPIDFileSessions(t *testing.T) {
	dir := t.TempDir()
	sessions := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(sessions, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("100.json", `{"pid":100,"sessionId":"s-file","cwd":"/x"}`)
	write("101.json", `{"pid":101,"sessionId":"s-moved"}`) // live under another pid
	write("102.json", `{"pid":102,"sessionId":"s-dead"}`)  // not live
	write("103.json", `not json`)
	live := map[string]claudecode.LiveSession{
		"s-file":  {PID: 100, SessionID: "s-file"},
		"s-moved": {PID: 7, SessionID: "s-moved"},
		"s-argv":  {PID: 8, SessionID: "s-argv"},
	}
	got := PIDFileSessions(dir, live)
	if len(got) != 1 || got["s-file"] != 100 {
		t.Fatalf("got %v, want only s-file:100", got)
	}
}
