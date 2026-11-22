package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
	"github.com/IZZY-Agency/quartermaster/internal/declog"
	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/route"
	"github.com/IZZY-Agency/quartermaster/internal/up"
)

const (
	adoptCoordID = "00000000-0000-4000-8000-000000000411"
	adoptHandID  = "00000000-0000-4000-8000-000000000413"
	adoptSideID  = "00000000-0000-4000-8000-000000000414"
)

// adoptAgterm is a fake agtermctl for adoption: tree lists workspace proj
// (id ws-1) with the live sessions' tabs; every call is recorded.
type adoptAgterm struct {
	calls []string
	// root is every tab's folder; workspace the workspace's name ("" is proj).
	root, workspace string
}

func (f *adoptAgterm) run(_ context.Context, _ string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	var result any = map[string]any{}
	if args[0] == "tree" {
		ws := f.workspace
		if ws == "" {
			ws = "proj"
		}
		tab := func(id, name string, fg ...string) map[string]any {
			return map[string]any{"id": id, "name": name, "cwd": f.root, "foreground": fg}
		}
		result = map[string]any{"tree": map[string]any{"workspaces": []any{
			map[string]any{"id": "ws-1", "name": ws, "sessions": []any{
				tab("s-c", "proj-coord", "claude", "-n", "proj-coord", "--model", "opus"),
				tab("s-h", "w-hand", "claude", "--model", "haiku", "--plugin-dir", "/p"),
				tab("s-x", "side", "claude"),
			}},
		}}}
	}
	return json.Marshal(map[string]any{"ok": true, "result": result})
}

// acted are the calls other than tree.
func (f *adoptAgterm) acted() []string {
	var out []string
	for _, c := range f.calls {
		if c != "tree --json" {
			out = append(out, c)
		}
	}
	return out
}

// adoptMachine is an upMachine with a live coordinator (pid 411) and a
// hand-started worker (pid 413) in the project root, both in agterm.
type adoptMachine struct {
	*upMachine
	procs  []claudecode.Proc
	agterm *adoptAgterm
	self   int
}

func newAdoptMachine(t *testing.T) *adoptMachine {
	t.Helper()
	m := &adoptMachine{upMachine: newUpMachine(t)}
	m.agterm = &adoptAgterm{root: m.root}
	m.live(t, 411, adoptCoordID, "proj-coord", "claude -n proj-coord --model opus")
	m.live(t, 413, adoptHandID, "w-hand", "claude --model haiku --plugin-dir /p")
	swapProcs(t, func() ([]claudecode.Proc, error) { return m.procs, nil })
	swap(t, &upAgtermCtl, launch.Agterm{Bin: "agtermctl", Run: m.agterm.run})
	swap(t, &upTmuxCtl, launch.Tmux{Bin: "tmux", Run: func(context.Context, string, ...string) ([]byte, error) {
		return nil, fmt.Errorf("no tmux")
	}})
	terms := map[int]string{411: "s-c", 413: "s-h", 414: "s-x"}
	swap(t, &upTermOf, func(_ context.Context, pid int) (claudecode.TermEnv, error) {
		return claudecode.TermEnv{AgtermSession: terms[pid]}, nil
	})
	swap(t, &upSelfPID, func([]claudecode.Proc) int { return m.self })
	return m
}

// live adds a running claude process with a session file and transcript.
func (m *adoptMachine) live(t *testing.T, pid int, id, name, cmd string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"pid": pid, "sessionId": id, "cwd": m.root, "name": name})
	dir := filepath.Join(m.claude, "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprint(pid)+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	m.fixture(t, "coord.jsonl", filepath.Join(m.claude, "projects", "p", id+".jsonl"))
	m.procs = append(m.procs, claudecode.Proc{PID: pid, PPID: 1, Cmd: cmd})
}

// inside runs qm up from a shell under the claude process pid.
func (m *adoptMachine) inside(pid int) {
	m.procs = append(m.procs, claudecode.Proc{PID: 900, PPID: pid, Cmd: "/bin/zsh -c qm up --adopt"}, claudecode.Proc{PID: 901, PPID: 900, Cmd: "qm up --adopt"})
	m.self = 901
}

func runQM(args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func stateOf(t *testing.T, root string) (up.Project, bool) {
	t.Helper()
	p, ok, err := up.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return p, ok
}

func TestUpAdoptFlagsUsage(t *testing.T) {
	m := newAdoptMachine(t)
	for _, args := range [][]string{{"up", "--new", "--adopt"}, {"up", "--new", "--adopt-coordinator", "0000"}} {
		if code, _, errs := runQM(args...); code != 2 || !strings.Contains(errs, "--new") {
			t.Fatalf("%q: exit %d, stderr %q", args, code, errs)
		}
	}
	m.project(t)
	for _, f := range []string{"--new", "--adopt-coordinator=0000"} {
		if code, _, errs := runQM("up", f); code != 2 || !strings.Contains(errs, "first run") {
			t.Fatalf("%s on a project with state: exit %d, stderr %q", f, code, errs)
		}
	}
	for _, s := range []string{"--adopt", "--adopt-coordinator ID", "--new", "qm up handoff"} {
		if !strings.Contains(upUsage, s) {
			t.Fatalf("usage lacks %q:\n%s", s, upUsage)
		}
	}
}

func TestUpYesWithoutAdoptRefuses(t *testing.T) {
	m := newAdoptMachine(t)
	code, _, errs := runQM("up", "--yes")
	if code != 1 || !strings.Contains(errs, "proj-coord") || !strings.Contains(errs, "w-hand") ||
		!strings.Contains(errs, "qm up --adopt") || !strings.Contains(errs, "qm up --new") {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
	if _, ok := stateOf(t, m.root); ok || len(m.term.calls) != 0 || len(m.agterm.acted()) != 0 {
		t.Fatalf("acted: state %v, calls %q %q", ok, m.term.calls, m.agterm.calls)
	}
}

func TestUpNoTerminalWithCandidatesRefuses(t *testing.T) {
	m := newAdoptMachine(t)
	code, _, errs := runQM("up")
	if code != 1 || !strings.Contains(errs, "qm up --adopt") || !strings.Contains(errs, "qm up --new") {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
	if _, ok := stateOf(t, m.root); ok || len(m.term.calls) != 0 {
		t.Fatal("acted without a terminal")
	}
}

func TestUpNewStartsCoordinator(t *testing.T) {
	m := newAdoptMachine(t)
	code, out, errs := runQM("up", "--new")
	if code != 0 {
		t.Fatalf("exit %d, %s %s", code, out, errs)
	}
	p, ok := stateOf(t, m.root)
	if !ok || p.Coordinator.ID == adoptCoordID || p.Coordinator.Adopted || len(p.Adopted) != 0 {
		t.Fatalf("state %+v", p)
	}
	if !strings.Contains(strings.Join(m.term.calls, "\n"), "session new") {
		t.Fatalf("no coordinator started: %q", m.term.calls)
	}
}

func TestUpAdoptDryRunWritesNothing(t *testing.T) {
	m := newAdoptMachine(t)
	code, out, _ := runQM("up", "--dry-run")
	if code != 0 || !strings.Contains(out, "proj-coord") || !strings.Contains(out, "would refuse") {
		t.Fatalf("no terminal: exit %d, out %q", code, out)
	}
	swap(t, &upTerminal, func() bool { return true })
	code, out, _ = runQM("up", "--dry-run")
	if code != 0 || !strings.Contains(out, "would ask") {
		t.Fatalf("terminal: exit %d, out %q", code, out)
	}
	if _, out, _ = runQM("up", "--dry-run", "--yes"); !strings.Contains(out, "would refuse") {
		t.Fatalf("--yes: out %q", out)
	}
	m.inside(411)
	code, out, errs := runQM("up", "--dry-run", "--adopt")
	if code != 0 || !strings.Contains(out, "adopt coordinator proj-coord") || !strings.Contains(out, "not adopted: w-hand") {
		t.Fatalf("exit %d, out %q, err %q", code, out, errs)
	}
	if _, ok := stateOf(t, m.root); ok || len(m.agterm.acted()) != 0 || len(m.term.calls) != 0 {
		t.Fatalf("dry run acted: %q", m.agterm.calls)
	}
	if recs, _ := declog.Read(time.Time{}); len(recs) != 0 {
		t.Fatalf("dry run logged %+v", recs)
	}
}

func TestUpAdoptAsks(t *testing.T) {
	m := newAdoptMachine(t)
	swap(t, &upTerminal, func() bool { return true })
	swap(t, &upStdin, io.Reader(strings.NewReader("1\n2\ny\n")))
	code, out, errs := runQM("up")
	if code != 0 {
		t.Fatalf("exit %d, out %q, err %q", code, out, errs)
	}
	for _, s := range []string{"Coordinator: row number", "Workers: row numbers", "adopt worker w-hand", "Proceed? [y/N]"} {
		if !strings.Contains(out, s) {
			t.Fatalf("output lacks %q:\n%s", s, out)
		}
	}
	p, ok := stateOf(t, m.root)
	if !ok || p.Coordinator.ID != adoptCoordID || !p.Coordinator.Adopted || p.Adopted[adoptHandID].Name != "w-hand" {
		t.Fatalf("state %+v", p)
	}
	if p.Sessions[adoptCoordID].ID != "s-c" || p.Workspace != "proj" {
		t.Fatalf("terminal %+v, workspace %s", p.Sessions, p.Workspace)
	}
	if len(m.term.calls) != 0 {
		t.Fatalf("a session was started: %q", m.term.calls)
	}
	if kinds := upKinds(t); len(kinds) != 2 || kinds[0] != "proj-coord adopt" {
		t.Fatalf("records %q", kinds)
	}
}

func TestUpAdoptAsksDeclined(t *testing.T) {
	m := newAdoptMachine(t)
	swap(t, &upTerminal, func() bool { return true })
	swap(t, &upStdin, io.Reader(strings.NewReader("1\n\nn\n")))
	if code, out, errs := runQM("up"); code != 0 || !strings.Contains(out, "nothing adopted") {
		t.Fatalf("exit %d, out %q, err %q", code, out, errs)
	}
	if _, ok := stateOf(t, m.root); ok {
		t.Fatal("state written")
	}
}

func TestUpAdoptSelf(t *testing.T) {
	m := newAdoptMachine(t)
	m.live(t, 414, adoptSideID, "side", "claude")
	// Two root sessions, neither known as a worker: --adopt needs Self.
	if code, _, errs := runQM("up", "--adopt"); code != 1 || !strings.Contains(errs, "--adopt-coordinator") {
		t.Fatalf("exit %d, err %q", code, errs)
	}
	m.inside(411)
	code, out, errs := runQM("up", "--adopt")
	if code != 0 {
		t.Fatalf("exit %d, out %q, err %q", code, out, errs)
	}
	p, _ := stateOf(t, m.root)
	if p.Coordinator.ID != adoptCoordID || len(p.Adopted) != 0 {
		t.Fatalf("state %+v", p)
	}
	for _, c := range m.agterm.calls {
		if strings.Contains(c, "s-x") || strings.Contains(c, "s-h") {
			t.Fatalf("an unmarked session was touched: %q", c)
		}
	}
}

func TestUpAdoptByCoordinatorID(t *testing.T) {
	m := newAdoptMachine(t)
	m.live(t, 414, adoptSideID, "side", "claude")
	if code, out, errs := runQM("up", "--adopt-coordinator", adoptSideID); code != 0 || !strings.Contains(out, "adopt coordinator side") {
		t.Fatalf("exit %d, out %q, err %q", code, out, errs)
	}
	if p, _ := stateOf(t, m.root); p.Coordinator.ID != adoptSideID {
		t.Fatalf("coordinator %+v", p.Coordinator)
	}
}

func TestUpAdoptKnownWorker(t *testing.T) {
	m := newAdoptMachine(t)
	if err := declog.Append(route.Record{Time: time.Now(), Kind: "worker", ClaudeSession: adoptHandID, Name: "w-hand", Cwd: m.root}); err != nil {
		t.Fatal(err)
	}
	if code, out, errs := runQM("up", "--adopt"); code != 0 || !strings.Contains(out, "adopt worker w-hand") {
		t.Fatalf("exit %d, out %q, err %q", code, out, errs)
	}
	if p, _ := stateOf(t, m.root); p.Adopted == nil {
		t.Fatalf("worker not adopted: %+v", p)
	}
}

func TestUpNoCandidatesFirstRunUnchanged(t *testing.T) {
	m := newAdoptMachine(t)
	m.procs = nil
	code, out, errs := runQM("up")
	if code != 0 || !strings.Contains(out, "started coordinator") {
		t.Fatalf("exit %d, out %q, err %q", code, out, errs)
	}
	if len(m.agterm.calls) != 0 {
		t.Fatalf("adoption read agterm: %q", m.agterm.calls)
	}
}

func TestUpHandoff(t *testing.T) {
	m := newAdoptMachine(t)
	if code, _, errs := runQM("up", "handoff"); code != 2 || !strings.Contains(errs, "no qm up project here") {
		t.Fatalf("no project: exit %d, err %q", code, errs)
	}
	p := m.project(t)
	dir, err := up.HandoffDir()
	if err != nil {
		t.Fatal(err)
	}
	// A worker or side session of the project is refused (here the live
	// hand-started session, pid 413).
	m.inside(413)
	if code, _, errs := runQM("up", "handoff"); code != 2 || !strings.Contains(errs, "not this project's coordinator") {
		t.Fatalf("other session: exit %d, err %q", code, errs)
	}
	m.live(t, 412, p.Coordinator.ID, "proj-coord", "claude --resume "+p.Coordinator.ID)
	m.inside(412)
	want := filepath.Join(dir, p.ID, "proj-coord.md") + "\n"
	if code, out, errs := runQM("up", "handoff"); code != 0 || out != want {
		t.Fatalf("exit %d, out %q (want %q), err %q", code, out, want, errs)
	}
	// One claude process can hold two live ids: its arguments name one
	// (--resume) and its session file another (after /clear). Any of them
	// being the coordinator's answers, every time, whatever the map order.
	m.live(t, 414, p.Coordinator.ID, "proj-coord", "claude --resume 00000000-0000-4000-8000-0000000000fe")
	m.inside(414)
	for range 20 {
		if code, out, errs := runQM("up", "handoff"); code != 0 || out != want {
			t.Fatalf("two ids for one process: exit %d, out %q, err %q", code, out, errs)
		}
	}
	fi, err := os.Stat(filepath.Join(dir, p.ID))
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("folder: %v, %v", fi, err)
	}
	t.Setenv("QM_PROJECT_ID", p.ID)
	swap(t, &upWorkDir, func() (string, error) { return t.TempDir(), nil })
	if code, out, _ := runQM("up", "handoff"); code != 0 || out != want {
		t.Fatalf("QM_PROJECT_ID: exit %d, out %q", code, out)
	}
	if code, _, _ := runQM("up", "handoff", "x"); code != 2 {
		t.Fatalf("extra argument: exit %d", code)
	}
}

func TestUpHandoffFromLinkedWorktree(t *testing.T) {
	routeHermetic(t)
	t.Setenv("QM_PROJECT_ID", "")
	main, linked := linkedWorktree(t)
	p := up.Project{ID: up.ProjectID(main), Root: main, Workspace: "main", Coordinator: up.Session{ID: adoptCoordID, Name: "main-coord"}}
	if err := up.Save(p); err != nil {
		t.Fatal(err)
	}
	swap(t, &upWorkDir, func() (string, error) { return linked, nil })
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	spawnFrom(t, adoptCoordID)
	code, out, errs := runQM("up", "handoff")
	if code != 0 || !strings.HasSuffix(out, filepath.Join(p.ID, "main-coord.md")+"\n") {
		t.Fatalf("exit %d, out %q, err %q", code, out, errs)
	}
}
