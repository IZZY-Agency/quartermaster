package main

import (
	"bytes"
	"errors"
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

const upFreshID = "00000000-0000-4000-8000-0000000000f1"

// interrupted is a project whose worker w1 was being started fresh, as
// upFreshID, when qm ended: reserved in the state, no Record.
func (m *upMachine) interrupted(t *testing.T) {
	t.Helper()
	p := m.project(t)
	m.worker(t, p, "w1")
	recs, err := declog.Read(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	r := recs[len(recs)-1]
	r.Time, r.ClaudeSession, r.Replaces = time.Now().UTC(), upFreshID, upWorkerID
	p.Sessions = map[string]launch.SessionRef{upFreshID: {Name: "w1"}}
	p.Starting = map[string]route.Record{upFreshID: r}
	if err := up.Save(p); err != nil {
		t.Fatal(err)
	}
}

func workerIDs(t *testing.T) []string {
	t.Helper()
	recs, err := declog.Read(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range recs {
		if r.Kind == "worker" {
			out = append(out, r.ClaudeSession)
		}
	}
	return out
}

// The worker a stopped qm up had started runs: the next qm up leaves it and
// does not start a second one.
func TestUpInterruptedWorkerRunsIsLeft(t *testing.T) {
	m := newUpMachine(t)
	m.interrupted(t)
	swapProcs(t, func() ([]claudecode.Proc, error) {
		return []claudecode.Proc{{PID: 501, Cmd: "claude -n w1 --session-id " + upFreshID}}, nil
	})
	var out, errb bytes.Buffer
	if code := run([]string{"up", "--yes"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if !slices.Equal(workerIDs(t), []string{upWorkerID, upFreshID}) {
		t.Errorf("worker Records %v", workerIDs(t))
	}
	if !slices.Contains(upKinds(t), "w1 leave") || len(m.term.acted()) != 2 { // the coordinator's new tab and its pin
		t.Errorf("up Records %v, agtermctl %v", upKinds(t), m.term.acted())
	}
	if !strings.Contains(out.String(), "worker w1: an earlier qm up stopped while starting session 00000000") {
		t.Errorf("stdout %q", out.String())
	}
	if p := loadProject(t, m.root); len(p.Starting) != 0 {
		t.Errorf("starting %v", p.Starting)
	}
}

// It never ran: the session it was to replace is restored.
func TestUpInterruptedWorkerNeverRan(t *testing.T) {
	m := newUpMachine(t)
	m.interrupted(t)
	var out, errb bytes.Buffer
	if code := run([]string{"up", "--yes"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if !slices.Contains(upKinds(t), "w1 resume") {
		t.Errorf("up Records %v, want w1 resumed", upKinds(t))
	}
}

func TestUpInterruptedDryRunChangesNothing(t *testing.T) {
	m := newUpMachine(t)
	m.interrupted(t)
	before := loadProject(t, m.root)
	var out, errb bytes.Buffer
	if code := run([]string{"up", "--dry-run"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if after := loadProject(t, m.root); !after.Updated.Equal(before.Updated) || len(after.Starting) != 1 || len(workerIDs(t)) != 1 {
		t.Errorf("a dry run changed %+v, worker Records %v", after, workerIDs(t))
	}
}

// A Record that still cannot be logged fails the run, which goes on.
func TestUpInterruptedNotLoggedExits1(t *testing.T) {
	m := newUpMachine(t)
	m.interrupted(t)
	swap(t, &upLog, func(r route.Record) error {
		if r.Kind == "worker" {
			return errors.New("read-only file system")
		}
		return declog.Append(r)
	})
	swapProcs(t, func() ([]claudecode.Proc, error) {
		return []claudecode.Proc{{PID: 500, Cmd: "claude -n proj-coord --session-id " + upCoordID}, {PID: 501, Cmd: "claude -n w1 --session-id " + upFreshID}}, nil
	})
	var out, errb bytes.Buffer
	if code := run([]string{"up", "--yes"}, &out, &errb); code != 1 {
		t.Fatalf("exit %d, want 1; stderr %q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "qm up: worker w1: its Record (session 00000000) was not logged: read-only file system") || !slices.Contains(upKinds(t), "w1 leave") {
		t.Errorf("stderr %q, up Records %v", errb.String(), upKinds(t))
	}
	if p := loadProject(t, m.root); len(p.Starting) != 1 {
		t.Errorf("starting %v", p.Starting)
	}
}

// qm up done marks the worker a stopped qm up started, not the one it
// replaced.
func TestUpDoneMarksInterruptedWorker(t *testing.T) {
	m := newUpMachine(t)
	m.interrupted(t)
	m.fixture(t, "coord.jsonl", filepath.Join(m.claude, "projects", "p", upFreshID+".jsonl"))
	var out, errb bytes.Buffer
	if code := run([]string{"up", "done", "w1"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	p := loadProject(t, m.root)
	if _, ok := p.Done[upFreshID]; !ok || len(p.Done) != 1 {
		t.Errorf("done %v, want %s", p.Done, upFreshID)
	}
}
