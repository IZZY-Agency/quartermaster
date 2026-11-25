package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
	"github.com/IZZY-Agency/quartermaster/internal/config"
	"github.com/IZZY-Agency/quartermaster/internal/declog"
	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/paths"
	"github.com/IZZY-Agency/quartermaster/internal/route"
	"github.com/IZZY-Agency/quartermaster/internal/spawn"
	"github.com/IZZY-Agency/quartermaster/internal/up"
)

// spawnStopped is a project whose worker w2 a qm spawn worker was starting,
// as upFreshID, when it ended.
func (m *upMachine) spawnStopped(t *testing.T) up.Project {
	t.Helper()
	p := m.project(t)
	state, err := paths.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal([]route.Record{{
		Time: time.Now().UTC(), Kind: "worker", Name: "w2", Cwd: m.root, UpProject: p.ID, ClaudeSession: upFreshID, Detail: "launch not confirmed: w2.sh",
		Decision: &route.Decision{Kind: "worker", Tier: route.Tier{Model: "sonnet", Effort: "low"}, Profile: &route.Profile{}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(state, "spawning")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, upFreshID+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func reserved(t *testing.T) int {
	t.Helper()
	st, err := spawn.ListStarting()
	if err != nil {
		t.Fatal(err)
	}
	return len(st)
}

// The worker a stopped qm spawn worker had started runs: the next qm up logs
// its Record and leaves it.
func TestUpRecoversAStoppedSpawn(t *testing.T) {
	m := newUpMachine(t)
	m.spawnStopped(t)
	swapProcs(t, func() ([]claudecode.Proc, error) {
		return []claudecode.Proc{{PID: 501, Cmd: "claude -n w2 --session-id " + upFreshID}}, nil
	})
	var out, errb bytes.Buffer
	if code := run([]string{"up", "--yes"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if !slices.Equal(workerIDs(t), []string{upFreshID}) || !slices.Contains(upKinds(t), "w2 leave") || reserved(t) != 0 {
		t.Errorf("worker Records %v, up Records %v, reserved %d", workerIDs(t), upKinds(t), reserved(t))
	}
	if !strings.Contains(out.String(), "worker w2: an earlier qm spawn worker stopped while starting session 00000000") {
		t.Errorf("stdout %q", out.String())
	}
}

// It never ran: its Record is logged, and nothing is started for it.
func TestUpRecoversASpawnThatNeverRan(t *testing.T) {
	m := newUpMachine(t)
	m.spawnStopped(t)
	var out, errb bytes.Buffer
	if code := run([]string{"up", "--yes"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if !slices.Equal(workerIDs(t), []string{upFreshID}) || reserved(t) != 0 || !strings.Contains(out.String(), "transcript missing") {
		t.Errorf("worker Records %v, reserved %d, stdout %q", workerIDs(t), reserved(t), out.String())
	}
}

func TestUpDryRunLeavesAStoppedSpawn(t *testing.T) {
	m := newUpMachine(t)
	m.spawnStopped(t)
	var out, errb bytes.Buffer
	if code := run([]string{"up", "--dry-run"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if len(workerIDs(t)) != 0 || reserved(t) != 1 || !strings.Contains(out.String(), "w2") {
		t.Errorf("worker Records %v, reserved %d, stdout %q", workerIDs(t), reserved(t), out.String())
	}
}

// A Record that cannot be logged fails the run, which goes on; the worker
// stays reserved, and known.
func TestUpStoppedSpawnNotLoggedExits1(t *testing.T) {
	m := newUpMachine(t)
	m.spawnStopped(t)
	swap(t, &upLog, func(r route.Record) error {
		if r.Kind == "worker" {
			return errors.New("read-only file system")
		}
		return declog.Append(r)
	})
	swapProcs(t, func() ([]claudecode.Proc, error) {
		return []claudecode.Proc{{PID: 500, Cmd: "claude -n proj-coord --session-id " + upCoordID}, {PID: 501, Cmd: "claude -n w2 --session-id " + upFreshID}}, nil
	})
	var out, errb bytes.Buffer
	if code := run([]string{"up", "--yes"}, &out, &errb); code != 1 {
		t.Fatalf("exit %d, want 1; stderr %q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "worker w2: its Record (session 00000000) was not logged: read-only file system") || !slices.Contains(upKinds(t), "w2 leave") || reserved(t) != 1 {
		t.Errorf("stderr %q, up Records %v, reserved %d", errb.String(), upKinds(t), reserved(t))
	}
}

// What runs cannot be read: a worker that may run is not decided on.
func TestUpStoppedSpawnLiveUnread(t *testing.T) {
	m := newUpMachine(t)
	m.spawnStopped(t)
	swapProcs(t, func() ([]claudecode.Proc, error) { return nil, errors.New("no ps") })
	var out, errb bytes.Buffer
	if code := run([]string{"up", "--yes"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "an earlier qm spawn worker stopped while starting a session") {
		t.Errorf("exit %d, stderr %q", code, errb.String())
	}
	if reserved(t) != 1 || len(m.term.acted()) != 0 {
		t.Errorf("reserved %d, agtermctl %v", reserved(t), m.term.acted())
	}
}

type spawnLauncher func(launch.LaunchSpec) (launch.SessionRef, error)

func (f spawnLauncher) Spawn(_ context.Context, s launch.LaunchSpec) (launch.SessionRef, error) {
	return f(s)
}

// A spawn waits for no lock of a qm up that asks a question (it holds its
// project's lock all the while), and what that qm up saves afterwards from
// its memory loses nothing: the worker stays held.
func TestSpawnDuringAQmUp(t *testing.T) {
	m := newUpMachine(t)
	p := m.project(t)
	unlock, err := up.Lock(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	worker := t.TempDir()
	t.Setenv("QM_WORKER_PLUGIN", worker)
	brief := filepath.Join(t.TempDir(), "brief.md")
	if err := os.WriteFile(brief, []byte("Do it.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	holds := func(what, sid string) {
		recs, err := readUpRecords(&bytes.Buffer{})
		if err != nil {
			t.Fatal(err)
		}
		if err := up.Save(p); err != nil { // the qm up's, from its memory
			t.Fatal(err)
		}
		if !up.Held(loadProject(t, m.root), recs)[sid] {
			t.Errorf("%s: session %s not held by the project", what, sid)
		}
	}
	l := spawnLauncher(func(s launch.LaunchSpec) (launch.SessionRef, error) {
		st, err := spawn.ListStarting()
		if err != nil || len(st) != 1 {
			t.Fatalf("reserved %+v, %v", st, err)
		}
		holds("while it starts", st[0].SessionID)
		return launch.SessionRef{Launcher: "agterm", ID: "s-9", Name: s.Name}, nil
	})
	began := time.Now()
	res, err := spawn.Worker(context.Background(), spawn.WorkerRequest{
		Brief: brief, Type: "docs", Size: "S", Name: "w9", Cwd: m.root, UpProject: p.ID,
		Hint: &route.Tier{Model: "sonnet", Effort: "low"},
	}, spawn.Deps{
		Config: config.Defaults(), Getenv: func(string) string { return "" },
		PS:      func(int) (string, int, error) { return "", 0, errors.New("none") },
		ProcExe: func(int) (string, error) { return "", errors.New("none") },
		Pick:    func(string, func(string) string) (launch.Launcher, error) { return l, nil },
		Exe:     "/nonexistent/bin/qm",
	})
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > 5*time.Second {
		t.Errorf("the spawn took %v", took)
	}
	holds("started", res.SessionID)
}

// An --adopt run whose recovery failed exits 1 too.
func TestUpAdoptStoppedSpawnNotLoggedExits1(t *testing.T) {
	m := newUpMachine(t)
	m.spawnStopped(t)
	swap(t, &upLog, func(route.Record) error { return errors.New("read-only file system") })
	swapProcs(t, func() ([]claudecode.Proc, error) { return nil, nil })
	var out, errb bytes.Buffer
	if code := run([]string{"up", "--adopt"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "was not logged: read-only file system") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out.String(), errb.String())
	}
}

// A worker only reserved, its qm gone, that runs is shown by qm watch; one
// whose qm still starts it is not yet.
func TestWatchShowsAStoppedSpawn(t *testing.T) {
	m := newUpMachine(t)
	p := m.spawnStopped(t)
	swapProcs(t, func() ([]claudecode.Proc, error) {
		return []claudecode.Proc{{PID: 501, Cmd: "claude -n w2 --session-id " + upFreshID}}, nil
	})
	names := func() (out []string) {
		snap, err := readWorkers(p.ID, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range snap.Rows {
			out = append(out, w.Name)
		}
		return out
	}
	if got := names(); !slices.Equal(got, []string{"w2"}) {
		t.Errorf("workers %v, want w2", got)
	}
	holdReservation(t)
	if got := names(); len(got) != 0 {
		t.Errorf("workers %v while it is being started, want none", got)
	}
}

// holdReservation locks upFreshID's reservation, as the qm that starts it.
func holdReservation(t *testing.T) {
	t.Helper()
	state, err := paths.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(state, "spawning", upFreshID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
}

// qm up done means the worker that ran, with the later one of the name that
// never did; nothing while one of the name is being started.
func TestUpDoneWithASpawnThatNeverRan(t *testing.T) {
	m := newUpMachine(t)
	p := m.project(t)
	m.worker(t, p, "w2")
	m.spawnStopped(t)
	var out, errb bytes.Buffer
	if code := run([]string{"up", "done", "w2"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if done := loadProject(t, m.root).Done; len(done) != 2 || done[upWorkerID].IsZero() || done[upFreshID].IsZero() {
		t.Errorf("done %v, want %s and %s", done, upWorkerID, upFreshID)
	}
	if want := "marked w2 done (" + upWorkerID + "): qm up will not restore it, nor 1 other session(s) of the name that never ran\n"; out.String() != want {
		t.Errorf("stdout %q, want %q", out.String(), want)
	}
	holdReservation(t)
	errb.Reset()
	if code := run([]string{"up", "done", "w2"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "qm spawn worker is starting w2") {
		t.Errorf("exit %d, stderr %q", code, errb.String())
	}
}

// A worker whose launch is confirmed and whose process is not there yet is not
// marked with an earlier one of its name that ran; nor is one whose spawn ends
// while qm up done reads the sessions.
func TestUpDoneLeavesAStartingWorker(t *testing.T) {
	m := newUpMachine(t)
	p := m.project(t)
	m.worker(t, p, "w2")
	m.spawnStopped(t)
	state, err := paths.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(state, "spawning", upFreshID+".json")
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	ended := false
	swapProcs(t, func() ([]claudecode.Proc, error) { // the spawn ends meanwhile
		if !ended {
			ended = true
			if err := declog.Append(route.Record{Time: time.Now().UTC(), Kind: "worker", Name: "w2", Cwd: m.root, UpProject: p.ID, ClaudeSession: upFreshID, Detail: "agterm s-9", TermLauncher: "agterm", TermID: "s-9"}); err != nil {
				t.Error(err)
			}
			_ = os.Remove(path)
			_ = f.Close()
		}
		return nil, nil
	})
	var out, errb bytes.Buffer
	if code := run([]string{"up", "done", "w2"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "qm spawn worker is starting w2") || !ended {
		t.Errorf("exit %d, stderr %q", code, errb.String())
	}
	errb.Reset()
	if code := run([]string{"up", "done", "w2"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if done := loadProject(t, m.root).Done; len(done) != 1 || done[upWorkerID].IsZero() {
		t.Errorf("done %v, want %s", done, upWorkerID)
	}
}

// Which sessions run is not known: one with no transcript may run, and is not
// marked.
func TestUpDoneLiveUnread(t *testing.T) {
	m := newUpMachine(t)
	p := m.project(t)
	m.worker(t, p, "w2")
	m.spawnStopped(t)
	swapProcs(t, func() ([]claudecode.Proc, error) { return nil, errors.New("no ps") })
	var out, errb bytes.Buffer
	if code := run([]string{"up", "done", "w2"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if done := loadProject(t, m.root).Done; len(done) != 1 || done[upWorkerID].IsZero() {
		t.Errorf("done %v, want %s", done, upWorkerID)
	}
}

// spawnWorker runs qm spawn worker --name w2 in m's project with launcher l.
func (m *upMachine) spawnWorker(t *testing.T, p up.Project, l spawnLauncher) (string, error) {
	t.Helper()
	brief := filepath.Join(t.TempDir(), "brief.md")
	if err := os.WriteFile(brief, []byte("Do it.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := spawn.Worker(context.Background(), spawn.WorkerRequest{
		Brief: brief, Type: "docs", Size: "S", Name: "w2", Cwd: m.root, UpProject: p.ID,
		Hint: &route.Tier{Model: "sonnet", Effort: "low"},
	}, spawn.Deps{
		Config: config.Defaults(), Getenv: func(string) string { return "" },
		PS:      func(int) (string, int, error) { return "", 0, errors.New("none") },
		ProcExe: func(int) (string, error) { return "", errors.New("none") },
		Pick:    func(string, func(string) string) (launch.Launcher, error) { return l, nil },
		Exe:     "/nonexistent/bin/qm",
	})
	return res.SessionID, err
}

// A launch without an answer that never ran, and its retry under the same
// name: the retry's handoff note is not the first one's, so qm up carries the
// task on once, and qm up done ends both.
func TestUnconfirmedLaunchAndItsRetry(t *testing.T) {
	m := newUpMachine(t)
	p := m.project(t)
	refused := func(launch.LaunchSpec) (launch.SessionRef, error) {
		return launch.SessionRef{}, launch.ErrMaybeStarted
	}
	started := func(s launch.LaunchSpec) (launch.SessionRef, error) {
		return launch.SessionRef{Launcher: "agterm", ID: "s-9", Name: s.Name}, nil
	}
	first, err := m.spawnWorker(t, p, refused)
	if !errors.Is(err, launch.ErrMaybeStarted) {
		t.Fatalf("spawn: %v", err)
	}
	retry, err := m.spawnWorker(t, p, started)
	if err != nil {
		t.Fatal(err)
	}
	m.fixture(t, "coord.jsonl", filepath.Join(m.claude, "projects", "p", retry+".jsonl"))
	dir, err := up.HandoffDir()
	if err != nil {
		t.Fatal(err)
	}
	note := filepath.Join(dir, p.ID, "w2.md")
	if err := os.MkdirAll(filepath.Dir(note), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(note, []byte("Left: the tests.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(note, later, later); err != nil {
		t.Fatal(err)
	}
	// plan maps each session of w2 to what qm up would do with it.
	plan := func() map[string]up.Action {
		t.Helper()
		recs, err := readUpRecords(&bytes.Buffer{})
		if err != nil {
			t.Fatal(err)
		}
		p := loadProject(t, m.root)
		steps, _, _, _, _, err := upPlan(&p, recs, false, "started")
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]up.Action{}
		for _, s := range steps {
			if s.Name == "w2" {
				got[s.SessionID] = s.Action
			}
		}
		return got
	}
	if got := plan(); len(got) != 2 || got[first] != up.Skip || got[retry] != up.Resume {
		t.Errorf("plan %v, want %s skipped and %s resumed", got, first, retry)
	}
	// The first one runs after all: both are left.
	swapProcs(t, func() ([]claudecode.Proc, error) {
		return []claudecode.Proc{{PID: 501, Cmd: "claude -n w2 --session-id " + first}, {PID: 502, Cmd: "claude -n w2 --session-id " + retry}}, nil
	})
	if got := plan(); got[first] != up.Leave || got[retry] != up.Leave {
		t.Errorf("plan while both run %v", got)
	}
	swapProcs(t, func() ([]claudecode.Proc, error) { return nil, nil })
	var out, errb bytes.Buffer
	if code := run([]string{"up", "done", "w2"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if done := loadProject(t, m.root).Done; len(done) != 2 || done[first].IsZero() || done[retry].IsZero() {
		t.Errorf("done %v, want %s and %s", done, first, retry)
	}
	if code := run([]string{"up", "--yes"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if got := plan(); got[first] != up.Skip || got[retry] != up.Skip || len(workerIDs(t)) != 2 || len(upKinds(t)) == 0 {
		t.Errorf("after qm up: plan %v, worker Records %v, up Records %v", got, workerIDs(t), upKinds(t))
	}
}
