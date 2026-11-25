package up

import (
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/route"
)

// Left is a project whose sessions ended with no qm down: what qm up would
// start there. With the coordinator among them, no worker's pull request was
// asked, so Workers is how many at most. LastAt is when the last of them was active (zero: unknown).
type Left struct {
	Project     Project
	Coordinator bool
	Workers     int
	LastAt      time.Time
}

// Ago is how long before now l was last active ("" when unknown).
func (l Left) Ago(now time.Time) string {
	if l.LastAt.IsZero() {
		return ""
	}
	return ago(now.Sub(l.LastAt))
}

// closedSlack is how long after qm down a session it closed may still write
// its transcript. One active later than that was resumed since.
const closedSlack = 5 * time.Minute

// notJudged is Finished's answer for a worker whose pull request could not
// be read.
const notJudged = "not judged: its pull request was not read"

// LeftBehind says what qm up would start in project p although no qm down
// closed it: the sessions of p that do not run, are not finished and that
// qm up does not skip (Gather and Plan, with env, which should be
// Shallow). ok is false when there is none.
//
// env reads no pull request. A project left with workers only, its
// coordinator running or closed by qm down, is the everyday state of one
// whose merged workers ended, so there each worker's pull request is
// asked through run; a worker whose answer cannot be had is left out. A nil
// run asks none and leaves none out.
func LeftBehind(p Project, recs []route.Record, env Env, run DirRunner) (l Left, ok bool, err error) {
	if !isDir(p.Root) {
		return Left{}, false, nil
	}
	env.Finished, env.Run = Finished(p, nil), nil
	l, workers, err := leftBehind(p, recs, env)
	if err != nil || l.Coordinator || l.Workers == 0 || run == nil {
		return l, l.Coordinator || l.Workers > 0, err
	}
	env.Finished, env.Run = judged(p, run, workers), run
	l, _, err = leftBehind(p, recs, env)
	return l, l.Workers > 0, err
}

// leftBehind also returns the session ids of the workers it counts.
func leftBehind(p Project, recs []route.Record, env Env) (Left, map[string]bool, error) {
	// Gather seeds the terminal sessions into p, which is the caller's.
	p.Sessions = effectiveSessions(p, recs)
	cands, err := Gather(&p, recs, env)
	if err != nil {
		return Left{}, nil, err
	}
	l, workers := Left{Project: p}, map[string]bool{}
	for _, c := range Plan(cands, false, env.Now) {
		if c.Action != Resume && c.Action != Fresh || p.closedByDown(c.Candidate) {
			continue
		}
		if c.Role == RoleCoordinator {
			l.Coordinator = true
		} else {
			l.Workers++
			workers[c.SessionID] = true
		}
		if c.Last.At.After(l.LastAt) {
			l.LastAt = c.Last.At
		}
	}
	return l, workers, nil
}

// closedByDown says whether qm down closed session c, and it was not active
// again since. A state saved by an older qm says only when qm down ran: it
// closed the sessions started before.
func (p *Project) closedByDown(c Candidate) bool {
	at, ok := p.Closed[c.SessionID]
	if !ok && c.Instead != "" { // qm down closed the session c stands in for
		at, ok = p.Closed[c.Instead]
	}
	if p.Closed == nil && !p.DownAt.IsZero() {
		at, ok = p.DownAt, !c.Started.After(p.DownAt)
	}
	return ok && !c.Last.At.After(at.Add(closedSlack))
}

// judged is Finished, with notJudged for a worker it is not sure of, and for
// one not in workers, which is not asked about: it is not counted anyway.
func judged(p Project, run DirRunner, workers map[string]bool) func(Candidate) string {
	check := finished(p, run)
	return func(c Candidate) string {
		if !workers[c.SessionID] {
			return notJudged
		}
		why, sure := check(c)
		if why == "" && !sure {
			return notJudged
		}
		return why
	}
}
