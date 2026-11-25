package up

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/route"
)

const (
	coordID = "00000000-0000-4000-8000-0000000000c1"
	warmID  = "00000000-0000-4000-8000-0000000000a1"
	smallID = "00000000-0000-4000-8000-0000000000a2"
	largeID = "00000000-0000-4000-8000-0000000000a3"
	lostID  = "00000000-0000-4000-8000-0000000000a4"
)

// fixture copies testdata/<name> to dst, with @ROOT@ replaced by root.
func fixture(t *testing.T, name, root, dst string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte(strings.ReplaceAll(string(b), "@ROOT@", root)), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

// world is a temporary machine: a project root, worker folders, a Claude
// folder with the fixture transcripts, session files and trust file.
type world struct {
	root, claude, handoff string
	transcripts           map[string]string
	project               Project
}

func newWorld(t *testing.T) *world {
	t.Helper()
	root := t.TempDir()
	w := &world{
		root:        root,
		claude:      filepath.Join(root, "claude"),
		handoff:     filepath.Join(root, "state", "handoff"),
		transcripts: map[string]string{},
	}
	mkdir(t, filepath.Join(root, "repo"), w.handoff)
	for _, d := range []string{"warm", "small", "large"} {
		mkdir(t, filepath.Join(root, "worktrees", d))
	}
	mkdir(t, filepath.Join(root, "elsewhere"))
	for id, f := range map[string]string{warmID: "warm.jsonl", smallID: "cold-small.jsonl", largeID: "cold-large.jsonl", coordID: "cold-small.jsonl"} {
		dst := filepath.Join(w.claude, "projects", "p", id+".jsonl")
		fixture(t, f, root, dst)
		w.transcripts[id] = dst
	}
	fixture(t, filepath.Join("sessions", "301.json"), root, filepath.Join(w.claude, "sessions", "301.json"))
	fixture(t, "claude.json", root, filepath.Join(w.claude, ".claude.json"))
	w.project = Project{
		ID: ProjectID(filepath.Join(root, "repo")), Root: filepath.Join(root, "repo"), Workspace: "repo",
		Coordinator: Session{ID: coordID, Name: "repo-coord", Started: now.Add(-5 * time.Hour)},
	}
	return w
}

func (w *world) env(t *testing.T, procs []claudecode.Proc) Env {
	t.Helper()
	live, err := claudecode.Live(w.claude, procs)
	if err != nil {
		t.Fatal(err)
	}
	return Env{
		Now:  now,
		Live: live,
		TranscriptPath: func(id string) (string, error) {
			return w.transcripts[id], nil
		},
		Trusted: func(dir string) (bool, error) {
			return claudecode.Trusted(filepath.Join(w.claude, ".claude.json"), dir)
		},
		HandoffDir: w.handoff,
	}
}

func rec(project, sid, name string, at time.Time) route.Record {
	return route.Record{Time: at, Kind: "worker", UpProject: project, ClaudeSession: sid, Name: name}
}

func (w *world) note(t *testing.T, name string, mod time.Time) string {
	t.Helper()
	path := filepath.Join(w.handoff, name+".md")
	if err := os.WriteFile(path, []byte("Done: the parser. Left: its tests.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
	return path
}

func ids(cs []Candidate) []string {
	var out []string
	for _, c := range cs {
		out = append(out, string(c.Role)+":"+c.Name)
	}
	return out
}

func TestGatherFixturesDecide(t *testing.T) {
	w := newWorld(t)
	started := now.Add(-4 * time.Hour)
	recs := []route.Record{
		rec(w.project.ID, warmID, "warm", started),
		rec(w.project.ID, smallID, "small", started.Add(time.Minute)),
		rec(w.project.ID, largeID, "large", started.Add(2*time.Minute)),
		rec(w.project.ID, lostID, "lost", started.Add(3*time.Minute)),
	}
	for i, d := range []string{"worktrees/warm", "worktrees/small", "worktrees/large", "elsewhere"} {
		recs[i].Cwd = filepath.Join(w.root, d)
	}
	w.note(t, "large", now.Add(-time.Hour))
	// The coordinator is live through its session file (pid 301).
	env := w.env(t, []claudecode.Proc{{PID: 301, Cmd: "claude --permission-mode acceptEdits -n repo-coord"}})
	cs, err := Gather(&w.project, recs, env)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		action Action
		why    string
	}{
		"repo-coord": {Leave, "live"},
		"warm":       {Resume, "cache warm (last activity 10 min ago)"},
		"small":      {Resume, "context 92k ≤ 150k"},
		"large":      {Fresh, "context 310k, handoff note"},
		"lost":       {Skip, "not trusted: run claude in " + filepath.Join(w.root, "elsewhere") + " once and accept the dialog"},
	}
	if len(cs) != len(want) || cs[0].Role != RoleCoordinator {
		t.Fatalf("candidates = %v", ids(cs))
	}
	for _, c := range cs {
		s := Decide(c, false, now)
		if wnt := want[c.Name]; s.Action != wnt.action || s.Why != wnt.why {
			t.Errorf("%s: %s %q, want %s %q", c.Name, s.Action, s.Why, wnt.action, wnt.why)
		}
	}
	if cs[0].Cwd != w.project.Root || cs[0].Transcript == "" {
		t.Errorf("coordinator = %+v", cs[0])
	}
}

func TestGatherTiesByProject(t *testing.T) {
	w := newWorld(t)
	w.project.Coordinator = Session{}
	at := now.Add(-time.Hour)
	review := rec(w.project.ID, smallID, "reviewer", at)
	review.Kind = "review"
	upRec := rec(w.project.ID, largeID, "up", at)
	upRec.Kind = "up"
	recs := []route.Record{
		rec(w.project.ID, warmID, "mine", at),
		rec("other-00000000", smallID, "theirs", at),
		rec("", lostID, "untied", at),
		review,
		upRec,
	}
	cs, err := Gather(&w.project, recs, w.env(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ids(cs), ","); got != "worker:mine" {
		t.Errorf("candidates = %s, want worker:mine", got)
	}
}

func TestGatherReplacedDropped(t *testing.T) {
	w := newWorld(t)
	w.project.Coordinator = Session{}
	at := now.Add(-3 * time.Hour)
	fresh := rec(w.project.ID, smallID, "w1", at.Add(time.Hour))
	fresh.Replaces = warmID
	recs := []route.Record{
		rec(w.project.ID, warmID, "w1", at),
		fresh,
		rec(w.project.ID, largeID, "w2-old-name", at),
		rec(w.project.ID, largeID, "w2", at.Add(30*time.Minute)),
	}
	cs, err := Gather(&w.project, recs, w.env(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ids(cs), ","); got != "worker:w2,worker:w1" {
		t.Fatalf("candidates = %s, want worker:w2,worker:w1", got)
	}
	if cs[1].SessionID != smallID || !cs[0].Started.Equal(at.Add(30*time.Minute)) {
		t.Errorf("candidates = %+v", cs)
	}
}

func TestGatherReplacesOnlyWhenLater(t *testing.T) {
	w := newWorld(t)
	w.project.Coordinator = Session{}
	at := now.Add(-3 * time.Hour)
	early := rec(w.project.ID, smallID, "w2", at.Add(-time.Hour))
	early.Replaces = warmID
	recs := []route.Record{rec(w.project.ID, warmID, "w1", at), early}
	cs, err := Gather(&w.project, recs, w.env(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ids(cs), ","); got != "worker:w2,worker:w1" {
		t.Errorf("candidates = %s, want both: the replacing Record is older", got)
	}
}

func TestGatherCwdFromTranscript(t *testing.T) {
	w := newWorld(t)
	w.project.Coordinator = Session{}
	at := now.Add(-time.Hour)
	recorded := rec(w.project.ID, smallID, "recorded", at.Add(time.Minute))
	recorded.Cwd = filepath.Join(w.root, "elsewhere")
	recs := []route.Record{rec(w.project.ID, warmID, "older", at), recorded}
	cs, err := Gather(&w.project, recs, w.env(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 {
		t.Fatalf("candidates = %v", ids(cs))
	}
	if want := filepath.Join(w.root, "worktrees", "warm"); cs[0].Cwd != want {
		t.Errorf("cwd without a Record cwd = %q, want the transcript's %q", cs[0].Cwd, want)
	}
	if cs[1].Cwd != recorded.Cwd {
		t.Errorf("cwd = %q, want the Record's %q", cs[1].Cwd, recorded.Cwd)
	}
	if !cs[0].Trusted || cs[1].Trusted {
		t.Errorf("trust = %v, %v; want true for warm, false for elsewhere", cs[0].Trusted, cs[1].Trusted)
	}
}

func TestGatherTranscriptMissing(t *testing.T) {
	w := newWorld(t)
	w.project.Coordinator = Session{}
	r := rec(w.project.ID, lostID, "lost", now.Add(-time.Hour))
	cs, err := Gather(&w.project, []route.Record{r}, w.env(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].Transcript != "" || cs[0].Cwd != "" {
		t.Fatalf("candidates = %+v", cs)
	}
	if s := Decide(cs[0], false, now); s.Action != Skip || s.Why != "folder unknown: respawn it by hand" {
		t.Errorf("Decide of a worker with no folder = %s %q", s.Action, s.Why)
	}
}

func TestGatherUnreadableTranscriptCountsAsMissing(t *testing.T) {
	// One bad read does not stop the run: that candidate's transcript counts
	// as missing, with the reason in its note, and the rest are gathered.
	w := newWorld(t)
	w.project.Coordinator = Session{}
	small := rec(w.project.ID, smallID, "small", now.Add(-2*time.Hour))
	small.Cwd = filepath.Join(w.root, "worktrees", "small")
	bad := rec(w.project.ID, largeID, "large", now.Add(-time.Hour))
	bad.Cwd = filepath.Join(w.root, "worktrees", "large")
	// A transcript that is a folder cannot be read.
	broken := filepath.Join(w.root, "broken.jsonl")
	mkdir(t, broken)
	w.transcripts[largeID] = broken
	env := w.env(t, nil)
	cs, err := Gather(&w.project, []route.Record{small, bad}, env)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 || cs[0].Transcript == "" || cs[1].Transcript != "" || !strings.Contains(cs[1].Note, "transcript not read") {
		t.Fatalf("candidates = %+v", cs)
	}
	if s := Plan(cs, false, now)[1]; s.Action != Skip || !strings.HasPrefix(s.Why, "transcript missing (transcript not read") {
		t.Errorf("plan of the unreadable one = %s %q", s.Action, s.Why)
	}
	env.TranscriptPath = func(string) (string, error) { return "", errors.New("glob failed") }
	cs, err = Gather(&w.project, []route.Record{small}, env)
	if err != nil || cs[0].Transcript != "" || !strings.Contains(cs[0].Note, "glob failed") {
		t.Errorf("TranscriptPath error: %+v, %v", cs, err)
	}
}

func TestGatherTrustErrorIsNotTrusted(t *testing.T) {
	w := newWorld(t)
	w.project.Coordinator = Session{}
	r := rec(w.project.ID, smallID, "small", now.Add(-time.Hour))
	r.Cwd = filepath.Join(w.root, "worktrees", "small")
	env := w.env(t, nil)
	env.Trusted = func(string) (bool, error) { return true, errors.New("unreadable") }
	cs, err := Gather(&w.project, []route.Record{r}, env)
	if err != nil || cs[0].Trusted || !strings.Contains(cs[0].Note, "unreadable") {
		t.Errorf("candidates %+v, %v: want not trusted, with a note", cs, err)
	}
}

func TestGatherSeedsWorkerTerminals(t *testing.T) {
	// A worker qm spawn worker started has its terminal session only in its
	// Record: Gather loads it into the project's sessions, from the term
	// fields or, for an older Record, from Detail.
	w := newWorld(t)
	a := rec(w.project.ID, warmID, "a", now.Add(-3*time.Hour))
	a.TermLauncher, a.TermID, a.Detail = "agterm", "t-a", "agterm ignored"
	b := rec(w.project.ID, smallID, "b", now.Add(-2*time.Hour))
	b.Detail = "tmux @7"
	c := rec(w.project.ID, largeID, "c", now.Add(-time.Hour))
	c.Detail = "no launcher: /state/launch/c.sh"
	d := rec(w.project.ID, lostID, "d", now.Add(-time.Hour))
	d.TermLauncher, d.TermID = "agterm", "t-d-old"
	w.project.Sessions = map[string]launch.SessionRef{lostID: {Launcher: "agterm", ID: "t-d"}}
	if _, err := Gather(&w.project, []route.Record{a, b, c, d}, w.env(t, nil)); err != nil {
		t.Fatal(err)
	}
	want := map[string]launch.SessionRef{
		warmID:  {Launcher: "agterm", ID: "t-a", Name: "a"},
		smallID: {Launcher: "tmux", ID: "@7", Name: "b"},
		lostID:  {Launcher: "agterm", ID: "t-d"}, // the state's own wins
	}
	if !maps.Equal(w.project.Sessions, want) {
		t.Errorf("sessions %v, want %v", w.project.Sessions, want)
	}
}

func TestGatherLiveWorkerNotChecked(t *testing.T) {
	// A live worker is left whatever its pull request says: gh is not run.
	w := newWorld(t)
	w.project.Coordinator = Session{}
	r := rec(w.project.ID, warmID, "warm", now.Add(-time.Hour))
	r.Cwd = filepath.Join(w.root, "worktrees", "warm")
	env := w.env(t, []claudecode.Proc{{PID: 9, Cmd: "claude --resume " + warmID}})
	called := false
	env.Finished = func(Candidate) string { called = true; return "merged" }
	cs, err := Gather(&w.project, []route.Record{r}, env)
	if err != nil || !cs[0].Live || called || cs[0].Finished != "" {
		t.Errorf("live %v, finished checked %v (%q), err %v", cs[0].Live, called, cs[0].Finished, err)
	}
}

func TestHandoffProjectFolder(t *testing.T) {
	// A note in the project's own folder wins; the flat folder of an older
	// qm is still read.
	w := newWorld(t)
	w.project.Coordinator = Session{}
	started := now.Add(-2 * time.Hour)
	r := rec(w.project.ID, largeID, "w1", started)
	r.Cwd = filepath.Join(w.root, "worktrees", "large")
	flat := w.note(t, "w1", started.Add(time.Minute))
	cs, err := Gather(&w.project, []route.Record{r}, w.env(t, nil))
	if err != nil || cs[0].Handoff != flat {
		t.Fatalf("flat note: Handoff %q, %v; want %q", cs[0].Handoff, err, flat)
	}
	mkdir(t, filepath.Join(w.handoff, w.project.ID))
	own := w.note(t, filepath.Join(w.project.ID, "w1"), started.Add(time.Minute))
	cs, err = Gather(&w.project, []route.Record{r}, w.env(t, nil))
	if err != nil || cs[0].Handoff != own {
		t.Errorf("project note: Handoff %q, %v; want %q", cs[0].Handoff, err, own)
	}
	// Another project's note of the same name is never read.
	other := w.project
	other.ID = "other-00000000"
	if err := os.Remove(flat); err != nil {
		t.Fatal(err)
	}
	cs, err = Gather(&other, []route.Record{rec(other.ID, largeID, "w1", started)}, w.env(t, nil))
	if err != nil || cs[0].Handoff != "" {
		t.Errorf("other project: Handoff %q, %v; want none", cs[0].Handoff, err)
	}
}

func TestHandoffOlderThanStartIgnored(t *testing.T) {
	w := newWorld(t)
	w.project.Coordinator = Session{}
	started := now.Add(-2 * time.Hour)
	r := rec(w.project.ID, largeID, "w1", started)
	r.Cwd = filepath.Join(w.root, "worktrees", "large")
	w.note(t, "w1", started.Add(-time.Minute))
	cs, err := Gather(&w.project, []route.Record{r}, w.env(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if cs[0].Handoff != "" {
		t.Errorf("a note older than the start was used: %q", cs[0].Handoff)
	}
	if s := Decide(cs[0], false, now); s.Action != Resume || s.Why != "context 310k, no handoff note" {
		t.Errorf("Decide = %s %q", s.Action, s.Why)
	}
	path := w.note(t, "w1", started.Add(time.Minute))
	cs, err = Gather(&w.project, []route.Record{r}, w.env(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if cs[0].Handoff != path {
		t.Errorf("Handoff = %q, want %q", cs[0].Handoff, path)
	}
}

func TestGatherBranchFromWorktree(t *testing.T) {
	w := newWorld(t)
	w.project.Coordinator = Session{}
	r := rec(w.project.ID, warmID, "w1", now.Add(-time.Hour))
	r.Branch, r.Cwd = "main", filepath.Join(w.root, "worktrees", "warm")
	env := w.env(t, nil)
	env.Run = func(_ context.Context, dir, bin string, args ...string) ([]byte, error) {
		if dir == r.Cwd && bin == "git" && strings.Join(args, " ") == "symbolic-ref --short HEAD" {
			return []byte("feature-x\n"), nil
		}
		return nil, errors.New("unexpected call")
	}
	var seen string
	env.Finished = func(c Candidate) string { seen = c.Branch; return "" }
	cs, err := Gather(&w.project, []route.Record{r}, env)
	if err != nil {
		t.Fatal(err)
	}
	if cs[0].Branch != "feature-x" || seen != "feature-x" {
		t.Errorf("Branch = %q, Finished saw %q; want feature-x", cs[0].Branch, seen)
	}
}

func TestGatherTiesAdoptedRecord(t *testing.T) {
	w := newWorld(t)
	w.project.Coordinator = Session{}
	w.project.Adopted = map[string]AdoptedSession{warmID: {}}
	at := now.Add(-3 * time.Hour)
	recs := []route.Record{rec("", warmID, "w-adopted", at), rec("", largeID, "w-other", at)}
	cs, err := Gather(&w.project, recs, w.env(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ids(cs), ","); got != "worker:w-adopted" {
		t.Fatalf("candidates = %s, want worker:w-adopted", got)
	}
	if cs[0].Record == nil || cs[0].SessionID != warmID || cs[0].Adopted != nil {
		t.Errorf("candidate = %+v, want its Record and no hand-started entry", cs[0])
	}
}

func TestGatherReplacerOfAdoptedContinuesChain(t *testing.T) {
	w := newWorld(t)
	w.project.Coordinator = Session{}
	w.project.Adopted = map[string]AdoptedSession{warmID: {}}
	at := now.Add(-3 * time.Hour)
	fresh := rec(w.project.ID, smallID, "w-adopted", at.Add(time.Hour))
	fresh.Replaces = warmID
	cs, err := Gather(&w.project, []route.Record{rec("", warmID, "w-adopted", at), fresh}, w.env(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].SessionID != smallID {
		t.Fatalf("candidates = %+v, want only the replacer", cs)
	}
	if !cs[0].LineStarted.Equal(at) {
		t.Errorf("LineStarted = %v, want the adopted Record's %v", cs[0].LineStarted, at)
	}
}

func TestGatherHandStarted(t *testing.T) {
	w := newWorld(t)
	started := now.Add(-4 * time.Hour)
	cwd := filepath.Join(w.root, "worktrees", "large")
	w.project.Adopted = map[string]AdoptedSession{
		largeID: {Name: "w-hand", Cwd: cwd, Model: "opus", Args: []string{"--model", "opus"}, Started: started},
		lostID:  {}, // a worker with a Record, but none here: no candidate
	}
	sess := `{"pid":302,"sessionId":"` + largeID + `","cwd":"` + cwd + `","name":"w-hand","kind":"interactive"}`
	if err := os.WriteFile(filepath.Join(w.claude, "sessions", "302.json"), []byte(sess), 0o600); err != nil {
		t.Fatal(err)
	}
	recs := []route.Record{rec(w.project.ID, warmID, "w-rec", now.Add(-time.Hour))}
	procs := []claudecode.Proc{{PID: 301, Cmd: "claude -n repo-coord"}, {PID: 302, Cmd: "claude --model opus"}}
	cs, err := Gather(&w.project, recs, w.env(t, procs))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ids(cs), ","); got != "coordinator:repo-coord,worker:w-rec,worker:w-hand" {
		t.Fatalf("candidates = %s, want the hand-started worker after the Record workers", got)
	}
	c := cs[2]
	if c.Adopted == nil || c.Record != nil || c.SessionID != largeID || c.Cwd != cwd {
		t.Errorf("candidate = %+v, want the hand-started entry", c)
	}
	if !c.Started.Equal(started) || !c.LineStarted.Equal(started) || !c.Live {
		t.Errorf("Started %v, LineStarted %v, Live %v: want the entry's start and live", c.Started, c.LineStarted, c.Live)
	}
}

// A session whose launch was not confirmed and that never ran is given no
// handoff note that another session of its name, one that ran or a later
// one, may have written. Its own, with no other of the name, it keeps.
func TestGatherUnconfirmedLaunchHandoff(t *testing.T) {
	const firstID, otherID = "00000000-0000-4000-8000-0000000000e1", "00000000-0000-4000-8000-0000000000e2"
	for _, c := range []struct {
		name   string
		detail string
		other  string    // the other session of the name ("" for none)
		at     time.Time // and its start
		note   bool
	}{
		{"alone", "launch failed, may have started: w.sh", "", time.Time{}, true},
		{"a retry that never ran", "launch failed, may have started: w.sh", otherID, now.Add(-time.Hour), false},
		{"a retry, after a stopped spawn", "launch not confirmed: w.sh", otherID, now.Add(-time.Hour), false},
		{"a retry that ran", "launch failed, may have started: w.sh", smallID, now.Add(-time.Hour), false},
		{"an earlier one that ran", "launch failed, may have started: w.sh", smallID, now.Add(-3 * time.Hour), false},
		{"an earlier one that never ran", "launch failed, may have started: w.sh", otherID, now.Add(-3 * time.Hour), true},
		{"a confirmed launch", "agterm s-1", smallID, now.Add(-3 * time.Hour), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			note := w.note(t, "w", now.Add(-time.Minute))
			r := rec(w.project.ID, firstID, "w", now.Add(-2*time.Hour))
			r.Detail = c.detail
			recs := []route.Record{r, rec(w.project.ID, warmID, "elsewhere", now)}
			if c.other != "" {
				recs = append(recs, rec(w.project.ID, c.other, "w", c.at))
			}
			cands, err := Gather(&w.project, recs, w.env(t, nil))
			if err != nil {
				t.Fatal(err)
			}
			for _, got := range cands {
				want := ""
				if c.note {
					want = note
				}
				if got.SessionID == firstID && got.Handoff != want {
					t.Errorf("handoff %q, want %q", got.Handoff, want)
				}
				if got.SessionID == c.other && got.Handoff != note {
					t.Errorf("the other's handoff %q, want %q", got.Handoff, note)
				}
			}
		})
	}
}
