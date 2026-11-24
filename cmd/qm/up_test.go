package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
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
	upCoordID  = "00000000-0000-4000-8000-0000000000c1"
	upWorkerID = "00000000-0000-4000-8000-0000000000a1"
)

// upAgterm answers agtermctl like the app and records every call; a session
// new whose --name is fail fails.
type upAgterm struct {
	calls []string
	fail  string
	n     int
	// open are the sessions tree --json lists in the workspace "proj", by
	// id, with their names; the ids in idle list a foregroundShell.
	open map[string]string
	idle map[string]bool
	// fg are the foreground argv of the sessions, by id.
	fg map[string][]string
}

// show lists the sessions ids (named like their id) in the workspace "proj".
func (f *upAgterm) show(ids ...string) {
	if f.open == nil {
		f.open = map[string]string{}
	}
	for _, id := range ids {
		f.open[id] = id
	}
}

// showIdle lists the session ids like show, each an idle shell: what a tab
// whose Claude session ended shows.
func (f *upAgterm) showIdle(ids ...string) {
	f.show(ids...)
	if f.idle == nil {
		f.idle = map[string]bool{}
	}
	for _, id := range ids {
		f.idle[id] = true
	}
}

// acted is every call but the read-only tree listing.
func (f *upAgterm) acted() []string {
	return slices.DeleteFunc(slices.Clone(f.calls), func(c string) bool { return c == "tree --json" })
}

func (f *upAgterm) run(_ context.Context, _ string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	result := any(map[string]any{})
	switch {
	case len(args) >= 2 && args[0] == "session" && args[1] == "new":
		if name := args[slices.Index(args, "--name")+1]; name == f.fail {
			return nil, errors.New("agtermctl: session new failed")
		}
		f.n++
		result = map[string]string{"id": fmt.Sprintf("s-%d", f.n)}
	case len(args) >= 2 && args[0] == "session" && args[1] == "type":
		// typed into a session: answered ok.
	case len(args) >= 2 && args[0] == "workspace" && args[1] == "new":
		result = map[string]string{"id": "w-new"}
	case args[0] == "tree":
		var sessions []any
		for id, name := range f.open {
			sess := map[string]string{"id": id, "name": name}
			entry := map[string]any{}
			for k, v := range sess {
				entry[k] = v
			}
			if f.idle[id] {
				entry["foregroundShell"] = "zsh"
			}
			if fg, ok := f.fg[id]; ok {
				entry["foreground"] = fg
			}
			sessions = append(sessions, entry)
		}
		result = map[string]any{"tree": map[string]any{"workspaces": []any{map[string]any{"name": "proj", "sessions": sessions}}}}
	}
	return json.Marshal(map[string]any{"ok": true, "result": result})
}

// upMachine is a hermetic machine for qm up: state, config and Claude folders
// in temporary folders, a project folder trusted by Claude Code, a fake
// agterm, ps listing nothing, and no git or gh.
type upMachine struct {
	root   string
	claude string
	term   *upAgterm
}

func newUpMachine(t *testing.T) *upMachine {
	t.Helper()
	routeHermetic(t)
	t.Setenv("AGTERM_SESSION_ID", "")
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("QM_PROJECT_ID", "")
	m := &upMachine{claude: filepath.Join(t.TempDir(), "claude"), term: &upAgterm{}}
	swap(t, &upSleep, func(time.Duration) {})
	t.Setenv("CLAUDE_CONFIG_DIR", m.claude)
	t.Setenv("QM_WORKER_PLUGIN", t.TempDir())
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m.root = root
	m.fixture(t, "claude.json", filepath.Join(m.claude, ".claude.json"))
	// Claude Code's sessions folder exists, so a first run can see that no
	// session runs.
	if err := os.MkdirAll(filepath.Join(m.claude, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	swap(t, &upPick, func(string) (launch.Launcher, error) {
		return launch.Agterm{Bin: "agtermctl", Run: m.term.run}, nil
	})
	swapProcs(t, func() ([]claudecode.Proc, error) { return nil, nil })
	swap(t, &upRun, func(context.Context, string, string, ...string) ([]byte, error) {
		return nil, errors.New("not here")
	})
	swap(t, &upTerminal, func() bool { return false })
	swap(t, &upGhNote, func() string { return "" })
	swap(t, &upWorkDir, func() (string, error) { return m.root, nil })
	swap(t, &upAgtermPin, func(_ context.Context, id string) error {
		m.term.calls = append(m.term.calls, "pin "+id)
		return nil
	})
	swap(t, &upTmuxWindow, func(string) (string, error) { return "", errors.New("no tmux") })
	return m
}

// swapProcs fakes the process list for every reader of it: upProcs and a
// detection's upProcsCtx.
func swapProcs(t *testing.T, f func() ([]claudecode.Proc, error)) {
	t.Helper()
	swap(t, &upProcs, f)
	swap(t, &upProcsCtx, func(context.Context) ([]claudecode.Proc, error) { return f() })
}

func swap[T any](t *testing.T, v *T, to T) {
	t.Helper()
	old := *v
	*v = to
	t.Cleanup(func() { *v = old })
}

// fixture copies testdata/up/<name> to dst with @ROOT@ replaced.
func (m *upMachine) fixture(t *testing.T, name, dst string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "up", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, bytes.ReplaceAll(b, []byte("@ROOT@"), []byte(m.root)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// project records a project at m.root whose coordinator has a transcript.
func (m *upMachine) project(t *testing.T) up.Project {
	t.Helper()
	p := up.Project{
		ID: up.ProjectID(m.root), Root: m.root, Workspace: "proj", Launcher: "agterm",
		ClaudeArgs:  []string{"--permission-mode", "acceptEdits"},
		Coordinator: up.Session{ID: upCoordID, Name: "proj-coord", Started: time.Now().Add(-5 * time.Hour)},
	}
	if err := up.Save(p); err != nil {
		t.Fatal(err)
	}
	m.fixture(t, "coord.jsonl", filepath.Join(m.claude, "projects", "p", upCoordID+".jsonl"))
	return p
}

// worker logs a worker Record of p with a launch profile and a transcript.
func (m *upMachine) worker(t *testing.T, p up.Project, name string) {
	t.Helper()
	r := route.Record{
		Time: time.Now().UTC().Add(-time.Hour), Kind: "worker", Name: name, Cwd: m.root,
		UpProject: p.ID, ClaudeSession: upWorkerID,
		Decision: &route.Decision{Kind: "worker", Tier: route.Tier{Model: "sonnet", Effort: "low"}, Profile: &route.Profile{
			Autocompact: 150000, PluginsOn: []string{"qm-worker"}, PluginDirs: []string{"/old/qm-worker"},
		}},
	}
	if err := declog.Append(r); err != nil {
		t.Fatal(err)
	}
	m.fixture(t, "coord.jsonl", filepath.Join(m.claude, "projects", "p", upWorkerID+".jsonl"))
}

func loadProject(t *testing.T, root string) up.Project {
	t.Helper()
	p, ok, err := up.Load(root)
	if err != nil || !ok {
		t.Fatalf("load: %v, %v", ok, err)
	}
	return p
}

func upKinds(t *testing.T) []string {
	t.Helper()
	recs, err := declog.Read(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range recs {
		if r.Kind == "up" {
			out = append(out, r.Name+" "+r.Action)
		}
	}
	return out
}

func TestUpCLIFirstRun(t *testing.T) {
	m := newUpMachine(t)
	var out, errb bytes.Buffer
	if code := run([]string{"up", "--", "--permission-mode", "acceptEdits"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	p := loadProject(t, m.root)
	if !slices.Equal(p.ClaudeArgs, []string{"--permission-mode", "acceptEdits"}) || p.Sessions[p.Coordinator.ID].ID != "s-1" {
		t.Errorf("project = %+v", p)
	}
	if !strings.Contains(out.String(), "started coordinator "+p.Coordinator.Name+" (agterm s-1) in workspace "+p.Workspace) {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestUpCLIRefusesResumeArg(t *testing.T) {
	m := newUpMachine(t)
	var out, errb bytes.Buffer
	if code := run([]string{"up", "--", "--resume", upCoordID}, &out, &errb); code != 2 {
		t.Errorf("exit %d, want 2; stderr %q", code, errb.String())
	}
	if len(m.term.calls) != 0 {
		t.Errorf("agtermctl called: %v", m.term.calls)
	}
}

func TestUpDryRunStartsNothing(t *testing.T) {
	m := newUpMachine(t)
	m.project(t)
	before := loadProject(t, m.root)
	var out, errb bytes.Buffer
	if code := run([]string{"up", "--dry-run", "--", "--model", "opus"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	for _, want := range []string{"ROLE", "proj-coord", "resume (cold)", "dry run: nothing started"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout lacks %q:\n%s", want, out.String())
		}
	}
	if len(m.term.acted()) != 0 || len(upKinds(t)) != 0 {
		t.Errorf("a dry run acted: agtermctl %v, Records %v", m.term.calls, upKinds(t))
	}
	if after := loadProject(t, m.root); !slices.Equal(after.ClaudeArgs, before.ClaudeArgs) || !after.Updated.Equal(before.Updated) {
		t.Errorf("a dry run changed the state: %+v", after)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("XDG_STATE_HOME"), "quartermaster", "launch")); !os.IsNotExist(err) {
		t.Errorf("a dry run wrote scripts: %v", err)
	}
}

func TestUpFirstRunDryRunWritesNothing(t *testing.T) {
	m := newUpMachine(t)
	var out, errb bytes.Buffer
	if code := run([]string{"up", "--dry-run"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if !strings.Contains(out.String(), "first run in "+m.root) || len(m.term.calls) != 0 {
		t.Errorf("stdout %q, agtermctl %v", out.String(), m.term.calls)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("XDG_STATE_HOME"), "quartermaster")); !os.IsNotExist(err) {
		t.Errorf("a dry run wrote state: %v", err)
	}
}

func TestUpNoTerminalNeedsYes(t *testing.T) {
	m := newUpMachine(t)
	m.project(t)
	var out, errb bytes.Buffer
	if code := run([]string{"up"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "--yes") {
		t.Errorf("exit %d, stderr %q; want 1 and a hint to --yes", code, errb.String())
	}
	if len(m.term.acted()) != 0 {
		t.Errorf("started without asking: %v", m.term.calls)
	}

	out.Reset()
	errb.Reset()
	if code := run([]string{"up", "--yes"}, &out, &errb); code != 0 {
		t.Fatalf("--yes: exit %d, stderr %q", code, errb.String())
	}
	if !strings.Contains(out.String(), "resumed proj-coord (agterm s-1)") {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestUpAsksOnATerminal(t *testing.T) {
	m := newUpMachine(t)
	m.project(t)
	swap(t, &upTerminal, func() bool { return true })
	swap[io.Reader](t, &upStdin, strings.NewReader("n\n"))
	var out, errb bytes.Buffer
	if code := run([]string{"up"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "Proceed? [y/N] nothing started") {
		t.Errorf("exit %d, stdout %q", code, out.String())
	}
	if len(m.term.acted()) != 0 {
		t.Errorf("started after no: %v", m.term.calls)
	}
}

func TestUpNoLauncherPrintsScripts(t *testing.T) {
	m := newUpMachine(t)
	p := m.project(t)
	m.worker(t, p, "w1")
	swap(t, &upPick, func(string) (launch.Launcher, error) { return nil, launch.ErrNoLauncher })
	var out, errb bytes.Buffer
	// Without a launcher qm up still records the new sessions' ids: it asks.
	if code := run([]string{"up"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "--yes") || strings.Contains(out.String(), "/bin/sh ") {
		t.Fatalf("no --yes: exit %d, stdout %q, stderr %q; want 1 and a hint to --yes", code, out.String(), errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := run([]string{"up", "--yes"}, &out, &errb); code != 1 {
		t.Fatalf("exit %d, want 1; stderr %q", code, errb.String())
	}
	var lines []string
	for l := range strings.Lines(out.String()) {
		if strings.HasPrefix(l, "/bin/sh ") {
			lines = append(lines, l)
		}
	}
	if len(lines) != 2 {
		t.Errorf("want one /bin/sh line per session, got:\n%s", out.String())
	}
	if after := loadProject(t, m.root); len(after.Sessions) != 0 {
		t.Errorf("recorded as started: %+v", after.Sessions)
	}
	if k := upKinds(t); len(k) != 0 {
		t.Errorf("logged %v", k)
	}
}

func TestUpPsFailureStopsBeforeStart(t *testing.T) {
	m := newUpMachine(t)
	m.project(t)
	swapProcs(t, func() ([]claudecode.Proc, error) { return nil, errors.New("ps: not permitted") })
	var out, errb bytes.Buffer
	if code := run([]string{"up", "--yes"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "nothing started") {
		t.Errorf("exit %d, stderr %q", code, errb.String())
	}
	if len(m.term.calls) != 0 {
		t.Errorf("started without ps: %v", m.term.calls)
	}
}

func TestUpRefusesWorkerWorktree(t *testing.T) {
	m := newUpMachine(t)
	p := m.project(t)
	wt, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := declog.Append(route.Record{Time: time.Now().UTC(), Kind: "worker", Name: "w1", Cwd: wt, UpProject: p.ID, ClaudeSession: upWorkerID}); err != nil {
		t.Fatal(err)
	}
	swap(t, &upWorkDir, func() (string, error) { return wt, nil })
	var out, errb bytes.Buffer
	if code := run([]string{"up", "--yes"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "this is worker w1's worktree; run qm up in "+m.root) {
		t.Errorf("exit %d, stderr %q", code, errb.String())
	}
	if len(m.term.calls) != 0 {
		t.Errorf("started: %v", m.term.calls)
	}
}

func TestUpOneFailureExits1(t *testing.T) {
	m := newUpMachine(t)
	p := m.project(t)
	m.worker(t, p, "w1")
	m.term.fail = "w1"
	var out, errb bytes.Buffer
	if code := run([]string{"up", "--yes"}, &out, &errb); code != 1 {
		t.Fatalf("exit %d, want 1; stderr %q", code, errb.String())
	}
	if !strings.Contains(out.String(), "resumed proj-coord (agterm s-1)") || !strings.Contains(errb.String(), "qm up: w1: agterm: ") {
		t.Errorf("stdout %q, stderr %q", out.String(), errb.String())
	}
	if after := loadProject(t, m.root); after.Sessions[upCoordID].ID != "s-1" || len(after.Sessions) != 1 {
		t.Errorf("state sessions = %+v, want the coordinator only", after.Sessions)
	}
}

func TestUpDone(t *testing.T) {
	m := newUpMachine(t)
	p := m.project(t)
	m.worker(t, p, "w1")
	var out, errb bytes.Buffer
	if code := run([]string{"up", "done", "w1"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if _, ok := loadProject(t, m.root).Done[upWorkerID]; !ok {
		t.Errorf("w1 not marked done")
	}
	if code := run([]string{"up", "done", "nobody"}, &out, &errb); code != 2 {
		t.Errorf("unknown worker: exit %d, want 2", code)
	}

	// A later qm up skips it.
	out.Reset()
	if code := run([]string{"up", "--dry-run"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "finished: marked done") {
		t.Errorf("exit %d, plan:\n%s", code, out.String())
	}
}

func TestUpDoneNoProject(t *testing.T) {
	newUpMachine(t)
	var out, errb bytes.Buffer
	if code := run([]string{"up", "done", "w1"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "no qm up project here") {
		t.Errorf("exit %d, stderr %q", code, errb.String())
	}
}

func writeConfig(t *testing.T, body string) {
	t.Helper()
	p := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "quartermaster", "config.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestUpEmptyDashDashIsNoArguments(t *testing.T) {
	m := newUpMachine(t)
	writeConfig(t, `{"up":{"claude_args":["--permission-mode","acceptEdits"]}}`)
	var out, errb bytes.Buffer
	if code := run([]string{"up", "--dry-run", "--"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "claude arguments []") {
		t.Errorf("dry run: exit %d, stdout %q", code, out.String())
	}
	if code := run([]string{"up", "--"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if p := loadProject(t, m.root); p.ClaudeArgs == nil || len(p.ClaudeArgs) != 0 {
		t.Errorf("saved claude args = %#v, want none", p.ClaudeArgs)
	}
}

func TestUpArgsReplacedOnlyOnceSaved(t *testing.T) {
	m := newUpMachine(t)
	m.project(t)
	swap(t, &upTerminal, func() bool { return true })
	swap[io.Reader](t, &upStdin, strings.NewReader("n\n"))
	var out, errb bytes.Buffer
	if code := run([]string{"up", "--", "--model", "opus"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if strings.Contains(out.String(), "replaced") || !slices.Equal(loadProject(t, m.root).ClaudeArgs, []string{"--permission-mode", "acceptEdits"}) {
		t.Errorf("declined run replaced the arguments: %q", out.String())
	}
	out.Reset()
	if code := run([]string{"up", "--yes", "--", "--model", "opus"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if !strings.Contains(out.String(), "coordinator claude arguments replaced: [\"--model\" \"opus\"]") || !slices.Equal(loadProject(t, m.root).ClaudeArgs, []string{"--model", "opus"}) {
		t.Errorf("stdout %q, args %v", out.String(), loadProject(t, m.root).ClaudeArgs)
	}
}

func TestUpLaterRunTmuxAttachHint(t *testing.T) {
	m := newUpMachine(t)
	m.project(t)
	swap(t, &upPick, func(string) (launch.Launcher, error) {
		return launch.Tmux{Bin: "tmux", Run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			m.term.calls = append(m.term.calls, strings.Join(args, " "))
			if args[0] == "-u" {
				args = args[1:]
			}
			switch args[0] {
			case "has-session":
				return nil, errors.New("can't find session")
			case "list-windows":
				return nil, errors.New("can't find session: proj")
			}
			return []byte("@7\n"), nil
		}}, nil
	})
	var out, errb bytes.Buffer
	if code := run([]string{"up", "--yes"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if !strings.Contains(out.String(), "resumed proj-coord (tmux @7)") || !strings.Contains(out.String(), "attach: tmux attach -t =proj\n") {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestUpRepinsAgtermRefUnderTmux(t *testing.T) {
	m := newUpMachine(t)
	p := m.project(t)
	p.Sessions = map[string]launch.SessionRef{upCoordID: {Launcher: "agterm", ID: "s-old", Name: "proj-coord"}}
	if err := up.Save(p); err != nil {
		t.Fatal(err)
	}
	swapProcs(t, func() ([]claudecode.Proc, error) {
		return []claudecode.Proc{{PID: 501, Cmd: "claude -n proj-coord --resume " + upCoordID}}, nil
	})
	swap(t, &upPick, func(string) (launch.Launcher, error) {
		return launch.Tmux{Bin: "tmux", Run: func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("unexpected") }}, nil
	})
	var out, errb bytes.Buffer
	if code := run([]string{"up", "--yes"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if !slices.Equal(m.term.calls, []string{"pin s-old"}) {
		t.Errorf("calls = %v, want the agterm ref pinned again", m.term.calls)
	}
}

func TestUpSelfHoldsAgtermSessionAndTmuxWindow(t *testing.T) {
	newUpMachine(t)
	t.Setenv("AGTERM_SESSION_ID", "s-here")
	t.Setenv("TMUX_PANE", "%3")
	swap(t, &upTmuxWindow, func(pane string) (string, error) {
		if pane != "%3" {
			return "", errors.New("wrong pane")
		}
		return "@9", nil
	})
	if got := selfSessions(); !slices.Equal(got, []string{"s-here", "@9"}) {
		t.Errorf("self = %v", got)
	}
}

func TestUpDoneUsesProjectID(t *testing.T) {
	// A coordinator whose shell sits in a worker's worktree: QM_PROJECT_ID
	// names the project, whatever the current folder.
	m := newUpMachine(t)
	p := m.project(t)
	m.worker(t, p, "w1")
	elsewhere := t.TempDir()
	swap(t, &upWorkDir, func() (string, error) { return elsewhere, nil })
	t.Setenv("QM_PROJECT_ID", p.ID)
	var out, errb bytes.Buffer
	if code := run([]string{"up", "done", "w1"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if _, ok := loadProject(t, m.root).Done[upWorkerID]; !ok {
		t.Errorf("w1 not marked done")
	}
	t.Setenv("QM_PROJECT_ID", "none-00000000")
	if code := run([]string{"up", "done", "w1"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "QM_PROJECT_ID") {
		t.Errorf("unknown project id: exit %d, stderr %q", code, errb.String())
	}
}

func TestUpPlanShowsReuseBeforeAsking(t *testing.T) {
	m := newUpMachine(t)
	p := m.project(t)
	p.Sessions = map[string]launch.SessionRef{upCoordID: {Launcher: "agterm", ID: "s-old", Name: "proj-coord"}}
	if err := up.Save(p); err != nil {
		t.Fatal(err)
	}
	m.term.open = map[string]string{"s-old": "proj-coord"}
	m.term.idle = map[string]bool{"s-old": true}
	swap(t, &upTerminal, func() bool { return true })
	swap[io.Reader](t, &upStdin, strings.NewReader("n\n"))
	var out, errb bytes.Buffer
	if code := run([]string{"up"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	s := out.String()
	line := "reuse proj-coord (agterm s-old)\n"
	if i, j := strings.Index(s, line), strings.Index(s, "Proceed? [y/N]"); i < 0 || j < i {
		t.Errorf("stdout lacks %q before the question:\n%s", line, s)
	}
	if len(m.term.acted()) != 0 {
		t.Errorf("acted before the answer: %v", m.term.calls)
	}
}

func TestUpFirstRunDryRunShowsTrust(t *testing.T) {
	m := newUpMachine(t)
	var out, errb bytes.Buffer
	if code := run([]string{"up", "--dry-run"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "trust: trusted") {
		t.Errorf("trusted root: exit %d, stdout %q", code, out.String())
	}
	other, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	swap(t, &upWorkDir, func() (string, error) { return other, nil })
	out.Reset()
	if code := run([]string{"up", "--dry-run"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "trust: not trusted by Claude Code: run claude in "+other) {
		t.Errorf("untrusted root: exit %d, stdout %q", code, out.String())
	}
	if len(m.term.calls) != 0 {
		t.Errorf("agtermctl %v", m.term.calls)
	}
}

func TestAttachHint(t *testing.T) {
	for _, tc := range []struct {
		l    launch.Launcher
		want string
	}{
		{launch.Tmux{}, "attach: tmux attach -t =proj\n"},
		{launch.Tmux{Inside: true}, "switch: tmux switch-client -t =proj\n"},
		{launch.Agterm{}, ""},
	} {
		var b bytes.Buffer
		attachHint(&b, tc.l, "proj", 1)
		if b.String() != tc.want {
			t.Errorf("%T inside %v: %q, want %q", tc.l, tc.l, b.String(), tc.want)
		}
	}
}

func TestUpNotesInPlan(t *testing.T) {
	// A .claude.json that does not parse reads as untrusting, with a note;
	// gh not signed in and a missing sessions folder are noted too.
	m := newUpMachine(t)
	m.project(t)
	if err := os.Remove(filepath.Join(m.claude, "sessions")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.claude, ".claude.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	swap(t, &upGhNote, func() string { return "gh not signed in (gh auth status failed): merged workers found by git only" })
	var out, errb bytes.Buffer
	if code := run([]string{"up", "--dry-run"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	for _, want := range []string{"note: Claude Code's .claude.json not read, every folder counted as not trusted", "note: gh not signed in", "note: Claude Code's sessions folder is missing", "not trusted"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout lacks %q:\n%s", want, out.String())
		}
	}
}
