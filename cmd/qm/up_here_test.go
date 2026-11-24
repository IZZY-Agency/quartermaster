package main

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/up"
)

// hereMachine is an upMachine whose tab is agterm session t-here, listed in
// the workspace "proj", with a terminal on standard input and output, and
// an exec that records the script it would run in place of qm.
func hereMachine(t *testing.T) (*upMachine, *[]string) {
	t.Helper()
	m := newUpMachine(t)
	t.Setenv("AGTERM_SESSION_ID", "t-here")
	m.term.show("t-here")
	m.term.fg = map[string][]string{"t-here": {"qm", "up", "--here"}}
	swap(t, &upArgv, []string{"qm", "up", "--here"})
	swap(t, &upAgtermCtl, launch.Agterm{Bin: "agtermctl", Run: m.term.run})
	swap(t, &upHereTerminal, func() bool { return true })
	var execd []string
	swap(t, &upExec, func(script string) error {
		execd = append(execd, script)
		return nil
	})
	return m, &execd
}

func hasCall(calls []string, prefix string) bool {
	return slices.ContainsFunc(calls, func(c string) bool { return strings.HasPrefix(c, prefix) })
}

// A restore with a worker: the worker starts through the launcher, the state
// is saved with both sessions and every Record logged, the project lock is
// free, and only then is qm replaced by the coordinator's script.
func TestUpHereRestoreStartsTheCoordinatorLastInThisTab(t *testing.T) {
	m, execd := hereMachine(t)
	p := m.project(t)
	m.worker(t, p, "w1")
	swap(t, &upExec, func(script string) error {
		*execd = append(*execd, script)
		if !hasCall(m.term.calls, "session new --workspace-name proj --create-workspace --cwd "+m.root+" --name w1") {
			t.Errorf("exec before the worker started: calls %v", m.term.calls)
		}
		if hasCall(m.term.calls, "session new --workspace-name proj --create-workspace --cwd "+m.root+" --name proj-coord") {
			t.Errorf("the coordinator was launched in a new tab: %v", m.term.calls)
		}
		after := loadProject(t, m.root)
		if after.Sessions[upCoordID].ID != "t-here" || !after.Sessions[upCoordID].Adopted || after.Sessions[upWorkerID].ID != "s-1" {
			t.Errorf("state at exec = %+v", after.Sessions)
		}
		if kinds := upKinds(t); !slices.Contains(kinds, "proj-coord resume") || !slices.Contains(kinds, "w1 resume") {
			t.Errorf("Records at exec = %v", kinds)
		}
		unlock, err := up.Lock(up.ProjectID(m.root))
		if err != nil {
			t.Errorf("project lock still held at exec: %v", err)
		} else {
			unlock()
		}
		if !strings.HasPrefix(filepath.Base(script), "proj-coord") {
			t.Errorf("exec'd %s, want the coordinator's script", script)
		}
		return nil
	})
	var out, errb bytes.Buffer
	if code := run([]string{"up", "--yes", "--here"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stdout %q stderr %q", code, out.String(), errb.String())
	}
	if len(*execd) != 1 {
		t.Fatalf("exec'd %v, want the coordinator's script once", *execd)
	}
	for _, want := range []string{"the coordinator will start in this tab", "resuming proj-coord in this tab (agterm t-here)", "resumed w1 (agterm s-1)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout %q, want %q", out.String(), want)
		}
	}
	if !hasCall(m.term.calls, "session restore --none --target t-here") {
		t.Errorf("this tab not pinned: %v", m.term.calls)
	}
}

// A first run: the workspace is created, this tab moved into it and pinned,
// the state saved with it, and the coordinator exec'd; no tab is opened.
func TestUpHereFirstRun(t *testing.T) {
	m, execd := hereMachine(t)
	var out, errb bytes.Buffer
	if code := run([]string{"up", "--here"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stdout %q stderr %q", code, out.String(), errb.String())
	}
	p := loadProject(t, m.root)
	want := []string{"workspace new --json -- " + p.Workspace, "session move w-new --target t-here --json", "session restore --none --target t-here --json"}
	if got := m.term.acted(); !slices.Equal(got, want) {
		t.Errorf("agtermctl calls = %v, want %v", got, want)
	}
	if ref := p.Sessions[p.Coordinator.ID]; ref.ID != "t-here" || !ref.Adopted {
		t.Errorf("saved ref = %+v", ref)
	}
	if len(*execd) != 1 || !strings.Contains(out.String(), "starting coordinator "+p.Coordinator.Name+" in this tab (agterm t-here)") {
		t.Errorf("exec'd %v, stdout %q", *execd, out.String())
	}
}

// With neither agterm nor tmux, and no launcher, the coordinator still
// starts here with no terminal session recorded.
func TestUpHereWithNoLauncherOrTab(t *testing.T) {
	m, execd := hereMachine(t)
	t.Setenv("AGTERM_SESSION_ID", "")
	swap(t, &upPick, func(string) (launch.Launcher, error) { return nil, launch.ErrNoLauncher })
	var out, errb bytes.Buffer
	if code := run([]string{"up", "--here"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stdout %q stderr %q", code, out.String(), errb.String())
	}
	p := loadProject(t, m.root)
	if len(*execd) != 1 || len(p.Sessions) != 0 || !strings.Contains(out.String(), "in this tab (no terminal session recorded)") || !strings.Contains(errb.String(), "neither agterm nor tmux") {
		t.Errorf("exec'd %v, sessions %+v, stdout %q, stderr %q", *execd, p.Sessions, out.String(), errb.String())
	}
}

// A step that fails before the exec execs nothing: here, the running
// sessions cannot be read again right before the starts.
func TestUpHereFailedStepDoesNotExec(t *testing.T) {
	m, execd := hereMachine(t)
	m.project(t)
	n := 0
	swapProcs(t, func() ([]claudecode.Proc, error) {
		if n++; n > 1 {
			return nil, errors.New("ps failed")
		}
		return nil, nil
	})
	var out, errb bytes.Buffer
	if code := run([]string{"up", "--yes", "--here"}, &out, &errb); code != 1 || len(*execd) != 0 {
		t.Errorf("exit %d, exec'd %v; stderr %q", code, *execd, errb.String())
	}
}

// A first run that stops (an untrusted folder) execs nothing.
func TestUpHereFirstRunThatStopsDoesNotExec(t *testing.T) {
	_, execd := hereMachine(t)
	swap(t, &upWorkDir, func() (string, error) { return t.TempDir(), nil })
	var out, errb bytes.Buffer
	if code := run([]string{"up", "--here"}, &out, &errb); code != 1 || len(*execd) != 0 {
		t.Errorf("exit %d, exec'd %v; stderr %q", code, *execd, errb.String())
	}
}

func TestUpHereUsageErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		args     []string
		terminal bool
		live     bool
		want     string
	}{
		"with --new-tab":           {args: []string{"--here", "--new-tab"}, terminal: true, want: "opposites"},
		"with --adopt":             {args: []string{"--here", "--adopt"}, terminal: true, want: "--adopt"},
		"with --adopt-coordinator": {args: []string{"--here", "--adopt-coordinator", "abc"}, terminal: true, want: "--adopt"},
		"without a terminal":       {args: []string{"--here"}, want: "needs a terminal"},
		"live coordinator":         {args: []string{"--here", "--yes"}, terminal: true, live: true, want: "no coordinator starts"},
	} {
		t.Run(name, func(t *testing.T) {
			m, execd := hereMachine(t)
			m.project(t)
			swap(t, &upHereTerminal, func() bool { return tc.terminal })
			if tc.live {
				swapProcs(t, func() ([]claudecode.Proc, error) {
					return []claudecode.Proc{{PID: 501, Cmd: "claude -n proj-coord --resume " + upCoordID}}, nil
				})
			}
			var out, errb bytes.Buffer
			if code := run(append([]string{"up"}, tc.args...), &out, &errb); code != 2 || !strings.Contains(errb.String(), tc.want) {
				t.Errorf("exit %d, stderr %q, want 2 and %q", code, errb.String(), tc.want)
			}
			if len(*execd) != 0 || len(m.term.acted()) != 0 {
				t.Errorf("exec'd %v, acted %v", *execd, m.term.acted())
			}
		})
	}
}

// The config's "here" is not applied where no coordinator starts: a live
// coordinator is left, and nothing is exec'd.
func TestUpHereConfigIgnoredWhenNoCoordinatorStarts(t *testing.T) {
	m, execd := hereMachine(t)
	m.project(t)
	writeConfig(t, `{"up":{"coordinator_tab":"here"}}`)
	swapProcs(t, func() ([]claudecode.Proc, error) {
		return []claudecode.Proc{{PID: 501, Cmd: "claude -n proj-coord --resume " + upCoordID}}, nil
	})
	var out, errb bytes.Buffer
	if code := run([]string{"up", "--yes"}, &out, &errb); code != 0 || len(*execd) != 0 {
		t.Errorf("exit %d, exec'd %v, stderr %q", code, *execd, errb.String())
	}
	if strings.Contains(out.String(), "coordinator will start") {
		t.Errorf("stdout %q names a tab for a coordinator that does not start", out.String())
	}
}

// The config's "here" without a terminal falls back to a new tab, with a note.
func TestUpHereConfigWithoutATerminalFallsBackToANewTab(t *testing.T) {
	m, execd := hereMachine(t)
	swap(t, &upHereTerminal, func() bool { return false })
	writeConfig(t, `{"up":{"coordinator_tab":"here"}}`)
	var out, errb bytes.Buffer
	if code := run([]string{"up"}, &out, &errb); code != 0 || len(*execd) != 0 {
		t.Fatalf("exit %d, exec'd %v, stderr %q", code, *execd, errb.String())
	}
	if !strings.Contains(out.String(), "note: up.coordinator_tab is here, but standard input or output is not a terminal") || !hasCall(m.term.calls, "session new") {
		t.Errorf("stdout %q, calls %v", out.String(), m.term.calls)
	}
}

// --new-tab wins over the config's "here"; the config's "here" alone starts
// the coordinator here.
func TestUpHereFlagsWinOverTheConfig(t *testing.T) {
	m, execd := hereMachine(t)
	writeConfig(t, `{"up":{"coordinator_tab":"here"}}`)
	var out, errb bytes.Buffer
	if code := run([]string{"up", "--new-tab"}, &out, &errb); code != 0 || len(*execd) != 0 || !hasCall(m.term.calls, "session new") {
		t.Fatalf("--new-tab: exit %d, exec'd %v, calls %v, stderr %q", code, *execd, m.term.calls, errb.String())
	}

	m, execd = hereMachine(t)
	writeConfig(t, `{"up":{"coordinator_tab":"here"}}`)
	if code := run([]string{"up"}, &out, &errb); code != 0 || len(*execd) != 1 || hasCall(m.term.calls, "session new") {
		t.Errorf("config here: exit %d, exec'd %v, calls %v", code, *execd, m.term.calls)
	}
}

func TestUpHereConfigInvalidValue(t *testing.T) {
	_, execd := hereMachine(t)
	writeConfig(t, `{"up":{"coordinator_tab":"left"}}`)
	var out, errb bytes.Buffer
	if code := run([]string{"up", "--dry-run"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "up.coordinator_tab") || len(*execd) != 0 {
		t.Errorf("exit %d, stderr %q", code, errb.String())
	}
}

// --dry-run says where the coordinator would start and starts nothing.
func TestUpHereDryRun(t *testing.T) {
	m, execd := hereMachine(t)
	var out, errb bytes.Buffer
	if code := run([]string{"up", "--here", "--dry-run"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "the coordinator would start in this tab") {
		t.Errorf("first run: exit %d, stdout %q", code, out.String())
	}
	m.project(t)
	out.Reset()
	if code := run([]string{"up", "--dry-run"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "the coordinator would start in a new tab") {
		t.Errorf("restore: exit %d, stdout %q", code, out.String())
	}
	out.Reset()
	if code := run([]string{"up", "--dry-run", "--here"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "the coordinator would start in this tab") {
		t.Errorf("restore --here: exit %d, stdout %q", code, out.String())
	}
	if len(*execd) != 0 || len(m.term.acted()) != 0 {
		t.Errorf("exec'd %v, acted %v", *execd, m.term.acted())
	}
}

// An exec that fails is reported with the line to run by hand, and the tab
// recorded for the coordinator is taken back: qm down never closes it.
func TestUpHereExecFailure(t *testing.T) {
	m, _ := hereMachine(t)
	swap(t, &upExec, func(string) error { return errors.New("exec refused") })
	var out, errb bytes.Buffer
	if code := run([]string{"up", "--here"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "exec refused") || !strings.Contains(errb.String(), "/bin/sh") {
		t.Errorf("exit %d, stderr %q", code, errb.String())
	}
	if p := loadProject(t, m.root); len(p.Sessions) != 0 {
		t.Errorf("sessions after a failed exec = %+v, want none", p.Sessions)
	}
}

// A tab whose foreground is not this qm (a terminal started from it inherits
// its id) is not recorded; the coordinator still starts here.
func TestUpHereTabNotRunningQM(t *testing.T) {
	m, execd := hereMachine(t)
	m.term.fg["t-here"] = []string{"vim", "notes"}
	var out, errb bytes.Buffer
	if code := run([]string{"up", "--here"}, &out, &errb); code != 0 || len(*execd) != 1 {
		t.Fatalf("exit %d, exec'd %v, stderr %q", code, *execd, errb.String())
	}
	if p := loadProject(t, m.root); len(p.Sessions) != 0 || !strings.Contains(errb.String(), "does not run this qm") || len(m.term.acted()) != 0 {
		t.Errorf("sessions %+v, acted %v, stderr %q", p.Sessions, m.term.acted(), errb.String())
	}
}

// A lone -- among the coordinator's arguments is refused, given or saved.
func TestUpRefusesALoneDoubleDash(t *testing.T) {
	m := newUpMachine(t)
	var out, errb bytes.Buffer
	if code := run([]string{"up", "--", "--"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "lone --") {
		t.Errorf("given: exit %d, stderr %q", code, errb.String())
	}
	p := m.project(t)
	p.ClaudeArgs = []string{"--"}
	if err := up.Save(p); err != nil {
		t.Fatal(err)
	}
	errb.Reset()
	if code := run([]string{"up", "--yes"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "saved coordinator arguments") || len(m.term.acted()) != 0 {
		t.Errorf("saved: exit %d, stderr %q, acted %v", code, errb.String(), m.term.acted())
	}
	// With nothing to start, the saved arguments are not used: no error.
	swapProcs(t, func() ([]claudecode.Proc, error) {
		return []claudecode.Proc{{PID: 501, Cmd: "claude -n proj-coord --resume " + upCoordID}}, nil
	})
	errb.Reset()
	if code := run([]string{"up", "--yes"}, &out, &errb); code != 0 {
		t.Errorf("nothing to start: exit %d, stderr %q", code, errb.String())
	}
}

// A first run with no launcher says the coordinator starts from its line.
func TestUpFirstRunDryRunWithoutALauncher(t *testing.T) {
	newUpMachine(t)
	swap(t, &upPick, func(string) (launch.Launcher, error) { return nil, launch.ErrNoLauncher })
	var out, errb bytes.Buffer
	if code := run([]string{"up", "--dry-run"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "the coordinator would start from its launch line") {
		t.Errorf("exit %d, stdout %q", code, out.String())
	}
}

// Without --here, a restore names the coordinator's earlier tab when the
// plan reuses it.
func TestUpRestoreNamesAReusedCoordinatorTab(t *testing.T) {
	m := newUpMachine(t)
	p := m.project(t)
	p.Sessions = map[string]launch.SessionRef{upCoordID: {Launcher: "agterm", ID: "s-coord", Name: "proj-coord"}}
	if err := up.Save(p); err != nil {
		t.Fatal(err)
	}
	m.term.showIdle("s-coord")
	var out, errb bytes.Buffer
	if code := run([]string{"up", "--dry-run"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "the coordinator would start in its earlier tab (agterm s-coord), reused") {
		t.Errorf("exit %d, stdout %q", code, out.String())
	}
}

// A first run that asks and adopts a running coordinator starts none: --here
// is refused after the choice, and the config's "here" is not applied.
func TestUpHereAdoptChosenWhenAsked(t *testing.T) {
	m := newAdoptMachine(t)
	swap(t, &upTerminal, func() bool { return true })
	swap(t, &upHereTerminal, func() bool { return true })
	var execd []string
	swap(t, &upExec, func(script string) error { execd = append(execd, script); return nil })
	swap(t, &upStdin, io.Reader(strings.NewReader("1\n2\ny\n")))
	if code, _, errs := runQM("up", "--here"); code != 2 || !strings.Contains(errs, "adopting a running one starts none") {
		t.Errorf("--here: exit %d, stderr %q", code, errs)
	}
	if _, ok := stateOf(t, m.root); ok || len(execd) != 0 {
		t.Errorf("state written or exec'd %v", execd)
	}
	writeConfig(t, `{"up":{"coordinator_tab":"here"}}`)
	swap(t, &upStdin, io.Reader(strings.NewReader("1\n2\ny\n")))
	if code, _, errs := runQM("up"); code != 0 || len(execd) != 0 {
		t.Errorf("config here: exit %d, exec'd %v, stderr %q", code, execd, errs)
	}
	if p, ok := stateOf(t, m.root); !ok || !p.Coordinator.Adopted {
		t.Errorf("state %+v, want the coordinator adopted", p)
	}
}

func TestUpHelpNamesHereAndNewTab(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"up", "--help"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "[--here | --new-tab]") || !strings.Contains(out.String(), "up.coordinator_tab") {
		t.Errorf("exit %d, help:\n%s", code, out.String())
	}
}
