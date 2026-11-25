package watch

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/route"
	"github.com/IZZY-Agency/quartermaster/internal/up"
)

// tab is row with a tab of its own.
func tab(name string, st State, act time.Time) Row {
	r := row(name, st, act)
	r.Tab = launch.SessionRef{Launcher: "agterm", ID: "t-" + name}
	return r
}

// tinted runs f's watch with a Tint that records "name state" and returns
// what it recorded; fail makes it fail.
func tinted(t *testing.T, f *fake, o Options, fail func(Row) error) []string {
	t.Helper()
	if f.now.IsZero() {
		f.now = t0
	}
	var got []string
	d := f.deps()
	d.Tint = func(ctx context.Context, r Row, _ time.Time) error {
		f.tinting = true
		defer func() { f.tinting = false }()
		if _, ok := ctx.Deadline(); !ok {
			t.Error("a tint without a deadline")
		}
		got = append(got, r.Name+" "+string(r.State))
		if fail != nil {
			return fail(r)
		}
		return nil
	}
	if err := Run(context.Background(), o, d); err != nil {
		t.Fatal(err)
	}
	return got
}

func timeout(d time.Duration) Options { return Options{Interval: 15 * time.Second, Timeout: d} }

func TestWatchTintsOnlyWhenTheColourChanges(t *testing.T) {
	act := t0.Add(time.Minute)
	f := &fake{snaps: []Snapshot{
		snap(tab("a", Working, t0), tab("b", Working, t0)),
		snap(tab("a", Working, t0), tab("b", Waiting, t0)),
		snap(tab("a", Unknown, t0), tab("b", Working, t0)),
		snap(tab("a", Stopped, act), row("no-tab", Working, t0), tab("b", Working, t0)),
	}}
	got := tinted(t, f, opts(), nil)
	if want := []string{"a working", "b working", "a stopped"}; !slices.Equal(got, want) {
		t.Fatalf("tinted %q, want %q", got, want)
	}
	if !strings.Contains(f.out.String(), "a stopped") {
		t.Errorf("out:\n%s", f.out.String())
	}
}

// A worker that is silent, or gone, when the watch begins is tinted though its
// event was reported before, and back when it works again.
func TestWatchTintsAnEventReportedBefore(t *testing.T) {
	f := &fake{snaps: []Snapshot{snap(tab("a", Silent, t0))}}
	if got := tinted(t, f, opts(), nil); !slices.Equal(got, []string{"a silent"}) {
		t.Fatalf("tinted %q", got)
	}
	again(f, snap(tab("a", Silent, t0)), snap(tab("a", Working, t0.Add(time.Hour))), snap(tab("a", Gone, t0.Add(time.Hour))))
	if got := tinted(t, f, opts(), nil); !slices.Equal(got, []string{"a silent", "a working", "a gone"}) {
		t.Fatalf("tinted %q", got)
	}
}

// A watch with a longer silence threshold reads the same activity as working:
// it leaves the silent tint of the other one.
func TestWatchesWithTwoThresholdsDoNotFight(t *testing.T) {
	f := &fake{snaps: []Snapshot{snap(tab("a", Silent, t0))}}
	tinted(t, f, opts(), nil)
	again(f, snap(tab("a", Working, t0)), snap(tab("a", Working, t0)))
	if got := tinted(t, f, timeout(time.Minute), nil); len(got) != 0 {
		t.Fatalf("tinted %q over a silence no activity followed", got)
	}
	again(f, snap(tab("a", Working, t0)), snap(tab("a", Working, t0.Add(time.Second))))
	if got := tinted(t, f, timeout(time.Minute), nil); !slices.Equal(got, []string{"a working"}) {
		t.Fatalf("tinted %q", got)
	}
}

// A reading older than the one the tint was given for changes nothing, and is
// checked again under the lock.
func TestWatchOlderReadingDoesNotUndoANewerTint(t *testing.T) {
	f := &fake{now: t0, snaps: []Snapshot{snap(tab("a", Working, t0))}}
	f.tints.onUpdate = func(m Marks) {
		// Another watch tinted it from a later reading meanwhile.
		got := m["s-a"]
		got.Tint = TintMark{State: Stopped, At: t0.Add(time.Hour).UnixNano()}
		m["s-a"] = got
	}
	if got := tinted(t, f, timeout(20*time.Second), nil); len(got) != 0 {
		t.Fatalf("tinted %q", got)
	}
}

// The other watch saw a stop this one missed, then ended: this one, whose own
// state did not change, puts the role's tint back.
func TestWatchRestoresATintAnotherWatchLeft(t *testing.T) {
	f := &fake{snaps: []Snapshot{snap(tab("a", Working, t0)), snap(tab("a", Working, t0)), snap(tab("a", Working, t0.Add(time.Minute)))}}
	n := 0
	f.snapFn = func(context.Context) error {
		if n++; n == 3 {
			f.tints.marks["s-a"] = Mark{Tint: TintMark{State: Stopped, Activity: t0.UnixNano(), At: f.now.UnixNano() - 1}}
		}
		return nil
	}
	if got := tinted(t, f, timeout(40*time.Second), nil); !slices.Equal(got, []string{"a working", "a working"}) {
		t.Fatalf("tinted %q", got)
	}
	if st := f.tints.marks["s-a"].Tint.State; st != Working {
		t.Errorf("marked %q", st)
	}
}

func TestWatchTintFailureNotedOnceAndRetriedLater(t *testing.T) {
	f := &fake{snaps: []Snapshot{snap(tab("a", Working, t0))}}
	got := tinted(t, f, timeout(150*time.Second), func(Row) error { return errors.New("no agterm") })
	if len(got) < 2 || len(got) > 4 || strings.Count(f.err.String(), "worker tabs not tinted: no agterm") != 1 {
		t.Fatalf("%d tries, err %q", len(got), f.err.String())
	}
	if f.tints.marks["s-a"].Tint != (TintMark{}) {
		t.Errorf("a failed tint was recorded: %+v", f.tints.marks["s-a"])
	}
}

// Tints neither touch the marks of the events, nor lock them.
func TestWatchTintsLeaveTheEventMarksAlone(t *testing.T) {
	f := &fake{snaps: []Snapshot{snap(tab("a", Working, t0)), snap(tab("a", Working, t0)), snap(tab("a", Stopped, t0))}}
	f.store.onUpdate = func(Marks) {
		if f.tinting {
			t.Error("a tint under the event marks' lock")
		}
	}
	tinted(t, f, opts(), func(Row) error { return nil })
	if f.store.updates != 1 || f.store.marks["s-a"] != (Mark{Stop: t0.UnixNano()}) {
		t.Errorf("%d event updates, marks %+v", f.store.updates, f.store.marks)
	}
}

// A state read after a failure is tried at once, and one tab's failure holds
// up no other's.
func TestWatchTintFailureIsPerTabAndState(t *testing.T) {
	f := &fake{snaps: []Snapshot{
		snap(tab("a", Working, t0), tab("b", Working, t0)),
		snap(tab("a", Working, t0), tab("b", Stopped, t0.Add(time.Second))),
	}}
	got := tinted(t, f, opts(), func(r Row) error {
		if r.State == Working {
			return errors.New("no agterm")
		}
		return nil
	})
	if want := []string{"a working", "b working", "b stopped"}; !slices.Equal(got, want) {
		t.Errorf("tinted %q, want %q", got, want)
	}
}

// A pass whose time is used up holds back no tab it did not come to.
func TestWatchTintPassThatRunsOutOfTimeLeavesTheRestDue(t *testing.T) {
	f := &fake{now: t0, snaps: []Snapshot{snap(tab("a", Working, t0), tab("b", Working, t0))}}
	w := &watcher{o: opts(), d: f.deps(), start: t0}
	var got []string
	w.d.Tint = func(ctx context.Context, r Row, _ time.Time) error {
		got = append(got, r.Name)
		if len(got) == 1 {
			<-ctx.Done()
		}
		return ctx.Err()
	}
	rows := f.snaps[0].Rows
	w.tint(context.Background(), rows, t0)
	f.now = t0.Add(15 * time.Second)
	w.tint(context.Background(), rows, f.now)
	if want := []string{"a", "b"}; !slices.Equal(got, want) {
		t.Errorf("tried %q, want %q: b at the next check, a held", got, want)
	}
	if st := f.tints.marks["s-b"].Tint.State; st != Working {
		t.Errorf("b was not tinted at the next check: %q", st)
	}
}

// A refused tint is neither noted nor taken for shown: the same state is tried
// again later.
func TestWatchRefusedTintIsTriedAgain(t *testing.T) {
	f := &fake{snaps: []Snapshot{snap(tab("a", Working, t0))}}
	n := 0
	got := tinted(t, f, timeout(150*time.Second), func(Row) error {
		if n++; n == 1 {
			return ErrNotNow
		}
		return nil
	})
	if len(got) != 2 || f.err.Len() != 0 || f.tints.marks["s-a"].Tint.State != Working {
		t.Errorf("tinted %q, err %q, marks %+v", got, f.err.String(), f.tints.marks)
	}
	f = &fake{snaps: []Snapshot{snap(tab("a", Working, t0))}}
	if tinted(t, f, timeout(20*time.Second), func(Row) error { return ErrNotNow }); len(f.tints.marks) != 0 {
		t.Errorf("a refused tint was recorded: %+v", f.tints.marks)
	}
}

// A worker that goes from stopped straight to waiting, and from silent to
// waiting with newer activity, gets the role's tint back.
func TestWatchWaitingGetsTheRoleTintBack(t *testing.T) {
	for _, st := range []State{Stopped, Silent, Gone} {
		f := &fake{snaps: []Snapshot{snap(tab("a", st, t0))}}
		tinted(t, f, opts(), nil)
		again(f, snap(tab("a", Waiting, t0.Add(time.Minute))))
		f.now = f.now.Add(time.Minute)
		if got := tinted(t, f, timeout(time.Minute), nil); !slices.Equal(got, []string{"a waiting"}) {
			t.Errorf("%s then waiting: tinted %q", st, got)
		}
	}
}

// An interrupt during the report begins no tint.
func TestWatchInterruptedByTheReportTintsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &fake{now: t0}
	w := &watcher{o: opts(), d: f.deps(), start: t0}
	w.d.Tint = func(context.Context, Row, time.Time) error { t.Error("tinted after the interrupt"); return nil }
	cancel()
	w.tint(ctx, []Row{tab("a", Stopped, t0)}, t0)
}

// A tab recorded for two sessions, a tmux window and a missing tab are no
// worker's tab.
func TestTabs(t *testing.T) {
	ref := func(l, id string) launch.SessionRef { return launch.SessionRef{Launcher: l, ID: id} }
	p := up.Project{ID: "p", Coordinator: up.Session{ID: "c"}, Sessions: map[string]launch.SessionRef{
		"c": ref("agterm", "shared"), "w1": ref("agterm", "shared"), "w2": ref("agterm", "t2"), "w3": ref("tmux", "@1"), "w4": {Name: "x"},
	}}
	recs := []route.Record{{Kind: "worker", Name: "w5", UpProject: "p", ClaudeSession: "w5", TermLauncher: "agterm", TermID: "t5", Time: t0}}
	var cands []up.Candidate
	for _, id := range []string{"w1", "w2", "w3", "w4", "w5"} {
		cands = append(cands, up.Candidate{Role: up.RoleWorker, SessionID: id})
	}
	got := Tabs(p, recs, cands)
	if len(got) != 2 || got["w2"].ID != "t2" || got["w5"].ID != "t5" {
		t.Errorf("tabs %+v", got)
	}
}
