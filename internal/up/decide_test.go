package up

import (
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/policy"
	"github.com/IZZY-Agency/quartermaster/internal/transcript"
)

var now = time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

// base is a trusted worker with its transcript, last active two hours ago at
// a context above the worker threshold, no handoff note.
func base(role Role) Candidate {
	return Candidate{
		Role: role, SessionID: "00000000-0000-4000-8000-0000000000a1", Name: "w1",
		Cwd: "/work/wt", Transcript: "/claude/projects/x/a1.jsonl", Trusted: true,
		Last:    transcript.Last{Context: 310000, At: now.Add(-2 * time.Hour)},
		Started: now.Add(-3 * time.Hour),
	}
}

func TestDecideRows(t *testing.T) {
	type tc struct {
		name   string
		c      func(c *Candidate)
		role   Role
		full   bool
		action Action
		why    string
		cold   bool
	}
	w, co := RoleWorker, RoleCoordinator
	cases := []tc{
		{"1 live", func(c *Candidate) { c.Live = true }, w, false, Leave, "live", false},
		{"2 finished", func(c *Candidate) { c.Finished = "merged" }, w, false, Skip, "finished: merged", false},
		{"3 untrusted", func(c *Candidate) { c.Trusted = false }, w, false, Skip, "not trusted: run claude in /work/wt once and accept the dialog", false},
		{"folder unknown", func(c *Candidate) { c.Cwd, c.Transcript, c.Handoff = "", "", "/h/w1.md" }, w, false, Skip, "folder unknown: respawn it by hand", false},
		{"folder unknown, marked done", func(c *Candidate) { c.Cwd, c.Finished = "", "marked done" }, w, false, Skip, "finished: marked done", false},
		{"4 lost, no note", func(c *Candidate) { c.Transcript = "" }, w, false, Skip, "transcript missing", false},
		{"4 lost, note", func(c *Candidate) { c.Transcript, c.Handoff = "", "/h/w1.md" }, w, false, Fresh, "transcript missing, handoff note", false},
		{"4 coordinator lost", func(c *Candidate) { c.Transcript = "" }, co, false, Fresh, "transcript missing: summary", false},
		{"4 coordinator lost, note", func(c *Candidate) { c.Transcript, c.Handoff = "", "/h/c.md" }, co, false, Fresh, "transcript missing: summary, handoff note", false},
		{"5 full, cold", func(c *Candidate) { c.Handoff = "/h/w1.md" }, w, true, Resume, "--full", true},
		{"5 full, warm", func(c *Candidate) { c.Last.At = now.Add(-time.Minute) }, w, true, Resume, "--full", false},
		{"5 full coordinator", nil, co, true, Resume, "--full", true},
		{"6 warm", func(c *Candidate) { c.Last.At = now.Add(-12 * time.Minute) }, w, false, Resume, "cache warm (last activity 12 min ago)", false},
		{"6 warm at 55 min exactly", func(c *Candidate) { c.Last.At = now.Add(-policy.UpWarm) }, w, false, Resume, "cache warm (last activity 55 min ago)", false},
		{"6 cold at 55 min 1 s", func(c *Candidate) { c.Last.At = now.Add(-policy.UpWarm - time.Second); c.Handoff = "/h/w1.md" }, w, false, Fresh, "context 310k, handoff note", false},
		{"7 small", func(c *Candidate) { c.Last.Context = 92400 }, w, false, Resume, "context 92k ≤ 150k", true},
		{"7 worker 150000", func(c *Candidate) { c.Last.Context = 150000; c.Handoff = "/h/w1.md" }, w, false, Resume, "context 150k ≤ 150k", true},
		{"8 worker 150001 with note", func(c *Candidate) { c.Last.Context = 150001; c.Handoff = "/h/w1.md" }, w, false, Fresh, "context 151k, handoff note", false},
		{"10 worker 150001 without note", func(c *Candidate) { c.Last.Context = 150001 }, w, false, Resume, "context 151k, no handoff note", true},
		{"7 coordinator 200000", func(c *Candidate) { c.Last.Context = 200000 }, co, false, Resume, "context 200k ≤ 200k", true},
		{"9 coordinator 200001", func(c *Candidate) { c.Last.Context = 200001 }, co, false, Fresh, "context 201k > 200k: summary", false},
		{"9 coordinator over, note ignored for row 8", func(c *Candidate) { c.Last.Context = 260000; c.Handoff = "/h/c.md" }, co, false, Fresh, "context 260k > 200k: summary", false},
		{"2 not for the coordinator", func(c *Candidate) { c.Finished = "merged"; c.Last.Context = 1000 }, co, false, Resume, "context 1k ≤ 200k", true},
		{"no assistant line yet", func(c *Candidate) { c.Last = transcript.Last{} }, w, false, Resume, "context 0k ≤ 150k", true},
		// --full beats rows 6 to 9 but not rows 1 to 4.
		{"full vs live", func(c *Candidate) { c.Live = true }, w, true, Leave, "live", false},
		{"full vs finished", func(c *Candidate) { c.Finished = "closed" }, w, true, Skip, "finished: closed", false},
		{"full vs untrusted", func(c *Candidate) { c.Trusted = false }, w, true, Skip, "not trusted: run claude in /work/wt once and accept the dialog", false},
		{"full vs lost", func(c *Candidate) { c.Transcript = "" }, w, true, Skip, "transcript missing", false},
		{"full vs coordinator summary", func(c *Candidate) { c.Last.Context = 260000 }, co, true, Resume, "--full", true},
	}
	for _, k := range cases {
		t.Run(k.name, func(t *testing.T) {
			c := base(k.role)
			if k.c != nil {
				k.c(&c)
			}
			got := Decide(c, k.full, now)
			if got.Action != k.action || got.Why != k.why || got.Cold != k.cold {
				t.Errorf("Decide = %s %q cold=%v; want %s %q cold=%v", got.Action, got.Why, got.Cold, k.action, k.why, k.cold)
			}
			if got.SessionID != c.SessionID {
				t.Errorf("Step lost its candidate: %+v", got.Candidate)
			}
		})
	}
}

func TestDecideLiveWins(t *testing.T) {
	c := base(RoleWorker)
	c.Live, c.Finished, c.Trusted, c.Transcript = true, "merged", false, ""
	if got := Decide(c, true, now); got.Action != Leave || got.Why != "live" {
		t.Errorf("live and merged = %s %q, want leave", got.Action, got.Why)
	}
}

func TestDecideHandStartedNeverFresh(t *testing.T) {
	hand := func(f func(c *Candidate)) Candidate {
		c := base(RoleWorker)
		c.Adopted = &AdoptedSession{Name: "w1", Cwd: c.Cwd, Started: c.Started}
		f(&c)
		return c
	}
	cases := []struct {
		name   string
		c      Candidate
		action Action
		why    string
		cold   bool
	}{
		{"live", hand(func(c *Candidate) { c.Live = true }), Leave, "live", false},
		{"marked done", hand(func(c *Candidate) { c.Finished = "marked done" }), Skip, "finished: marked done", false},
		{"untrusted", hand(func(c *Candidate) { c.Trusted = false }), Skip, "not trusted: run claude in /work/wt once and accept the dialog", false},
		{"transcript missing, handoff note", hand(func(c *Candidate) { c.Transcript, c.Handoff = "", "/h/w1.md" }), Skip, "transcript missing", false},
		{"warm", hand(func(c *Candidate) { c.Last.At = now.Add(-10 * time.Minute) }), Resume, "cache warm (last activity 10 min ago)", false},
		{"310k cold", hand(func(*Candidate) {}), Resume, "context 310k, hand-started: resumed (no launch profile to start fresh from)", true},
		{"310k cold, handoff note", hand(func(c *Candidate) { c.Handoff = "/h/w1.md" }), Resume, "context 310k, hand-started: resumed (no launch profile to start fresh from)", true},
	}
	for _, tc := range cases {
		s := Decide(tc.c, false, now)
		if s.Action != tc.action || s.Why != tc.why || s.Cold != tc.cold {
			t.Errorf("%s: %s %q cold %v, want %s %q cold %v", tc.name, s.Action, s.Why, s.Cold, tc.action, tc.why, tc.cold)
		}
	}
}
