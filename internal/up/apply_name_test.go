package up

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
	"github.com/IZZY-Agency/quartermaster/internal/route"
)

func TestUpWorkerWithUnusableNameIsNotStarted(t *testing.T) {
	var logged []route.Record
	f := &fakeAgterm{ws: "repo"}
	d := upDeps(t, f, &logged)
	w := newWorld(t)
	name := "../../escaped"
	recs := []route.Record{profiled(w, warmID, name, "warm", now.Add(-time.Hour))}
	steps := w.plan(t, recs, []claudecode.Proc{{PID: 301, Cmd: "claude -n repo-coord"}})
	outs := Apply(context.Background(), &w.project, steps, d)
	o := outcomeOf(t, outs, name)
	if o.Err == nil || !strings.Contains(o.Err.Error(), "not a usable session name") || o.Script != "" {
		t.Fatalf("outcome = %+v, want a refusal and no script", o)
	}
	if c := outcomeOf(t, outs, "repo-coord"); c.Err != nil {
		t.Errorf("coordinator outcome = %+v, want it untouched by the worker's refusal", c)
	}
	state := os.Getenv("XDG_STATE_HOME")
	if m, _ := filepath.Glob(filepath.Join(state, "escaped*")); len(m) != 0 {
		t.Errorf("files written outside the launch folder: %q", m)
	}
}

func TestRelativeWorkerFolderIsNotTrusted(t *testing.T) {
	w := newWorld(t)
	r := profiled(w, warmID, "w-warm", "warm", now.Add(-time.Hour))
	r.Cwd = "worktrees/warm"
	cs, err := Gather(&w.project, []route.Record{r}, w.env(t, []claudecode.Proc{{PID: 301, Cmd: "claude -n repo-coord"}}))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		if c.SessionID != warmID {
			continue
		}
		if c.Trusted || !strings.Contains(c.Note, "not an absolute path") {
			t.Errorf("candidate trusted %v, note %q, want untrusted with a note", c.Trusted, c.Note)
		}
		return
	}
	t.Fatal("no candidate for the worker")
}
