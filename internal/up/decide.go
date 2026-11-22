package up

import (
	"fmt"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/policy"
)

// Action is what qm up does with a session.
type Action string

// The actions of the restore decision (spec 6).
const (
	// Leave: the session is live; nothing is done.
	Leave Action = "leave"
	// Skip: the session is not started (finished, untrusted, lost).
	Skip Action = "skip"
	// Resume: claude --resume with the session's id.
	Resume Action = "resume"
	// Fresh: a new session, from the worker's handoff note or, for the
	// coordinator, from the summary.
	Fresh Action = "fresh"
)

// Prefixes of the Why text of a Skip, shared with the summary: the finished
// reason follows WhyFinished, WhyNotTrusted starts the untrusted-folder row.
const (
	WhyFinished   = "finished: "
	WhyNotTrusted = "not trusted"
)

// Step is the decision for one candidate. Why names the row that decided.
// Cold is set on a resume whose prompt cache is no longer warm: its whole
// context is written to the cache again.
type Step struct {
	Candidate
	Action Action
	Why    string
	Cold   bool
}

// Decide applies the restore table of spec 6 to c: the first matching row
// wins. A hand-started worker (c.Adopted set) is never started fresh: with no
// transcript it is skipped, past rows 5 to 7 it is resumed. full is qm up
// --full (resume rather than start fresh); now is the time the decision is
// made.
func Decide(c Candidate, full bool, now time.Time) Step {
	coord := c.Role == RoleCoordinator
	limit := policy.UpWorkerSmall
	if coord {
		limit = policy.UpCoordinatorSmall
	}
	warm := !c.Last.At.IsZero() && now.Sub(c.Last.At) <= policy.UpWarm
	step := func(a Action, why string) Step {
		return Step{Candidate: c, Action: a, Why: why, Cold: a == Resume && !warm}
	}
	switch {
	case c.Live: // 1
		return step(Leave, "live")
	case !coord && c.Finished != "": // 2
		return step(Skip, WhyFinished+c.Finished)
	case c.Cwd == "": // no folder to start in or to check the trust of
		return step(Skip, "folder unknown: respawn it by hand")
	case !c.Trusted: // 3
		return step(Skip, fmt.Sprintf("%s: run claude in %s once and accept the dialog", WhyNotTrusted, c.Cwd))
	case c.Transcript == "": // 4
		switch {
		case c.Adopted != nil: // hand-started: no profile to start fresh from
			return step(Skip, "transcript missing")
		case coord && c.Handoff != "":
			return step(Fresh, "transcript missing: summary, handoff note")
		case coord:
			return step(Fresh, "transcript missing: summary")
		case c.Handoff != "":
			return step(Fresh, "transcript missing, handoff note")
		}
		return step(Skip, "transcript missing")
	case full: // 5
		return step(Resume, "--full")
	case warm: // 6
		return step(Resume, fmt.Sprintf("cache warm (last activity %d min ago)", int(now.Sub(c.Last.At)/time.Minute)))
	case c.Last.Context <= limit: // 7
		return step(Resume, fmt.Sprintf("context %s ≤ %s", kTokens(c.Last.Context), kTokens(limit)))
	case c.Adopted != nil: // hand-started: never started fresh (adopt design 4.3)
		return step(Resume, fmt.Sprintf("context %s, hand-started: resumed (no launch profile to start fresh from)", kTokensUp(c.Last.Context)))
	case !coord && c.Handoff != "": // 8
		return step(Fresh, fmt.Sprintf("context %s, handoff note", kTokensUp(c.Last.Context)))
	case coord: // 9
		return step(Fresh, fmt.Sprintf("context %s > %s: summary", kTokensUp(c.Last.Context), kTokens(limit)))
	}
	return step(Resume, fmt.Sprintf("context %s, no handoff note", kTokensUp(c.Last.Context))) // 10
}

// kTokens is n in thousands, rounded down: 92400 is "92k". It is used at or
// under a threshold, so the text never reads over it.
func kTokens(n int) string {
	return fmt.Sprintf("%dk", n/1000)
}

// kTokensUp is n in thousands, rounded up: 200001 is "201k". It is used over
// a threshold, so the text never reads at it.
func kTokensUp(n int) string {
	return fmt.Sprintf("%dk", (n+999)/1000)
}
