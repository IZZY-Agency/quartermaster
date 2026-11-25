package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/up"
)

// unreadable makes pid's session file one nothing can read (a folder in its
// place, which root cannot read as a file either) and returns its path.
func (m *upMachine) unreadable(t *testing.T, pid int) string {
	t.Helper()
	path := filepath.Join(m.claude, "sessions", fmt.Sprint(pid)+".json")
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// wantRefused fails unless errs names path, how to recover, and what was
// not done.
func wantRefused(t *testing.T, code int, errs, path string, also ...string) {
	t.Helper()
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	for _, s := range append(also, path, "make it readable") {
		if !strings.Contains(errs, s) {
			t.Errorf("stderr %q, want it to hold %q", errs, s)
		}
	}
}

// A restore that would start a session refuses while a running claude's
// session file cannot be read: it may be that session's. On every path: with
// a launcher, without one (the lines to run by hand) and in a dry run.
func TestUpRestoreRefusesOnIncompleteSessions(t *testing.T) {
	for _, launcher := range []bool{true, false} {
		for _, flag := range []string{"--yes", "--dry-run"} {
			t.Run(fmt.Sprintf("%s launcher %v", flag, launcher), func(t *testing.T) {
				m := newUpMachine(t)
				p := m.project(t)
				m.worker(t, p, "w1")
				if !launcher {
					swap(t, &upPick, func(string) (launch.Launcher, error) { return nil, launch.ErrNoLauncher })
				}
				swapProcs(t, func() ([]claudecode.Proc, error) {
					return []claudecode.Proc{{PID: 77, PPID: 1, Cmd: "claude"}}, nil
				})
				path := m.unreadable(t, 77)
				code, out, errs := runQM("up", flag)
				wantRefused(t, code, errs, path, "process 77", "nothing started")
				if a := m.term.acted(); len(a) != 0 {
					t.Errorf("agterm called: %q", a)
				}
				if k := upKinds(t); len(k) != 0 || strings.Contains(out, "/bin/sh ") {
					t.Errorf("logged %v, stdout %q, want nothing started", k, out)
				}
				if !strings.Contains(out, "proj-coord") {
					t.Errorf("stdout %q, want the plan", out)
				}
			})
		}
	}
}

// The refusal goes by the read the plan was made from: one that recovers
// after it does not let a plan stand that starts a running session.
func TestUpRestoreRefusesOnThePlansOwnRead(t *testing.T) {
	for _, flag := range []string{"--yes", "--dry-run"} {
		m := newUpMachine(t)
		m.project(t)
		path := filepath.Join(m.claude, "sessions", "77.json")
		reads := 0
		swapProcs(t, func() ([]claudecode.Proc, error) {
			if reads++; reads == 2 {
				if err := os.Remove(path); err != nil {
					t.Error(err)
				}
				m.sessionFile(t, 77, upCoordID)
			}
			return []claudecode.Proc{{PID: 77, PPID: 1, Cmd: "claude"}}, nil
		})
		m.unreadable(t, 77)
		code, _, errs := runQM("up", flag)
		wantRefused(t, code, errs, path, "nothing started")
		if a := m.term.acted(); len(a) != 0 {
			t.Errorf("agterm called: %q", a)
		}
	}
}

// Without a launcher, too, the running sessions are read again before each
// launch line is made: a session resumed, or data lost, while qm up asked
// gets none.
func TestUpNoLauncherReadsAgainAfterTheQuestion(t *testing.T) {
	for name, tc := range map[string]struct {
		meanwhile func(t *testing.T, m *upMachine)
		want      string
	}{
		"resumed by hand": {func(t *testing.T, m *upMachine) { m.sessionFile(t, 77, upCoordID) }, "started running while qm up asked"},
		"data lost":       {func(t *testing.T, m *upMachine) { m.unreadable(t, 77) }, "were not read again"},
	} {
		t.Run(name, func(t *testing.T) {
			m := newUpMachine(t)
			m.project(t)
			swap(t, &upPick, func(string) (launch.Launcher, error) { return nil, launch.ErrNoLauncher })
			swapProcs(t, func() ([]claudecode.Proc, error) {
				return []claudecode.Proc{{PID: 77, PPID: 1, Cmd: "claude"}}, nil
			})
			swap(t, &upTerminal, func() bool { return true })
			swap(t, &upStdin, io.Reader(&answer{func() { tc.meanwhile(t, m) }}))
			code, out, errs := runQM("up")
			if code != 1 || strings.Contains(out, "/bin/sh ") || !strings.Contains(errs, tc.want) {
				t.Errorf("exit %d, stdout %q, stderr %q, want no launch line and %q", code, out, errs, tc.want)
			}
			if name == "resumed by hand" && !strings.Contains(out, "left proj-coord running") {
				t.Errorf("stdout %q, want it said", out)
			}
		})
	}
}

// A reservation an interrupted qm up left stays as it is on incomplete data:
// its Record is not logged and nothing starts, though the plan would.
func TestUpInterruptedStartKeptOnIncompleteSessions(t *testing.T) {
	for _, flag := range []string{"--yes", "--dry-run"} {
		m := newUpMachine(t)
		m.interrupted(t)
		before, ids := loadProject(t, m.root), workerIDs(t)
		swapProcs(t, func() ([]claudecode.Proc, error) {
			return []claudecode.Proc{{PID: 77, PPID: 1, Cmd: "claude"}}, nil
		})
		path := m.unreadable(t, 77)
		code, _, errs := runQM("up", flag)
		wantRefused(t, code, errs, path, "nothing started")
		after := loadProject(t, m.root)
		if len(after.Starting) != 1 || !after.Updated.Equal(before.Updated) || len(workerIDs(t)) != len(ids) || len(m.term.acted()) != 0 {
			t.Errorf("%s: starting %v, workers %v, agterm %q, want all as it was", flag, after.Starting, workerIDs(t), m.term.acted())
		}
	}
}

// answer is a yes typed after first ran.
type answer struct{ first func() }

func (a *answer) Read(b []byte) (int, error) {
	if a.first == nil {
		return 0, io.EOF
	}
	a.first()
	a.first = nil
	return copy(b, "y\n"), nil
}

// sessionFile writes the session file of process pid, holding session id.
func (m *upMachine) sessionFile(t *testing.T, pid int, id string) {
	t.Helper()
	b := fmt.Sprintf(`{"pid":%d,"sessionId":%q,"cwd":%q}`, pid, id, m.root)
	if err := os.WriteFile(filepath.Join(m.claude, "sessions", fmt.Sprint(pid)+".json"), []byte(b), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A sessions folder that exists but cannot be listed refuses the same way.
func TestUpRestoreRefusesOnUnlistableSessions(t *testing.T) {
	m := newUpMachine(t)
	m.project(t)
	path := filepath.Join(m.claude, "sessions")
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errs := runQM("up", "--yes")
	wantRefused(t, code, errs, path, "nothing started")
	if a := m.term.acted(); len(a) != 0 {
		t.Errorf("agterm called: %q", a)
	}
}

// The unreadable file of a process that ended stops nothing.
func TestUpRestoreIgnoresUnreadableFileOfDeadProcess(t *testing.T) {
	m := newUpMachine(t)
	m.project(t)
	m.unreadable(t, 77)
	if code, out, errs := runQM("up", "--yes"); code != 0 || !strings.Contains(out, "resumed proj-coord") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
}

// A restore with nothing to start goes on, with a note, and still names the
// running sessions the project does not hold.
func TestUpRestoreWithNothingToStartNotesIncompleteSessions(t *testing.T) {
	m := newLaterMachine(t)
	m.procs = append(m.procs, claudecode.Proc{PID: 77, PPID: 1, Cmd: "claude"})
	path := m.unreadable(t, 77)
	for _, flag := range []string{"--yes", "--dry-run"} {
		code, out, errs := runQM("up", flag)
		if code != 0 || !strings.Contains(out, "note: ") || !strings.Contains(out, path) || !strings.Contains(out, laterNote) {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", flag, code, out, errs)
		}
	}
}

// The reads Apply and the permission cap decide with fail on incomplete
// data, unless it answers them anyway.
func TestUpLiveReadsFailOnIncompleteSessions(t *testing.T) {
	m := newAdoptMachine(t)
	if args, live, err := upCoordinatorArgs(adoptHandID); err != nil || !live || len(args) != 0 {
		t.Fatalf("complete: %q, %v, %v", args, live, err)
	}
	if _, live, err := upCoordinatorArgs(upCoordID); err != nil || live {
		t.Fatalf("complete, stopped: %v, %v", live, err)
	}
	if _, err := upLive(); err != nil {
		t.Fatal(err)
	}
	m.unreadable(t, 413)
	var inc *claudecode.IncompleteError
	if live, err := upLive(); !errors.As(err, &inc) || live[adoptCoordID].PID != 411 {
		t.Errorf("upLive = %+v, %v, want an IncompleteError and the readable session", live, err)
	}
	if _, live, err := upCoordinatorArgs(adoptHandID); !errors.As(err, &inc) || live {
		t.Errorf("upCoordinatorArgs(unread) = %v, %v, want an IncompleteError", live, err)
	}
	if _, live, err := upCoordinatorArgs(adoptCoordID); err != nil || !live {
		t.Errorf("upCoordinatorArgs(read) = %v, %v, want it live", live, err)
	}
	if _, err := upAdoptDeps(up.Deps{}, m.claude).Live(); !errors.As(err, &inc) {
		t.Errorf("AdoptDeps.Live err = %v, want an IncompleteError", err)
	}
}

// Adoption refuses before it asks: a first run and a later one.
func TestUpAdoptRefusesOnIncompleteSessions(t *testing.T) {
	m := newAdoptMachine(t)
	m.procs = append(m.procs, claudecode.Proc{PID: 77, PPID: 1, Cmd: "claude"})
	path := m.unreadable(t, 77)
	for _, args := range [][]string{{"up"}, {"up", "--adopt", "--yes"}, {"up", "--dry-run"}} {
		code, _, errs := runQM(args...)
		wantRefused(t, code, errs, path, "qm up --new")
	}
	if _, ok := stateOf(t, m.root); ok || len(m.agterm.acted()) != 0 || len(m.term.acted()) != 0 {
		t.Errorf("saved %v, agterm %q %q, want nothing", ok, m.agterm.acted(), m.term.acted())
	}

	m = newLaterMachine(t)
	m.procs = append(m.procs, claudecode.Proc{PID: 77, PPID: 1, Cmd: "claude"})
	path = m.unreadable(t, 77)
	code, _, errs := runQM("up", "--adopt", "--yes")
	wantRefused(t, code, errs, path, "nothing adopted")
	if p := loadProject(t, m.root); len(p.Adopted) != 0 {
		t.Errorf("adopted %+v", p.Adopted)
	}
}

// qm up --new starts a new session, so it needs no complete data.
func TestUpNewWithIncompleteSessions(t *testing.T) {
	m := newAdoptMachine(t)
	m.unreadable(t, 413)
	if code, out, errs := runQM("up", "--new", "--yes"); code != 0 {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
}

// The caller's own unreadable session file is named, rather than the caller
// taken for no session; another process's changes nothing.
func TestCallerSessionsUnread(t *testing.T) {
	m := newAdoptMachine(t)
	m.inside(411)
	m.unreadable(t, 413)
	if ids, err := callerSessions(); err != nil || len(ids) != 1 || ids[0] != adoptCoordID {
		t.Errorf("another's file unread: %q, %v", ids, err)
	}
	path := m.unreadable(t, 411)
	if ids, err := callerSessions(); err == nil || !strings.Contains(err.Error(), path) || len(ids) != 0 {
		t.Errorf("own file unread: %q, %v, want an error naming %s", ids, err, path)
	}
}

// qm down's check of a tmux window needs only the session it is about.
func TestDownPaneRunsWithIncompleteSessions(t *testing.T) {
	m := newAdoptMachine(t)
	swap(t, &upPaneRuns, func(_ context.Context, _ string, pid int) (bool, error) { return pid == 411, nil })
	path := m.unreadable(t, 413)
	ref := launch.SessionRef{Launcher: "tmux", ID: "@1"}
	if runs, err := (refTerms{}).PaneRuns(context.Background(), ref, adoptCoordID); err != nil || !runs {
		t.Errorf("read session: %v, %v, want it to run", runs, err)
	}
	if runs, err := (refTerms{}).PaneRuns(context.Background(), ref, adoptHandID); err == nil || runs || !strings.Contains(err.Error(), path) {
		t.Errorf("unread session: %v, %v, want an error naming %s", runs, err, path)
	}
}

// qm up done goes on as it did.
func TestUpDoneWithIncompleteSessions(t *testing.T) {
	m := newUpMachine(t)
	p := m.project(t)
	m.worker(t, p, "w1")
	swapProcs(t, func() ([]claudecode.Proc, error) {
		return []claudecode.Proc{{PID: 77, PPID: 1, Cmd: "claude"}}, nil
	})
	m.unreadable(t, 77)
	if code, out, errs := runQM("up", "done", "w1"); code != 0 {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
}
