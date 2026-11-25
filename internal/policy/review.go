package policy

import "time"

// ReviewStateField is the state field that holds the redacted review prompt.
const ReviewStateField = "review_request"

// ReviewQuestions are the questions Jev answers about a review request
// (design 5.4), worded as sent on 2026-11-19.
var ReviewQuestions = map[string]Question{
	"security": {
		Type:         "noul",
		Instructions: "Does the change under review in `review_request` touch authentication, access control, secrets, payments, or what a server exposes to the internet?",
	},
	"data_risk": {
		Type:         "noul",
		Instructions: "Could a bug in the change under review in `review_request` delete or corrupt real data, or break a live production system?",
	},
}

// Review table line thresholds: a diff under SmallDiffUnder lines is small;
// a risky one over RiskLargeDiffOver lines is "risk-large". A review is risky
// when security or data_risk is above NoAt (uncertain counts as risk) or the
// prompt or a changed path holds a risk phrase (RiskIn).
const (
	SmallDiffUnder    = 300
	RiskLargeDiffOver = 1000
)

// ReviewRow is the tiers of one review-table row: the Codex model and
// effort, and the Claude fallback.
type ReviewRow struct {
	CodexModel, CodexEffort string
	Claude                  Tier
}

// ReviewRows is the review table (design 5.4); the conditions are in
// route.DecideReview.
var ReviewRows = map[string]ReviewRow{
	"fix-check":  {"gpt-6.2-sol", "medium", Tier{"sonnet", "medium"}},
	"small":      {"gpt-6.2-sol", "medium", Tier{"sonnet", "high"}},
	"large":      {"gpt-6.2-sol", "high", Tier{"opus", "medium"}},
	"risk":       {"gpt-6.2-sol", "xhigh", Tier{"opus", "high"}},
	"risk-large": {"gpt-6.2-astra", "xhigh", Tier{"opus", "xhigh"}},
}

// ReviewRiskRows are the rows the Claude downshift never touches.
var ReviewRiskRows = []string{"risk", "risk-large"}

// CodexModels are the Codex models a review may use; a Codex effort is at
// most xhigh.
var (
	CodexModels  = []string{"gpt-6.2-sol", "gpt-6.2-astra"}
	CodexEfforts = []string{"low", "medium", "high", "xhigh"}
)

// CodexLimitPercent: Codex is unavailable once the used percentage of any of
// its limit windows reaches it. Paid credits do not count (design 5.4).
const CodexLimitPercent = 95.0

// CodexLimitedFallback is how long a Codex usage-limit error that states no
// reset time keeps Codex off, when the rollout's window reset is unknown.
const CodexLimitedFallback = 5 * time.Hour

// ClaudeLimitMaxAge: a Claude limit file older than this is stale (the
// status-line wrapper is gone), so the Claude limit state is unknown.
const ClaudeLimitMaxAge = time.Hour

// CodexStaleAfter: a Codex usage reading older than this is shown as stale.
// It is the Claude limit file's age limit: the shortest window, 5 hours, can
// move by tens of points in an hour of work.
const CodexStaleAfter = ClaudeLimitMaxAge

// Asking Codex for its usage (codex app-server): an answer stands for
// CodexUsageMaxAge and a failure for CodexUsageRetry, and Codex has
// CodexUsageTimeout to answer, about four times what it was measured to take.
const (
	CodexUsageMaxAge  = 5 * time.Minute
	CodexUsageRetry   = 2 * time.Minute
	CodexUsageTimeout = 4 * time.Second
)
