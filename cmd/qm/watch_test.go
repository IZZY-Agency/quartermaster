package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
	"github.com/IZZY-Agency/quartermaster/internal/config"
	"github.com/IZZY-Agency/quartermaster/internal/declog"
	"github.com/IZZY-Agency/quartermaster/internal/hooks"
	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/route"
	"github.com/IZZY-Agency/quartermaster/internal/spawn"
	"github.com/IZZY-Agency/quartermaster/internal/up"
	"github.com/IZZY-Agency/quartermaster/internal/watch"
)

// liveWorker makes ps list the test worker's claude process.
func liveWorker(t *testing.T) {
	t.Helper()
	swap(t, &upProcs, func() ([]claudecode.Proc, error) {
		return []claudecode.Proc{{PID: 4242, PPID: 1, Cmd: "claude --session-id " + upWorkerID}}, nil
	})
}

// turnHook runs a turn hook for the test worker's session.
func turnHook(t *testing.T, name, event string) {
	t.Helper()
	b, err := json.Marshal(map[string]any{"session_id": upWorkerID, "hook_event_name": event, "cwd": t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("QM_ROLE", "worker")
	t.Setenv("CLAUDE_PID", "4242")
	hooks.Handle(name, bytes.NewReader(b), config.Defaults())
	t.Setenv("QM_ROLE", "")
}

// fakeClock makes qm watch's clock move only when it sleeps; each sleep
// runs tick first, with the number of sleeps so far.
func fakeClock(t *testing.T, tick func(n int)) {
	t.Helper()
	now := time.Now()
	n := 0
	swap(t, &watchNow, func() time.Time { return now })
	swap(t, &watchSleep, func(_ context.Context, d time.Duration) error {
		n++
		if tick != nil {
			tick(n)
		}
		now = now.Add(d)
		return nil
	})
}

func TestWatchNoProject(t *testing.T) {
	newUpMachine(t)
	var out, errb bytes.Buffer
	if code := run([]string{"watch"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "qm watch: no project here") {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
}

func TestWatchUnknownNameIsUsage(t *testing.T) {
	m := newUpMachine(t)
	m.worker(t, m.project(t), "w")
	liveWorker(t)
	var out, errb bytes.Buffer
	if code := run([]string{"watch", "nope"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), `"nope"`) {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
}

func TestWatchBadFlag(t *testing.T) {
	newUpMachine(t)
	var out, errb bytes.Buffer
	if code := run([]string{"watch", "--silent", "0s"}, &out, &errb); code != 2 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
}

func TestWatchWakesOnTurnStop(t *testing.T) {
	m := newUpMachine(t)
	m.worker(t, m.project(t), "w")
	liveWorker(t)
	turnHook(t, "turn-start", "UserPromptSubmit")
	sleeps := 0
	fakeClock(t, func(n int) {
		sleeps = n
		if n == 2 {
			turnHook(t, "turn-stop", "Stop")
		}
	})
	var out, errb bytes.Buffer
	// A huge silence threshold: the fixture transcript is old.
	if code := run([]string{"watch", "--silent", "100000h", "--interval", "1s"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "w stopped ") || !strings.Contains(got, "sonnet/low") || sleeps != 2 {
		t.Fatalf("sleeps %d, out:\n%s", sleeps, got)
	}
}

func TestWatchDetectionFailedTimesOut(t *testing.T) {
	m := newUpMachine(t)
	m.worker(t, m.project(t), "w")
	swap(t, &upProcs, func() ([]claudecode.Proc, error) { return nil, errors.New("ps: exit 1") })
	fakeClock(t, nil)
	var out, errb bytes.Buffer
	if code := run([]string{"watch", "--timeout", "1m"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	got := out.String()
	if strings.Count(got, "ps: exit 1") != 1 || !strings.Contains(got, "unknown") || !strings.Contains(got, "timeout after 1m0s") {
		t.Fatalf("out:\n%s", got)
	}
}

func TestStatusListsWorkers(t *testing.T) {
	m := newUpMachine(t)
	p := m.project(t)
	m.worker(t, p, "w")
	liveWorker(t)
	turnHook(t, "turn-stop", "Stop")
	var out, errb bytes.Buffer
	if code := run([]string{"status"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "Workers ("+p.ID+"):") || !strings.Contains(got, "  w ") || !strings.Contains(got, "stopped") {
		t.Fatalf("out:\n%s", got)
	}
	if !strings.Contains(got, "turn-start") || !strings.Contains(got, "turn-stop") {
		t.Fatalf("turn hooks not listed under Rules:\n%s", got)
	}
}

func TestStatusNoWorkersSection(t *testing.T) {
	newUpMachine(t)
	var out, errb bytes.Buffer
	if code := run([]string{"status"}, &out, &errb); code != 0 || strings.Contains(out.String(), "\nWorkers") {
		t.Fatalf("exit %d, out:\n%s", code, out.String())
	}
}

func TestSpawnPrintsWatchLine(t *testing.T) {
	var out, errb bytes.Buffer
	res := spawn.Result{Ref: launch.SessionRef{Launcher: "agterm", ID: "s-1", Name: "w1"}}
	if err := reportStarted(res, &out, &errb); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out.String(), "\nwatch it: qm watch\n") {
		t.Fatalf("stdout %q", out.String())
	}
}

func TestWatchDoneDuringWatchLeavesWithoutEvent(t *testing.T) {
	m := newUpMachine(t)
	p := m.project(t)
	m.worker(t, p, "w")
	liveWorker(t)
	fakeClock(t, func(n int) {
		if n == 1 {
			// qm up done w, between two checks.
			q := loadProject(t, m.root)
			q.Done = map[string]time.Time{upWorkerID: time.Now()}
			if err := up.Save(q); err != nil {
				t.Fatal(err)
			}
		}
	})
	var out, errb bytes.Buffer
	if code := run([]string{"watch", "--silent", "100000h"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "qm watch: no worker left to wait for") || strings.Contains(got, "w gone") {
		t.Fatalf("out:\n%s", got)
	}
}

func TestWatchReplacedDuringWatchLeavesWithoutEvent(t *testing.T) {
	m := newUpMachine(t)
	p := m.project(t)
	m.worker(t, p, "w")
	liveWorker(t)
	const fresh = "00000000-0000-4000-8000-0000000000a2"
	fakeClock(t, func(n int) {
		if n == 1 {
			// A fresh session replaces w; the old process ends.
			swap(t, &upProcs, func() ([]claudecode.Proc, error) {
				return []claudecode.Proc{{PID: 4343, PPID: 1, Cmd: "claude --session-id " + fresh}}, nil
			})
			r := route.Record{Time: time.Now().UTC(), Kind: "worker", Name: "w", Cwd: m.root, UpProject: p.ID, ClaudeSession: fresh, Replaces: upWorkerID}
			if err := declog.Append(r); err != nil {
				t.Fatal(err)
			}
		}
	})
	var out, errb bytes.Buffer
	if code := run([]string{"watch", "--silent", "100000h", "--timeout", "1m"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	got := out.String()
	if strings.Contains(got, "w gone") || !strings.Contains(got, "timeout after 1m0s") {
		t.Fatalf("out:\n%s", got)
	}
}

func TestWatchInterruptDuringCollection(t *testing.T) {
	m := newUpMachine(t)
	m.worker(t, m.project(t), "w")
	ctx, cancel := context.WithCancel(context.Background())
	blockProcs(t, cancel)
	var out, errb bytes.Buffer
	err := watchMain(ctx, nil, &out, &errb)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	if code := watchExit(err, 2, &errb); code != 130 {
		t.Fatalf("exit %d", code)
	}
}

func TestWatchMarksFileMergesOverlappingWriters(t *testing.T) {
	routeHermetic(t)
	a, b := fileMarks{path: marksPath("pid")}, fileMarks{path: marksPath("pid")}
	// Watch a read the marks, watch b claimed one, then a claims another.
	if _, err := a.Load(); err != nil {
		t.Fatal(err)
	}
	if err := b.Update(context.Background(), func(m watch.Marks) bool { m["s-b"] = watch.Mark{Gone: "s-b"}; return true }); err != nil {
		t.Fatal(err)
	}
	var sawB bool
	if err := a.Update(context.Background(), func(m watch.Marks) bool {
		_, sawB = m["s-b"]
		m["s-a"] = watch.Mark{Stop: 1}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	got, err := a.Load()
	if err != nil || !sawB || got["s-a"].Stop != 1 || got["s-b"].Gone != "s-b" {
		t.Fatalf("marks %v, saw b %v, err %v", got, sawB, err)
	}
}

func TestWatchMarksFileNullIsEmpty(t *testing.T) {
	routeHermetic(t)
	path := marksPath("pid")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("null"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := fileMarks{path: path}
	m, err := f.Load()
	if err != nil || m == nil {
		t.Fatalf("Load: %v, %v (want an empty map)", m, err)
	}
	// An update of a file holding null must not panic on assignment.
	if err := f.Update(context.Background(), func(m watch.Marks) bool { m["s-a"] = watch.Mark{Stop: 1}; return true }); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.Load(); got["s-a"].Stop != 1 {
		t.Fatalf("marks %v", got)
	}
}

func TestWatchMarksLockGivesUpWhenContextEnds(t *testing.T) {
	routeHermetic(t)
	f := fileMarks{path: marksPath("pid")}
	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = f.Update(context.Background(), func(watch.Marks) bool { close(held); <-release; return false })
	}()
	<-held
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.Update(ctx, func(watch.Marks) bool { return false }) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err %v, want the deadline", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Update kept waiting for the lock past its context")
	}
}

func TestStatusNoProcessListWithoutWorkers(t *testing.T) {
	m := newUpMachine(t)
	m.project(t)
	swap(t, &upProcs, func() ([]claudecode.Proc, error) {
		t.Error("ps read for a project without workers")
		return nil, nil
	})
	var out, errb bytes.Buffer
	if code := run([]string{"status"}, &out, &errb); code != 0 || strings.Contains(out.String(), "\nWorkers") {
		t.Fatalf("exit %d, out:\n%s", code, out.String())
	}
}

func TestStatusWorkersBounded(t *testing.T) {
	m := newUpMachine(t)
	m.worker(t, m.project(t), "w")
	blockProcs(t, func() {})
	swap(t, &statusWorkersBound, 50*time.Millisecond)
	var out, errb bytes.Buffer
	if code := run([]string{"status"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "Workers: not read within 50ms") {
		t.Fatalf("exit %d, out:\n%s", code, out.String())
	}
}

func TestStatusBadSilentMinutes(t *testing.T) {
	m := newUpMachine(t)
	m.worker(t, m.project(t), "w")
	liveWorker(t)
	writeConfig(t, `{"watch":{"silent_minutes":0}}`)
	var out, errb bytes.Buffer
	if code := run([]string{"status"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "Workers: watch.silent_minutes is 0") {
		t.Fatalf("exit %d, out:\n%s", code, out.String())
	}
	if code := run([]string{"watch"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "watch.silent_minutes is 0") {
		t.Fatalf("watch exit %d, stderr %q", code, errb.String())
	}
}

// blockProcs makes ps hang (after calling started) until the test ends;
// the cleanup releases it and waits for it before the swaps are undone.
func blockProcs(t *testing.T, started func()) {
	t.Helper()
	block, returned := make(chan struct{}), make(chan struct{})
	swap(t, &upProcs, func() ([]claudecode.Proc, error) {
		defer close(returned)
		started()
		<-block
		return nil, errors.New("released")
	})
	t.Cleanup(func() {
		close(block)
		<-returned
	})
}
