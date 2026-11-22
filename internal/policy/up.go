package policy

import "time"

// Thresholds of qm up's restore decision (qm up design, section 6).
const (
	// UpWarm is how long after a session's last main-thread assistant
	// message its prompt cache still counts as warm. The cache lives 60
	// minutes from its last read; 5 minutes cover the plan, the
	// confirmation and the launch.
	UpWarm = 55 * time.Minute
	// UpWorkerSmall is the largest context, in tokens, a cold worker is
	// resumed with in full rather than started fresh from its handoff note.
	UpWorkerSmall = 150000
	// UpCoordinatorSmall is the largest context, in tokens, a cold
	// coordinator is resumed with rather than started fresh from a summary.
	// It is higher than a worker's: a coordinator's conversation carries the
	// owner's rulings, which no record holds.
	UpCoordinatorSmall = 200000
)
