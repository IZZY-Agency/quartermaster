package limits

import (
	"fmt"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/policy"
)

// codexBucket is the metered bucket Codex's own usage counts in.
const codexBucket = "codex"

// weekMinutes is the longest limit window Codex has.
const weekMinutes = 7 * 24 * 60

// maxWindowMinutes is the longest length that counts as stated, a year.
const maxWindowMinutes = 366 * 24 * 60

// limitWindow is one limit window of a Codex reading. Minutes is its length
// (0: not stated) and ResetsAt when it resets (zero: not stated).
type limitWindow struct {
	UsedPercent float64   `json:"used_percent"`
	Minutes     int64     `json:"window_minutes,omitempty"`
	ResetsAt    time.Time `json:"resets_at,omitzero"`
}

// maxReset is the last day of the year 9999, in Unix seconds.
const maxReset = 253402300800 - 24*60*60

// newWindow is a window from the numbers Codex states. A length outside one
// minute to a year, or a reset outside the years a JSON time can hold in any
// time zone, counts as not stated.
func newWindow(used float64, minutes, reset *int64) limitWindow {
	w := limitWindow{UsedPercent: used}
	if minutes != nil && *minutes > 0 && *minutes <= maxWindowMinutes {
		w.Minutes = *minutes
	}
	if reset != nil && *reset > 0 && *reset < maxReset {
		w.ResetsAt = time.Unix(*reset, 0)
	}
	return w
}

// name names the window by its length, never by its position in the reading:
// Codex states the weekly window first when there is no 5-hour one.
func (w limitWindow) name() string {
	switch {
	case w.Minutes == 0:
		return ""
	case w.Minutes == weekMinutes:
		return "weekly"
	case w.Minutes == 24*60:
		return "daily"
	case w.Minutes%(24*60) == 0:
		return fmt.Sprintf("%d-day", w.Minutes/(24*60))
	case w.Minutes%60 == 0:
		return fmt.Sprintf("%d-hour", w.Minutes/60)
	}
	return fmt.Sprintf("%d-minute", w.Minutes)
}

// reading is what Codex said about its usage at At (zero: not known when).
// Allowed is whether Codex may be used within the plan (nil: not stated,
// which is never a yes). Unowned is true for a session file that names no
// account.
type reading struct {
	At      time.Time
	Source  string
	Windows []limitWindow
	Allowed *bool
	Unowned bool
}

// over says whether w has reset by now. A window that states no reset is over
// once its length (a week when it states none either) has passed since the
// reading.
func (r *reading) over(w limitWindow, now time.Time) bool {
	end := w.ResetsAt
	if end.IsZero() {
		if r.At.IsZero() {
			return false
		}
		minutes := w.Minutes
		if minutes == 0 {
			minutes = weekMinutes
		}
		end = r.At.Add(time.Duration(minutes) * time.Minute)
	}
	return !end.After(now)
}

// top is the window that decides: the highest used of those that have not
// reset by now (of two equal ones, the one that resets last). When all have,
// live is false and top is the highest used of them.
func (r *reading) top(now time.Time) (top limitWindow, live bool) {
	higher := func(w limitWindow) bool {
		return w.UsedPercent > top.UsedPercent || (w.UsedPercent == top.UsedPercent && w.ResetsAt.After(top.ResetsAt))
	}
	for i, w := range r.Windows {
		lives := !r.over(w, now)
		if i == 0 || (lives && !live) || (lives == live && higher(w)) {
			top, live = w, lives
		}
	}
	return top, live
}

// state is the reading as a CodexState, with nothing decided yet, and whether
// its deciding window is at the limit. A nil reading is an empty state.
func (r *reading) state(now time.Time) (st CodexState, full bool) {
	if r == nil {
		return CodexState{}, false
	}
	top, live := r.top(now)
	age := now.Sub(r.At)
	return CodexState{
		UsedPercent: top.UsedPercent,
		ResetsAt:    top.ResetsAt,
		UsageKnown:  live,
		Window:      top.name(),
		Source:      r.Source,
		ReadAt:      r.At,
		Stale:       (!live && len(r.Windows) > 0) || r.At.IsZero() || age < 0 || age > policy.CodexStaleAfter,
	}, live && top.UsedPercent >= policy.CodexLimitPercent
}
