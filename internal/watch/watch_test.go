package watch

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// memStore is a mark store in memory; onUpdate runs inside each Update,
// before fn, so a test can play another watch writing in between.
type memStore struct {
	marks    Marks
	updates  int
	onUpdate func(Marks)
	// deadlines counts the Updates whose context had a deadline.
	deadlines int
}

func (s *memStore) Load() (Marks, error) { return s.marks.clone(), nil }

func (s *memStore) Update(ctx context.Context, fn func(Marks) bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, ok := ctx.Deadline(); ok {
		s.deadlines++
	}
	m := s.marks.clone()
	if s.onUpdate != nil {
		s.onUpdate(m)
	}
	if fn(m) {
		s.marks = m
		s.updates++
	}
	return nil
}

// fake is a watch's machine: snapshots in order (the last one repeats), a
// clock that sleeping moves, marks in memory, a notifier that records.
type fake struct {
	snaps    []Snapshot
	calls    int
	now      time.Time
	store    memStore
	tints    memStore
	tinting  bool
	notified []string
	notifyFn func(string) error
	snapFn   func(context.Context) error
	out, err bytes.Buffer
}

func (f *fake) deps() Deps {
	return Deps{
		Snapshot: func(ctx context.Context) (Snapshot, error) {
			if f.snapFn != nil {
				if err := f.snapFn(ctx); err != nil {
					return Snapshot{}, err
				}
			}
			s := f.snaps[min(f.calls, len(f.snaps)-1)]
			f.calls++
			return s, nil
		},
		Now: func() time.Time { return f.now },
		Sleep: func(_ context.Context, d time.Duration) error {
			f.now = f.now.Add(d)
			return nil
		},
		Marks: &f.store,
		Tints: &f.tints,
		Notify: func(_ context.Context, s string) error {
			f.notified = append(f.notified, s)
			if f.notifyFn != nil {
				return f.notifyFn(s)
			}
			return nil
		},
		Home: "/hd/me",
		Out:  &f.out,
		Err:  &f.err,
	}
}

func row(name string, st State, act time.Time) Row {
	r := Row{Name: name, SessionID: "s-" + name, Current: "s-" + name, State: st, Activity: act, HasSince: st != Gone, Folder: "."}
	switch st {
	case Stopped:
		r.Since, r.StopAt = 12*time.Second, act
	case Silent:
		r.Since = 10 * time.Minute
	}
	return r
}

func snap(rows ...Row) Snapshot { return Snapshot{Rows: rows} }

func opts() Options { return Options{Interval: 15 * time.Second} }

func run(t *testing.T, f *fake, o Options) error {
	t.Helper()
	if f.now.IsZero() {
		f.now = t0
	}
	return Run(context.Background(), o, f.deps())
}

func again(f *fake, snaps ...Snapshot) {
	f.out.Reset()
	f.calls = 0
	f.notified = nil
	f.snaps = snaps
}

func TestWatchStopsOnStoppedWithHandoff(t *testing.T) {
	stopped := row("a", Stopped, t0)
	stopped.Handoff = "/hd/me/.local/state/quartermaster/handoff/pid/a.md"
	f := &fake{snaps: []Snapshot{snap(row("a", Working, t0)), snap(row("a", Working, t0)), snap(stopped)}}
	if err := run(t, f, opts()); err != nil {
		t.Fatal(err)
	}
	want := "a stopped 12s ago; handoff note: ~/.local/state/quartermaster/handoff/pid/a.md"
	if !strings.Contains(f.out.String(), want) || f.calls != 3 {
		t.Fatalf("calls %d, out:\n%s", f.calls, f.out.String())
	}
	// The notifier gets the name and the state, never a path.
	if len(f.notified) != 1 || f.notified[0] != "a stopped; handoff note written" {
		t.Fatalf("notified %q", f.notified)
	}
}

func TestWatchReportsStopBeforeStartOnce(t *testing.T) {
	// a stopped before the watch began: reported at once.
	f := &fake{snaps: []Snapshot{snap(row("a", Stopped, t0), row("b", Working, t0))}}
	if err := run(t, f, opts()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.out.String(), "a stopped 12s ago") || f.calls != 1 {
		t.Fatalf("calls %d, out:\n%s", f.calls, f.out.String())
	}
	// Restarted: a is listed as reported and does not wake it again.
	again(f, snap(row("a", Stopped, t0), row("b", Working, t0)), snap(row("a", Stopped, t0), row("b", Gone, t0)))
	if err := run(t, f, opts()); err != nil {
		t.Fatal(err)
	}
	out := f.out.String()
	if !strings.Contains(out, "stopped (reported)") || strings.Contains(out, "a stopped 12s") || !strings.Contains(out, "b gone\n") {
		t.Fatalf("out:\n%s", out)
	}
	// a works and stops again: a new stop, reported.
	again(f, snap(row("a", Working, t0.Add(time.Minute)), row("b", Gone, t0)), snap(row("a", Stopped, t0.Add(2*time.Minute)), row("b", Gone, t0)))
	if err := run(t, f, opts()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.out.String(), "a stopped 12s ago") {
		t.Fatalf("second stop:\n%s", f.out.String())
	}
}

func TestWatchNewWorkerFirstSeenStopped(t *testing.T) {
	f := &fake{snaps: []Snapshot{snap(row("a", Working, t0)), snap(row("a", Working, t0), row("b", Stopped, t0))}}
	if err := run(t, f, opts()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.out.String(), "b stopped 12s ago") {
		t.Fatalf("out:\n%s", f.out.String())
	}
}

func TestWatchNothingToWaitFor(t *testing.T) {
	f := &fake{store: memStore{marks: Marks{"s-a": {Stop: t0.UnixNano()}, "s-b": {Gone: "s-b"}}},
		snaps: []Snapshot{snap(row("a", Stopped, t0), row("b", Gone, t0))}}
	if err := run(t, f, opts()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.out.String(), "qm watch: no worker to wait for") || f.calls != 1 || len(f.notified) != 0 {
		t.Fatalf("out:\n%s", f.out.String())
	}
}

func TestWatchSilentOnceAcrossWatches(t *testing.T) {
	act := t0.Add(-11 * time.Minute)
	f := &fake{snaps: []Snapshot{snap(row("a", Working, act), row("b", Working, t0)), snap(row("a", Silent, act), row("b", Working, t0))}}
	if err := run(t, f, opts()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.out.String(), "a silent for 10 min") {
		t.Fatalf("first watch:\n%s", f.out.String())
	}
	again(f, snap(row("a", Silent, act), row("b", Working, t0)), snap(row("a", Silent, act), row("b", Stopped, t0)))
	if err := run(t, f, opts()); err != nil {
		t.Fatal(err)
	}
	out := f.out.String()
	if !strings.Contains(out, "silent (reported)") || strings.Contains(out, "a silent for") || !strings.Contains(out, "b stopped") {
		t.Fatalf("second watch:\n%s", out)
	}
	// a writes again: working; silent again is a new event.
	later := t0.Add(time.Minute)
	again(f, snap(row("a", Working, later)), snap(row("a", Silent, later)))
	if err := run(t, f, opts()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.out.String(), "a silent for 10 min") {
		t.Fatalf("third watch:\n%s", f.out.String())
	}
}

func TestWatchSilentAgainBetweenChecks(t *testing.T) {
	// a was reported silent; it wrote and went silent again between two
	// checks of one watch: a new silence, reported.
	act := t0.Add(-20 * time.Minute)
	f := &fake{store: memStore{marks: Marks{"s-a": {Silent: act.UnixNano()}}},
		snaps: []Snapshot{snap(row("a", Silent, act), row("b", Working, t0)), snap(row("a", Silent, t0), row("b", Working, t0))}}
	if err := run(t, f, opts()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.out.String(), "a silent for 10 min") || f.calls != 2 {
		t.Fatalf("calls %d, out:\n%s", f.calls, f.out.String())
	}
}

func TestWatchLeavesWithoutEventWhenRowsGo(t *testing.T) {
	f := &fake{snaps: []Snapshot{snap(row("a", Working, t0), row("b", Working, t0)), snap(row("b", Working, t0)), snap()}}
	if err := run(t, f, opts()); err != nil {
		t.Fatal(err)
	}
	out := f.out.String()
	if !strings.Contains(out, "qm watch: no worker left to wait for") || strings.Contains(out, "a gone") || len(f.notified) != 0 {
		t.Fatalf("out:\n%s", out)
	}
}

func TestWatchKeepsMembershipWhileDetectionFails(t *testing.T) {
	failed := snap(row("old", Unknown, t0)) // the restore fallback's guess
	failed.Note = "ps: exit 1"
	f := &fake{snaps: []Snapshot{snap(row("a", Working, t0)), failed, snap(row("a", Stopped, t0))}}
	if err := run(t, f, opts()); err != nil {
		t.Fatal(err)
	}
	out := f.out.String()
	if strings.Contains(out, "no worker left") || strings.Contains(out, "old") || !strings.Contains(out, "a stopped") {
		t.Fatalf("out:\n%s", out)
	}
}

func TestWatchTimeout(t *testing.T) {
	f := &fake{snaps: []Snapshot{snap(row("a", Working, t0))}}
	o := opts()
	o.Timeout = time.Minute
	if err := run(t, f, o); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.out.String(), "qm watch: timeout after 1m0s; no change") || !f.now.Equal(t0.Add(time.Minute)) {
		t.Fatalf("now %v, out:\n%s", f.now, f.out.String())
	}
}

func TestWatchSnapshotGetsDeadline(t *testing.T) {
	f := &fake{snaps: []Snapshot{snap(row("a", Working, t0))}}
	var deadlines int
	f.snapFn = func(ctx context.Context) error {
		if _, ok := ctx.Deadline(); ok {
			deadlines++
		}
		return nil
	}
	o := opts()
	o.Timeout = 30 * time.Second
	if err := run(t, f, o); err != nil {
		t.Fatal(err)
	}
	if deadlines != f.calls || deadlines == 0 {
		t.Fatalf("%d of %d snapshots had a deadline", deadlines, f.calls)
	}
}

func TestWatchSnapshotPastTimeout(t *testing.T) {
	// A snapshot stuck past --timeout ends the watch with the timeout.
	f := &fake{snaps: []Snapshot{snap(row("a", Working, t0))}}
	f.snapFn = func(ctx context.Context) error {
		if f.calls == 0 {
			return nil
		}
		return context.DeadlineExceeded
	}
	o := opts()
	o.Timeout = time.Hour
	if err := run(t, f, o); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.out.String(), "qm watch: timeout after 1h0m0s; no change") {
		t.Fatalf("out:\n%s", f.out.String())
	}
}

func TestWatchUnknownName(t *testing.T) {
	f := &fake{snaps: []Snapshot{snap(row("a", Working, t0))}}
	o := opts()
	o.Names = []string{"a", "nope"}
	err := run(t, f, o)
	if !errors.Is(err, ErrUnknownName) || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err %v", err)
	}
}

func TestWatchNamesOnly(t *testing.T) {
	f := &fake{snaps: []Snapshot{snap(row("a", Working, t0), row("b", Working, t0)), snap(row("a", Working, t0), row("b", Stopped, t0)), snap(row("a", Gone, t0), row("b", Stopped, t0))}}
	o := opts()
	o.Names = []string{"a"}
	if err := run(t, f, o); err != nil {
		t.Fatal(err)
	}
	out := f.out.String()
	if strings.Contains(out, "b stopped") || !strings.Contains(out, "a gone") || strings.Contains(out, "  b ") {
		t.Fatalf("out:\n%s", out)
	}
}

func TestWatchNotifierFailureIgnored(t *testing.T) {
	f := &fake{
		snaps:    []Snapshot{snap(row("a", Working, t0), row("b", Working, t0)), snap(row("a", Gone, t0), row("b", Stopped, t0))},
		notifyFn: func(string) error { return errors.New("no notifier") },
	}
	if err := run(t, f, opts()); err != nil {
		t.Fatal(err)
	}
	if len(f.notified) != 2 || !strings.Contains(f.err.String(), "no notifier") {
		t.Fatalf("notified %q, stderr %q", f.notified, f.err.String())
	}
}

func TestWatchOverlappingWatchesNotifyOnce(t *testing.T) {
	// Another watch claims a's stop and b's silence between this watch's
	// load and its claim; it keeps an unrelated mark of c.
	act := t0.Add(-time.Hour)
	f := &fake{snaps: []Snapshot{snap(row("a", Working, t0), row("b", Working, act)), snap(row("a", Stopped, t0), row("b", Silent, act))}}
	f.store.onUpdate = func(m Marks) {
		m["s-a"] = Mark{Stop: t0.UnixNano()}
		m["s-c"] = Mark{Gone: "s-c"}
	}
	if err := run(t, f, opts()); err != nil {
		t.Fatal(err)
	}
	if len(f.notified) != 1 || f.notified[0] != "b silent for 10 min" {
		t.Fatalf("notified %q", f.notified)
	}
	if f.store.marks["s-c"].Gone != "s-c" || f.store.marks["s-b"].Silent != act.UnixNano() {
		t.Fatalf("marks %v", f.store.marks)
	}
}

func TestWatchUnknownIsNoEvent(t *testing.T) {
	unknown := snap(row("a", Unknown, t0))
	unknown.Note = "ps: exit 1"
	f := &fake{snaps: []Snapshot{unknown}}
	o := opts()
	o.Timeout = 30 * time.Second
	if err := run(t, f, o); err != nil {
		t.Fatal(err)
	}
	out := f.out.String()
	if strings.Count(out, "ps: exit 1") != 1 || !strings.Contains(out, "timeout") {
		t.Fatalf("out:\n%s", out)
	}
}

func TestWatchInterruptedDuringSleep(t *testing.T) {
	f := &fake{snaps: []Snapshot{snap(row("a", Working, t0))}}
	d := f.deps()
	ctx, cancel := context.WithCancel(context.Background())
	d.Sleep = func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() }
	f.now = t0
	if err := Run(ctx, opts(), d); !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
}

func TestWatchInterruptedDuringSnapshotNeverSucceeds(t *testing.T) {
	// The interrupt lands while a snapshot that holds an event is read.
	f := &fake{snaps: []Snapshot{snap(row("a", Working, t0)), snap(row("a", Stopped, t0))}}
	ctx, cancel := context.WithCancel(context.Background())
	f.snapFn = func(context.Context) error {
		if f.calls == 1 {
			cancel()
		}
		return nil
	}
	f.now = t0
	if err := Run(ctx, opts(), f.deps()); !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, out:\n%s", err, f.out.String())
	}
	if strings.Contains(f.out.String(), "a stopped") {
		t.Fatalf("reported after the interrupt:\n%s", f.out.String())
	}
}

func TestWatchInterruptedDuringNotifyNeverSucceeds(t *testing.T) {
	f := &fake{snaps: []Snapshot{snap(row("a", Stopped, t0))}}
	ctx, cancel := context.WithCancel(context.Background())
	d := f.deps()
	d.Notify = func(ctx context.Context, _ string) error { cancel(); return ctx.Err() }
	f.now = t0
	if err := Run(ctx, opts(), d); !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
}

func TestWatchKeepsSilenceMarkItDidNotSupersede(t *testing.T) {
	// A one-minute watch reported a's silence; a ten-minute watch sees the
	// unchanged worker as working. The mark stays: the next one-minute watch
	// must not report the same silence again.
	act := t0.Add(-5 * time.Minute)
	f := &fake{store: memStore{marks: Marks{"s-a": {Silent: act.UnixNano()}}},
		snaps: []Snapshot{snap(row("a", Working, act))}}
	o := opts()
	o.Timeout = 30 * time.Second
	if err := run(t, f, o); err != nil {
		t.Fatal(err)
	}
	if got := f.store.marks["s-a"].Silent; got != act.UnixNano() {
		t.Fatalf("silence mark %d, want %d kept; marks %v", got, act.UnixNano(), f.store.marks)
	}
}

func TestWatchClearsOnlyWhatNewerActivitySupersedes(t *testing.T) {
	// The working snapshot saw activity at t0. A stop another watch claimed
	// since (after it) stays; the older silence mark is superseded.
	f := &fake{store: memStore{marks: Marks{"s-a": {Stop: t0.Add(-time.Hour).UnixNano(), Silent: t0.Add(-time.Hour).UnixNano()}}},
		snaps: []Snapshot{snap(row("a", Working, t0))}}
	f.store.onUpdate = func(m Marks) {
		mk := m["s-a"]
		mk.Stop = t0.Add(time.Minute).UnixNano()
		m["s-a"] = mk
	}
	o := opts()
	o.Timeout = 30 * time.Second
	if err := run(t, f, o); err != nil {
		t.Fatal(err)
	}
	got := f.store.marks["s-a"]
	if got.Stop != t0.Add(time.Minute).UnixNano() || got.Silent != 0 {
		t.Fatalf("marks %+v: want the newer stop kept and the older silence cleared", got)
	}
}

func TestWatchLosingTheClaimKeepsWaiting(t *testing.T) {
	// Another watch claims a's stop between this watch's load and its claim.
	// This watch reports nothing and keeps waiting for b.
	f := &fake{snaps: []Snapshot{snap(row("a", Stopped, t0), row("b", Working, t0)), snap(row("a", Stopped, t0), row("b", Stopped, t0.Add(time.Minute)))}}
	f.store.onUpdate = func(m Marks) {
		if _, ok := m["s-a"]; !ok {
			m["s-a"] = Mark{Stop: t0.UnixNano()}
		}
	}
	if err := run(t, f, opts()); err != nil {
		t.Fatal(err)
	}
	out := f.out.String()
	if strings.Contains(out, "a stopped 12s ago") || f.calls != 2 || !strings.Contains(out, "b stopped") {
		t.Fatalf("calls %d, out:\n%s", f.calls, out)
	}
	if len(f.notified) != 1 || f.notified[0] != "b stopped" {
		t.Fatalf("notified %q", f.notified)
	}
}

func TestWatchClaimsOneEventAtATime(t *testing.T) {
	// a and b stop together; the interrupt lands during a's notifier. b was
	// not claimed, so a restarted watch still reports it.
	f := &fake{snaps: []Snapshot{snap(row("a", Stopped, t0), row("b", Stopped, t0))}}
	ctx, cancel := context.WithCancel(context.Background())
	d := f.deps()
	d.Notify = func(ctx context.Context, s string) error {
		f.notified = append(f.notified, s)
		cancel()
		return ctx.Err()
	}
	f.now = t0
	if err := Run(ctx, opts(), d); !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	if _, ok := f.store.marks["s-b"]; ok || len(f.notified) != 1 || strings.Contains(f.out.String(), "b stopped") {
		t.Fatalf("marks %v, notified %q, out:\n%s", f.store.marks, f.notified, f.out.String())
	}
	if _, ok := f.store.marks["s-a"]; !ok {
		t.Fatalf("a not claimed: %v", f.store.marks)
	}
}

func TestWatchOneDeadlineForMarksAndNotifier(t *testing.T) {
	f := &fake{snaps: []Snapshot{snap(row("a", Stopped, t0))}}
	d := f.deps()
	var notifyDeadline bool
	d.Notify = func(ctx context.Context, _ string) error {
		_, notifyDeadline = ctx.Deadline()
		return nil
	}
	o := opts()
	o.Timeout = 30 * time.Second
	f.now = t0
	if err := Run(context.Background(), o, d); err != nil {
		t.Fatal(err)
	}
	if !notifyDeadline || f.store.deadlines == 0 {
		t.Fatalf("notifier deadline %v, marks updates with a deadline %d", notifyDeadline, f.store.deadlines)
	}
}

func TestWatchTimeoutCutsTheNotifierAndLeavesTheRestUnclaimed(t *testing.T) {
	f := &fake{snaps: []Snapshot{snap(row("a", Stopped, t0), row("b", Stopped, t0))}}
	d := f.deps()
	d.Notify = func(ctx context.Context, s string) error {
		f.notified = append(f.notified, s)
		f.now = f.now.Add(time.Minute) // the notifier ran past the deadline
		return context.DeadlineExceeded
	}
	o := opts()
	o.Timeout = 30 * time.Second
	f.now = t0
	if err := Run(context.Background(), o, d); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.store.marks["s-b"]; ok || len(f.notified) != 1 {
		t.Fatalf("marks %v, notified %q", f.store.marks, f.notified)
	}
}
