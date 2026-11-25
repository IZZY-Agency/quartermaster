package up

import (
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/plain"
	"github.com/IZZY-Agency/quartermaster/internal/route"
)

// Plan decides every candidate (Decide), then turns a worker start that has
// no launch profile (a Record of an older qm) into a skip naming the manual
// resume line (spec 8); a hand-started worker needs none (adopt design 4.3).
// A candidate's Note follows its reason.
func Plan(cands []Candidate, full bool, now time.Time) []Step {
	steps := make([]Step, 0, len(cands))
	for _, c := range cands {
		s := Decide(c, full, now)
		if s.Role == RoleWorker && (s.Action == Resume || s.Action == Fresh) && !hasProfile(c.Record) && c.Adopted == nil {
			s.Action, s.Cold = Skip, false
			if c.Transcript == "" {
				s.Why = "no launch profile (logged by an older qm) and no transcript: respawn it by hand"
			} else {
				s.Why = fmt.Sprintf("no launch profile (logged by an older qm): resume it by hand: cd %s && claude --resume %s", launch.Quote(c.Cwd), c.SessionID)
			}
		}
		if c.Note != "" {
			s.Why += " (" + c.Note + ")"
		}
		steps = append(steps, s)
	}
	return steps
}

func hasProfile(r *route.Record) bool {
	return r != nil && r.Decision != nil && r.Decision.Profile != nil
}

// Starts counts the steps that start a session (resume or fresh).
func Starts(steps []Step) int {
	n := 0
	for _, s := range steps {
		if s.Action == Resume || s.Action == Fresh {
			n++
		}
	}
	return n
}

// Render is the plan of spec 6: one row per session (role, name, session id
// prefix, branch, last activity, context, action, reason), then the totals.
func Render(steps []Step, now time.Time) string {
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ROLE\tNAME\tSESSION\tBRANCH\tLAST\tCONTEXT\tACTION\tREASON")
	var warm, cold, fresh, leave, skip, coldTokens int
	for _, s := range steps {
		last, ctxk := "-", "-"
		if !s.Last.At.IsZero() {
			last, ctxk = ago(now.Sub(s.Last.At)), kTokens(s.Last.Context)
		}
		action := string(s.Action)
		switch s.Action {
		case Resume:
			if s.Cold {
				action = "resume (cold)"
				cold++
				coldTokens += s.Last.Context
			} else {
				action = "resume (warm)"
				warm++
			}
		case Fresh:
			fresh++
		case Leave:
			leave++
		case Skip:
			skip++
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.Role, oneLine(s.Name), prefix(s.SessionID), orDash(oneLine(s.Branch)), last, ctxk, action, oneLine(s.Why))
	}
	_ = tw.Flush()
	fmt.Fprintf(&b, "resume warm %d, resume cold %d (%s tokens written to cache), start fresh %d, leave %d, skip %d\n", warm, cold, kTokensUp(coldTokens), fresh, leave, skip)
	return b.String()
}

func prefix(id string) string {
	id = plain.Line(id)
	if len(id) > 8 {
		return id[:8]
	}
	return orDash(id)
}

// ago is d as "N min ago", "N h ago" or "N d ago".
func ago(d time.Duration) string {
	switch {
	case d < 0:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d/time.Hour))
	}
	return fmt.Sprintf("%d d ago", int(d/(24*time.Hour)))
}
