package up

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
	"github.com/IZZY-Agency/quartermaster/internal/route"
)

// capWorld is a project saved with bypass, its coordinator running in plan
// mode, a bypass worker with a launch profile (w-warm) and a hand-started
// bypass worker (w-hand), both to restore.
func capWorld(t *testing.T) (*world, []Step, Deps, []claudecode.Proc) {
	t.Helper()
	var logged []route.Record
	d := upDeps(t, &fakeAgterm{ws: "repo"}, &logged)
	w := newWorld(t)
	w.project.ClaudeArgs = []string{"--dangerously-skip-permissions"}
	w.project.Adopted = map[string]AdoptedSession{
		smallID: {Name: "w-hand", Cwd: filepath.Join(w.root, "worktrees", "small"), Model: "opus", Args: []string{"--dangerously-skip-permissions", "--model", "opus"}, Started: now.Add(-4 * time.Hour)},
	}
	recs := []route.Record{profiled(w, warmID, "w-warm", "warm", now.Add(-time.Hour), "--dangerously-skip-permissions")}
	procs := []claudecode.Proc{{PID: 301, Cmd: "claude -n repo-coord --permission-mode plan"}}
	return w, w.plan(t, recs, procs), d, procs
}

// A live coordinator stricter than the saved arguments caps both kinds of
// worker at its own flags.
func TestUpWorkersCappedAtLiveCoordinator(t *testing.T) {
	w, steps, d, procs := capWorld(t)
	d.CoordinatorArgs = func(id string) ([]string, bool, error) {
		live := map[string]claudecode.LiveSession{coordID: {PID: 301, SessionID: coordID}}
		return LiveArgs(id, live, procs)
	}
	outs := Apply(context.Background(), &w.project, steps, d)
	for _, name := range []string{"w-warm", "w-hand"} {
		o := outcomeOf(t, outs, name)
		if o.Err != nil || o.Step.Action != Resume {
			t.Fatalf("%s outcome = %+v", name, o)
		}
		script := readFile(t, o.Script)
		if strings.Contains(script, "--dangerously-skip-permissions") || !strings.Contains(script, " claude --permission-mode plan ") {
			t.Errorf("%s not capped at the live coordinator's plan mode:\n%s", name, script)
		}
	}
}

// A stopped coordinator caps at the saved arguments.
func TestUpWorkersCappedAtSavedWhenCoordinatorStopped(t *testing.T) {
	w, steps, d, _ := capWorld(t)
	w.project.ClaudeArgs = []string{"--permission-mode", "acceptEdits"}
	d.CoordinatorArgs = func(string) ([]string, bool, error) { return nil, false, nil }
	outs := Apply(context.Background(), &w.project, steps, d)
	for _, name := range []string{"w-warm", "w-hand"} {
		o := outcomeOf(t, outs, name)
		if o.Err != nil {
			t.Fatalf("%s outcome = %+v", name, o)
		}
		if script := readFile(t, o.Script); !strings.Contains(script, " claude --permission-mode acceptEdits ") {
			t.Errorf("%s not capped at the saved acceptEdits:\n%s", name, script)
		}
	}
}

// When the running coordinator's flags cannot be read, no worker starts:
// the saved flags could allow more than the coordinator has.
func TestUpWorkersNotStartedWhenCoordinatorFlagsUnread(t *testing.T) {
	w, steps, d, _ := capWorld(t)
	d.CoordinatorArgs = func(string) ([]string, bool, error) { return nil, true, errors.New("ps failed") }
	outs := Apply(context.Background(), &w.project, steps, d)
	for _, name := range []string{"w-warm", "w-hand"} {
		o := outcomeOf(t, outs, name)
		if o.Err == nil || !strings.Contains(o.Err.Error(), "permission flags") || o.Script != "" {
			t.Errorf("%s outcome = %+v, want a refusal naming the permission flags", name, o)
		}
	}
}

func TestLiveArgs(t *testing.T) {
	live := map[string]claudecode.LiveSession{"a": {PID: 7}, "b": {PID: 8}}
	procs := []claudecode.Proc{{PID: 7, Cmd: "claude --permission-mode plan"}, {PID: 8, Cmd: "vim"}}
	if args, ok, err := LiveArgs("a", live, procs); err != nil || !ok || strings.Join(args, " ") != "--permission-mode plan" {
		t.Errorf("a: %v %v %v", args, ok, err)
	}
	if _, ok, err := LiveArgs("c", live, procs); err != nil || ok {
		t.Errorf("c: %v %v", ok, err)
	}
	if _, ok, err := LiveArgs("b", live, procs); err == nil || !ok {
		t.Errorf("b: %v %v", ok, err)
	}
}

// The cap never raises: a worker in plan mode stays in plan mode under an
// acceptEdits coordinator, on both script paths.
func TestUpWorkerCapNeverRaises(t *testing.T) {
	var logged []route.Record
	d := upDeps(t, &fakeAgterm{ws: "repo"}, &logged)
	w := newWorld(t)
	w.project.ClaudeArgs = []string{"--permission-mode", "acceptEdits"}
	w.project.Adopted = map[string]AdoptedSession{
		smallID: {Name: "w-hand", Cwd: filepath.Join(w.root, "worktrees", "small"), Args: []string{"--permission-mode", "plan"}, Started: now.Add(-4 * time.Hour)},
	}
	recs := []route.Record{profiled(w, warmID, "w-warm", "warm", now.Add(-time.Hour), "--permission-mode", "plan")}
	steps := w.plan(t, recs, []claudecode.Proc{{PID: 301, Cmd: "claude -n repo-coord"}})
	outs := Apply(context.Background(), &w.project, steps, d)
	for _, name := range []string{"w-warm", "w-hand"} {
		o := outcomeOf(t, outs, name)
		if o.Err != nil {
			t.Fatalf("%s outcome = %+v", name, o)
		}
		if script := readFile(t, o.Script); !strings.Contains(script, " claude --permission-mode plan ") {
			t.Errorf("%s raised from plan mode:\n%s", name, script)
		}
	}
}

// Prompt text that names a permission flag is not read as the coordinator's:
// the words after -- are not flags, and of several readings the weakest wins.
func TestUpWorkersCappedPastPromptText(t *testing.T) {
	for _, cmd := range []string{
		"claude -n repo-coord --permission-mode plan -- drop --dangerously-skip-permissions",
		"claude -n repo-coord --permission-mode plan fix the --dangerously-skip-permissions docs",
	} {
		w, steps, d, _ := capWorld(t)
		procs := []claudecode.Proc{{PID: 301, Cmd: cmd}}
		d.CoordinatorArgs = func(id string) ([]string, bool, error) {
			return LiveArgs(id, map[string]claudecode.LiveSession{coordID: {PID: 301, SessionID: coordID}}, procs)
		}
		outs := Apply(context.Background(), &w.project, steps, d)
		for _, name := range []string{"w-warm", "w-hand"} {
			o := outcomeOf(t, outs, name)
			if o.Err != nil {
				t.Fatalf("%s outcome = %+v", name, o)
			}
			if script := readFile(t, o.Script); !strings.Contains(script, " claude --permission-mode plan ") {
				t.Errorf("%q: %s not capped at plan:\n%s", cmd, name, script)
			}
		}
	}
}

// pastWorld is a project saved with bypass whose launcherless run gave it a
// new coordinator id that was never run, so the past coordinator coordID is
// the one a restore picks; plus capWorld's two bypass workers. probe reads
// coordID as running in plan mode, every other id as stopped.
func pastWorld(t *testing.T) (w *world, recs []route.Record, d Deps) {
	t.Helper()
	var logged []route.Record
	d = upDeps(t, &fakeAgterm{ws: "repo"}, &logged)
	none := d
	none.Launcher, none.Lister, none.Closer, none.Pin = nil, nil, nil, nil
	w = newWorld(t)
	w.project.ClaudeArgs = []string{"--dangerously-skip-permissions"}
	fixture(t, "cold-large.jsonl", w.root, w.transcripts[coordID])
	first := outcomeOf(t, Apply(context.Background(), &w.project, w.plan(t, nil, nil), none), "repo-coord")
	if first.NewSessionID == "" || w.project.Coordinator.ID == coordID {
		t.Fatalf("no-launcher run: %+v", first)
	}
	fixture(t, "cold-small.jsonl", w.root, w.transcripts[coordID])
	w.project.Adopted = map[string]AdoptedSession{
		smallID: {Name: "w-hand", Cwd: filepath.Join(w.root, "worktrees", "small"), Args: []string{"--dangerously-skip-permissions"}, Started: now.Add(-4 * time.Hour)},
	}
	recs = []route.Record{profiled(w, warmID, "w-warm", "warm", now.Add(-time.Hour), "--dangerously-skip-permissions")}
	procs := []claudecode.Proc{{PID: 301, Cmd: "claude -n repo-coord --permission-mode plan"}}
	d.CoordinatorArgs = func(id string) ([]string, bool, error) {
		return LiveArgs(id, map[string]claudecode.LiveSession{coordID: {PID: 301, SessionID: coordID}}, procs)
	}
	return w, recs, d
}

func assertPlanCapped(t *testing.T, outs []Outcome) {
	t.Helper()
	for _, name := range []string{"w-warm", "w-hand"} {
		o := outcomeOf(t, outs, name)
		if o.Err != nil {
			t.Fatalf("%s outcome = %+v", name, o)
		}
		if script := readFile(t, o.Script); !strings.Contains(script, " claude --permission-mode plan ") {
			t.Errorf("%s not capped at the running past coordinator's plan mode:\n%s", name, script)
		}
	}
}

// The past coordinator runs (resumed by hand, in plan mode) and the restore
// leaves it: the cap reads it, not the saved id that never ran.
func TestUpCapReadsTheLivePastCoordinator(t *testing.T) {
	w, recs, d := pastWorld(t)
	steps := w.plan(t, recs, []claudecode.Proc{{PID: 301, Cmd: "claude -n repo-coord --permission-mode plan"}})
	if s := steps[0]; s.SessionID != coordID || s.Action != Leave {
		t.Fatalf("coordinator step %+v, want the live past coordinator left", s)
	}
	assertPlanCapped(t, Apply(context.Background(), &w.project, steps, d))
}

// The past coordinator starts running after the plan (resumed by hand while
// qm up asked): the restore leaves it, and the cap reads it.
func TestUpCapReadsACoordinatorLiveAfterThePlan(t *testing.T) {
	w, recs, d := pastWorld(t)
	steps := w.plan(t, recs, nil)
	if s := steps[0]; s.SessionID != coordID || s.Action != Resume {
		t.Fatalf("coordinator step %+v, want the past coordinator resumed", s)
	}
	d.Live = func() (map[string]claudecode.LiveSession, error) {
		return map[string]claudecode.LiveSession{coordID: {PID: 301, SessionID: coordID}}, nil
	}
	outs := Apply(context.Background(), &w.project, steps, d)
	if o := outcomeOf(t, outs, "repo-coord"); o.Step.Action != Leave {
		t.Fatalf("coordinator outcome %+v, want a leave", o)
	}
	assertPlanCapped(t, outs)
}

// Two modes with no mode in common refuse the worker: a plan-mode worker
// under a dontAsk coordinator is not started, and the error says why.
func TestUpWorkerNotStartedWithoutCommonMode(t *testing.T) {
	w, steps, d, _ := capWorld(t)
	w.project.ClaudeArgs = []string{"--permission-mode", "dontAsk"}
	for i := range steps {
		if steps[i].Name == "w-hand" {
			a := *steps[i].Adopted
			a.Args = []string{"--permission-mode", "plan"}
			steps[i].Adopted = &a
		}
	}
	d.CoordinatorArgs = func(string) ([]string, bool, error) { return nil, false, nil }
	outs := Apply(context.Background(), &w.project, steps, d)
	o := outcomeOf(t, outs, "w-hand")
	if o.Err == nil || !strings.Contains(o.Err.Error(), "no permission mode in common") || o.Script != "" {
		t.Errorf("w-hand outcome = %+v, want a refusal saying the modes have none in common", o)
	}
}

// The cap is never above the project's saved arguments: prompt text a live
// read takes for the bypass flag (ps keeps no argument boundaries) leaves
// the workers at the saved plan mode, as v0.1.1 capped them.
func TestUpCapNeverAboveSaved(t *testing.T) {
	w, steps, d, _ := capWorld(t)
	w.project.ClaudeArgs = []string{"--permission-mode", "plan"}
	procs := []claudecode.Proc{{PID: 301, Cmd: "claude -n repo-coord fix the --dangerously-skip-permissions handling"}}
	d.CoordinatorArgs = func(id string) ([]string, bool, error) {
		live := map[string]claudecode.LiveSession{coordID: {PID: 301, SessionID: coordID}}
		return LiveArgs(id, live, procs)
	}
	outs := Apply(context.Background(), &w.project, steps, d)
	for _, name := range []string{"w-warm", "w-hand"} {
		o := outcomeOf(t, outs, name)
		if o.Err != nil {
			t.Fatalf("%s outcome = %+v", name, o)
		}
		script := readFile(t, o.Script)
		if strings.Contains(script, "--dangerously-skip-permissions") || !strings.Contains(script, " claude --permission-mode plan ") {
			t.Errorf("%s raised above the saved plan mode by a live read:\n%s", name, script)
		}
	}
}
