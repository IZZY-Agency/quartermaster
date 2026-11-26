package watch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/plain"
	"github.com/IZZY-Agency/quartermaster/internal/up"
)

// ErrUnknownName marks a watched name the project has no worker of.
var ErrUnknownName = errors.New("unknown worker")

// Snapshot is the project's workers at one check. Note, when set, says why
// the states are unknown (the process list failed); such a snapshot's
// membership is not trusted.
type Snapshot struct {
	Rows []Row
	Note string
}

// Mark is what watches reported about one worker: the stop (its turn's
// end, Unix nanoseconds), the silence (the activity it was measured from)
// and the disappearance (the session id that went).
type Mark struct {
	Stop   int64  `json:"stop,omitempty"`
	Silent int64  `json:"silent,omitempty"`
	Gone   string `json:"gone,omitempty"`
	// GoneAt is when the disappearance was claimed (Unix nanoseconds); a
	// mark without one is older than any snapshot.
	GoneAt int64 `json:"gone_at,omitempty"`
	// Tint is the tint a watch last gave the worker's tab, in the store of the
	// tints (Deps.Tints) only.
	Tint TintMark `json:"tint,omitzero"`
}

// Marks holds the marks by worker session id (Row.SessionID).
type Marks map[string]Mark

func (m Marks) clone() Marks {
	if m == nil {
		return Marks{}
	}
	return maps.Clone(m)
}

// event is r's state as something to report, keyed so that one stop, one
// silence or one disappearance is reported once: ok is false for working,
// waiting and unknown.
func event(r Row) (Mark, bool) {
	switch r.State {
	case Stopped:
		return Mark{Stop: r.StopAt.UnixNano()}, true
	case Silent:
		return Mark{Silent: r.Activity.UnixNano()}, true
	case Gone:
		return Mark{Gone: r.Current}, true
	}
	return Mark{}, false
}

// reported says whether r's current event is marked.
func (m Marks) reported(r Row) bool {
	e, ok := event(r)
	if !ok {
		return false
	}
	got := m[r.SessionID]
	switch r.State {
	case Stopped:
		return got.Stop == e.Stop
	case Silent:
		return got.Silent == e.Silent
	}
	return got.Gone == e.Gone
}

// set marks r's current event, claimed at now.
func (m Marks) set(r Row, now time.Time) {
	e, _ := event(r)
	got := m[r.SessionID]
	switch r.State {
	case Stopped:
		got.Stop = e.Stop
	case Silent:
		got.Silent = e.Silent
	case Gone:
		got.Gone, got.GoneAt = e.Gone, now.UnixNano()
	}
	m[r.SessionID] = got
}

// supersede drops from r's marks what r, seen live in a
// snapshot taken at observed, proves is over: a stop or a silence that activity newer than it
// followed, a disappearance claimed before the snapshot. A mark newer than
// what the snapshot saw (another watch claimed it since) stays, and so does
// one the same activity still stands behind. It says whether it changed m.
func (m Marks) supersede(r Row, observed time.Time) bool {
	got, ok := m[r.SessionID]
	if !ok {
		return false
	}
	was := got
	if !r.Activity.IsZero() {
		at := r.Activity.UnixNano()
		if got.Stop != 0 && at > got.Stop {
			got.Stop = 0
		}
		if got.Silent != 0 && at > got.Silent {
			got.Silent = 0
		}
	}
	if got.Gone != "" && observed.UnixNano() > got.GoneAt {
		got.Gone, got.GoneAt = "", 0
	}
	if got == was {
		return false
	}
	if got == (Mark{}) {
		delete(m, r.SessionID)
	} else {
		m[r.SessionID] = got
	}
	return true
}

// MarkStore keeps the marks across watches. Load reads them; Update locks
// them (giving up when ctx ends), reads the stored ones afresh, calls fn, and
// writes them back when fn says it changed them, so concurrent watches never
// lose each other's marks. A nil map read from the store is an empty one.
type MarkStore interface {
	Load() (Marks, error)
	Update(ctx context.Context, fn func(Marks) bool) error
}

// Options are qm watch's: the names to watch (none: every worker), how long
// to wait at most (0: no limit), counted from Start (zero: the first call of
// Now), and how often to check.
type Options struct {
	Names []string
	// Prefixes are the project's worker-name prefixes (up.Prefixes): a bare
	// name in Names also means PREFIX-name.
	Prefixes []string
	Timeout  time.Duration
	Start    time.Time
	Interval time.Duration
	// WaitStart is how long a named worker already reported stopped is waited
	// for to work again (0: not waited for); it counts from Start and the
	// timeout caps it.
	WaitStart time.Duration
}

// Deps is what a watch reads and writes through. Snapshot, the marks and
// Notify get a context that ends at an interrupt and at the timeout.
type Deps struct {
	Snapshot func(ctx context.Context) (Snapshot, error)
	Now      func() time.Time
	Sleep    func(ctx context.Context, d time.Duration) error
	Marks    MarkStore
	// Tint gives a worker's tab the tint of the worker's state, as read at
	// observed; nil tints nothing. A tab that is closed or the owner's is no
	// error; one that must not be tinted now (another session in front, a newer
	// word by another qm) is ErrNotNow.
	Tint func(ctx context.Context, r Row, observed time.Time) error
	// Tints keeps what was tinted (Mark.Tint), apart from Marks: its lock is
	// held while a watch tints, and a report must not wait for that.
	Tints MarkStore
	// Notify sends an event line through the user's notifier.
	Notify func(ctx context.Context, text string) error
	Home   string
	Out    io.Writer
	Err    io.Writer
}

// Sleep waits d or until ctx ends.
func Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// errTimeout ends a watch at --timeout.
var errTimeout = errors.New("timeout")

// Run watches the workers until one of them stops, goes silent or goes away
// (design 5): each stop, silence and disappearance is reported once, by the
// first watch that sees it, whether it happened before the watch began or
// during it. It returns ctx's error when interrupted, never nil after an
// interrupt; an ErrUnknownName error for a name the project has no worker
// of.
func Run(ctx context.Context, o Options, d Deps) error {
	w := &watcher{o: o, d: d}
	start := o.Start
	if start.IsZero() {
		start = d.Now()
	}
	w.start = start
	var reliable []Row // the last membership a working process list gave
	for first := true; ; first = false {
		if !first {
			if err := w.sleep(ctx, start); err != nil {
				return w.end(ctx, err)
			}
		}
		observed := d.Now()
		s, err := w.snapshot(ctx, start)
		if err != nil {
			// The first snapshot, an interrupt or the timeout ends the
			// watch; a later failure is printed and the next check tries
			// again.
			if first || ctx.Err() != nil || errors.Is(err, errTimeout) {
				return w.end(ctx, err)
			}
			_, _ = fmt.Fprintf(d.Err, "qm watch: %s\n", plain.Text(err.Error()))
			continue
		}
		rows := s.Rows
		if s.Note != "" {
			w.note(s.Note)
			if reliable != nil {
				// A failed process list says nothing about who the workers
				// are: keep the last membership, its states unknown.
				rows = make([]Row, len(reliable))
				for i, r := range reliable {
					r.State, r.HasSince = Unknown, false
					rows[i] = r
				}
			}
		} else {
			reliable = rows
		}
		if first {
			if err := w.resolveNames(rows); err != nil {
				return err
			}
		}
		rows = w.pick(rows)
		// The marks, their lock and the notifier run under the one deadline
		// of the whole watch.
		lctx, cancel := w.bound(ctx, start)
		done, err := w.step(lctx, rows, observed, first)
		cancel()
		if err == nil {
			w.tint(ctx, rows, observed) // after the report, so it delays none
		}
		if err != nil {
			return w.end(ctx, err)
		}
		if done {
			return nil
		}
	}
}

// step is one check's work on rows: it clears the marks live workers
// supersede, and reports the events it claims. done says the watch is over
// (something was reported, or nothing is left to wait for). An error is ctx's
// (an interrupt, or the timeout as errTimeout).
func (w *watcher) step(ctx context.Context, rows []Row, observed time.Time, first bool) (done bool, err error) {
	marks, err := w.d.Marks.Load()
	if err != nil {
		_, _ = fmt.Fprintf(w.d.Err, "qm watch: marks not read: %s\n", plain.Text(err.Error()))
		marks = Marks{}
	}
	if err := w.clearLive(ctx, rows, marks, observed); err != nil {
		return false, err
	}
	if marks, err = w.d.Marks.Load(); err != nil {
		marks = Marks{}
	}
	labels := map[string]string{}
	var events []Row
	left := 0         // the workers to wait for
	var stopped []Row // named workers reported stopped: waited for to start again
	for _, r := range rows {
		switch r.State {
		case Working, Waiting, Silent, Unknown:
			// A silent worker lives, so it can still stop, go silent again or
			// go away after its silence was reported.
			left++
		}
		_, isEvent := event(r)
		switch {
		case !isEvent:
		case marks.reported(r):
			labels[r.SessionID] = "reported"
			if r.State == Stopped && len(w.o.Names) > 0 {
				stopped = append(stopped, r)
			}
		default:
			events = append(events, r)
		}
	}
	if first {
		w.print(Table(rows, labels, w.d.Home))
	}
	if len(events) > 0 {
		claimed, err := w.report(ctx, events)
		if claimed > 0 {
			// Another watch's events are theirs; the table shows them. The
			// timeout after a report is no news: the report is the result.
			w.print(Table(rows, labels, w.d.Home))
			if errors.Is(err, errTimeout) {
				err = nil
			}
			return true, err
		}
		if err != nil {
			return false, err
		}
		// Every event was claimed by another watch: keep waiting for the rest.
	}
	w.trackAwaited(rows, stopped, first)
	w.awaiting = false
	if left == 0 && len(w.awaited) > 0 && w.waitingStart() {
		if first {
			w.print(fmt.Sprintf("qm watch: waiting up to %v for a stopped worker to start working\n", w.o.WaitStart))
		}
		w.awaiting = true
		return false, nil
	}
	if left == 0 {
		for _, r := range stopped {
			if _, ok := w.awaited[r.SessionID]; ok {
				w.print(fmt.Sprintf("qm watch: %s did not start working within %v\n", plain.Text(r.Name), w.o.WaitStart))
			}
		}
		if first {
			w.print("qm watch: no worker to wait for\n")
		} else {
			w.print("qm watch: no worker left to wait for\n")
		}
		return true, nil
	}
	return false, nil
}

// trackAwaited keeps the workers waited for to start: at the first check the
// named ones reported stopped (when WaitStart allows); later it drops each
// that is no longer stopped with the same stop, since it started, or stopped
// again.
func (w *watcher) trackAwaited(rows, stopped []Row, first bool) {
	if first {
		w.awaited = map[string]int64{}
		if w.o.WaitStart > 0 {
			for _, r := range stopped {
				w.awaited[r.SessionID] = r.StopAt.UnixNano()
			}
		}
		return
	}
	for id, at := range w.awaited {
		if !slices.ContainsFunc(stopped, func(r Row) bool { return r.SessionID == id && r.StopAt.UnixNano() == at }) {
			delete(w.awaited, id)
		}
	}
}

// waitStartLeft is the time left to wait for a stopped worker to start.
func (w *watcher) waitStartLeft() time.Duration {
	return w.o.WaitStart - w.d.Now().Sub(w.start)
}

// waitingStart says whether the wait for a stopped worker to start goes on.
func (w *watcher) waitingStart() bool { return w.waitStartLeft() > 0 }

// bound is ctx limited to the time left to the timeout.
func (w *watcher) bound(ctx context.Context, start time.Time) (context.Context, context.CancelFunc) {
	if left, ok := w.left(start); ok {
		return context.WithTimeout(ctx, max(left, 0))
	}
	return context.WithCancel(ctx)
}

// watcher is one run's state.
type watcher struct {
	o     Options
	d     Deps
	start time.Time
	noted bool
	// awaited: named workers reported stopped at the first check, by session id
	// (their stop's time), still waited for to start; awaiting says the last
	// check kept waiting for one.
	awaited  map[string]int64
	awaiting bool
	tints    tinter
}

// left is the time to the timeout; ok is false without one.
func (w *watcher) left(start time.Time) (time.Duration, bool) {
	if w.o.Timeout <= 0 {
		return 0, false
	}
	return w.o.Timeout - w.d.Now().Sub(start), true
}

func (w *watcher) sleep(ctx context.Context, start time.Time) error {
	wait := w.o.Interval
	if left, ok := w.left(start); ok {
		if left <= 0 {
			return errTimeout
		}
		wait = min(wait, left)
	}
	if w.awaiting {
		if left := w.waitStartLeft(); left > 0 {
			wait = min(wait, left)
		}
	}
	return w.d.Sleep(ctx, wait)
}

// snapshot reads one snapshot under ctx and the time left to the timeout.
func (w *watcher) snapshot(ctx context.Context, start time.Time) (Snapshot, error) {
	sctx := ctx
	if left, ok := w.left(start); ok {
		if left <= 0 {
			return Snapshot{}, errTimeout
		}
		var cancel context.CancelFunc
		sctx, cancel = context.WithTimeout(ctx, left)
		defer cancel()
	}
	s, err := w.d.Snapshot(sctx)
	if ctx.Err() != nil {
		return Snapshot{}, ctx.Err()
	}
	if err != nil && errors.Is(err, context.DeadlineExceeded) && w.o.Timeout > 0 {
		return Snapshot{}, errTimeout
	}
	return s, err
}

// end turns the error that ended a watch into its result: an interrupt
// stays an error, the timeout is a quiet success.
func (w *watcher) end(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, errTimeout) {
		w.print(fmt.Sprintf("qm watch: timeout after %v; no change\n", w.o.Timeout))
		return nil
	}
	return err
}

// resolveNames makes each watched name the full name of a worker: a bare name
// is read with the project's prefixes in front (up.ResolveName), and an
// ambiguous one is refused. Then it fails on a name no worker has.
func (w *watcher) resolveNames(rows []Row) error {
	if len(w.o.Names) == 0 {
		return nil
	}
	existing := make([]up.NamedWorker, len(rows))
	for i, r := range rows {
		cwd := r.Cwd
		existing[i] = up.NamedWorker{Name: r.Name, Base: r.Base, Finished: func() bool { return up.MarkedOrGone(up.Project{}, up.Candidate{Cwd: cwd}) }}
	}
	resolved := make([]string, len(w.o.Names))
	for i, n := range w.o.Names {
		full, _, err := up.ResolveName(existing, n, w.o.Prefixes)
		if err != nil {
			return err
		}
		resolved[i] = full
	}
	w.o.Names = resolved
	return w.checkNames(rows)
}

// checkNames fails on a name no worker has.
func (w *watcher) checkNames(rows []Row) error {
	for _, n := range w.o.Names {
		if !slices.ContainsFunc(rows, func(r Row) bool { return r.Name == n }) {
			return fmt.Errorf("%w %q", ErrUnknownName, n)
		}
	}
	return nil
}

// pick keeps the rows of the watched names (every row without names).
func (w *watcher) pick(rows []Row) []Row {
	if len(w.o.Names) == 0 {
		return rows
	}
	var out []Row
	for _, r := range rows {
		if slices.Contains(w.o.Names, r.Name) {
			out = append(out, r)
		}
	}
	return out
}

// note prints why states are unknown, once per watch.
func (w *watcher) note(s string) {
	if !w.noted {
		w.noted = true
		w.print(fmt.Sprintf("note: worker states unknown: %s\n", s))
	}
}

// clearLive drops, from the marks of the live workers (working, waiting,
// silent), what their newer activity, or being alive, supersedes (Marks.supersede). The comparison is made
// on the marks as they are under the lock, not on the ones loaded earlier. A
// store failure is printed; only the end of ctx is returned, as errTimeout
// (the deadline) or ctx's error.
func (w *watcher) clearLive(ctx context.Context, rows []Row, marks Marks, observed time.Time) error {
	var back []Row
	for _, r := range rows {
		if _, ok := marks[r.SessionID]; ok && (r.State == Working || r.State == Waiting || r.State == Silent) {
			back = append(back, r)
		}
	}
	if len(back) == 0 {
		return nil
	}
	err := w.d.Marks.Update(ctx, func(m Marks) bool {
		changed := false
		for _, r := range back {
			if m.supersede(r, observed) {
				changed = true
			}
		}
		return changed
	})
	return w.stored(ctx, err)
}

// stored says what a marks failure means: nil, or the end of ctx; any other
// failure is printed and nil.
func (w *watcher) stored(ctx context.Context, err error) error {
	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return ctxEnd(ctx)
	}
	_, _ = fmt.Fprintf(w.d.Err, "qm watch: marks not saved: %s\n", plain.Text(err.Error()))
	return nil
}

// ctxEnd is why ctx ended: errTimeout for its deadline, else its error. An
// interrupt of the parent context shows through w.end.
func ctxEnd(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errTimeout
	}
	return ctx.Err()
}

// report claims the events one at a time, in order: it claims an event's
// mark, prints its line and sends it through the notifier, then checks ctx
// before it claims the next, so the events it never reached stay reportable.
// An event another watch claimed first is left to that watch. It returns how
// many events this watch claimed and, when ctx ended, why.
func (w *watcher) report(ctx context.Context, events []Row) (int, error) {
	n := 0
	for _, r := range events {
		if ctx.Err() != nil {
			return n, ctxEnd(ctx)
		}
		if left, ok := w.left(w.start); ok && left <= 0 {
			return n, errTimeout // the watch's clock, which ctx's deadline follows
		}
		claimed := false
		err := w.d.Marks.Update(ctx, func(m Marks) bool {
			if m.reported(r) {
				return false
			}
			m.set(r, w.d.Now())
			claimed = true
			return true
		})
		if err != nil {
			if ctx.Err() != nil {
				return n, ctxEnd(ctx)
			}
			// Unsaved, the marks cannot stop a second report: better twice
			// than never.
			_, _ = fmt.Fprintf(w.d.Err, "qm watch: marks not saved: %s\n", plain.Text(err.Error()))
			claimed = true
		}
		if !claimed {
			continue
		}
		n++
		w.print(w.line(r) + "\n")
		if err := w.d.Notify(ctx, notice(r)); err != nil && ctx.Err() == nil {
			_, _ = fmt.Fprintf(w.d.Err, "qm watch: %s\n", plain.Text(err.Error()))
		}
	}
	if ctx.Err() != nil {
		return n, ctxEnd(ctx)
	}
	return n, nil
}

// line is an event's local line, with the handoff note's path.
func (w *watcher) line(r Row) string {
	r.Name, r.Handoff = plain.Line(r.Name), plain.Line(r.Handoff)
	var s string
	switch r.State {
	case Stopped:
		s = fmt.Sprintf("%s stopped %s ago", r.Name, Duration(r.Since))
	case Silent:
		return fmt.Sprintf("%s silent for %d min", r.Name, int(r.Since/time.Minute))
	default:
		s = r.Name + " gone"
	}
	if r.Handoff != "" {
		s += "; handoff note: " + Tilde(r.Handoff, w.d.Home)
	}
	return s
}

// notice is an event as the notifier gets it: the worker's name and state,
// never a path or the project.
func notice(r Row) string {
	r.Name = plain.Line(r.Name)
	var s string
	switch r.State {
	case Stopped:
		s = r.Name + " stopped"
	case Silent:
		return fmt.Sprintf("%s silent for %d min", r.Name, int(r.Since/time.Minute))
	default:
		s = r.Name + " gone"
	}
	if r.Handoff != "" {
		s += "; handoff note written"
	}
	return s
}

func (w *watcher) print(s string) { _, _ = io.WriteString(w.d.Out, s) }
