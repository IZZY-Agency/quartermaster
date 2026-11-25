package up

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
	"github.com/IZZY-Agency/quartermaster/internal/route"
)

// noRun fails the test when a command is run.
func noRun(t *testing.T) DirRunner {
	return func(_ context.Context, _, bin string, args ...string) ([]byte, error) {
		t.Errorf("ran %s %v", bin, args)
		return nil, errors.New("not here")
	}
}

// noteWorld is a world with one worker, in the trusted folder of the warm
// one, whose transcripts were last written five hours before now and cannot
// be read: a note reads none.
func noteWorld(t *testing.T) (*world, []route.Record, Env) {
	t.Helper()
	w := newWorld(t)
	for _, path := range w.transcripts {
		at := now.Add(-5 * time.Hour)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
	}
	r := rec(w.project.ID, warmID, "w-warm", now.Add(-6*time.Hour))
	r.Cwd, r.Branch = filepath.Join(w.root, "worktrees", "warm"), "feature"
	r.Decision = &route.Decision{Profile: &route.Profile{}}
	env := w.env(t, nil)
	env.Shallow = true
	return w, []route.Record{r}, env
}

func TestLeftBehind(t *testing.T) {
	coordRuns := func(w *world, _ []route.Record, env *Env) {
		env.Live = map[string]claudecode.LiveSession{coordID: {SessionID: coordID}}
	}
	for _, c := range []struct {
		name  string
		set   func(*world, []route.Record, *Env)
		coord bool
		n     int
	}{
		{name: "nothing runs", coord: true, n: 1},
		{name: "a worker marked done", coord: true, set: func(w *world, _ []route.Record, _ *Env) {
			w.project.Done = map[string]time.Time{warmID: now}
		}},
		{name: "a worker whose worktree is gone", coord: true, set: func(_ *world, recs []route.Record, _ *Env) {
			recs[0].Cwd += "-gone"
		}},
		{name: "a worker with no transcript", coord: true, set: func(w *world, _ []route.Record, _ *Env) {
			w.transcripts[warmID] = ""
		}},
		{name: "everything runs", set: func(w *world, _ []route.Record, env *Env) {
			env.Live = map[string]claudecode.LiveSession{coordID: {}, warmID: {}}
		}},
		{name: "an untrusted folder", set: func(_ *world, _ []route.Record, env *Env) {
			env.Trusted = func(string) (bool, error) { return false, nil }
		}},
		{name: "the project folder is gone", set: func(w *world, _ []route.Record, _ *Env) {
			w.project.Root += "-gone"
		}},
		{name: "closed by qm down", set: func(w *world, _ []route.Record, _ *Env) {
			w.project.DownAt = now.Add(-5 * time.Hour)
			w.project.Closed = map[string]time.Time{coordID: w.project.DownAt, warmID: w.project.DownAt}
		}},
		{name: "qm down --keep-open", coord: true, n: 1, set: func(w *world, _ []route.Record, _ *Env) {
			w.project.DownAt = now.Add(-5 * time.Hour)
			w.project.Closed = map[string]time.Time{}
		}},
		{name: "closed by qm down, resumed by hand since", coord: true, n: 1, set: func(w *world, _ []route.Record, _ *Env) {
			w.project.DownAt = now.Add(-5*time.Hour - closedSlack - time.Minute)
			w.project.Closed = map[string]time.Time{coordID: w.project.DownAt, warmID: w.project.DownAt}
		}},
		{name: "qm down of an older qm", set: func(w *world, _ []route.Record, _ *Env) {
			w.project.DownAt = now.Add(-5 * time.Hour)
		}},
		{name: "qm down of an older qm, coordinator started since", coord: true, set: func(w *world, _ []route.Record, _ *Env) {
			w.project.DownAt = now.Add(-5 * time.Hour)
			w.project.Coordinator.Started = now.Add(-4 * time.Hour)
		}},
		{name: "a fresh coordinator that never ran, its past one running", set: func(w *world, recs []route.Record, env *Env) {
			w.project.PastCoordinators = []string{coordID}
			w.project.Coordinator.ID = "never-ran"
			env.Live = map[string]claudecode.LiveSession{coordID: {}, warmID: {}}
		}},
		{name: "a worker with no launch profile", coord: true, set: func(_ *world, recs []route.Record, _ *Env) {
			recs[0].Decision = nil
		}},
		{name: "a fresh coordinator that never ran, closed by qm down", set: func(w *world, _ []route.Record, _ *Env) {
			w.project.PastCoordinators = []string{coordID}
			w.project.Coordinator.ID = "never-ran"
			w.project.DownAt = now.Add(-5 * time.Hour)
			w.project.Closed = map[string]time.Time{"never-ran": w.project.DownAt, warmID: w.project.DownAt}
		}},
		{name: "another project's worker", coord: true, set: func(_ *world, recs []route.Record, _ *Env) {
			recs[0].UpProject = "other-12345678"
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			w, recs, env := noteWorld(t)
			if c.set != nil {
				c.set(w, recs, &env)
			}
			l, ok, err := LeftBehind(w.project, recs, env, noRun(t))
			if err != nil || ok != (c.coord || c.n > 0) || l.Coordinator != c.coord || l.Workers != c.n {
				t.Errorf("left %+v, %v, %v: want coordinator %v, %d workers", l, ok, err, c.coord, c.n)
			}
			if ok && !l.LastAt.Equal(now.Add(-5*time.Hour)) {
				t.Errorf("last active %v, want the transcript's time", l.LastAt)
			}
		})
	}
	// With only workers left, their pull requests decide.
	for _, c := range []struct {
		name, prs string
		ghErr     error
		project   string // the Record's project folder
		want      int
	}{
		{name: "merged", prs: `[{"state":"MERGED","createdAt":"2099-01-01T00:00:00Z"}]`},
		{name: "open", prs: `[{"state":"OPEN","createdAt":"2099-01-01T00:00:00Z"}]`, want: 1},
		{name: "no pull request", prs: `[]`, want: 1},
		{name: "gh failed: not judged", ghErr: errors.New("gh: offline")},
		{name: "gh's answer unreadable: not judged", prs: "{"},
		{name: "no project folder to ask gh in: not judged", project: "/gone/project"},
	} {
		t.Run("coordinator running, worker "+c.name, func(t *testing.T) {
			w, recs, env := noteWorld(t)
			coordRuns(w, recs, &env)
			recs[0].Project = c.project
			l, ok, err := LeftBehind(w.project, recs, env, func(_ context.Context, _, bin string, args ...string) ([]byte, error) {
				switch {
				case bin == "gh":
					return []byte(c.prs), c.ghErr
				case strings.Join(args, " ") == "symbolic-ref --short refs/remotes/origin/HEAD":
					return []byte("origin/main\n"), nil
				}
				return nil, errors.New("not here")
			})
			if err != nil || l.Coordinator || l.Workers != c.want || ok != (c.want > 0) {
				t.Errorf("left %+v, %v, %v: want %d workers", l, ok, err, c.want)
			}
		})
	}
}

// No pull request is asked of a worker that is not counted.
func TestLeftBehindAsksOnlyOfCountedWorkers(t *testing.T) {
	w, recs, env := noteWorld(t)
	closed := rec(w.project.ID, smallID, "w-small", now.Add(-7*time.Hour))
	closed.Cwd, closed.Branch, closed.Decision = filepath.Join(w.root, "worktrees", "small"), "closed-branch", recs[0].Decision
	env.Live = map[string]claudecode.LiveSession{coordID: {}}
	w.project.DownAt = now.Add(-5 * time.Hour)
	w.project.Closed = map[string]time.Time{smallID: w.project.DownAt}
	var asked []string
	l, ok, err := LeftBehind(w.project, append(recs, closed), env, func(_ context.Context, _, bin string, args ...string) ([]byte, error) {
		if bin != "gh" {
			return nil, errors.New("not here")
		}
		asked = append(asked, args[3])
		return []byte("[]"), nil
	})
	if err != nil || !ok || l.Workers != 1 || strings.Join(asked, ",") != "feature" {
		t.Errorf("left %+v, %v, %v; asked of %v, want feature only", l, ok, err, asked)
	}
}

// The state given is the caller's: reading it changes nothing in it.
func TestLeftBehindLeavesTheProjectAlone(t *testing.T) {
	w, recs, env := noteWorld(t)
	recs[0].TermLauncher, recs[0].TermID = "agterm", "t-1"
	w.project.Sessions = nil
	if _, ok, err := LeftBehind(w.project, recs, env, noRun(t)); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if w.project.Sessions != nil {
		t.Errorf("sessions seeded into the caller's project: %v", w.project.Sessions)
	}
}
