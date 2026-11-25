package watch

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/plain"
)

// TintMark is what watches know of the tint of a worker's tab: the state it
// was last given for (Working for the role's tint), the activity that state
// was read with, and when it was read (Unix nanoseconds).
type TintMark struct {
	State    State `json:"state"`
	Activity int64 `json:"activity,omitempty"`
	At       int64 `json:"at"`
}

// tintState is the state r's tab shows: a waiting worker keeps the role's
// tint, as a working one does. ok is false for a worker whose state is
// unknown, and for one with no tab.
func tintState(r Row) (st State, ok bool) {
	if r.Tab.ID == "" {
		return "", false
	}
	switch r.State {
	case Working, Waiting:
		return Working, true
	case Stopped, Silent, Gone:
		return r.State, true
	}
	return "", false
}

// outdated says whether r, read at observed, must not change the tint got
// records, so that watches running at once do not fight: a reading older than
// the one the tint was given for, and, for a worker shown as silent, one that
// saw no newer activity (a watch with a longer silence threshold reads it as
// working).
func (got TintMark) outdated(r Row, observed time.Time) bool {
	if got == (TintMark{}) {
		return false
	}
	if observed.UnixNano() < got.At {
		return true
	}
	st, _ := tintState(r)
	return got.State == Silent && st == Working && r.Activity.UnixNano() <= got.Activity
}

const (
	// tintTimeout bounds the tints of one check, the wait for their lock
	// included: none begins after it.
	tintTimeout = 3 * time.Second
	// tintRetry is how long a watch leaves a tab alone after a tint for a state
	// failed.
	tintRetry = time.Minute
)

// ErrNotNow: the worker's tab must not be tinted now, which says nothing about
// the next reading.
var ErrNotNow = errors.New("tab not to be tinted now")

// tinter is what one watch remembers of the tabs it tinted, by session id and
// tab.
type tinter struct {
	shown  map[string]State
	failed map[string]failedTint
	noted  bool
}

// failedTint: the tint for state failed, or was refused (ErrNotNow), and is not
// tried again before until.
type failedTint struct {
	state State
	until time.Time
}

// tint gives the tabs of rows, read at observed, the tints of their states:
// only those whose state is not the one this watch last gave the tab, or not
// the one the tints' store records. The store's lock is held meanwhile, so one
// watch at a time tints. A failure is noted once per watch, a refusal
// (ErrNotNow) is not; the tab is tried again after tintRetry, or at once when
// its state is another. A check begun is finished after an interrupt or the
// watch's timeout, within tintTimeout; the tabs it did not come to are due at
// the next check.
func (w *watcher) tint(ctx context.Context, rows []Row, observed time.Time) {
	if w.d.Tint == nil || w.d.Tints == nil || ctx.Err() != nil {
		return
	}
	t := &w.tints
	if t.shown == nil {
		t.shown, t.failed = map[string]State{}, map[string]failedTint{}
	}
	key := func(r Row) string { return r.SessionID + " " + r.Tab.ID }
	candidate := func(r Row) bool {
		st, ok := tintState(r)
		f := t.failed[key(r)]
		return ok && (f.state != st || !w.d.Now().Before(f.until))
	}
	if !slices.ContainsFunc(rows, candidate) {
		return
	}
	var failed error
	hold := func(r Row) {
		st, _ := tintState(r)
		t.failed[key(r)] = failedTint{st, w.d.Now().Add(tintRetry)}
	}
	// A store that cannot be read records nothing: Update says why, if any
	// tint is due.
	marks, _ := w.d.Tints.Load()
	due := func(m Marks, r Row) bool {
		st, ok := tintState(r)
		got := m[r.SessionID].Tint
		return ok && (t.shown[key(r)] != st || got.State != st) && !got.outdated(r, observed)
	}
	var todo []Row
	for _, r := range rows {
		if candidate(r) && due(marks, r) {
			todo = append(todo, r)
		}
	}
	if len(todo) == 0 {
		return
	}
	tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tintTimeout)
	defer cancel()
	err := w.d.Tints.Update(tctx, func(m Marks) bool {
		changed := false
		for _, r := range todo {
			if tctx.Err() != nil {
				break
			}
			if !due(m, r) {
				continue
			}
			if err := w.d.Tint(tctx, r, observed); err != nil {
				hold(r)
				if failed == nil && !errors.Is(err, ErrNotNow) {
					failed = err
				}
				continue
			}
			st, _ := tintState(r)
			t.shown[key(r)] = st
			m[r.SessionID] = Mark{Tint: TintMark{State: st, Activity: r.Activity.UnixNano(), At: observed.UnixNano()}}
			changed = true
		}
		return changed
	})
	if err != nil {
		if tctx.Err() == nil { // the store fails, not a lock another watch holds
			for _, r := range todo {
				hold(r)
			}
		}
		if failed == nil {
			failed = err
		}
	}
	if failed != nil && !t.noted {
		t.noted = true
		_, _ = fmt.Fprintf(w.d.Err, "qm watch: note: worker tabs not tinted: %s\n", plain.Text(failed.Error()))
	}
}
