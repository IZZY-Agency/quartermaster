package up

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/route"
	"github.com/IZZY-Agency/quartermaster/internal/transcript"
)

func lastAt(at time.Time, context int) transcript.Last {
	return transcript.Last{Context: context, At: at}
}

// fakeAgterm answers agtermctl calls like the app: session new prints a new
// id, tree lists the sessions in tree, and every call is recorded. A session
// new whose --name is in fail fails. Sessions whose id is in idle list a
// foregroundShell; a session type call answers ok, or fails "session not
// realized" (agterm's own text)
// while typeFail is positive (counting it down); typed holds the typed texts.
type fakeAgterm struct {
	calls    [][]string
	tree     []launch.SessionRef // sessions of the project's workspace
	ws       string
	fail     map[string]bool
	idle     map[string]bool
	typeFail int
	typeErr  error // every session type call fails with it
	typed    []string
	n        int
}

func (f *fakeAgterm) run(_ context.Context, _ string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, slices.Clone(args))
	ok := func(result any) ([]byte, error) {
		b, _ := json.Marshal(map[string]any{"ok": true, "result": result})
		return b, nil
	}
	switch {
	case len(args) >= 2 && args[0] == "session" && args[1] == "new":
		name := args[slices.Index(args, "--name")+1]
		if f.fail[name] {
			return nil, errors.New("agtermctl: session new failed")
		}
		f.n++
		return ok(map[string]string{"id": fmt.Sprintf("s-%d", f.n)})
	case len(args) >= 2 && args[0] == "session" && args[1] == "type":
		f.typed = append(f.typed, args[len(args)-1])
		if f.typeErr != nil {
			return nil, f.typeErr
		}
		if f.typeFail > 0 {
			f.typeFail--
			return nil, errors.New(`agtermctl: {"ok":false,"error":"session not realized"}`)
		}
		return ok(map[string]any{})
	case args[0] == "tree":
		var ss []map[string]string
		for _, r := range f.tree {
			m := map[string]string{"id": r.ID, "name": r.Name}
			if f.idle[r.ID] {
				m["foregroundShell"] = "zsh"
			}
			ss = append(ss, m)
		}
		return ok(map[string]any{"tree": map[string]any{"workspaces": []any{map[string]any{"name": f.ws, "sessions": ss}}}})
	}
	return ok(map[string]any{})
}

// verbs is the agtermctl calls as "session new NAME", "session close ID",
// "session restore ID" or "tree".
func (f *fakeAgterm) verbs() []string {
	var out []string
	for _, c := range f.calls {
		switch {
		case c[0] == "tree":
			out = append(out, "tree")
		case c[1] == "new":
			out = append(out, "session new "+c[slices.Index(c, "--name")+1])
		default:
			out = append(out, "session "+c[1]+" "+c[slices.Index(c, "--target")+1])
		}
	}
	return out
}

// upDeps wires a fake agterm into Deps, with every Record logged to *logged.
func upDeps(t *testing.T, f *fakeAgterm, logged *[]route.Record) Deps {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	t.Setenv("QM_WORKER_PLUGIN", t.TempDir())
	exe := filepath.Join(t.TempDir(), "qm")
	if err := os.WriteFile(exe, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	a := launch.Agterm{Bin: "agtermctl", Run: f.run}
	return Deps{
		Launcher: a, Lister: a, Closer: a, Pin: a.Pin, Exe: exe,
		Log: func(r route.Record) error {
			*logged = append(*logged, r)
			return nil
		},
		Now: func() time.Time { return now },
	}
}

// savedProject reads the state file of p as Save wrote it.
func savedProject(t *testing.T, p Project) Project {
	t.Helper()
	dir, err := projectsDir()
	if err != nil {
		t.Fatal(err)
	}
	got, err := readProject(filepath.Join(dir, p.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// promptOf is the prompt file a script reads its first prompt from.
func promptOf(t *testing.T, script string) string {
	t.Helper()
	_, after, ok := strings.Cut(script, "; cat ")
	if !ok {
		t.Fatalf("script has no prompt file:\n%s", script)
	}
	path, _, _ := strings.Cut(after, ")")
	return readFile(t, strings.Trim(path, "'"))
}

func upRecords(recs []route.Record) map[string]route.Record {
	out := map[string]route.Record{}
	for _, r := range recs {
		if r.Kind == "up" {
			out[r.Name] = r
		}
	}
	return out
}

// profiled is a worker Record with a launch profile, as qm spawn worker logs it.
func profiled(w *world, sid, name, dir string, at time.Time, perm ...string) route.Record {
	r := rec(w.project.ID, sid, name, at)
	r.Cwd = filepath.Join(w.root, "worktrees", dir)
	r.Decision = &route.Decision{
		Kind: "worker", Tier: route.Tier{Model: "sonnet", Effort: "medium"},
		Profile: &route.Profile{
			Autocompact: 150000, PluginsOn: []string{"qm-worker"}, PluginDirs: []string{"/old/qm-worker"},
			PluginsOff: []string{"qm-coordinator@quartermaster"},
			Env:        map[string]string{"QM_PROJECT_ID": w.project.ID, "QM_WORKSPACE": "repo", "QM_LSP": "go"},
			Permission: perm,
		},
	}
	return r
}

func (w *world) plan(t *testing.T, recs []route.Record, procs []claudecode.Proc) []Step {
	t.Helper()
	cs, err := Gather(&w.project, recs, w.env(t, procs))
	if err != nil {
		t.Fatal(err)
	}
	return Plan(cs, false, now)
}

func outcomeOf(t *testing.T, outs []Outcome, name string) Outcome {
	t.Helper()
	for _, o := range outs {
		if o.Step.Name == name {
			return o
		}
	}
	t.Fatalf("no outcome for %s", name)
	return Outcome{}
}

func TestUpLeavesLiveCoordinator(t *testing.T) {
	var logged []route.Record
	f := &fakeAgterm{ws: "repo", tree: []launch.SessionRef{{ID: "s-old", Name: "repo-coord"}}}
	d := upDeps(t, f, &logged)
	w := newWorld(t)
	w.project.Sessions = map[string]launch.SessionRef{coordID: {Launcher: "agterm", ID: "s-old", Name: "repo-coord"}}
	steps := w.plan(t, nil, []claudecode.Proc{{PID: 301, Cmd: "claude -n repo-coord"}})
	outs := Apply(context.Background(), &w.project, steps, d)
	if len(outs) != 1 || outs[0].Step.Action != Leave || outs[0].Err != nil {
		t.Fatalf("outcomes = %+v", outs)
	}
	if got := f.verbs(); !slices.Equal(got, []string{"session restore s-old"}) {
		t.Errorf("agtermctl calls = %v, want only the re-pin of the live session", got)
	}
	if r := upRecords(logged)["repo-coord"]; r.Action != "leave" || r.Detail != "live" {
		t.Errorf("up Record = %+v", r)
	}
}

func TestUpResumesWarmWorker(t *testing.T) {
	var logged []route.Record
	f := &fakeAgterm{ws: "repo"}
	d := upDeps(t, f, &logged)
	w := newWorld(t)
	w.project.ClaudeArgs = []string{"--model", "opus", "--permission-mode", "acceptEdits"}
	recs := []route.Record{profiled(w, warmID, "w-warm", "warm", now.Add(-time.Hour), "--dangerously-skip-permissions")}
	steps := w.plan(t, recs, []claudecode.Proc{{PID: 301, Cmd: "claude -n repo-coord"}})
	outs := Apply(context.Background(), &w.project, steps, d)
	o := outcomeOf(t, outs, "w-warm")
	if o.Err != nil || o.Step.Action != Resume || o.Step.Cold {
		t.Fatalf("outcome = %+v", o)
	}
	script := readFile(t, o.Script)
	for _, want := range []string{
		"QM_PROJECT_ID=" + w.project.ID, "QM_LSP=go",
		" claude --permission-mode acceptEdits --model claude-sonnet-",
		" --effort medium --autocompact 150000 --strict-mcp-config ",
		"--plugin-dir " + os.Getenv("QM_WORKER_PLUGIN") + " -n w-warm --resume " + warmID + "\n",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "/old/qm-worker") || strings.Contains(script, "--dangerously-skip-permissions") {
		t.Errorf("script keeps the logged plugin folder or an uncapped permission:\n%s", script)
	}
	if got := w.project.Sessions[warmID]; got.ID != "s-1" || got.Warning != "" {
		t.Errorf("state session = %+v", got)
	}
	if saved := savedProject(t, w.project); saved.Sessions[warmID].ID != "s-1" {
		t.Errorf("saved state lacks the resumed worker: %+v", saved.Sessions)
	}
	if r := upRecords(logged)["w-warm"]; r.Action != "resume" || r.ClaudeSession != warmID {
		t.Errorf("up Record = %+v", r)
	}
}

func TestUpFreshWorkerFromNote(t *testing.T) {
	var logged []route.Record
	f := &fakeAgterm{ws: "repo"}
	d := upDeps(t, f, &logged)
	w := newWorld(t)
	start := now.Add(-4 * time.Hour)
	old := profiled(w, largeID, "w-large", "large", start)
	old.Session, old.Override, old.Project, old.Type = "00000000-0000-4000-8000-0000000000c0", "forced", w.root, "feature"
	recs := []route.Record{old}
	w.note(t, "w-large", start.Add(time.Hour))
	steps := w.plan(t, recs, []claudecode.Proc{{PID: 301, Cmd: "claude -n repo-coord"}})
	outs := Apply(context.Background(), &w.project, steps, d)
	o := outcomeOf(t, outs, "w-large")
	if o.Err != nil || o.Step.Action != Fresh || o.NewSessionID == "" || o.NewSessionID == largeID {
		t.Fatalf("outcome = %+v", o)
	}
	script := readFile(t, o.Script)
	if !strings.Contains(script, " -n w-large --session-id "+o.NewSessionID+" ") {
		t.Errorf("script does not start the new session:\n%s", script)
	}
	prompt := promptOf(t, script)
	for _, want := range []string{"You continue the task of worker w-large, whose session ended at 310k tokens of context.", w.transcripts[largeID], "Done: the parser. Left: its tests.\n"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	var nr *route.Record
	for i := range logged {
		if logged[i].Kind == "worker" {
			nr = &logged[i]
		}
	}
	if nr == nil || nr.ClaudeSession != o.NewSessionID || nr.Replaces != largeID || nr.Name != "w-large" || nr.UpProject != w.project.ID || nr.Decision.Tier != (route.Tier{Model: "sonnet", Effort: "medium"}) {
		t.Fatalf("new worker Record = %+v", nr)
	}
	if nr.Session != "" || nr.Override != "" || nr.Project != w.root || nr.Type != "feature" || nr.Cwd != old.Cwd || !nr.Time.Equal(now) {
		t.Errorf("new worker Record carries the wrong fields: %+v", nr)
	}
	if nr.TermLauncher != "agterm" || nr.TermID != o.Ref.ID {
		t.Errorf("new Record's terminal %q %q, want agterm %s", nr.TermLauncher, nr.TermID, o.Ref.ID)
	}
	if nr.Decision.Profile.PluginDirs[0] != os.Getenv("QM_WORKER_PLUGIN") {
		t.Errorf("new Record's plugin folder = %v, want it resolved again", nr.Decision.Profile.PluginDirs)
	}
	// The next qm up sees the new session, running, in place of the old one.
	cs, err := Gather(&w.project, append(recs, logged...), w.env(t, []claudecode.Proc{{PID: 402, Cmd: "claude -n w-large --session-id " + o.NewSessionID}}))
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 || cs[1].SessionID != o.NewSessionID {
		t.Errorf("later gather = %v, want the coordinator and the new session", ids(cs))
	}
	if _, ok := w.project.Sessions[o.NewSessionID]; !ok {
		t.Errorf("state lacks the new session: %+v", w.project.Sessions)
	}
}

func TestUpFreshCoordinatorFromSummary(t *testing.T) {
	var logged []route.Record
	f := &fakeAgterm{ws: "repo"}
	d := upDeps(t, f, &logged)
	w := newWorld(t)
	fixture(t, "cold-large.jsonl", w.root, w.transcripts[coordID])
	w.project.ClaudeArgs = []string{"--permission-mode", "acceptEdits"}
	w.project.PastCoordinators = []string{"00000000-0000-4000-8000-0000000000c0"}
	steps := w.plan(t, nil, nil)
	outs := Apply(context.Background(), &w.project, steps, d)
	o := outcomeOf(t, outs, "repo-coord")
	if o.Err != nil || o.Step.Action != Fresh || o.NewSessionID == "" {
		t.Fatalf("outcome = %+v", o)
	}
	if w.project.Coordinator.ID != o.NewSessionID || !slices.Equal(w.project.PastCoordinators, []string{"00000000-0000-4000-8000-0000000000c0", coordID}) {
		t.Errorf("project coordinator %+v, past %v", w.project.Coordinator, w.project.PastCoordinators)
	}
	script := readFile(t, o.Script)
	if !strings.Contains(script, " claude --permission-mode acceptEdits -n repo-coord --session-id "+o.NewSessionID+" ") {
		t.Errorf("script:\n%s", script)
	}
	prompt := promptOf(t, script)
	if !strings.HasPrefix(prompt, "# Restart summary: repo (qm up, ") || !strings.Contains(prompt, "context was 310k tokens (threshold 200k)") || !strings.Contains(prompt, "Port the module.") {
		t.Errorf("prompt is not the summary:\n%s", prompt)
	}
	if saved := savedProject(t, w.project); saved.Coordinator.ID != o.NewSessionID {
		t.Errorf("saved coordinator = %+v", saved.Coordinator)
	}
}

func TestUpSkipsUntrusted(t *testing.T) {
	var logged []route.Record
	f := &fakeAgterm{ws: "repo"}
	d := upDeps(t, f, &logged)
	w := newWorld(t)
	bad := profiled(w, smallID, "w-out", "small", now.Add(-time.Hour))
	bad.Cwd = filepath.Join(w.root, "elsewhere")
	recs := []route.Record{bad, profiled(w, warmID, "w-warm", "warm", now.Add(-time.Hour))}
	steps := w.plan(t, recs, []claudecode.Proc{{PID: 301, Cmd: "claude -n repo-coord"}})
	outs := Apply(context.Background(), &w.project, steps, d)
	if o := outcomeOf(t, outs, "w-out"); o.Step.Action != Skip || !strings.HasPrefix(o.Step.Why, "not trusted") || o.Script != "" {
		t.Errorf("untrusted outcome = %+v", o)
	}
	if got := f.verbs(); !slices.Equal(got, []string{"tree", "session new w-warm", "session restore s-1"}) {
		t.Errorf("agtermctl calls = %v", got)
	}
}

// reuseWorld is a world whose coordinator is live in s-coord and whose warm
// worker w-warm has a recorded agterm tab s-old, listed in the workspace;
// busy tabs are listed too, idle ones show a foregroundShell.
func reuseWorld(t *testing.T, tabs []launch.SessionRef, idle ...string) (*world, *fakeAgterm, Deps, []Step, *[]route.Record) {
	t.Helper()
	logged := new([]route.Record)
	f := &fakeAgterm{ws: "repo", tree: tabs, idle: map[string]bool{}}
	for _, id := range idle {
		f.idle[id] = true
	}
	d := upDeps(t, f, logged)
	w := newWorld(t)
	w.project.Sessions = map[string]launch.SessionRef{
		coordID: {Launcher: "agterm", ID: "s-coord", Name: "repo-coord"},
		warmID:  {Launcher: "agterm", ID: "s-old", Name: "w-warm"},
	}
	recs := []route.Record{profiled(w, warmID, "w-warm", "warm", now.Add(-time.Hour))}
	steps := w.plan(t, recs, []claudecode.Proc{{PID: 301, Cmd: "claude -n repo-coord"}})
	return w, f, d, steps, logged
}

var reuseTabs = []launch.SessionRef{{ID: "s-coord", Name: "repo-coord"}, {ID: "s-old", Name: "w-warm"}}

func TestUpReusesIdleTab(t *testing.T) {
	w, f, d, steps, _ := reuseWorld(t, reuseTabs, "s-old")
	outs := Apply(context.Background(), &w.project, steps, d)
	o := outcomeOf(t, outs, "w-warm")
	if o.Err != nil || o.Ref.ID != "s-old" {
		t.Fatalf("outcome = %+v", o)
	}
	want := []string{"tree", "session restore s-coord", "tree", "session type s-old", "session restore s-old"}
	if got := f.verbs(); !slices.Equal(got, want) {
		t.Errorf("agtermctl calls = %v, want %v", got, want)
	}
	if len(f.typed) != 1 || f.typed[0] != "\x15/bin/sh "+o.Script+"\n" {
		t.Errorf("typed = %q, want Ctrl-U, /bin/sh <script> and a newline", f.typed)
	}
	if w.project.Sessions[warmID].ID != "s-old" {
		t.Errorf("state session = %+v, want the reused tab", w.project.Sessions[warmID])
	}
}

func TestUpReuseFreshStart(t *testing.T) {
	var logged []route.Record
	f := &fakeAgterm{ws: "repo", tree: []launch.SessionRef{{ID: "s-coord", Name: "repo-coord"}, {ID: "s-old", Name: "w-large"}}, idle: map[string]bool{"s-old": true}}
	d := upDeps(t, f, &logged)
	w := newWorld(t)
	start := now.Add(-4 * time.Hour)
	old := profiled(w, largeID, "w-large", "large", start)
	old.Session, old.Project, old.Type = "00000000-0000-4000-8000-0000000000c0", w.root, "feature"
	w.note(t, "w-large", start.Add(time.Hour))
	w.project.Sessions = map[string]launch.SessionRef{
		coordID: {Launcher: "agterm", ID: "s-coord", Name: "repo-coord"},
		largeID: {Launcher: "agterm", ID: "s-old", Name: "w-large"},
	}
	steps := w.plan(t, []route.Record{old}, []claudecode.Proc{{PID: 301, Cmd: "claude -n repo-coord"}})
	o := outcomeOf(t, Apply(context.Background(), &w.project, steps, d), "w-large")
	if o.Err != nil || o.Step.Action != Fresh || o.NewSessionID == "" {
		t.Fatalf("outcome = %+v", o)
	}
	if got := w.project.Sessions[o.NewSessionID]; got.ID != "s-old" {
		t.Errorf("the new session's tab = %+v, want s-old", got)
	}
	if _, ok := w.project.Sessions[largeID]; ok || !w.project.Dropped[largeID] {
		t.Errorf("the old session id is not dropped: %+v, %v", w.project.Sessions, w.project.Dropped)
	}
	for _, v := range f.verbs() {
		if strings.HasPrefix(v, "session new") || strings.HasPrefix(v, "session close") {
			t.Errorf("agtermctl calls = %v, want a reuse only", f.verbs())
		}
	}
}

func TestUpBusyTabOpensNew(t *testing.T) {
	w, f, d, steps, _ := reuseWorld(t, reuseTabs)
	sp := PlanStale(context.Background(), &w.project, steps, d)
	if got := RenderStale(sp); !strings.Contains(got, "w-warm: its tab (agterm s-old) is busy: left open, a new tab will be opened\n") {
		t.Errorf("RenderStale = %q", got)
	}
	d.Stale = sp
	Apply(context.Background(), &w.project, steps, d)
	want := []string{"tree", "session restore s-coord", "session new w-warm", "session restore s-1"}
	if got := f.verbs(); !slices.Equal(got, want) {
		t.Errorf("agtermctl calls = %v, want %v", got, want)
	}
}

func TestUpUnlistedRefOpensNew(t *testing.T) {
	w, f, d, steps, _ := reuseWorld(t, []launch.SessionRef{{ID: "s-coord", Name: "repo-coord"}}, "s-old")
	Apply(context.Background(), &w.project, steps, d)
	want := []string{"tree", "session restore s-coord", "session new w-warm", "session restore s-1"}
	if got := f.verbs(); !slices.Equal(got, want) {
		t.Errorf("agtermctl calls = %v, want %v", got, want)
	}
	if got := RenderStale(PlanStale(context.Background(), &w.project, steps, d)); got != "" {
		t.Errorf("RenderStale = %q, want nothing", got)
	}
}

func TestUpNeverClosesByName(t *testing.T) {
	// Two listed tabs named like the session, neither its recorded tab:
	// none is closed or typed into.
	tabs := []launch.SessionRef{{ID: "s-coord", Name: "repo-coord"}, {ID: "s-a", Name: "w-warm"}, {ID: "s-b", Name: "w-warm"}}
	w, f, d, steps, _ := reuseWorld(t, tabs, "s-a", "s-b")
	delete(w.project.Sessions, warmID)
	Apply(context.Background(), &w.project, steps, d)
	want := []string{"tree", "session restore s-coord", "session new w-warm", "session restore s-1"}
	if got := f.verbs(); !slices.Equal(got, want) {
		t.Errorf("agtermctl calls = %v, want %v", got, want)
	}
}

func TestUpReuseFailsFallsBack(t *testing.T) {
	w, f, d, steps, _ := reuseWorld(t, reuseTabs, "s-old")
	f.typeFail = 2
	o := outcomeOf(t, Apply(context.Background(), &w.project, steps, d), "w-warm")
	if o.Err != nil || o.Ref.ID != "s-1" {
		t.Fatalf("outcome = %+v", o)
	}
	want := []string{"tree", "session restore s-coord", "tree", "session type s-old", "session type s-old", "session new w-warm", "session restore s-1", "session close s-old"}
	if got := f.verbs(); !slices.Equal(got, want) {
		t.Errorf("agtermctl calls = %v, want %v", got, want)
	}
	if w.project.Sessions[warmID].ID != "s-1" {
		t.Errorf("state session = %+v, want the new tab", w.project.Sessions[warmID])
	}
}

func TestUpTmuxNeverClosesByName(t *testing.T) {
	// A tmux window with the step's name that is not the recorded window.
	w := newWorld(t)
	w.project.Sessions = map[string]launch.SessionRef{warmID: {Launcher: "tmux", ID: "@3", Name: "w-warm"}}
	steps := []Step{{Candidate: Candidate{Role: RoleWorker, SessionID: warmID, Name: "w-warm"}, Action: Resume}}
	l := fakeLister{{Launcher: "tmux", ID: "@9", Name: "w-warm", Workspace: "repo", Idle: true}}
	sp := PlanStale(context.Background(), &w.project, steps, Deps{Lister: l, Closer: &fakeCloser{p: &w.project}})
	if len(sp.Close) != 0 || len(sp.Reuse) != 0 {
		t.Errorf("would close %v, reuse %v", sp.Close, sp.Reuse)
	}
}

func TestUpReusePlanShownThenUsed(t *testing.T) {
	// PlanStale lists only; Apply types into the planned tab without
	// listing again.
	w, f, d, steps, _ := reuseWorld(t, reuseTabs, "s-old")
	sp := PlanStale(context.Background(), &w.project, steps, d)
	if got := f.verbs(); !slices.Equal(got, []string{"tree"}) {
		t.Fatalf("PlanStale ran %v, want the listing only", got)
	}
	if got := RenderStale(sp); got != "reuse w-warm (agterm s-old)\n" {
		t.Errorf("RenderStale = %q", got)
	}
	d.Stale = sp
	Apply(context.Background(), &w.project, steps, d)
	want := []string{"tree", "session restore s-coord", "tree", "session type s-old", "session restore s-old"}
	if got := f.verbs(); !slices.Equal(got, want) {
		t.Errorf("agtermctl calls = %v, want %v", got, want)
	}
}

func TestUpStaleTmuxRefNeedsItsName(t *testing.T) {
	// A recorded tmux window id that the workspace now shows under another
	// name is another window: not a stale shell of the session.
	w := newWorld(t)
	w.project.Sessions = map[string]launch.SessionRef{warmID: {Launcher: "tmux", ID: "@3", Name: "w-warm"}}
	steps := []Step{{Candidate: Candidate{Role: RoleWorker, SessionID: warmID, Name: "w-warm"}, Action: Resume}}
	l := fakeLister{{Launcher: "tmux", ID: "@3", Name: "repo-coord", Workspace: "repo"}}
	sp := PlanStale(context.Background(), &w.project, steps, Deps{Lister: l, Closer: &fakeCloser{p: &w.project}})
	if len(sp.Close) != 0 {
		t.Errorf("would close %v", sp.Close)
	}
	l[0].Name, l[0].Idle = "w-warm", true
	sp = PlanStale(context.Background(), &w.project, steps, Deps{Lister: l, Closer: &fakeCloser{p: &w.project}})
	if len(sp.Close) != 1 {
		t.Errorf("would close %v, want the window with its name", sp.Close)
	}
}

func TestUpTmuxWindowClosedOnlyWhenIdleAndQms(t *testing.T) {
	// Finding 2: a recorded tmux window that now runs something else, or
	// that came from adoption (the user's own), is never closed: a new
	// window is opened and a note says so.
	for _, c := range []struct {
		name    string
		adopted bool
		idle    bool
		closed  bool
		note    string
	}{
		{"qm's own idle window", false, true, true, ""},
		{"qm's window running something else", false, false, false, "runs something else now"},
		{"adopted idle window", true, true, false, "is your own (adopted)"},
		{"adopted busy window", true, false, false, "is your own (adopted)"},
	} {
		w := newWorld(t)
		w.project.Sessions = map[string]launch.SessionRef{coordID: {Launcher: "tmux", ID: "@4", Name: "repo-coord", Adopted: c.adopted}}
		steps := []Step{{Candidate: Candidate{Role: RoleCoordinator, SessionID: coordID, Name: "repo-coord"}, Action: Resume}}
		l := fakeLister{{Launcher: "tmux", ID: "@4", Name: "repo-coord", Workspace: "repo", Idle: c.idle}}
		sp := PlanStale(context.Background(), &w.project, steps, Deps{Lister: l, Closer: &fakeCloser{p: &w.project}})
		if got := len(sp.Close) == 1; got != c.closed {
			t.Errorf("%s: closes %v, want %v", c.name, sp.Close, c.closed)
		}
		if out := RenderStale(sp); c.note != "" && !strings.Contains(out, c.note) {
			t.Errorf("%s: RenderStale = %q, want a note %q", c.name, out, c.note)
		}
	}
}

// liveSeq is a Deps.Live that answers each read with the next of reads (the
// last one again once they run out).
func liveSeq(reads ...map[string]claudecode.LiveSession) (func() (map[string]claudecode.LiveSession, error), *int) {
	n := new(int)
	return func() (map[string]claudecode.LiveSession, error) {
		i := min(*n, len(reads)-1)
		*n++
		return reads[i], nil
	}, n
}

func TestUpReuseTabBusyAfterQuestion(t *testing.T) {
	// Finding 1: the plan saw an idle tab; by the time qm up acts, the user
	// has started something in it. Nothing is typed there: a new tab opens.
	w, f, d, steps, _ := reuseWorld(t, reuseTabs, "s-old")
	d.Stale = PlanStale(context.Background(), &w.project, steps, d)
	if _, ok := d.Stale.Reuse[warmID]; !ok {
		t.Fatalf("plan = %+v, want a reuse", d.Stale)
	}
	delete(f.idle, "s-old")
	o := outcomeOf(t, Apply(context.Background(), &w.project, steps, d), "w-warm")
	if len(f.typed) != 0 {
		t.Errorf("typed %q into a busy tab", f.typed)
	}
	if o.Err != nil || o.Ref.ID != "s-1" || !strings.Contains(o.Ref.Warning, "no longer an idle shell") {
		t.Errorf("outcome = %+v, want a new tab and a note", o)
	}
	for _, v := range f.verbs() {
		if v == "session close s-old" {
			t.Errorf("closed the busy tab: %v", f.verbs())
		}
	}
}

func TestUpSessionResumedByHandDuringQuestion(t *testing.T) {
	// Finding 1: a session the user resumed by hand while qm up asked is
	// left, whether its start was a reuse or a new tab: never started twice.
	for _, idle := range [][]string{{"s-old"}, nil} {
		w, f, d, steps, logged := reuseWorld(t, reuseTabs, idle...)
		d.Stale = PlanStale(context.Background(), &w.project, steps, d)
		d.Live, _ = liveSeq(map[string]claudecode.LiveSession{warmID: {PID: 999, SessionID: warmID}})
		o := outcomeOf(t, Apply(context.Background(), &w.project, steps, d), "w-warm")
		if o.Err != nil || o.Step.Action != Leave || !strings.Contains(o.Ref.Warning, "started running while qm up asked") {
			t.Errorf("idle %v: outcome = %+v, want a leave with a warning", idle, o)
		}
		for _, v := range f.verbs() {
			if strings.HasPrefix(v, "session new") || strings.HasPrefix(v, "session type") {
				t.Errorf("idle %v: agtermctl calls = %v, want nothing started", idle, f.verbs())
			}
		}
		if r := upRecords(*logged)["w-warm"]; r.Action != "leave" {
			t.Errorf("idle %v: up Record = %+v, want a leave", idle, r)
		}
	}
}

func TestUpReuseUnknownErrorNoFallback(t *testing.T) {
	// Finding 9: a failure that is not agterm's definite refusal may have
	// typed the line: no second tab, no close, the Outcome's error says so.
	w, f, d, steps, _ := reuseWorld(t, reuseTabs, "s-old")
	f.typeErr = errors.New("agtermctl: signal: killed")
	o := outcomeOf(t, Apply(context.Background(), &w.project, steps, d), "w-warm")
	if o.Err == nil || !strings.Contains(o.Err.Error(), "may have received the launch line") {
		t.Errorf("err = %v, want one that says the tab may have the line", o.Err)
	}
	for _, v := range f.verbs() {
		if strings.HasPrefix(v, "session new") || strings.HasPrefix(v, "session close") {
			t.Errorf("agtermctl calls = %v, want no new tab and no close", f.verbs())
		}
	}
}

func TestUpReusedTabClaudeNeverStarts(t *testing.T) {
	// The line was typed but claude never showed in the live sessions: a
	// warning. A fresh start's worker Record is logged all the same, so the
	// next plan knows the new session and, since it never ran, falls back to
	// the one it replaces (Instead).
	var logged []route.Record
	f := &fakeAgterm{ws: "repo", tree: []launch.SessionRef{{ID: "s-coord", Name: "repo-coord"}, {ID: "s-old", Name: "w-large"}}, idle: map[string]bool{"s-old": true}}
	d := upDeps(t, f, &logged)
	w := newWorld(t)
	start := now.Add(-4 * time.Hour)
	old := profiled(w, largeID, "w-large", "large", start)
	old.Session, old.Project, old.Type = "00000000-0000-4000-8000-0000000000c0", w.root, "feature"
	w.note(t, "w-large", start.Add(time.Hour))
	w.project.Sessions = map[string]launch.SessionRef{
		coordID: {Launcher: "agterm", ID: "s-coord", Name: "repo-coord"},
		largeID: {Launcher: "agterm", ID: "s-old", Name: "w-large"},
	}
	procs := []claudecode.Proc{{PID: 301, Cmd: "claude -n repo-coord"}}
	steps := w.plan(t, []route.Record{old}, procs)
	d.Live, _ = liveSeq(map[string]claudecode.LiveSession{coordID: {PID: 301}})
	slept := 0
	d.Sleep = func(time.Duration) { slept++ }
	var progress []string
	d.Progress = func(l string) { progress = append(progress, l) }
	o := outcomeOf(t, Apply(context.Background(), &w.project, steps, d), "w-large")
	if o.Err != nil || !strings.Contains(o.Ref.Warning, "typed into agterm s-old but claude did not start") {
		t.Fatalf("outcome = %+v, want the warning", o)
	}
	if slept != 30 || len(progress) != 1 {
		t.Errorf("slept %d times, progress %q; want 30 (15 seconds in steps of 500ms) and one line", slept, progress)
	}
	var rec *route.Record
	for i := range logged {
		if logged[i].Kind == "worker" {
			rec = &logged[i]
		}
	}
	if rec == nil || rec.ClaudeSession != o.NewSessionID || rec.Replaces != largeID {
		t.Fatalf("worker Record = %+v, want the new session replacing %s", rec, largeID)
	}
	next := w.plan(t, []route.Record{old, *rec}, procs)
	var found bool
	for _, s := range next {
		if s.Name == "w-large" {
			found = s.SessionID == largeID && s.Instead == o.NewSessionID
		}
	}
	if !found {
		t.Errorf("next plan %+v, want w-large on %s instead of %s", next, largeID, o.NewSessionID)
	}
}

func TestUpWaitsOnceForAllReusedTabs(t *testing.T) {
	// Finding 8 (re-review): two reused tabs share one deadline, and a
	// cancelled context stops the wait.
	var logged []route.Record
	f := &fakeAgterm{ws: "repo", tree: []launch.SessionRef{{ID: "s-coord", Name: "repo-coord"}, {ID: "s-old", Name: "w-warm"}, {ID: "s-sm", Name: "w-small"}}, idle: map[string]bool{"s-old": true, "s-sm": true}}
	d := upDeps(t, f, &logged)
	w := newWorld(t)
	w.project.Sessions = map[string]launch.SessionRef{
		coordID: {Launcher: "agterm", ID: "s-coord", Name: "repo-coord"},
		warmID:  {Launcher: "agterm", ID: "s-old", Name: "w-warm"},
		smallID: {Launcher: "agterm", ID: "s-sm", Name: "w-small"},
	}
	recs := []route.Record{profiled(w, warmID, "w-warm", "warm", now.Add(-time.Hour)), profiled(w, smallID, "w-small", "small", now.Add(-time.Hour))}
	steps := w.plan(t, recs, []claudecode.Proc{{PID: 301, Cmd: "claude -n repo-coord"}})
	starts := 0
	for _, s := range steps {
		if s.Action == Resume || s.Action == Fresh {
			starts++
		}
	}
	if starts != 2 {
		t.Fatalf("plan %+v, want two starts", steps)
	}
	d.Live, _ = liveSeq(map[string]claudecode.LiveSession{})
	slept := 0
	d.Sleep = func(time.Duration) { slept++ }
	outs := Apply(context.Background(), &w.project, steps, d)
	if slept != 30 {
		t.Errorf("slept %d times for two tabs, want 30: one 15-second deadline", slept)
	}
	for _, n := range []string{"w-warm", "w-small"} {
		if o := outcomeOf(t, outs, n); !strings.Contains(o.Ref.Warning, "did not start") {
			t.Errorf("%s: %+v, want the warning", n, o)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	slept = 0
	o := []Outcome{{Ref: launch.SessionRef{ID: "s-x"}, waitFor: "x"}}
	waitStarted(ctx, d, o)
	if slept != 0 || !strings.Contains(o[0].Ref.Warning, "stopped before claude was seen") {
		t.Errorf("cancelled: slept %d, warning %q", slept, o[0].Ref.Warning)
	}
}

func TestUpLeftStepDropsRefOfOtherTab(t *testing.T) {
	// Finding 3 (re-review): the session was resumed by hand in another tab
	// while qm up asked; its recorded tab does not run it: the ref is
	// dropped, so qm down never closes that tab.
	w, _, d, steps, _ := reuseWorld(t, reuseTabs, "s-old")
	d.Live, _ = liveSeq(map[string]claudecode.LiveSession{warmID: {PID: 999}})
	o := outcomeOf(t, Apply(context.Background(), &w.project, steps, d), "w-warm")
	if o.Step.Action != Leave {
		t.Fatalf("outcome %+v, want a leave", o)
	}
	if _, ok := w.project.Sessions[warmID]; ok {
		t.Errorf("the ref of a tab that does not run the session was kept: %v", w.project.Sessions)
	}
	if _, ok := savedProject(t, w.project).Sessions[warmID]; ok {
		t.Errorf("the drop was not saved")
	}
}

func TestUpInsteadSessionLiveIsLeft(t *testing.T) {
	// Finding 4 (re-review): the step stands in for NEW, a fresh start that
	// never ran; NEW starts while qm up asks: the step is left, not run.
	w, f, d, steps, _ := reuseWorld(t, reuseTabs)
	for i := range steps {
		if steps[i].Name == "w-warm" {
			steps[i].Instead = "00000000-0000-4000-8000-0000000000ff"
		}
	}
	d.Live, _ = liveSeq(map[string]claudecode.LiveSession{"00000000-0000-4000-8000-0000000000ff": {PID: 998}})
	o := outcomeOf(t, Apply(context.Background(), &w.project, steps, d), "w-warm")
	if o.Step.Action != Leave || !strings.Contains(o.Ref.Warning, "started running while qm up asked") {
		t.Errorf("outcome %+v, want a leave", o)
	}
	for _, v := range f.verbs() {
		if strings.HasPrefix(v, "session new") || strings.HasPrefix(v, "session type") {
			t.Errorf("started: %v", f.verbs())
		}
	}
}

func TestUpReusedTabClaudeStarts(t *testing.T) {
	// Finding 11: once the session shows, no warning and no more waiting.
	w, _, d, steps, _ := reuseWorld(t, reuseTabs, "s-old")
	d.Live, _ = liveSeq(map[string]claudecode.LiveSession{}, map[string]claudecode.LiveSession{}, map[string]claudecode.LiveSession{warmID: {PID: 7}})
	slept := 0
	d.Sleep = func(time.Duration) { slept++ }
	o := outcomeOf(t, Apply(context.Background(), &w.project, steps, d), "w-warm")
	if o.Err != nil || o.Ref.ID != "s-old" || o.Ref.Warning != "" || slept != 1 {
		t.Errorf("outcome = %+v, slept %d, want the reuse with no warning after one wait", o, slept)
	}
}

type fakeLister []launch.SessionRef

func (f fakeLister) Sessions(context.Context, string) ([]launch.SessionRef, error) { return f, nil }

func TestUpPrunesUnlistedRefs(t *testing.T) {
	// A recorded terminal session the workspace no longer shows is dropped,
	// unless its session is live (pinned again wherever it is).
	var logged []route.Record
	f := &fakeAgterm{ws: "repo"}
	d := upDeps(t, f, &logged)
	w := newWorld(t)
	w.project.Sessions = map[string]launch.SessionRef{
		coordID: {Launcher: "agterm", ID: "s-coord"},
		"gone":  {Launcher: "agterm", ID: "s-old"},
		"tmuxy": {Launcher: "tmux", ID: "@3"},
	}
	recs := []route.Record{profiled(w, warmID, "w-warm", "warm", now.Add(-time.Hour))}
	steps := w.plan(t, recs, []claudecode.Proc{{PID: 301, Cmd: "claude -n repo-coord"}})
	Apply(context.Background(), &w.project, steps, d)
	if _, ok := w.project.Sessions["gone"]; ok {
		t.Errorf("an unlisted ref was kept: %v", w.project.Sessions)
	}
	if _, ok := w.project.Sessions[coordID]; !ok {
		t.Errorf("the live coordinator's ref was dropped")
	}
	if _, ok := w.project.Sessions["tmuxy"]; !ok {
		t.Errorf("a ref of another launcher was dropped without a listing of it")
	}
}

func TestUpNeverClosesItsOwnSession(t *testing.T) {
	var logged []route.Record
	f := &fakeAgterm{ws: "repo", tree: []launch.SessionRef{{ID: "s-here", Name: "repo-coord"}}}
	d := upDeps(t, f, &logged)
	d.Self = []string{"s-here"}
	f.idle = map[string]bool{"s-here": true}
	w := newWorld(t)
	w.project.Sessions = map[string]launch.SessionRef{coordID: {Launcher: "agterm", ID: "s-here", Name: "repo-coord"}}
	steps := w.plan(t, nil, nil)
	Apply(context.Background(), &w.project, steps, d)
	for _, v := range f.verbs() {
		if v == "session close s-here" || v == "session type s-here" {
			t.Errorf("closed or typed into the session qm up runs in: %v", f.verbs())
		}
	}
}

func TestUpOneFailureOthersGoOn(t *testing.T) {
	var logged []route.Record
	f := &fakeAgterm{ws: "repo", fail: map[string]bool{"w-small": true}}
	d := upDeps(t, f, &logged)
	w := newWorld(t)
	recs := []route.Record{
		profiled(w, smallID, "w-small", "small", now.Add(-3*time.Hour)),
		profiled(w, warmID, "w-warm", "warm", now.Add(-2*time.Hour)),
	}
	steps := w.plan(t, recs, []claudecode.Proc{{PID: 301, Cmd: "claude -n repo-coord"}})
	outs := Apply(context.Background(), &w.project, steps, d)
	if o := outcomeOf(t, outs, "w-small"); o.Err == nil {
		t.Errorf("w-small: want a failure, got %+v", o)
	}
	if o := outcomeOf(t, outs, "w-warm"); o.Err != nil || o.Ref.ID == "" {
		t.Errorf("w-warm: %+v", o)
	}
	saved := savedProject(t, w.project)
	if _, ok := saved.Sessions[warmID]; !ok || len(saved.Sessions) != 1 {
		t.Errorf("saved sessions = %+v, want the started worker only", saved.Sessions)
	}
	if _, ok := upRecords(logged)["w-small"]; ok {
		t.Errorf("a failed start was logged as done")
	}
}

func TestUpLogsLeaveAndSkip(t *testing.T) {
	var logged []route.Record
	f := &fakeAgterm{ws: "repo"}
	d := upDeps(t, f, &logged)
	w := newWorld(t)
	w.project.Done = map[string]time.Time{smallID: now}
	recs := []route.Record{profiled(w, smallID, "w-done", "small", now.Add(-time.Hour))}
	cs, err := Gather(&w.project, recs, func() Env {
		e := w.env(t, []claudecode.Proc{{PID: 301, Cmd: "claude -n repo-coord"}})
		e.Finished = Finished(w.project, nil)
		return e
	}())
	if err != nil {
		t.Fatal(err)
	}
	Apply(context.Background(), &w.project, Plan(cs, false, now), d)
	got := upRecords(logged)
	if got["repo-coord"].Action != "leave" || got["w-done"].Action != "skip" || got["w-done"].Detail != "finished: marked done" {
		t.Errorf("up Records = %+v", got)
	}
	for _, r := range got {
		if r.UpProject != w.project.ID || r.Time != now || r.ClaudeSession == "" {
			t.Errorf("up Record %+v lacks its project, time or session", r)
		}
	}
}

func TestUpNoLauncherWritesScripts(t *testing.T) {
	var logged []route.Record
	f := &fakeAgterm{ws: "repo"}
	d := upDeps(t, f, &logged)
	d.Launcher, d.Lister, d.Closer, d.Pin = nil, nil, nil, nil
	w := newWorld(t)
	recs := []route.Record{profiled(w, warmID, "w-warm", "warm", now.Add(-time.Hour))}
	steps := w.plan(t, recs, nil)
	outs := Apply(context.Background(), &w.project, steps, d)
	for _, o := range outs {
		if o.Err != nil || o.Script == "" || o.Ref.ID != "" {
			t.Errorf("outcome %+v: want a script and no session", o)
		}
		if _, err := os.Stat(o.Script); err != nil {
			t.Errorf("script not written: %v", err)
		}
	}
	if len(logged) != 0 || len(w.project.Sessions) != 0 {
		t.Errorf("recorded as started: %d Records, sessions %v", len(logged), w.project.Sessions)
	}
	if dir, _ := projectsDir(); fileExists(filepath.Join(dir, w.project.ID+".json")) {
		t.Errorf("state saved without a launcher")
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Without a launcher, fresh sessions keep their new ids, so running the
// printed lines and then qm up again leaves them alone.
func TestUpNoLauncherRecordsFreshIDs(t *testing.T) {
	var logged []route.Record
	f := &fakeAgterm{}
	d := upDeps(t, f, &logged)
	d.Launcher, d.Lister, d.Closer, d.Pin = nil, nil, nil, nil
	w := newWorld(t)
	fixture(t, "cold-large.jsonl", w.root, w.transcripts[coordID])
	start := now.Add(-4 * time.Hour)
	recs := []route.Record{profiled(w, largeID, "w-large", "large", start)}
	w.note(t, "w-large", start.Add(time.Hour))
	outs := Apply(context.Background(), &w.project, w.plan(t, recs, nil), d)
	coord, worker := outcomeOf(t, outs, "repo-coord"), outcomeOf(t, outs, "w-large")
	for _, o := range []Outcome{coord, worker} {
		if o.Err != nil || o.Step.Action != Fresh || o.NewSessionID == "" || o.Script == "" || o.Ref.ID != "" {
			t.Fatalf("outcome = %+v", o)
		}
	}
	saved := savedProject(t, w.project)
	if saved.Coordinator.ID != coord.NewSessionID || !slices.Equal(saved.PastCoordinators, []string{coordID}) || len(saved.Sessions) != 0 {
		t.Errorf("saved = %+v", saved)
	}
	var kinds []string
	for _, r := range logged {
		kinds = append(kinds, r.Kind)
	}
	if !slices.Equal(kinds, []string{"worker"}) || logged[0].Replaces != largeID || logged[0].ClaudeSession != worker.NewSessionID {
		t.Fatalf("logged %+v, want only the replacing worker Record", logged)
	}
	// The user runs the two lines; the next qm up finds both live.
	procs := []claudecode.Proc{
		{PID: 401, Cmd: "claude -n repo-coord --session-id " + coord.NewSessionID},
		{PID: 402, Cmd: "claude --model claude-sonnet -n w-large --session-id " + worker.NewSessionID},
	}
	steps := w.plan(t, append(recs, logged...), procs)
	if len(steps) != 2 {
		t.Fatalf("later plan = %+v", steps)
	}
	for _, s := range steps {
		if s.Action != Leave || (s.SessionID != coord.NewSessionID && s.SessionID != worker.NewSessionID) {
			t.Errorf("later step %s %s: %s, want leave on the new id", s.Name, s.SessionID, s.Action)
		}
	}
}

// Without a launcher the printed lines were never run: the fresh sessions
// have no transcript and no process. A later run restores the worker from the
// session it replaces (its transcript and handoff note), not as "transcript
// missing"; the fresh start replaces the never-run session, so on every later
// run, with a launcher or without, at most one session of the worker starts.
func TestUpFreshNeverRunFallsBackToReplaced(t *testing.T) {
	var logged []route.Record
	f := &fakeAgterm{ws: "repo"}
	d := upDeps(t, f, &logged)
	none := d
	none.Launcher, none.Lister, none.Closer, none.Pin = nil, nil, nil, nil
	w := newWorld(t)
	start := now.Add(-4 * time.Hour)
	recs := []route.Record{profiled(w, largeID, "w-large", "large", start)}
	note := w.note(t, "w-large", start.Add(time.Hour))
	coordLive := claudecode.Proc{PID: 301, Cmd: "claude -n repo-coord"}
	tick := 0
	run := func(dd Deps, procs ...claudecode.Proc) (Step, Outcome) {
		t.Helper()
		logged = nil
		tick++
		at := now.Add(time.Duration(tick) * time.Second) // each run logs later
		dd.Now = func() time.Time { return at }
		steps := w.plan(t, recs, append(procs, coordLive))
		var mine []Step
		for _, s := range steps {
			if s.Name == "w-large" {
				mine = append(mine, s)
			}
		}
		if len(mine) != 1 {
			t.Fatalf("plan has %d w-large rows: %+v", len(mine), mine)
		}
		o := outcomeOf(t, Apply(context.Background(), &w.project, steps, dd), "w-large")
		if o.Err != nil {
			t.Fatalf("outcome = %+v", o)
		}
		recs = append(recs, logged...)
		return mine[0], o
	}
	starts := func() int {
		n := 0
		for _, v := range f.verbs() {
			if v == "session new w-large" {
				n++
			}
		}
		return n
	}

	// Run 1, no launcher: B is printed, never run.
	_, o1 := run(none)
	b := o1.NewSessionID

	// Run 2, launcher: A again, fresh from its note; C replaces B.
	s2, o2 := run(d)
	if s2.SessionID != largeID || s2.Instead != b || s2.Transcript != w.transcripts[largeID] || s2.Handoff != note || s2.Action != Fresh {
		t.Fatalf("run 2 step = %+v, want A standing in for B", s2)
	}
	if !strings.Contains(promptOf(t, readFile(t, o2.Script)), "Done: the parser. Left: its tests.") {
		t.Errorf("the fresh start lacks the old handoff note")
	}
	var nr *route.Record
	for i := range logged {
		if logged[i].Kind == "worker" {
			nr = &logged[i]
		}
	}
	if nr == nil || nr.Replaces != b || nr.ClaudeSession != o2.NewSessionID {
		t.Fatalf("run 2 Record = %+v, want C replacing B", nr)
	}
	c := o2.NewSessionID

	// Run 3, launcher: C runs; it is left alone.
	if s3, _ := run(d, claudecode.Proc{PID: 402, Cmd: "claude -n w-large --session-id " + c}); s3.SessionID != c || s3.Action != Leave {
		t.Errorf("run 3 step = %+v, want C left running", s3)
	}

	// Run 4, no launcher: C has ended with a transcript; it is resumed (a
	// script only), never A.
	w.transcripts[c] = filepath.Join(w.claude, "projects", "p", c+".jsonl")
	fixture(t, "warm.jsonl", w.root, w.transcripts[c])
	if s4, _ := run(none); s4.SessionID != c || s4.Action != Resume {
		t.Errorf("run 4 step = %+v, want C resumed", s4)
	}
	if n := starts(); n != 1 {
		t.Errorf("w-large started %d times, want once", n)
	}
}

// Two runs without a launcher chain C -> B -> A: the next run with a launcher
// starts one session from A, and later runs none.
func TestUpTwoNoLauncherRunsStartOnce(t *testing.T) {
	var logged []route.Record
	f := &fakeAgterm{ws: "repo"}
	d := upDeps(t, f, &logged)
	none := d
	none.Launcher, none.Lister, none.Closer, none.Pin = nil, nil, nil, nil
	w := newWorld(t)
	start := now.Add(-4 * time.Hour)
	recs := []route.Record{profiled(w, largeID, "w-large", "large", start)}
	w.note(t, "w-large", start.Add(time.Hour))
	coordLive := []claudecode.Proc{{PID: 301, Cmd: "claude -n repo-coord"}}
	var newIDs []string
	for i, dd := range []Deps{none, none, d, d} {
		logged = nil
		at := now.Add(time.Duration(i+1) * time.Second) // each run logs later
		dd.Now = func() time.Time { return at }
		steps := w.plan(t, recs, coordLive)
		if len(steps) != 2 {
			t.Fatalf("run %d: plan = %v, want one worker row", i+1, ids(func() []Candidate {
				var cs []Candidate
				for _, s := range steps {
					cs = append(cs, s.Candidate)
				}
				return cs
			}()))
		}
		o := outcomeOf(t, Apply(context.Background(), &w.project, steps, dd), "w-large")
		if o.Err != nil {
			t.Fatalf("run %d: %+v", i+1, o)
		}
		newIDs = append(newIDs, o.NewSessionID)
		recs = append(recs, logged...)
		if i == 2 {
			// The session started in run 3 runs from then on.
			coordLive = append(coordLive, claudecode.Proc{PID: 403, Cmd: "claude -n w-large --session-id " + o.NewSessionID})
		}
	}
	n := 0
	for _, v := range f.verbs() {
		if v == "session new w-large" {
			n++
		}
	}
	if n != 1 || newIDs[3] != "" {
		t.Errorf("w-large started %d times (new ids %v), want once", n, newIDs)
	}
}

func TestGatherKeepsReplacingSessionThatRan(t *testing.T) {
	w := newWorld(t)
	old := rec(w.project.ID, largeID, "w1", now.Add(-3*time.Hour))
	repl := rec(w.project.ID, warmID, "w1", now.Add(-time.Hour))
	repl.Replaces = largeID
	cs, err := Gather(&w.project, []route.Record{old, repl}, w.env(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 || cs[1].SessionID != warmID {
		t.Errorf("gather = %v, want the replacing session (it has a transcript)", ids(cs))
	}
}

// Without a launcher the fresh coordinator's line was never run: a later run
// with a launcher restores the newest past coordinator with a transcript,
// which becomes the project's coordinator again.
func TestUpCoordinatorNeverRunFallsBackToPast(t *testing.T) {
	var logged []route.Record
	f := &fakeAgterm{ws: "repo"}
	d := upDeps(t, f, &logged)
	none := d
	none.Launcher, none.Lister, none.Closer, none.Pin = nil, nil, nil, nil
	w := newWorld(t)
	fixture(t, "cold-large.jsonl", w.root, w.transcripts[coordID])
	first := outcomeOf(t, Apply(context.Background(), &w.project, w.plan(t, nil, nil), none), "repo-coord")
	if first.Err != nil || first.NewSessionID == "" || w.project.Coordinator.ID != first.NewSessionID {
		t.Fatalf("no-launcher run: %+v, coordinator %+v", first, w.project.Coordinator)
	}

	// The line was never run. The old coordinator's context is small by now
	// (compacted), so the later run resumes it.
	fixture(t, "cold-small.jsonl", w.root, w.transcripts[coordID])
	steps := w.plan(t, nil, nil)
	if s := steps[0]; s.SessionID != coordID || s.Transcript != w.transcripts[coordID] || s.Action != Resume {
		t.Fatalf("later step = %+v, want the past coordinator resumed", s)
	}
	o := outcomeOf(t, Apply(context.Background(), &w.project, steps, d), "repo-coord")
	if o.Err != nil || o.Ref.ID == "" || !strings.Contains(readFile(t, o.Script), "--resume "+coordID) {
		t.Fatalf("outcome = %+v", o)
	}
	saved := savedProject(t, w.project)
	if saved.Coordinator.ID != coordID || slices.Contains(saved.PastCoordinators, coordID) || saved.Sessions[coordID].ID != o.Ref.ID {
		t.Errorf("saved = %+v, want the past coordinator current again", saved)
	}
}

// A past coordinator restored by the fallback starts at its transcript's
// first timestamp: a handoff note older than that is not its own.
func TestUpPastCoordinatorStartsAtItsTranscript(t *testing.T) {
	var logged []route.Record
	d := upDeps(t, &fakeAgterm{ws: "repo"}, &logged)
	d.Launcher, d.Lister, d.Closer, d.Pin = nil, nil, nil, nil
	w := newWorld(t)
	fixture(t, "cold-large.jsonl", w.root, w.transcripts[coordID])
	Apply(context.Background(), &w.project, w.plan(t, nil, nil), d)
	if w.project.Coordinator.ID == coordID {
		t.Fatal("no fresh coordinator recorded")
	}
	first := time.Date(2026, 11, 20, 6, 50, 0, 0, time.UTC) // cold-large.jsonl's first line
	w.note(t, "repo-coord", first.Add(-time.Minute))
	s := w.plan(t, nil, nil)[0]
	if s.SessionID != coordID || !s.Started.Equal(first) || s.Handoff != "" {
		t.Errorf("step = %s started %v, note %q; want the past coordinator from %v, the older note ignored", s.SessionID, s.Started, s.Handoff, first)
	}
	note := w.note(t, "repo-coord", first.Add(time.Minute))
	if s := w.plan(t, nil, nil)[0]; s.Handoff != note {
		t.Errorf("a note written after the past coordinator started is not used: %q", s.Handoff)
	}
}

func TestAdoptedArgsCapped(t *testing.T) {
	acceptEdits := []string{"--permission-mode", "acceptEdits"}
	cases := []struct {
		name        string
		args, coord []string
		want        string
	}{
		{"bypass under acceptEdits", []string{"--dangerously-skip-permissions", "--model", "opus"}, acceptEdits, "--permission-mode acceptEdits --model opus"},
		{"allow dropped without coordinator bypass", []string{"--allow-dangerously-skip-permissions", "--permission-mode", "acceptEdits", "--effort", "high"}, acceptEdits, "--permission-mode acceptEdits --effort high"},
		{"allow kept under coordinator bypass", []string{"--allow-dangerously-skip-permissions", "--permission-mode", "acceptEdits"}, []string{"--dangerously-skip-permissions"}, "--permission-mode acceptEdits --allow-dangerously-skip-permissions"},
		{"allow kept under coordinator allow", []string{"--model", "opus", "--allow-dangerously-skip-permissions"}, []string{"--allow-dangerously-skip-permissions", "--permission-mode", "acceptEdits"}, "--allow-dangerously-skip-permissions --model opus"},
		{"mode under no-flag coordinator", []string{"--permission-mode=acceptEdits", "--effort", "high"}, nil, "--effort high"},
		{"no permission flags", []string{"--model", "opus"}, acceptEdits, "--model opus"},
		{"plan kept under acceptEdits", []string{"--permission-mode", "plan", "--model", "opus"}, acceptEdits, "--permission-mode plan --model opus"},
		{"default kept under acceptEdits", []string{"--permission-mode", "default"}, acceptEdits, "--permission-mode default"},
		{"plan kept under bypass", []string{"--permission-mode=plan"}, []string{"--dangerously-skip-permissions"}, "--permission-mode plan"},
		{"acceptEdits lowered to plan", []string{"--permission-mode", "acceptEdits"}, []string{"--permission-mode", "plan"}, "--permission-mode plan"},
		{"acceptEdits lowered to dontAsk", []string{"--permission-mode", "acceptEdits"}, []string{"--permission-mode", "dontAsk"}, "--permission-mode dontAsk"},
		{"dontAsk kept under acceptEdits", []string{"--permission-mode", "dontAsk"}, acceptEdits, "--permission-mode dontAsk"},
	}
	for _, tc := range cases {
		args, err := adoptedArgs(tc.args, tc.coord)
		if got := strings.Join(args, " "); err != nil || got != tc.want {
			t.Errorf("%s: %q, %v, want %q", tc.name, got, err, tc.want)
		}
	}
	// plan and dontAsk have no mode in common: refused, in both orders.
	for _, pair := range [][2]string{{"plan", "dontAsk"}, {"dontAsk", "plan"}} {
		if got, err := adoptedArgs([]string{"--permission-mode", pair[0]}, []string{"--permission-mode", pair[1]}); err == nil {
			t.Errorf("%s under %s: %q, want a refusal", pair[0], pair[1], got)
		}
	}
}

func TestUpResumesHandStarted(t *testing.T) {
	var logged []route.Record
	f := &fakeAgterm{ws: "repo"}
	d := upDeps(t, f, &logged)
	w := newWorld(t)
	w.project.ClaudeArgs = []string{"--permission-mode", "acceptEdits"}
	cwd := filepath.Join(w.root, "worktrees", "small")
	w.project.Adopted = map[string]AdoptedSession{
		smallID: {Name: "w-hand", Cwd: cwd, Model: "opus", Args: []string{"--dangerously-skip-permissions", "--model", "opus"}, Started: now.Add(-4 * time.Hour)},
	}
	steps := w.plan(t, nil, []claudecode.Proc{{PID: 301, Cmd: "claude -n repo-coord"}})
	outs := Apply(context.Background(), &w.project, steps, d)
	o := outcomeOf(t, outs, "w-hand")
	if o.Err != nil || o.Step.Action != Resume {
		t.Fatalf("outcome = %+v", o)
	}
	n := 0
	for _, v := range f.verbs() {
		if strings.HasPrefix(v, "session new") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("verbs = %v, want one session new", f.verbs())
	}
	script := readFile(t, o.Script)
	want := "exec env QM_PROJECT_ID=" + w.project.ID + " QM_WORKSPACE=repo claude --permission-mode acceptEdits --model opus -n w-hand --resume " + smallID + "\n"
	if !strings.HasSuffix(script, want) || !strings.Contains(script, "cd "+cwd+" ") || strings.Contains(script, "QM_ROLE") {
		t.Errorf("script =\n%s\nwant it to end with\n%s", script, want)
	}
	if got := w.project.Sessions[smallID]; got.ID != "s-1" {
		t.Errorf("state session = %+v", got)
	}
	if r := upRecords(logged)["w-hand"]; r.Action != "resume" || r.ClaudeSession != smallID {
		t.Errorf("up Record = %+v", r)
	}
	for _, r := range logged {
		if r.Kind == "worker" {
			t.Errorf("a worker Record was logged: %+v", r)
		}
	}
}

func TestAdoptedArgsDropsUnkept(t *testing.T) {
	args := []string{"--settings", `{"permissions":{"defaultMode":"bypassPermissions"}}`, "--allowedTools", "Bash", "--model=opus", "--effort", "high", "--verbose"}
	kept, err := adoptedArgs(args, nil)
	if got := strings.Join(kept, " "); err != nil || got != "--model=opus --effort high" {
		t.Errorf("adoptedArgs = %q, %v, want only the kept flags", got, err)
	}
}

func TestUpHandStartedBadNameNotStarted(t *testing.T) {
	var logged []route.Record
	f := &fakeAgterm{ws: "repo"}
	d := upDeps(t, f, &logged)
	w := newWorld(t)
	w.project.Adopted = map[string]AdoptedSession{
		smallID: {Name: "feat/login", Cwd: filepath.Join(w.root, "worktrees", "small"), Started: now.Add(-4 * time.Hour)},
	}
	steps := w.plan(t, nil, []claudecode.Proc{{PID: 301, Cmd: "claude -n repo-coord"}})
	o := outcomeOf(t, Apply(context.Background(), &w.project, steps, d), "feat/login")
	if o.Err == nil || !strings.Contains(o.Err.Error(), "claude --resume "+smallID) {
		t.Errorf("outcome err = %v, want a refusal with the manual line", o.Err)
	}
	for _, v := range f.verbs() {
		if strings.HasPrefix(v, "session new") {
			t.Errorf("started %v", f.verbs())
		}
	}
}

// seqLister lists each of lists in turn, the last again after that.
type seqLister struct {
	lists [][]launch.SessionRef
	n     int
}

func (s *seqLister) Sessions(context.Context, string) ([]launch.SessionRef, error) {
	l := s.lists[min(s.n, len(s.lists)-1)]
	s.n++
	return l, nil
}

// A stale tmux window is listed again right before it is closed: one that
// started a job, gained a pane or is another window by now (the prompt was
// open meanwhile) is left open.
func TestUpStaleWindowRecheckedBeforeClose(t *testing.T) {
	before := launch.SessionRef{Launcher: "tmux", ID: "@4", Name: "repo-coord", Workspace: "repo", Idle: true, Panes: 1}
	busy, pane, renamed := before, before, before
	busy.Idle = false
	pane.Idle, pane.Panes = false, 2
	renamed.Name = "vim"
	for _, c := range []struct {
		name   string
		now    launch.SessionRef
		closed bool
		warns  bool
	}{
		{"still idle", before, true, false},
		{"a job started", busy, false, true},
		{"a pane added", pane, false, true},
		{"another window", renamed, false, false},
	} {
		w := newWorld(t)
		w.project.Sessions = map[string]launch.SessionRef{coordID: {Launcher: "tmux", ID: "@4", Name: "repo-coord"}}
		steps := []Step{{Candidate: Candidate{Role: RoleCoordinator, SessionID: coordID, Name: "repo-coord"}, Action: Resume}}
		closer := &fakeCloser{p: &w.project}
		d := Deps{Lister: &seqLister{lists: [][]launch.SessionRef{{before}, {c.now}}}, Closer: closer}
		sp := PlanStale(context.Background(), &w.project, steps, d)
		if len(sp.Close) != 1 {
			t.Fatalf("%s: planned closes %v", c.name, sp.Close)
		}
		warn := closeStale(context.Background(), sp, d)
		if got := slices.Contains(closer.closed, "@4"); got != c.closed {
			t.Errorf("%s: closed %v, want %v", c.name, closer.closed, c.closed)
		}
		if got := len(warn[coordID]) > 0; got != c.warns {
			t.Errorf("%s: warnings %q, want some: %v", c.name, warn[coordID], c.warns)
		}
	}
}
