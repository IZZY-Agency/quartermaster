package up

import (
	"bufio"
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
	"github.com/IZZY-Agency/quartermaster/internal/outcomes"
	"github.com/IZZY-Agency/quartermaster/internal/route"
)

// The sessions of the adoption fixtures (testdata/sessions/411..413.json).
const (
	adCoord = "00000000-0000-4000-8000-000000000411"
	adQM    = "00000000-0000-4000-8000-000000000412"
	adHand  = "00000000-0000-4000-8000-000000000413"
	adSide  = "00000000-0000-4000-8000-000000000414"
)

// adoptTerm is a fake agtermctl: tree lists ws; every call is recorded.
type adoptTerm struct {
	ws    []launch.Workspace
	calls []string
	fail  map[string]bool // by verb ("session move", "session restore")
	idle  map[string]bool // session ids listed as idle shells
	// fg and cwd are each tab's foreground argv and folder, by session id.
	fg  map[string][]string
	cwd map[string]string
}

func (f *adoptTerm) run(_ context.Context, _ string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	verb := args[0]
	if len(args) > 1 && args[0] == "session" {
		verb = "session " + args[1]
	}
	if f.fail[verb] {
		return []byte(`{"ok":false,"error":"refused"}`), nil
	}
	var result any = map[string]any{}
	if verb == "tree" {
		var wss []map[string]any
		for _, w := range f.ws {
			var ss []map[string]any
			for _, s := range w.Sessions {
				e := map[string]any{"id": s.ID, "name": s.Name}
				if f.idle[s.ID] {
					e["foregroundShell"] = "zsh"
					e["foreground"] = []string{"-zsh"}
				} else if fg, ok := f.fg[s.ID]; ok {
					e["foreground"] = fg
				}
				if c, ok := f.cwd[s.ID]; ok {
					e["cwd"] = c
				}
				ss = append(ss, e)
			}
			wss = append(wss, map[string]any{"id": w.ID, "name": w.Name, "sessions": ss})
		}
		result = map[string]any{"tree": map[string]any{"workspaces": wss}}
	}
	return json.Marshal(map[string]any{"ok": true, "result": result})
}

// verbs are the calls' first two words (tree, session restore, ...).
func (f *adoptTerm) verbs() []string {
	var out []string
	for _, c := range f.calls {
		w := strings.Fields(c)
		if len(w) > 1 && w[0] == "session" {
			out = append(out, w[0]+" "+w[1])
			continue
		}
		out = append(out, w[0])
	}
	return out
}

func (f *adoptTerm) has(call string) bool { return slices.Contains(f.calls, call) }

// adoptWorld is a machine with a coordinator in the project root, a qm
// worker and a hand-started worker in linked worktrees, all live in agterm
// workspace proj.
type adoptWorld struct {
	root, repo, claude, wtQM, wtHand string
	procs                            []claudecode.Proc
	recs                             []route.Record
	all                              []Project
	term                             map[int]claudecode.TermEnv
	panes                            map[string][3]string
	windowPanes                      map[string]int // by window id; 1 when absent
	agterm                           *adoptTerm
	untrusted                        map[string]bool
	logged                           []route.Record
	selfPID                          int
	starts                           map[string]time.Time
}

func newAdoptWorld(t *testing.T) *adoptWorld {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := &adoptWorld{
		root: root, repo: filepath.Join(root, "repo"), claude: filepath.Join(root, "claude"),
		wtQM: filepath.Join(root, "wt", "w-qm"), wtHand: filepath.Join(root, "wt", "w-hand"),
		term: map[int]claudecode.TermEnv{
			411: {AgtermSession: "s-c"}, 412: {AgtermSession: "s-q"}, 413: {AgtermSession: "s-h"},
		},
		panes:     map[string][3]string{},
		untrusted: map[string]bool{},
		starts: map[string]time.Time{
			adCoord: now.Add(-6 * time.Hour), adQM: now.Add(-3 * time.Hour), adHand: now.Add(-2 * time.Hour), adSide: now.Add(-time.Hour),
		},
	}
	mkdir(t, w.repo, w.wtQM, w.wtHand, filepath.Join(root, "elsewhere"))
	for _, n := range []string{"411", "412", "413"} {
		fixture(t, filepath.Join("sessions", n+".json"), root, filepath.Join(w.claude, "sessions", n+".json"))
	}
	for id, at := range w.starts {
		w.transcript(t, id, at)
	}
	w.procs = []claudecode.Proc{
		{PID: 411, PPID: 1, Cmd: "claude -n proj-coord --model opus --plugin-dir /p --dangerously-skip-permissions"},
		{PID: 412, PPID: 1, Cmd: "claude --model sonnet --effort low"},
		{PID: 413, PPID: 1, Cmd: "claude --permission-mode acceptEdits --model haiku --mcp-config a.json"},
	}
	w.recs = []route.Record{{
		Time: now.Add(-3 * time.Hour), Kind: "worker", ClaudeSession: adQM, Name: "w-qm", Cwd: w.wtQM,
		Decision: &route.Decision{},
	}}
	w.agterm = &adoptTerm{ws: []launch.Workspace{
		{ID: "ws-1", Name: "proj", Sessions: []launch.SessionRef{{ID: "s-c", Name: "proj-coord"}, {ID: "s-q", Name: "w-qm"}, {ID: "s-h", Name: "w-hand"}}},
		{ID: "ws-2", Name: "other"},
	}}
	// Each tab runs its session's claude, in its folder.
	w.agterm.fg, w.agterm.cwd = map[string][]string{}, map[string]string{}
	for i, tab := range []string{"s-c", "s-q", "s-h"} {
		w.agterm.fg[tab] = strings.Fields(w.procs[i].Cmd)
	}
	w.agterm.cwd["s-c"], w.agterm.cwd["s-q"], w.agterm.cwd["s-h"] = w.repo, w.wtQM, w.wtHand
	return w
}

// transcript writes a minimal transcript for id, first written at at.
func (w *adoptWorld) transcript(t *testing.T, id string, at time.Time) {
	t.Helper()
	path := filepath.Join(w.claude, "projects", "p", id+".jsonl")
	mkdir(t, filepath.Dir(path))
	line := fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"usage":{"input_tokens":1000,"cache_creation_input_tokens":0,"cache_read_input_tokens":49000}}}`+"\n", at.Format(time.RFC3339))
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
}

// session writes a session file for pid.
func (w *adoptWorld) session(t *testing.T, pid int, id, cwd, name string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"pid": pid, "sessionId": id, "cwd": cwd, "name": name})
	path := filepath.Join(w.claude, "sessions", fmt.Sprint(pid)+".json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// side adds a fourth live session in the root, which nobody picks.
func (w *adoptWorld) side(t *testing.T) {
	w.session(t, 414, adSide, w.repo, "side")
	w.procs = append(w.procs, claudecode.Proc{PID: 414, PPID: 1, Cmd: "claude"})
	w.term[414] = claudecode.TermEnv{AgtermSession: "s-x"}
	w.agterm.ws[1].Sessions = append(w.agterm.ws[1].Sessions, launch.SessionRef{ID: "s-x", Name: "side"})
	w.agterm.fg["s-x"], w.agterm.cwd["s-x"] = []string{"claude"}, w.repo
}

func (w *adoptWorld) transcriptPath(id string) (string, error) {
	path := filepath.Join(w.claude, "projects", "p", id+".jsonl")
	if _, err := os.Stat(path); err != nil {
		return "", nil
	}
	return path, nil
}

func (w *adoptWorld) live(t *testing.T) map[string]claudecode.LiveSession {
	t.Helper()
	live, err := claudecode.Live(w.claude, w.procs)
	if err != nil {
		t.Fatal(err)
	}
	return live
}

func (w *adoptWorld) detect(t *testing.T) []AdoptCandidate {
	t.Helper()
	return w.detected(t).Candidates
}

func (w *adoptWorld) detected(t *testing.T) Detected {
	t.Helper()
	return Detect(w.repo, w.all, w.recs, DetectEnv{
		Procs: w.procs, Live: w.live(t), Worktrees: []string{w.repo, w.wtQM, w.wtHand},
		TranscriptPath: w.transcriptPath, SelfPID: w.selfPID,
	})
}

func (w *adoptWorld) deps() AdoptDeps {
	a := launch.Agterm{Bin: "agtermctl", Run: w.agterm.run}
	return AdoptDeps{
		Workspaces: a.Workspaces, Move: a.Move, Pin: a.Pin,
		PaneOf: func(_ context.Context, pane string) (string, string, string, error) {
			p, ok := w.panes[pane]
			if !ok {
				return "", "", "", errors.New("no such pane")
			}
			return p[0], p[1], p[2], nil
		},
		WindowPanes: func(_ context.Context, wid string) (int, error) {
			if n, ok := w.windowPanes[wid]; ok {
				return n, nil
			}
			return 1, nil
		},
		TermOf: func(pid int) (claudecode.TermEnv, error) { return w.term[pid], nil },
		Live: func() (map[string]claudecode.LiveSession, error) {
			return claudecode.Live(w.claude, w.procs)
		},
		Trusted: func(dir string) (bool, error) { return !w.untrusted[dir], nil },
		Log: func(r route.Record) error {
			w.logged = append(w.logged, r)
			return nil
		},
		Now:    func() time.Time { return now },
		Config: []string{"--model", "sonnet"},
	}
}

// plan plans the adoption of the coordinator and both workers.
func (w *adoptWorld) plan(t *testing.T, ch Choice, args []string, given bool) (AdoptPlan, error) {
	t.Helper()
	return PlanAdopt(context.Background(), w.repo, w.all, w.detect(t), ch, args, given, w.deps())
}

var allThree = Choice{Coordinator: adCoord, Workers: []string{adQM, adHand}}

func (w *adoptWorld) adopt(t *testing.T, ch Choice) AdoptPlan {
	t.Helper()
	ap, err := w.plan(t, ch, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := Adopt(context.Background(), &ap, w.deps()); err != nil {
		t.Fatal(err)
	}
	return ap
}

func candIDs(cs []AdoptCandidate) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.SessionID)
	}
	return out
}

func notesHold(notes []string, s string) bool {
	return slices.ContainsFunc(notes, func(n string) bool { return strings.Contains(n, s) })
}

func TestDetectNone(t *testing.T) {
	w := newAdoptWorld(t)
	w.procs = nil
	if got := w.detect(t); len(got) != 0 {
		t.Fatalf("candidates %v, want none", candIDs(got))
	}
}

func TestDetectRootAndWorktree(t *testing.T) {
	w := newAdoptWorld(t)
	w.session(t, 415, "00000000-0000-4000-8000-000000000415", filepath.Join(w.root, "elsewhere"), "away")
	w.procs = append(w.procs, claudecode.Proc{PID: 415, PPID: 1, Cmd: "claude"})
	got := w.detect(t)
	if want := []string{adCoord, adQM, adHand}; !slices.Equal(candIDs(got), want) {
		t.Fatalf("candidates %v, want %v", candIDs(got), want)
	}
	c := got[0]
	if c.Name != "proj-coord" || c.Cwd != w.repo || c.PID != 411 || !c.Started.Equal(w.starts[adCoord]) || c.Last.Context != 50000 {
		t.Fatalf("coordinator candidate %+v", c)
	}
}

func TestDetectArgsOnlyNotCandidate(t *testing.T) {
	w := newAdoptWorld(t)
	w.procs = append(w.procs, claudecode.Proc{PID: 416, PPID: 1, Cmd: "claude --resume 00000000-0000-4000-8000-000000000416 -n x"})
	if got := w.detect(t); len(got) != 3 {
		t.Fatalf("candidates %v, want the three with session files", candIDs(got))
	}
}

func TestDetectPrintLeftOut(t *testing.T) {
	w := newAdoptWorld(t)
	w.procs[2].Cmd = "claude -p hi"
	if want := []string{adCoord, adQM}; !slices.Equal(candIDs(w.detect(t)), want) {
		t.Fatalf("candidates %v, want %v", candIDs(w.detect(t)), want)
	}
	w.procs[2].Cmd = "claude --print hi"
	if len(w.detect(t)) != 2 {
		t.Fatal("--print session is a candidate")
	}
}

func TestDetectPrintInPromptIsCandidate(t *testing.T) {
	w := newAdoptWorld(t)
	w.procs[2].Cmd = `claude --model haiku -n w-hand \012Run mkdir -p out and go test -p 1`
	if len(w.detect(t)) != 3 {
		t.Fatalf("candidates %v: a prompt mentioning -p is not the flag", candIDs(w.detect(t)))
	}
}

func TestDetectUnnamed(t *testing.T) {
	w := newAdoptWorld(t)
	w.session(t, 411, adCoord, w.repo, "")
	if got := w.detect(t); len(got) != 3 {
		t.Fatalf("candidates %v: an unnamed session is a candidate", candIDs(got))
	}
	ap := w.adopt(t, allThree)
	if ap.Project.Coordinator.Name != "proj-coord" || !notesHold(ap.Notes, "coordinator named proj-coord") {
		t.Fatalf("name %q, notes %q", ap.Project.Coordinator.Name, ap.Notes)
	}
}

func TestDetectReviewLeftOut(t *testing.T) {
	w := newAdoptWorld(t)
	w.recs = append(w.recs, route.Record{Time: now, Kind: "worker", Type: "review", ClaudeSession: adHand})
	if want := []string{adCoord, adQM}; !slices.Equal(candIDs(w.detect(t)), want) {
		t.Fatalf("candidates %v, want %v", candIDs(w.detect(t)), want)
	}
}

func TestDetectHeldElsewhere(t *testing.T) {
	other := func() Project {
		return Project{ID: "elsewhere-1a2b3c4d", Root: "/elsewhere", Workspace: "else", Coordinator: Session{ID: "00000000-0000-4000-8000-0000000000ff"}}
	}
	for name, mod := range map[string]func(w *adoptWorld, p *Project){
		"coordinator": func(_ *adoptWorld, p *Project) { p.Coordinator.ID = adHand },
		"past":        func(_ *adoptWorld, p *Project) { p.PastCoordinators = []string{adHand} },
		"adopted":     func(_ *adoptWorld, p *Project) { p.Adopted = map[string]AdoptedSession{adHand: {}} },
		"sessions": func(_ *adoptWorld, p *Project) {
			p.Sessions = map[string]launch.SessionRef{adHand: {Launcher: "agterm", ID: "s-h"}}
		},
		"record": func(w *adoptWorld, p *Project) {
			w.recs = append(w.recs, route.Record{Time: now, Kind: "worker", UpProject: p.ID, ClaudeSession: adHand, Name: "w-hand"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newAdoptWorld(t)
			p := other()
			mod(w, &p)
			w.all = []Project{p}
			if want := []string{adCoord, adQM}; !slices.Equal(candIDs(w.detect(t)), want) {
				t.Fatalf("candidates %v, want %v", candIDs(w.detect(t)), want)
			}
		})
	}
}

func TestDetectSelf(t *testing.T) {
	w := newAdoptWorld(t)
	w.procs = append(w.procs, claudecode.Proc{PID: 900, PPID: 411, Cmd: "/bin/zsh -c qm up"}, claudecode.Proc{PID: 901, PPID: 900, Cmd: "qm up"})
	w.selfPID = 901
	for _, c := range w.detect(t) {
		if c.Self != (c.SessionID == adCoord) {
			t.Fatalf("%s Self %v", c.Name, c.Self)
		}
	}
}

func TestDetectKnownWorker(t *testing.T) {
	w := newAdoptWorld(t)
	got := w.detect(t)
	for _, c := range got {
		if c.Worker != (c.SessionID == adQM) {
			t.Fatalf("%s Worker %v", c.Name, c.Worker)
		}
	}
	h := got[2]
	if want := []string{"--permission-mode", "acceptEdits", "--model", "haiku"}; !slices.Equal(h.Kept, want) {
		t.Fatalf("kept %q, want %q", h.Kept, want)
	}
	if want := []string{"--mcp-config"}; !slices.Equal(h.Dropped, want) {
		t.Fatalf("dropped %q, want %q", h.Dropped, want)
	}
}

func TestChooseAutoSelf(t *testing.T) {
	cs := []AdoptCandidate{{SessionID: "a", Name: "a", Cwd: "/r"}, {SessionID: "b", Name: "b", Cwd: "/r/x", Self: true}, {SessionID: "c", Name: "c", Cwd: "/r/y", Worker: true}}
	ch, err := ChooseAuto(Detected{Candidates: cs}, "/r", "")
	if err != nil || ch.Coordinator != "b" || !slices.Equal(ch.Workers, []string{"c"}) {
		t.Fatalf("ChooseAuto = %+v, %v", ch, err)
	}
}

func TestChooseAutoSingleRoot(t *testing.T) {
	cs := []AdoptCandidate{{SessionID: "a", Name: "a", Cwd: "/r"}, {SessionID: "b", Name: "b", Cwd: "/r", Worker: true}, {SessionID: "c", Name: "c", Cwd: "/r/y"}}
	ch, err := ChooseAuto(Detected{Candidates: cs}, "/r", "")
	if err != nil || ch.Coordinator != "a" || !slices.Equal(ch.Workers, []string{"b"}) {
		t.Fatalf("ChooseAuto = %+v, %v", ch, err)
	}
}

func TestChooseAutoNoneOrSeveral(t *testing.T) {
	several := []AdoptCandidate{{SessionID: "aaaa1111-x", Name: "one", Cwd: "/r"}, {SessionID: "bbbb2222-x", Name: "two", Cwd: "/r"}}
	_, err := ChooseAuto(Detected{Candidates: several}, "/r", "")
	if !errors.Is(err, ErrAdoptCoordinator) {
		t.Fatalf("err %v, want ErrAdoptCoordinator", err)
	}
	for _, s := range []string{"one (aaaa1111, /r)", "two (bbbb2222, /r)", "--adopt-coordinator ID", "several"} {
		if !strings.Contains(err.Error(), s) {
			t.Fatalf("error %q lacks %q", err, s)
		}
	}
	_, err = ChooseAuto(Detected{Candidates: []AdoptCandidate{{SessionID: "c", Name: "w", Cwd: "/r/y", Worker: true}}}, "/r", "")
	if !errors.Is(err, ErrAdoptCoordinator) || !strings.Contains(err.Error(), "no coordinator candidate") {
		t.Fatalf("err %v", err)
	}
}

func TestChooseAutoByID(t *testing.T) {
	cs := []AdoptCandidate{{SessionID: "aaaa1111-x", Name: "one", Cwd: "/r"}, {SessionID: "aaaa2222-x", Name: "two", Cwd: "/r"}, {SessionID: "bbbb3333-x", Name: "three", Cwd: "/r/y", Worker: true}}
	ch, err := ChooseAuto(Detected{Candidates: cs}, "/r", "aaaa2")
	if err != nil || ch.Coordinator != "aaaa2222-x" || !slices.Equal(ch.Workers, []string{"bbbb3333-x"}) {
		t.Fatalf("ChooseAuto = %+v, %v", ch, err)
	}
	for _, id := range []string{"aaaa", "cccc"} {
		if _, err := ChooseAuto(Detected{Candidates: cs}, "/r", id); !errors.Is(err, ErrInput) {
			t.Fatalf("%s: err %v, want ErrInput", id, err)
		}
	}
}

func TestAskAdopt(t *testing.T) {
	cs := []AdoptCandidate{{SessionID: "a", Name: "a"}, {SessionID: "b", Name: "b", Worker: true}, {SessionID: "c", Name: "c"}}
	for _, tc := range []struct {
		in   string
		want Choice
		err  error
	}{
		{"2\n1 3\n", Choice{Coordinator: "b", Workers: []string{"a", "c"}}, nil},
		{"2\n1,3\n", Choice{Coordinator: "b", Workers: []string{"a", "c"}}, nil},
		{"n\n", Choice{New: true}, nil},
		{"1\na\n", Choice{Coordinator: "a", Workers: []string{"b", "c"}}, nil},
		{"1\nq\n", Choice{Coordinator: "a", Workers: []string{"b"}}, nil},
		{"1\n\n", Choice{Coordinator: "a"}, nil},
		{"x\n", Choice{}, ErrInput},
		{"4\n", Choice{}, ErrInput},
		{"1\n1\n", Choice{}, ErrInput},
		{"1\n9\n", Choice{}, ErrInput},
		{"", Choice{}, errAdoptAborted},
		{"1\n", Choice{}, errAdoptAborted},
	} {
		var out strings.Builder
		got, err := AskAdopt(cs, bufio.NewReader(strings.NewReader(tc.in)), &out)
		if !errors.Is(err, tc.err) || (tc.err == nil && (got.New != tc.want.New || got.Coordinator != tc.want.Coordinator || !slices.Equal(got.Workers, tc.want.Workers))) {
			t.Fatalf("%q: AskAdopt = %+v, %v; want %+v, %v", tc.in, got, err, tc.want, tc.err)
		}
		if !strings.Contains(out.String(), "Coordinator: row number, or n to start a new coordinator") {
			t.Fatalf("%q: prompt %q", tc.in, out.String())
		}
	}
}

func TestRenderCandidates(t *testing.T) {
	w := newAdoptWorld(t)
	out := RenderCandidates(w.detect(t), now)
	for _, s := range []string{"1", "proj-coord", "00000000", w.repo, "6 h ago", "50k", "qm worker"} {
		if !strings.Contains(out, s) {
			t.Fatalf("rows lack %q:\n%s", s, out)
		}
	}
}

// The list names the terminal tab a session runs in: a session's own name is
// often not what the user sees in the sidebar.
func TestRenderCandidatesTabColumn(t *testing.T) {
	w := newAdoptWorld(t)
	cands := w.detect(t)
	if strings.Contains(RenderCandidates(cands, now), "TAB") {
		t.Fatal("TAB column shown with no tab known")
	}
	c := cands[0]
	wss := []launch.Workspace{{ID: "w-1", Name: "Shop v2", Sessions: []launch.SessionRef{
		{Launcher: "agterm", ID: "s-1", Name: "Shop - CC", Foreground: "claude --resume " + c.SessionID},
		{Launcher: "agterm", ID: "s-2", Name: "notes", Foreground: "vim"},
	}}}
	termOf := func(pid int) (claudecode.TermEnv, error) {
		if pid == c.PID {
			return claudecode.TermEnv{AgtermSession: "s-1"}, nil
		}
		return claudecode.TermEnv{AgtermSession: "s-2"}, nil // inherited: not this session's tab
	}
	LabelTabs(cands, wss, termOf)
	if cands[0].Tab != "Shop v2 / Shop - CC" {
		t.Fatalf("tab = %q", cands[0].Tab)
	}
	for _, o := range cands[1:] {
		if o.Tab != "" {
			t.Fatalf("%s labelled with a tab that does not run it: %q", o.Name, o.Tab)
		}
	}
	out := RenderCandidates(cands, now)
	if !strings.Contains(out, "TAB") || !strings.Contains(out, "Shop v2 / Shop - CC") {
		t.Fatalf("rows:\n%s", out)
	}
}

func TestAdoptRecordsState(t *testing.T) {
	w := newAdoptWorld(t)
	w.adopt(t, allThree)
	p, ok, err := Load(w.repo)
	if err != nil || !ok {
		t.Fatalf("Load = %v, %v", ok, err)
	}
	want := Session{ID: adCoord, Name: "proj-coord", Started: w.starts[adCoord], Adopted: true}
	if p.Coordinator.ID != want.ID || p.Coordinator.Name != want.Name || !p.Coordinator.Started.Equal(want.Started) || !p.Coordinator.Adopted {
		t.Fatalf("coordinator %+v, want %+v", p.Coordinator, want)
	}
	if wa := []string{"--model", "opus", "--dangerously-skip-permissions"}; !slices.Equal(p.ClaudeArgs, wa) {
		t.Fatalf("claude_args %q, want %q", p.ClaudeArgs, wa)
	}
	if p.Workspace != "proj" || p.Launcher != "agterm" || p.ID != ProjectID(w.repo) || p.Root != w.repo {
		t.Fatalf("project %+v", p)
	}
	if a, ok := p.Adopted[adQM]; !ok || a.Name != "" || a.Cwd != "" || a.Args != nil {
		t.Fatalf("qm worker entry %+v, %v; want the zero value", a, ok)
	}
	h := p.Adopted[adHand]
	if h.Name != "w-hand" || h.Cwd != w.wtHand || h.Model != "haiku" || !h.Started.Equal(w.starts[adHand]) ||
		!slices.Equal(h.Args, []string{"--permission-mode", "acceptEdits", "--model", "haiku"}) {
		t.Fatalf("hand-started entry %+v", h)
	}
	for id, ref := range map[string]string{adCoord: "s-c", adQM: "s-q", adHand: "s-h"} {
		if p.Sessions[id].ID != ref || p.Sessions[id].Launcher != "agterm" {
			t.Fatalf("sessions[%s] = %+v, want agterm %s", id, p.Sessions[id], ref)
		}
	}
	var adopts []string
	for _, r := range w.logged {
		if r.Kind != "up" || r.Action != "adopt" || r.UpProject != p.ID {
			t.Fatalf("logged %+v", r)
		}
		adopts = append(adopts, r.Name+":"+r.Detail)
	}
	if want := []string{"proj-coord:coordinator", "w-qm:worker (qm worker)", "w-hand:worker (hand-started)"}; !slices.Equal(adopts, want) {
		t.Fatalf("records %q, want %q", adopts, want)
	}
	for _, v := range w.agterm.verbs() {
		if v != "tree" && v != "session restore" {
			t.Fatalf("agterm calls %q: adoption only lists and pins", w.agterm.calls)
		}
	}
}

func TestAdoptLeavesUnmarked(t *testing.T) {
	w := newAdoptWorld(t)
	w.side(t)
	ap := w.adopt(t, allThree)
	if len(ap.Left) != 1 || ap.Left[0].SessionID != adSide {
		t.Fatalf("left %v, want the side session", candIDs(ap.Left))
	}
	p, _, _ := Load(w.repo)
	if _, ok := p.Adopted[adSide]; ok {
		t.Fatal("unmarked session adopted")
	}
	if _, ok := p.Sessions[adSide]; ok {
		t.Fatal("unmarked session's terminal recorded")
	}
	for _, r := range w.logged {
		if r.ClaudeSession == adSide {
			t.Fatal("unmarked session logged")
		}
	}
	for _, c := range w.agterm.calls {
		if strings.Contains(c, "s-x") {
			t.Fatalf("unmarked session touched: %q", c)
		}
	}
	if !strings.Contains(RenderAdopt(ap), "not adopted: side (00000000)") {
		t.Fatalf("plan:\n%s", RenderAdopt(ap))
	}
}

func TestAdoptUnsafeNameFallsBack(t *testing.T) {
	w := newAdoptWorld(t)
	w.session(t, 411, adCoord, w.repo, "proj coord")
	w.agterm.ws[0].Name = "main"
	ap := w.adopt(t, allThree)
	if ap.Project.Coordinator.Name != "main-coord" || !notesHold(ap.Notes, "coordinator named main-coord") {
		t.Fatalf("name %q, notes %q", ap.Project.Coordinator.Name, ap.Notes)
	}
}

func TestAdoptUnsafeWorkspaceName(t *testing.T) {
	w := newAdoptWorld(t)
	w.session(t, 411, adCoord, w.repo, "proj coord")
	w.agterm.ws[0].Name = "my proj"
	ap := w.adopt(t, allThree)
	if ap.Project.Coordinator.Name != "repo-coord" || ap.Project.Workspace != "my proj" {
		t.Fatalf("name %q, workspace %q", ap.Project.Coordinator.Name, ap.Project.Workspace)
	}
}

func TestAdoptCoordinatorOutsideRoot(t *testing.T) {
	w := newAdoptWorld(t)
	if _, err := w.plan(t, Choice{Coordinator: adHand, Workers: []string{adQM}}, nil, false); !errors.Is(err, ErrInput) || !strings.Contains(err.Error(), "not in the project root") {
		t.Fatalf("err %v, want ErrInput", err)
	}
}

func TestAdoptIdleAgtermTab(t *testing.T) {
	w := newAdoptWorld(t)
	w.agterm.idle = map[string]bool{"s-h": true}
	ap := w.adopt(t, allThree)
	if _, ok := ap.Project.Sessions[adHand]; ok || w.agterm.has("session restore --none --target s-h --json") {
		t.Fatal("an idle tab was recorded or pinned as the worker's")
	}
	if _, ok := ap.Project.Adopted[adHand]; !ok || !notesHold(ap.Notes, "idle shell") {
		t.Fatalf("not adopted, or no note: %q", ap.Notes)
	}
}

func TestAdoptSharedAgtermTab(t *testing.T) {
	w := newAdoptWorld(t)
	w.side(t)
	// The unpicked session runs in the same folder and inherited the tab's
	// id: the tab's claude names no session, so either could be its own.
	w.session(t, 414, adSide, w.wtHand, "side")
	w.term[414] = claudecode.TermEnv{AgtermSession: "s-h"}
	w.agterm.ws[0].Sessions = w.agterm.ws[0].Sessions[:2]
	w.agterm.ws[1].Sessions = []launch.SessionRef{{ID: "s-h", Name: "w-hand"}}
	ap := w.adopt(t, allThree)
	if _, ok := ap.Project.Sessions[adHand]; ok || len(ap.Moves) != 0 {
		t.Fatalf("a shared tab was recorded or moved: %+v %+v", ap.Project.Sessions, ap.Moves)
	}
	for _, c := range w.agterm.calls {
		if strings.Contains(c, "s-h") {
			t.Fatalf("shared tab touched: %q", c)
		}
	}
	if !notesHold(ap.Notes, "named by another running session") {
		t.Fatalf("notes %q", ap.Notes)
	}
}

func TestAdoptTmuxSharedWindow(t *testing.T) {
	w := newAdoptWorld(t)
	w.term[411] = claudecode.TermEnv{TmuxPane: "%1"}
	w.term[412] = claudecode.TermEnv{TmuxPane: "%2"}
	w.panes["%1"] = [3]string{"tmx", "@1", "proj-coord"}
	w.panes["%2"] = [3]string{"tmx", "@2", "w-qm"}
	w.windowPanes = map[string]int{"@1": 2}
	ap := w.adopt(t, allThree)
	p, _, _ := Load(w.repo)
	if _, ok := p.Sessions[adCoord]; ok || p.Workspace != "tmx" || p.Launcher != "tmux" {
		t.Fatalf("project %+v", p)
	}
	if p.Sessions[adQM].ID != "@2" || !notesHold(ap.Notes, "has 2 panes") {
		t.Fatalf("worker ref %+v, notes %q", p.Sessions[adQM], ap.Notes)
	}
}

func TestAdoptHandStartedBadName(t *testing.T) {
	w := newAdoptWorld(t)
	w.session(t, 413, adHand, w.wtHand, "w hand")
	ap := w.adopt(t, allThree)
	if got := ap.Project.Adopted[adHand].Name; got != "w-00000000" {
		t.Fatalf("name %q, want w-00000000", got)
	}
	if !notesHold(ap.Notes, "w-00000000") {
		t.Fatalf("notes %q", ap.Notes)
	}
}

func TestAdoptArgsPrecedence(t *testing.T) {
	w := newAdoptWorld(t)
	ap, err := w.plan(t, allThree, []string{"--model", "haiku"}, true)
	if err != nil || !slices.Equal(ap.Project.ClaudeArgs, []string{"--model", "haiku"}) {
		t.Fatalf("given: %q, %v", ap.Project.ClaudeArgs, err)
	}
	if _, err := w.plan(t, allThree, []string{"-n", "x"}, true); !errors.Is(err, ErrInput) {
		t.Fatalf("refused arg: %v", err)
	}
	ap, _ = w.plan(t, allThree, nil, false)
	if !slices.Equal(ap.Project.ClaudeArgs, []string{"--model", "opus", "--dangerously-skip-permissions"}) {
		t.Fatalf("kept: %q", ap.Project.ClaudeArgs)
	}
	w.procs[0].Cmd = "claude -n proj-coord"
	ap, _ = w.plan(t, allThree, nil, false)
	if !slices.Equal(ap.Project.ClaudeArgs, []string{"--model", "sonnet"}) {
		t.Fatalf("config: %q", ap.Project.ClaudeArgs)
	}
}

func TestAdoptAgtermMoveAndPin(t *testing.T) {
	w := newAdoptWorld(t)
	w.agterm.ws[0].Sessions = w.agterm.ws[0].Sessions[:2]
	w.agterm.ws[1].Sessions = []launch.SessionRef{{ID: "s-h", Name: "w-hand"}}
	ap := w.adopt(t, allThree)
	if len(ap.Moves) != 1 || ap.Moves[0] != (AdoptMove{Name: "w-hand", SessionID: "s-h", From: "other", To: "proj", ToID: "ws-1"}) {
		t.Fatalf("moves %+v", ap.Moves)
	}
	if !strings.Contains(RenderAdopt(ap), "move w-hand (agterm s-h) from workspace other to proj") {
		t.Fatalf("plan:\n%s", RenderAdopt(ap))
	}
	if !w.agterm.has("session move ws-1 --target s-h --json") {
		t.Fatalf("calls %q", w.agterm.calls)
	}
	for _, id := range []string{"s-c", "s-q", "s-h"} {
		if !w.agterm.has("session restore --none --target " + id + " --json") {
			t.Fatalf("%s not pinned: %q", id, w.agterm.calls)
		}
	}
}

func TestAdoptMoveFails(t *testing.T) {
	w := newAdoptWorld(t)
	w.agterm.ws[0].Sessions = w.agterm.ws[0].Sessions[:2]
	w.agterm.ws[1].Sessions = []launch.SessionRef{{ID: "s-h", Name: "w-hand"}}
	w.agterm.fail = map[string]bool{"session move": true}
	ap := w.adopt(t, allThree)
	p, _, _ := Load(w.repo)
	if _, ok := p.Sessions[adHand]; ok {
		t.Fatal("worker whose move failed keeps a ref")
	}
	if _, ok := p.Adopted[adHand]; !ok || !notesHold(ap.Notes, "w-hand") {
		t.Fatalf("not adopted, or no note: %q", ap.Notes)
	}
}

func TestAdoptWorkspaceTaken(t *testing.T) {
	w := newAdoptWorld(t)
	w.all = []Project{{ID: "elsewhere-1a2b3c4d", Root: "/elsewhere", Workspace: "proj"}}
	if _, err := w.plan(t, allThree, nil, false); !errors.Is(err, ErrWorkspaceName) || !strings.Contains(err.Error(), "rename the workspace proj") {
		t.Fatalf("taken: %v", err)
	}
	w = newAdoptWorld(t)
	w.agterm.ws = append(w.agterm.ws, launch.Workspace{ID: "ws-3", Name: "proj"})
	if _, err := w.plan(t, allThree, nil, false); !errors.Is(err, ErrWorkspaceName) {
		t.Fatalf("two of the name: %v", err)
	}
	if _, ok, _ := Load(w.repo); ok {
		t.Fatal("state written")
	}
}

func TestAdoptNoTerminal(t *testing.T) {
	w := newAdoptWorld(t)
	w.term = map[int]claudecode.TermEnv{}
	w.adopt(t, allThree)
	p, _, _ := Load(w.repo)
	if p.Workspace != "repo" || p.Launcher != "" || len(p.Sessions) != 0 {
		t.Fatalf("project %+v", p)
	}
	if slices.Contains(w.agterm.verbs(), "session restore") {
		t.Fatalf("pinned without agterm: %q", w.agterm.calls)
	}
}

func TestAdoptTmuxWinsOverAgterm(t *testing.T) {
	w := newAdoptWorld(t)
	w.term[411] = claudecode.TermEnv{AgtermSession: "s-c", TmuxPane: "%1"}
	w.term[412] = claudecode.TermEnv{TmuxPane: "%2"}
	w.panes["%1"] = [3]string{"tmx", "@1", "proj-coord"}
	w.panes["%2"] = [3]string{"tmx", "@2", "w-qm"}
	w.adopt(t, allThree)
	p, _, _ := Load(w.repo)
	if p.Workspace != "tmx" || p.Launcher != "tmux" {
		t.Fatalf("project %+v", p)
	}
	if got := p.Sessions[adCoord]; got != (launch.SessionRef{Launcher: "tmux", ID: "@1", Name: "proj-coord", Adopted: true}) {
		t.Fatalf("coordinator ref %+v", got)
	}
	if got := p.Sessions[adQM]; got.ID != "@2" {
		t.Fatalf("worker ref %+v", got)
	}
	if _, ok := p.Sessions[adHand]; ok {
		t.Fatal("agterm worker under a tmux coordinator got a ref")
	}
	if slices.Contains(w.agterm.verbs(), "session restore") || slices.Contains(w.agterm.verbs(), "session move") {
		t.Fatalf("agterm acted: %q", w.agterm.calls)
	}
}

func TestAdoptTmuxOtherSession(t *testing.T) {
	w := newAdoptWorld(t)
	w.term[411] = claudecode.TermEnv{TmuxPane: "%1"}
	w.term[412] = claudecode.TermEnv{TmuxPane: "%2"}
	w.panes["%1"] = [3]string{"tmx", "@1", "proj-coord"}
	w.panes["%2"] = [3]string{"else", "@9", "w-qm"}
	ap := w.adopt(t, allThree)
	p, _, _ := Load(w.repo)
	if _, ok := p.Sessions[adQM]; ok || len(ap.Moves) != 0 {
		t.Fatalf("worker in another tmux session: ref %+v, moves %+v", p.Sessions[adQM], ap.Moves)
	}
	if _, ok := p.Adopted[adQM]; !ok {
		t.Fatal("not adopted")
	}
}

func TestAdoptUntrustedReported(t *testing.T) {
	w := newAdoptWorld(t)
	w.untrusted[w.wtHand] = true
	ap := w.adopt(t, allThree)
	if !notesHold(ap.Notes, "not trusted: w-hand's folder "+w.wtHand) {
		t.Fatalf("notes %q", ap.Notes)
	}
	if _, ok := ap.Project.Adopted[adHand]; !ok {
		t.Fatal("untrusted worker not adopted")
	}
}

func TestAdoptWorkerExited(t *testing.T) {
	w := newAdoptWorld(t)
	ap, err := w.plan(t, allThree, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	w.procs = w.procs[:2]
	if err := Adopt(context.Background(), &ap, w.deps()); err != nil {
		t.Fatal(err)
	}
	p, _, _ := Load(w.repo)
	if _, ok := p.Adopted[adHand]; ok {
		t.Fatal("exited worker adopted")
	}
	if _, ok := p.Sessions[adHand]; ok || w.agterm.has("session restore --none --target s-h --json") {
		t.Fatal("exited worker's terminal recorded or pinned")
	}
	if !notesHold(ap.Notes, "w-hand exited before the confirmation: not adopted") || len(w.logged) != 2 {
		t.Fatalf("notes %q, %d records", ap.Notes, len(w.logged))
	}
}

func TestAdoptCoordinatorExited(t *testing.T) {
	w := newAdoptWorld(t)
	w.agterm.ws[0].Sessions = w.agterm.ws[0].Sessions[:2]
	w.agterm.ws[1].Sessions = []launch.SessionRef{{ID: "s-h", Name: "w-hand"}}
	ap, err := w.plan(t, allThree, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	w.procs = w.procs[1:]
	if err := Adopt(context.Background(), &ap, w.deps()); !errors.Is(err, ErrCoordinatorGone) {
		t.Fatalf("err %v, want ErrCoordinatorGone", err)
	}
	if _, ok, _ := Load(w.repo); ok {
		t.Fatal("state written")
	}
	if len(w.logged) != 0 {
		t.Fatalf("logged %+v", w.logged)
	}
	for _, v := range w.agterm.verbs() {
		if v != "tree" {
			t.Fatalf("agterm acted: %q", w.agterm.calls)
		}
	}
}

func TestReportCountsSameWorkers(t *testing.T) {
	w := newAdoptWorld(t)
	count := func(recs []route.Record) int {
		rep := outcomes.Build(outcomes.Input{
			Records: recs,
			Worker:  func(route.Record) (outcomes.Transcript, bool) { return outcomes.Transcript{}, false },
			Session: func(string) (outcomes.Transcript, bool) { return outcomes.Transcript{}, false },
			PR:      func(string) (outcomes.PR, bool) { return outcomes.PR{}, false },
		})
		return len(rep.Workers) + rep.UnresolvedWorkers
	}
	before := count(w.recs)
	w.adopt(t, allThree)
	if after := count(append(slices.Clone(w.recs), w.logged...)); after != before {
		t.Fatalf("workers %d after adoption, %d before", after, before)
	}
}

func TestAdoptedThenPlanLeavesAll(t *testing.T) {
	w := newAdoptWorld(t)
	w.adopt(t, allThree)
	p, _, _ := Load(w.repo)
	cands, err := Gather(&p, append(slices.Clone(w.recs), w.logged...), Env{
		Now: now, Live: w.live(t), TranscriptPath: w.transcriptPath,
		Trusted: func(string) (bool, error) { return true, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	steps := Plan(cands, false, now)
	if len(steps) != 3 {
		t.Fatalf("%d steps, want 3", len(steps))
	}
	for _, s := range steps {
		if s.Action != Leave {
			t.Fatalf("%s: %s (%s), want leave", s.Name, s.Action, s.Why)
		}
	}
}
