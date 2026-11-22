package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
)

// liveIn adds a running claude process whose session file says it runs in
// cwd.
func (m *adoptMachine) liveIn(t *testing.T, pid int, id, name, cmd, cwd string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"pid": pid, "sessionId": id, "cwd": cwd, "name": name})
	dir := filepath.Join(m.claude, "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprint(pid)+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(m.claude, "projects", "p", id+".jsonl")
	m.fixture(t, "coord.jsonl", path)
	m.startedIn(t, path, m.root, cwd)
	m.procs = append(m.procs, claudecode.Proc{PID: pid, PPID: 1, Cmd: cmd})
}

// startedIn makes the transcript at path name dir, in place of from, as the
// folder its session started in (the fixture names the project root).
func (m *adoptMachine) startedIn(t *testing.T, path, from, dir string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	q := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	data = []byte(strings.ReplaceAll(string(data), `"cwd":`+q(from), `"cwd":`+q(dir)))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A session started in the project root that has since changed folder (its
// session file names the folder it works in now) is still the project's: it
// is listed, and adopted with the folder it is resumed from.
func TestUpAdoptSessionThatChangedFolder(t *testing.T) {
	m := newAdoptMachine(t)
	m.procs = nil
	elsewhere := t.TempDir()
	m.liveIn(t, 411, adoptCoordID, "proj-coord", "claude -n proj-coord", elsewhere)
	m.startedIn(t, filepath.Join(m.claude, "projects", "p", adoptCoordID+".jsonl"), elsewhere, m.root)
	code, out, errs := runQM("up", "--adopt")
	if code != 0 || !strings.Contains(out, "adopted coordinator proj-coord") {
		t.Fatalf("exit %d, out %q, err %q", code, out, errs)
	}
	if p, ok := stateOf(t, m.root); !ok || p.Coordinator.ID != adoptCoordID {
		t.Fatalf("state = %+v, %v", p, ok)
	}
}

// startedNothing fails when a state was written or a session started.
func (m *adoptMachine) startedNothing(t *testing.T, root string) {
	t.Helper()
	if _, ok := stateOf(t, root); ok || len(m.term.calls) != 0 || len(m.agterm.acted()) != 0 {
		t.Fatalf("acted: state %v, calls %q %q", ok, m.term.calls, m.agterm.calls)
	}
}

func TestUpAdoptNoCandidateFails(t *testing.T) {
	for _, args := range [][]string{{"up", "--adopt"}, {"up", "--adopt-coordinator", adoptCoordID}} {
		m := newAdoptMachine(t)
		m.procs = nil
		code, _, errs := runQM(args...)
		if code != 1 || !strings.Contains(errs, "no running session to adopt; qm up --new starts a new coordinator") {
			t.Fatalf("%q: exit %d, stderr %q", args, code, errs)
		}
		m.startedNothing(t, m.root)
	}
}

func TestUpBlindDetectionRefuses(t *testing.T) {
	for _, args := range [][]string{{"up"}, {"up", "--yes"}, {"up", "--adopt"}} {
		m := newAdoptMachine(t)
		if err := os.RemoveAll(filepath.Join(m.claude, "sessions")); err != nil {
			t.Fatal(err)
		}
		code, _, errs := runQM(args...)
		if code != 1 || !strings.Contains(errs, "sessions folder is missing") || !strings.Contains(errs, "qm up --new") {
			t.Fatalf("%q: exit %d, stderr %q", args, code, errs)
		}
		m.startedNothing(t, m.root)
	}
	m := newAdoptMachine(t)
	if err := os.RemoveAll(filepath.Join(m.claude, "sessions")); err != nil {
		t.Fatal(err)
	}
	if code, out, errs := runQM("up", "--new"); code != 0 || !strings.Contains(out, "started coordinator") {
		t.Fatalf("--new: exit %d, out %q, err %q", code, out, errs)
	}
}

func TestUpSubfolderSessionRefusesScripted(t *testing.T) {
	m := newAdoptMachine(t)
	m.procs = nil
	app := filepath.Join(m.root, "app")
	if err := os.Mkdir(app, 0o755); err != nil {
		t.Fatal(err)
	}
	m.liveIn(t, 411, adoptCoordID, "proj-coord", "claude -n proj-coord", app)
	code, _, errs := runQM("up", "--yes")
	if code != 1 || !strings.Contains(errs, "proj-coord") || !strings.Contains(errs, "runs in a subfolder: not adoptable") {
		t.Fatalf("--yes: exit %d, stderr %q", code, errs)
	}
	m.startedNothing(t, m.root)
	if code, _, errs = runQM("up", "--adopt"); code != 1 || !strings.Contains(errs, "no coordinator candidate") {
		t.Fatalf("--adopt: exit %d, stderr %q", code, errs)
	}
	if code, _, errs = runQM("up", "--adopt-coordinator", adoptCoordID); code != 2 || !strings.Contains(errs, "runs in a subfolder") {
		t.Fatalf("--adopt-coordinator: exit %d, stderr %q", code, errs)
	}
	m.startedNothing(t, m.root)
}

// A coordinator whose Bash tool sits in a worker's linked worktree runs
// qm up --adopt there: the worker must not become the coordinator of a
// project of its own.
func TestUpAdoptFromLinkedWorktreeRefuses(t *testing.T) {
	m := newAdoptMachine(t)
	m.procs = nil
	main, linked := linkedWorktree(t)
	m.liveIn(t, 411, adoptCoordID, "proj-coord", "claude -n proj-coord", main)
	m.liveIn(t, 413, adoptHandID, "w-hand", "claude --model haiku", linked)
	m.agterm.root = linked
	m.inside(411)
	swap(t, &upWorkDir, func() (string, error) { return linked, nil })
	code, _, errs := runQM("up", "--adopt")
	if code != 1 || !strings.Contains(errs, "linked worktree of "+main) {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
	m.startedNothing(t, linked)
	// Named explicitly, the session is adopted.
	if code, out, errs := runQM("up", "--adopt-coordinator", adoptHandID); code != 0 || !strings.Contains(out, "adopted coordinator w-hand") {
		t.Fatalf("explicit: exit %d, out %q, err %q", code, out, errs)
	}
}
