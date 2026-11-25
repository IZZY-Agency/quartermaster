package watch

import (
	"strings"
	"testing"
	"time"
)

func TestWatchWaitingIsNoEvent(t *testing.T) {
	// The only worker waits for its own background task: the watch goes on.
	f := &fake{snaps: []Snapshot{snap(row("a", Waiting, t0))}}
	o := opts()
	o.Timeout = time.Minute
	if err := run(t, f, o); err != nil {
		t.Fatal(err)
	}
	got := f.out.String()
	if !strings.Contains(got, "waiting") || !strings.Contains(got, "timeout after 1m0s") || f.calls < 2 || len(f.notified) != 0 {
		t.Fatalf("calls %d, notified %q, out:\n%s", f.calls, f.notified, got)
	}
	if len(f.store.marks) != 0 {
		t.Fatalf("a waiting worker was marked: %v", f.store.marks)
	}
}

// A background task's end re-invokes the session. Claude Code's hooks
// documentation lists a background subagent's report and a scheduled wakeup
// among what fires UserPromptSubmit, and not a background shell's end (2.1.332
// fires it for that too). The turn is then reopened (working) or not (still
// waiting): either way the stop that follows is reported, once.
func TestWatchReportsStopAfterWaitingOnce(t *testing.T) {
	for name, resumed := range map[string]State{"with a prompt hook": Working, "without": Waiting} {
		t.Run(name, func(t *testing.T) {
			waits := row("a", Waiting, t0.Add(10*time.Second))
			back := row("a", resumed, t0.Add(20*time.Second))
			stop := row("a", Stopped, t0.Add(30*time.Second))
			f := &fake{snaps: []Snapshot{snap(waits), snap(back), snap(waits), snap(back), snap(stop)}}
			if err := run(t, f, opts()); err != nil {
				t.Fatal(err)
			}
			if f.calls != 5 || len(f.notified) != 1 || f.notified[0] != "a stopped" {
				t.Fatalf("calls %d, notified %q", f.calls, f.notified)
			}
			// A restarted watch is not woken by it again.
			again(f, snap(stop, row("b", Working, t0)))
			o := opts()
			o.Timeout = time.Minute
			if err := run(t, f, o); err != nil {
				t.Fatal(err)
			}
			if len(f.notified) != 0 || !strings.Contains(f.out.String(), "stopped (reported)") {
				t.Fatalf("notified %q, out:\n%s", f.notified, f.out.String())
			}
		})
	}
}

func TestWatchReportedStopThenWaitingThenStop(t *testing.T) {
	// A reported stop; the worker is prompted, waits, is re-invoked with no
	// prompt hook and stops for real: that stop is reported too.
	f := &fake{snaps: []Snapshot{snap(row("a", Stopped, t0))}}
	if err := run(t, f, opts()); err != nil {
		t.Fatal(err)
	}
	again(f, snap(row("a", Waiting, t0.Add(time.Minute))), snap(row("a", Stopped, t0.Add(2*time.Minute))))
	if err := run(t, f, opts()); err != nil {
		t.Fatal(err)
	}
	if f.calls != 2 || len(f.notified) != 1 || f.notified[0] != "a stopped" {
		t.Fatalf("calls %d, notified %q", f.calls, f.notified)
	}
}

func TestWatchWaitingPastThresholdIsSilentOnce(t *testing.T) {
	// A finished worker that left a dev server running: found by silence,
	// once; seen waiting again (its task wrote), it can go silent again.
	f := &fake{snaps: []Snapshot{snap(row("a", Waiting, t0)), snap(row("a", Silent, t0))}}
	if err := run(t, f, opts()); err != nil {
		t.Fatal(err)
	}
	if len(f.notified) != 1 || f.notified[0] != "a silent for 10 min" {
		t.Fatalf("notified %q", f.notified)
	}
	again(f, snap(row("a", Silent, t0)))
	o := opts()
	o.Timeout = time.Minute
	if err := run(t, f, o); err != nil {
		t.Fatal(err)
	}
	if len(f.notified) != 0 || !strings.Contains(f.out.String(), "timeout after 1m0s") {
		t.Fatalf("notified %q, out:\n%s", f.notified, f.out.String())
	}
	// Resumed by its task and waiting again: the old silence is over.
	again(f, snap(row("a", Waiting, t0.Add(time.Minute))), snap(row("a", Silent, t0.Add(time.Minute))))
	if err := run(t, f, opts()); err != nil {
		t.Fatal(err)
	}
	if len(f.notified) != 1 {
		t.Fatalf("notified %q", f.notified)
	}
	// And a real stop after a reported silence is its own event.
	again(f, snap(row("a", Stopped, t0.Add(2*time.Minute))))
	if err := run(t, f, opts()); err != nil {
		t.Fatal(err)
	}
	if len(f.notified) != 1 || f.notified[0] != "a stopped" {
		t.Fatalf("notified %q", f.notified)
	}
}

func TestWatchWaitingSupersedesOlderMarks(t *testing.T) {
	// A worker seen waiting is as alive as a working one: the marks its newer
	// activity supersedes go, a newer one stays.
	f := &fake{snaps: []Snapshot{snap(row("a", Waiting, t0))}}
	f.store.marks = Marks{"s-a": {Stop: t0.Add(time.Second).UnixNano(), Silent: t0.Add(-time.Second).UnixNano(), Gone: "s-a"}}
	o := opts()
	o.Timeout = 10 * time.Second
	if err := run(t, f, o); err != nil {
		t.Fatal(err)
	}
	if got, want := f.store.marks["s-a"], (Mark{Stop: t0.Add(time.Second).UnixNano()}); got != want {
		t.Fatalf("mark %+v, want %+v", got, want)
	}
}

func TestTableWaitingHasNoHandoffLine(t *testing.T) {
	r := row("a", Waiting, t0)
	r.Handoff = "/h/a.md"
	if got := Table([]Row{r}, nil, "/h"); !strings.Contains(got, "waiting") || strings.Contains(got, "handoff note") {
		t.Fatalf("table:\n%s", got)
	}
}

// A live worker whose silence was reported can still stop, go silent again or
// go away: a restarted watch waits for it, quietly, and reports that once.
func TestWatchWaitsForReportedSilentWorker(t *testing.T) {
	for _, before := range []State{Working, Waiting} {
		for _, end := range []State{Stopped, Gone} {
			t.Run(string(before)+" then "+string(end), func(t *testing.T) {
				silent, over := row("a", Silent, t0), row("a", end, t0.Add(time.Hour))
				f := &fake{snaps: []Snapshot{snap(row("a", before, t0)), snap(silent)}}
				if err := run(t, f, opts()); err != nil {
					t.Fatal(err)
				}
				if len(f.notified) != 1 || f.notified[0] != "a silent for 10 min" {
					t.Fatalf("notified %q", f.notified)
				}
				// Restarted: each check sleeps, nothing is notified again, then
				// the end is, whether or not the worker was seen resumed.
				again(f, snap(silent), snap(silent), snap(silent), snap(over))
				began := f.now
				if err := run(t, f, opts()); err != nil {
					t.Fatal(err)
				}
				if len(f.notified) != 1 || f.notified[0] != "a "+string(end) || f.calls != 4 || f.now.Sub(began) != 45*time.Second {
					t.Fatalf("calls %d in %v, notified %q", f.calls, f.now.Sub(began), f.notified)
				}
				if got := f.out.String(); strings.Count(got, "silent (reported)") != 1 || strings.Contains(got, "a silent for") {
					t.Fatalf("out:\n%s", got)
				}
				// Reported once: nothing is left to wait for.
				again(f, snap(over))
				if err := run(t, f, opts()); err != nil {
					t.Fatal(err)
				}
				if len(f.notified) != 0 || f.calls != 1 || !strings.Contains(f.out.String(), "no worker to wait for") {
					t.Fatalf("calls %d, notified %q, out:\n%s", f.calls, f.notified, f.out.String())
				}
			})
		}
	}
}

func TestWatchLosingASilenceClaimKeepsWaiting(t *testing.T) {
	// Another watch claims the only worker's silence first: this one waits on.
	silent := row("a", Silent, t0)
	f := &fake{snaps: []Snapshot{snap(silent), snap(silent), snap(row("a", Stopped, t0.Add(time.Minute)))}}
	f.store.onUpdate = func(m Marks) {
		if len(m) == 0 {
			m.set(silent, t0)
			f.store.marks = m.clone()
		}
	}
	if err := run(t, f, opts()); err != nil {
		t.Fatal(err)
	}
	if f.calls != 3 || len(f.notified) != 1 || f.notified[0] != "a stopped" {
		t.Fatalf("calls %d, notified %q", f.calls, f.notified)
	}
}

func TestWatchSilentSupersedesOlderDisappearance(t *testing.T) {
	// A worker reported gone came back and went silent: gone again is news.
	f := &fake{snaps: []Snapshot{snap(row("a", Silent, t0)), snap(row("a", Gone, t0))}}
	f.store.marks = Marks{"s-a": {Silent: t0.UnixNano(), Gone: "s-a"}}
	if err := run(t, f, opts()); err != nil {
		t.Fatal(err)
	}
	if f.calls != 2 || len(f.notified) != 1 || f.notified[0] != "a gone" {
		t.Fatalf("calls %d, notified %q", f.calls, f.notified)
	}
}
