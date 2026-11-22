package up

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
	"github.com/IZZY-Agency/quartermaster/internal/route"
)

func TestRenderPlan(t *testing.T) {
	steps := []Step{
		{Candidate: Candidate{Role: RoleCoordinator, SessionID: coordID, Name: "repo-coord", Branch: "main", Last: lastAt(now.Add(-2*time.Hour), 120000)}, Action: Resume, Why: "context 120k ≤ 200k", Cold: true},
		{Candidate: Candidate{Role: RoleWorker, SessionID: warmID, Name: "w-warm", Branch: "feat-a", Last: lastAt(now.Add(-10*time.Minute), 63000)}, Action: Resume, Why: "cache warm (last activity 10 min ago)"},
		{Candidate: Candidate{Role: RoleWorker, SessionID: largeID, Name: "w-large", Last: lastAt(now.Add(-3*time.Hour), 310000)}, Action: Fresh, Why: "context 310k, handoff note"},
		{Candidate: Candidate{Role: RoleWorker, SessionID: lostID, Name: "w-lost"}, Action: Skip, Why: "transcript missing"},
	}
	out := Render(steps, now)
	for _, want := range []string{"ROLE", "coordinator", "repo-coord", "00000000", "feat-a", "2 h ago", "10 min ago", "120k", "resume (cold)", "resume (warm)", "fresh", "transcript missing",
		"resume warm 1, resume cold 1 (120k tokens written to cache), start fresh 1, leave 0, skip 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan lacks %q:\n%s", want, out)
		}
	}
}

func TestUpRecordWithoutProfileListedNotStarted(t *testing.T) {
	var logged []route.Record
	f := &fakeAgterm{ws: "repo"}
	d := upDeps(t, f, &logged)
	w := newWorld(t)
	old := rec(w.project.ID, warmID, "w-old", now.Add(-time.Hour))
	old.Cwd = filepath.Join(w.root, "worktrees", "warm")
	steps := w.plan(t, []route.Record{old}, []claudecode.Proc{{PID: 301, Cmd: "claude -n repo-coord"}})
	s := steps[1]
	if s.Action != Skip || !strings.Contains(s.Why, "claude --resume "+warmID) || !strings.Contains(s.Why, old.Cwd) {
		t.Fatalf("step = %+v, want a skip with the manual line", s)
	}
	Apply(context.Background(), &w.project, steps, d)
	for _, v := range f.verbs() {
		if strings.HasPrefix(v, "session new") {
			t.Errorf("started %v", f.verbs())
		}
	}
}

func TestPlanKeepsHandStarted(t *testing.T) {
	c := base(RoleWorker)
	c.Adopted = &AdoptedSession{Name: "w1", Cwd: c.Cwd, Started: c.Started}
	steps := Plan([]Candidate{c}, false, now)
	if steps[0].Action != Resume || !steps[0].Cold {
		t.Errorf("step = %s %q, want a cold resume", steps[0].Action, steps[0].Why)
	}
}
