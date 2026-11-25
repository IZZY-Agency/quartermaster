// Package limits reads the Codex and Claude usage-limit state. This file holds
// the shared types.
package limits

import "time"

// CodexState says whether Codex can take a review now.
type CodexState struct {
	Available   bool      `json:"available"`
	UsedPercent float64   `json:"used_percent"`
	ResetsAt    time.Time `json:"resets_at"`
	Reason      string    `json:"reason,omitempty"`
	// UsageKnown is true when UsedPercent and ResetsAt come from Codex's own
	// usage data and their window has not reset since; false means the usage
	// now is unknown, not 0 (UsedPercent is then the last one read, if any).
	UsageKnown bool `json:"usage_known"`
	// Window names the limit window UsedPercent is of, by its length
	// ("5-hour", "weekly"); "" when the reading states no length.
	Window string `json:"window,omitempty"`
	// Source is where the reading is from (SourceSessions, SourceAppServer)
	// and ReadAt when Codex had it; "" and zero without one.
	Source string    `json:"source,omitempty"`
	ReadAt time.Time `json:"read_at,omitzero"`
	// Stale is true when the reading is older than policy.CodexStaleAfter, or
	// every window it has was reset since.
	Stale bool `json:"stale,omitempty"`
	// Limited is true when a recorded usage-limit error keeps Codex off;
	// ResetsAt is then when that ends, whatever the window says.
	Limited bool `json:"limited,omitempty"`
	// ReadFailure is why Codex itself could not be asked, in qm's words, when
	// the reading is not a fresh one for that reason.
	ReadFailure string `json:"read_failure,omitempty"`
	// AccountNote says, naming no account, that the login cannot be told or
	// that the last figure was of another one.
	AccountNote string `json:"account_note,omitempty"`
}

// Where a Codex reading is from, CodexState.Source.
const (
	SourceSessions  = "session files"
	SourceAppServer = "codex app-server"
)

// ClaudeState is the latest Claude rate-limit usage; Known is false without
// the status-line wrapper.
type ClaudeState struct {
	FiveHourUsed float64   `json:"five_hour_used"`
	SevenDayUsed float64   `json:"seven_day_used"`
	UpdatedAt    time.Time `json:"updated_at"`
	Known        bool      `json:"known"`
}
