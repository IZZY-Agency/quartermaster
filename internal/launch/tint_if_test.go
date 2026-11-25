package launch

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestApplyTintIfAsksAboutTheListedTab(t *testing.T) {
	stateDir(t)
	f := &fakeCtl{windows: make([][]*fakeSession, 1)}
	f.add(0, "s1", "worker", "teal", "icon")
	remember(t, "s1", WorkerTint("teal"))
	var asked []string
	refuse := func(s SessionRef) bool { asked = append(asked, s.ID+" "+s.Name); return false }
	res, err := f.agterm().ApplyTintIf(context.Background(), "s1", AttentionTint("red"), refuse)
	if err != nil || res != TintLeft {
		t.Fatalf("refused: %v, %v", res, err)
	}
	if !slices.Equal(asked, []string{"s1 worker"}) || f.count("session", "tint") != 0 {
		t.Errorf("asked %q, calls %q", asked, f.calls)
	}
	if m, ok := memory(t, "s1"); !ok || m != WorkerTint("teal") {
		t.Errorf("memory changed: %+v %v", m, ok)
	}
	res, err = f.agterm().ApplyTintIf(context.Background(), "s1", AttentionTint("red"), func(SessionRef) bool { return true })
	if err != nil || res != TintApplied || f.find("s1").color != "red" {
		t.Fatalf("allowed: %v, %v, tab %+v", res, err, f.find("s1"))
	}
}

// allow adds to the ownership rule, it does not replace it.
func TestApplyTintIfLeavesAHandTintedTabAlone(t *testing.T) {
	stateDir(t)
	f := &fakeCtl{windows: make([][]*fakeSession, 1)}
	f.add(0, "s1", "worker", "pink", "icon")
	res, err := f.agterm().ApplyTintIf(context.Background(), "s1", AttentionTint("red"), func(SessionRef) bool { return true })
	if err != nil || res != TintLeft || f.find("s1").color != "pink" {
		t.Fatalf("%v, %v, tab %+v", res, err, f.find("s1"))
	}
}

func TestClearTint(t *testing.T) {
	stateDir(t)
	f := &fakeCtl{windows: make([][]*fakeSession, 1)}
	f.add(0, "mine", "worker", "teal", "icon")
	f.add(0, "hand", "worker", "pink", "icon")
	f.add(0, "changed", "worker", "pink", "icon")
	remember(t, "mine", WorkerTint("teal"))
	remember(t, "changed", WorkerTint("teal"))
	// A tab qm never tinted: no word to agterm.
	if res, err := f.agterm().ClearTint(context.Background(), "hand"); err != nil || res != TintUnchanged || len(f.calls) != 0 {
		t.Fatalf("hand: %v, %v, calls %q", res, err, f.calls)
	}
	if res, err := f.agterm().ClearTint(context.Background(), "mine"); err != nil || res != TintApplied || f.find("mine").color != "" {
		t.Fatalf("mine: %v, %v, tab %+v", res, err, f.find("mine"))
	}
	if _, ok := memory(t, "mine"); ok {
		t.Error("the cleared tab is still remembered")
	}
	// A tab the owner tinted since.
	if res, err := f.agterm().ClearTint(context.Background(), "changed"); err != nil || res != TintLeft || f.find("changed").color != "pink" {
		t.Fatalf("changed: %v, %v, tab %+v", res, err, f.find("changed"))
	}
}

func TestApplyTintGivesUpOnTheLockWhenItsContextEnds(t *testing.T) {
	dir := stateDir(t)
	lock := filepath.Join(dir, "quartermaster", "tints.json.lock")
	if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
		t.Fatal(err)
	}
	held, err := os.OpenFile(lock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	f := &fakeCtl{windows: make([][]*fakeSession, 1)}
	f.add(0, "s1", "worker", "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = f.agterm().ApplyTint(ctx, "s1", WorkerTint("teal"))
	if !errors.Is(err, context.DeadlineExceeded) || len(f.calls) != 0 {
		t.Errorf("err = %v, calls %q", err, f.calls)
	}
	if waited := time.Since(start); waited > tintsWait/2 {
		t.Errorf("waited %v after its context ended", waited)
	}
}

// A deadline that passes while agterm changes the tint does not cut it off:
// the tab stays one qm remembers.
func TestApplyTintIsNotCutOffInTheMiddle(t *testing.T) {
	stateDir(t)
	f := &fakeCtl{windows: make([][]*fakeSession, 1)}
	f.add(0, "s1", "worker", "teal", "icon")
	remember(t, "s1", WorkerTint("teal"))
	ctx, cancel := context.WithCancel(context.Background())
	a := Agterm{Bin: "agtermctl", Run: func(rctx context.Context, bin string, args ...string) ([]byte, error) {
		if len(args) > 1 && args[0] == "session" && args[1] == "tint" {
			cancel()
			if rctx.Err() != nil {
				return nil, rctx.Err()
			}
			if _, ok := rctx.Deadline(); !ok {
				t.Error("the change has no deadline of its own")
			}
		}
		return f.run(rctx, bin, args...)
	}}
	res, err := a.ApplyTint(ctx, "s1", AttentionTint("red"))
	if m, ok := memory(t, "s1"); err != nil || res != TintApplied || !ok || m != AttentionTint("red") {
		t.Errorf("%v, %v, memory %+v %v", res, err, m, ok)
	}
}

// Once its context ended, no change begins, and the memory keeps the tab.
func TestApplyTintBeginsNoChangeAfterItsContextEnded(t *testing.T) {
	stateDir(t)
	f := &fakeCtl{windows: make([][]*fakeSession, 1)}
	f.add(0, "s1", "worker", "teal", "icon")
	remember(t, "s1", WorkerTint("teal"))
	ctx, cancel := context.WithCancel(context.Background())
	_, err := f.agterm().ApplyTintIf(ctx, "s1", AttentionTint("red"), func(SessionRef) bool { cancel(); return true })
	if m, ok := memory(t, "s1"); !errors.Is(err, context.Canceled) || f.count("session", "tint") != 0 || !ok || m != WorkerTint("teal") {
		t.Errorf("%v, calls %q, memory %+v %v", err, f.calls, m, ok)
	}
}

func TestExecInItsOwnProcessGroup(t *testing.T) {
	const leads = "kill -0 -$$ 2>/dev/null && echo own || echo shared"
	for want, ctx := range map[string]context.Context{"own": ownGroup(context.Background()), "shared": context.Background()} {
		if out, err := Exec(ctx, "sh", "-c", leads); err != nil || strings.TrimSpace(string(out)) != want {
			t.Errorf("%q, %v, want %s", out, err, want)
		}
	}
}

// A signal that ends qm while agterm changes a tint ends it once the memory is
// written: a process that gets one mid-change dies of it, and remembers the tab.
func TestApplyTintHoldsSignalsUntilTheMemoryIsWritten(t *testing.T) {
	if os.Getenv("QM_TEST_SIGNAL_CHILD") != "" {
		f := &fakeCtl{windows: make([][]*fakeSession, 1)}
		f.add(0, "s1", "worker", "teal", "icon")
		remember(t, "s1", WorkerTint("teal"))
		f.onTint = func(string) {
			_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
			time.Sleep(200 * time.Millisecond) // long enough to die of it
		}
		_, _ = f.agterm().ApplyTint(context.Background(), "s1", AttentionTint("red"))
		time.Sleep(5 * time.Second)
		os.Exit(0) // not reached: the signal was raised again
	}
	stateDir(t)
	cmd := exec.Command(os.Args[0], "-test.run", "^"+t.Name()+"$")
	cmd.Env = append(os.Environ(), "QM_TEST_SIGNAL_CHILD=1")
	err := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("the child ended with %v, want SIGTERM", err)
	}
	if ws, ok := exit.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGTERM {
		t.Errorf("the child ended with %v, want SIGTERM", err)
	}
	if m, ok := memory(t, "s1"); !ok || m != AttentionTint("red") {
		t.Errorf("memory %+v %v: the tab changed and is not remembered", m, ok)
	}
}
