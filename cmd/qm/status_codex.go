package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/limits"
)

// codexLine is the Codex line of qm status: whether it is on, the usage and
// which window it is of, when that resets, where the figure is from and how
// old it is, and why there is no newer one. Only a figure known for now is
// shown as the usage; another is the last one read. Last comes whether the
// figure may be, or was, of another account than the login's.
func codexLine(st limits.CodexState, now time.Time) string {
	figure := fmt.Sprintf("%g%%", st.UsedPercent)
	if st.Window != "" {
		figure += " of the " + st.Window + " limit"
	}
	from := st.Source + ", at an unknown time"
	if !st.ReadAt.IsZero() {
		from = st.Source + ", " + ago(now.Sub(st.ReadAt))
	}
	if st.Stale {
		from += ", stale"
	}
	var parts []string
	if !st.Available {
		parts = append(parts, st.Reason)
	}
	switch {
	case st.UsageKnown:
		used := figure + " used"
		// Limited, ResetsAt is the usage-limit error's, which Reason states.
		if !st.ResetsAt.IsZero() && !st.Limited {
			used += ", resets " + st.ResetsAt.Local().Format(time.DateTime)
		}
		parts = append(parts, used+" ("+from+")")
	case st.Available:
		usage := "usage unknown"
		if st.Reason != "" {
			usage += " (" + st.Reason
			if st.Window != "" || st.UsedPercent > 0 {
				usage += ": " + figure
			}
			if st.Source != "" {
				usage += ", " + from
			}
			usage += ")"
		}
		parts = append(parts, usage)
	}
	if st.ReadFailure != "" {
		parts = append(parts, "no newer figure: "+st.ReadFailure)
	}
	if st.AccountNote != "" {
		parts = append(parts, st.AccountNote)
	}
	state := "available, "
	if !st.Available {
		state = "off: "
	}
	return state + strings.Join(parts, "; ")
}
