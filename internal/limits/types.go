// Package limits reads the Codex and Claude usage-limit state. This file holds
// the shared types; the readers come later.
package limits

import "time"

// CodexState says whether Codex can take a review now.
type CodexState struct {
	Available   bool      `json:"available"`
	UsedPercent float64   `json:"used_percent"`
	ResetsAt    time.Time `json:"resets_at"`
	Reason      string    `json:"reason,omitempty"`
	// UsageKnown is true when UsedPercent and ResetsAt come from Codex's own
	// usage data; false means UsedPercent is unknown, not 0.
	UsageKnown bool `json:"usage_known"`
}

// ClaudeState is the latest Claude rate-limit usage; Known is false without
// the status-line wrapper.
type ClaudeState struct {
	FiveHourUsed float64   `json:"five_hour_used"`
	SevenDayUsed float64   `json:"seven_day_used"`
	UpdatedAt    time.Time `json:"updated_at"`
	Known        bool      `json:"known"`
}
