package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
	"github.com/IZZY-Agency/quartermaster/internal/up"
)

// stallWorktrees makes the worktree probe stall until its context ends, and
// the detection deadline short.
func stallWorktrees(t *testing.T) {
	t.Helper()
	swap(t, &upDetectTimeout, 200*time.Millisecond)
	inner := upRun
	swap(t, &upRun, up.DirRunner(func(ctx context.Context, dir, bin string, args ...string) ([]byte, error) {
		if bin == "git" && len(args) > 0 && args[0] == "worktree" {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return inner(ctx, dir, bin, args...)
	}))
}

// A first run whose detection outlasts its deadline adopts and starts
// nothing: its candidate list may be incomplete.
func TestUpFirstRunDetectionPastDeadlineStartsNothing(t *testing.T) {
	m := newAdoptMachine(t)
	m.inside(411)
	stallWorktrees(t)
	code, out, errs := runQM("up", "--adopt")
	if code == 0 || !strings.Contains(errs, "not all read") || !strings.Contains(errs, "nothing started") {
		t.Fatalf("exit %d, out %q, err %q", code, out, errs)
	}
	if _, ok := stateOf(t, m.root); ok || len(m.agterm.acted()) != 0 || len(m.term.calls) != 0 {
		t.Fatalf("acted: %q %q", m.agterm.calls, m.term.calls)
	}
}

// A later adoption whose detection outlasts its deadline adopts nothing and
// says why.
func TestUpAdoptLaterDetectionPastDeadlineAdoptsNothing(t *testing.T) {
	m := newLaterMachine(t)
	m.live(t, 414, adoptSideID, "side", "claude")
	stallWorktrees(t)
	code, out, errs := runQM("up", "--adopt", "--yes")
	if code == 0 || !strings.Contains(errs, "not all read") || !strings.Contains(errs, "nothing adopted") {
		t.Fatalf("exit %d, out %q, err %q", code, out, errs)
	}
}

// A process list that stalls stops at the detection's deadline: the run
// says the sessions were not all read and starts nothing, in time.
func TestUpDetectionStopsAStalledProcessList(t *testing.T) {
	m := newAdoptMachine(t)
	m.inside(411)
	swap(t, &upDetectTimeout, 200*time.Millisecond)
	swap(t, &upProcsCtx, func(ctx context.Context) ([]claudecode.Proc, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(30 * time.Second):
			return nil, errors.New("stalled")
		}
	})
	start := time.Now()
	code, out, errs := runQM("up", "--adopt")
	if code == 0 || !strings.Contains(errs, "not all read") || !strings.Contains(errs, "nothing started") {
		t.Fatalf("exit %d, out %q, err %q", code, out, errs)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %v: the stalled process list held the run", d)
	}
	if _, ok := stateOf(t, m.root); ok {
		t.Fatal("state saved")
	}
}

// The tree read succeeds in time, then a label read stalls: the labels stop
// at the detection's deadline and the run goes on.
func TestUpLabelReadsStopAtTheDeadline(t *testing.T) {
	newAdoptMachine(t)
	swap(t, &upDetectTimeout, 500*time.Millisecond)
	stalled := make(chan bool, 8)
	swap(t, &upTermOf, func(ctx context.Context, pid int) (claudecode.TermEnv, error) {
		if _, ok := ctx.Deadline(); !ok {
			return claudecode.TermEnv{}, nil // planning, outside the labels
		}
		select {
		case <-ctx.Done():
			stalled <- true
			return claudecode.TermEnv{}, ctx.Err()
		case <-time.After(30 * time.Second):
			return claudecode.TermEnv{}, errors.New("stalled")
		}
	})
	start := time.Now()
	code, out, errs := runQM("up", "--dry-run")
	if code != 0 || !strings.Contains(out, "would refuse") {
		t.Fatalf("exit %d, out %q, err %q", code, out, errs)
	}
	if d := time.Since(start); d > 5*time.Second || len(stalled) == 0 {
		t.Fatalf("took %v, label reads cut %d", d, len(stalled))
	}
}
