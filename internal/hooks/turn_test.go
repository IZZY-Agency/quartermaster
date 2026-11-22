package hooks

import (
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/paths"
)

// workerTurns sets up a worker session for the turn hooks: QM_ROLE=worker,
// CLAUDE_PID naming the claude process, and a fixed clock.
func workerTurns(t *testing.T) (*time.Time, func()) {
	t.Helper()
	t.Setenv("QM_ROLE", "worker")
	t.Setenv("CLAUDE_PID", "4242")
	at := time.Unix(1_800_000_000, 0)
	old := clock
	clock = func() time.Time { return at }
	return &at, func() { clock = old }
}

func TestTurnHooksRecordOpenAndEnd(t *testing.T) {
	cfg := setup(t)
	at, restore := workerTurns(t)
	defer restore()

	if out := fireEvent(t, cfg, "turn-start", "UserPromptSubmit", "", nil, ""); len(out) != 0 {
		t.Fatalf("turn-start printed %q", out)
	}
	tr, err := ReadTurn("test-session")
	if err != nil || !tr.Known || !tr.Open || !tr.At.Equal(*at) || tr.PID != 4242 {
		t.Fatalf("after turn-start: %+v %v", tr, err)
	}

	*at = at.Add(90 * time.Second)
	if out := fireEvent(t, cfg, "turn-stop", "Stop", "", nil, ""); len(out) != 0 {
		t.Fatalf("turn-stop printed %q", out)
	}
	tr, _ = ReadTurn("test-session")
	if !tr.Known || tr.Open || !tr.At.Equal(*at) {
		t.Fatalf("after turn-stop: %+v", tr)
	}
}

func TestTurnStartLateDoesNotHideNewerStop(t *testing.T) {
	cfg := setup(t)
	at, restore := workerTurns(t)
	defer restore()
	fireEvent(t, cfg, "turn-stop", "Stop", "", nil, "")
	// A start stamped before the stop (it ran late) leaves the turn ended.
	*at = at.Add(-time.Second)
	fireEvent(t, cfg, "turn-start", "UserPromptSubmit", "", nil, "")
	if tr, _ := ReadTurn("test-session"); tr.Open {
		t.Fatalf("a late start reopened the turn: %+v", tr)
	}
	// An older stop written after a newer one does not move it back.
	*at = at.Add(10 * time.Second)
	fireEvent(t, cfg, "turn-stop", "Stop", "", nil, "")
	newest := *at
	*at = at.Add(-5 * time.Second)
	fireEvent(t, cfg, "turn-stop", "Stop", "", nil, "")
	if tr, _ := ReadTurn("test-session"); !tr.At.Equal(newest) {
		t.Fatalf("stop went back to %v, want %v", tr.At, newest)
	}
}

func TestTurnWriteWaitsForItsOwnLock(t *testing.T) {
	setup(t)
	_, restore := workerTurns(t)
	defer restore()
	// The compare and the replace of one turn file are one step per session
	// and event: while another hook holds that file's lock, a write waits.
	path, err := turnPath("test-session", "stop")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- writeTurn("test-session", "stop") }()
	select {
	case err := <-done:
		t.Fatalf("write did not wait for the turn lock (err %v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	if _, found, _ := readTurnMark(path); found {
		t.Fatal("the turn file was written under another hook's lock")
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("write never finished after the lock was released")
	}
	if tr, _ := ReadTurn("test-session"); !tr.Known {
		t.Fatal("no turn after the lock was released")
	}
}

func TestTurnHooksNeedWorkerRole(t *testing.T) {
	cfg := setup(t)
	_, restore := workerTurns(t)
	defer restore()
	for _, role := range []string{"", "coordinator", "review"} {
		t.Setenv("QM_ROLE", role)
		fireEvent(t, cfg, "turn-start", "UserPromptSubmit", "", nil, "")
		if tr, _ := ReadTurn("test-session"); tr.Known {
			t.Fatalf("QM_ROLE=%q wrote a turn: %+v", role, tr)
		}
	}
}

func TestTurnHooksOffWriteNothing(t *testing.T) {
	cfg := setup(t)
	_, restore := workerTurns(t)
	defer restore()
	cfg.Rules = map[string]string{"turn-start": "off"}
	fireEvent(t, cfg, "turn-start", "UserPromptSubmit", "", nil, "")
	if tr, _ := ReadTurn("test-session"); tr.Known {
		t.Fatalf("an off rule wrote a turn: %+v", tr)
	}
}

func TestTurnHooksSkipSessionStateLock(t *testing.T) {
	cfg := setup(t)
	_, restore := workerTurns(t)
	defer restore()
	// Another hook holding the session state lock must not delay a turn hook.
	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = withState("test-session", func(*State) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	done := make(chan struct{})
	go func() {
		fireEvent(t, cfg, "turn-start", "UserPromptSubmit", "", nil, "")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("turn-start waited for the session state lock")
	}
	close(release)
}

func TestTurnPIDFallsBackToParent(t *testing.T) {
	cfg := setup(t)
	_, restore := workerTurns(t)
	defer restore()
	t.Setenv("CLAUDE_PID", "")
	fireEvent(t, cfg, "turn-start", "UserPromptSubmit", "", nil, "")
	if tr, _ := ReadTurn("test-session"); tr.PID != os.Getppid() {
		t.Fatalf("pid %d, want the parent %d", tr.PID, os.Getppid())
	}
}

func TestReadTurnMissingAndBad(t *testing.T) {
	setup(t)
	if tr, err := ReadTurn("no-such-session"); err != nil || tr.Known {
		t.Fatalf("missing: %+v %v", tr, err)
	}
	if _, err := ReadTurn("../bad"); err == nil {
		t.Fatal("bad id must fail")
	}
	dir, err := paths.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "turns", "cut.stop")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{"+strconv.Itoa(1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if tr, err := ReadTurn("cut"); err == nil || tr.Known {
		t.Fatalf("cut file: %+v %v", tr, err)
	}
}

func TestTurnHooksListed(t *testing.T) {
	names := map[string]bool{}
	for _, n := range Names() {
		names[n] = true
	}
	if !names["turn-start"] || !names["turn-stop"] {
		t.Fatalf("Names() = %v", Names())
	}
}
