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
	"github.com/IZZY-Agency/quartermaster/internal/launch"
)

// A coordinator whose session file cannot be read may run stricter than the
// saved flags: no worker starts, and the error names the file. Once its
// process ended, the saved flags are the cap again.
func TestUpWorkersNotStartedWhenCoordinatorFileUnread(t *testing.T) {
	w, steps, d, _ := capWorld(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions", "301.json")
	if err := os.MkdirAll(path, 0o700); err != nil { // a folder: no file to read
		t.Fatal(err)
	}
	procs := []claudecode.Proc{{PID: 301, Cmd: "claude -n repo-coord --permission-mode plan"}}
	d.CoordinatorArgs = func(id string) ([]string, bool, error) {
		live, err := claudecode.LiveComplete(context.Background(), dir, procs)
		if err != nil {
			return nil, false, err
		}
		return LiveArgs(id, live, procs)
	}
	outs := Apply(context.Background(), &w.project, steps, d)
	for _, name := range []string{"w-warm", "w-hand"} {
		o := outcomeOf(t, outs, name)
		if o.Err == nil || !strings.Contains(o.Err.Error(), "no worker is started") || !strings.Contains(o.Err.Error(), path) || o.Script != "" {
			t.Errorf("%s outcome = %+v, want a refusal naming %s", name, o, path)
		}
	}

	w, steps, d2, _ := capWorld(t)
	d2.CoordinatorArgs, procs = d.CoordinatorArgs, nil
	outs = Apply(context.Background(), &w.project, steps, d2)
	for _, name := range []string{"w-warm", "w-hand"} {
		if o := outcomeOf(t, outs, name); o.Err != nil {
			t.Errorf("%s outcome = %+v, want it started: the process ended", name, o)
		}
	}
}

// The wait for claude in a reused tab uses the sessions that were read when
// not all of them are known.
func TestWaitStartedWithIncompleteSessions(t *testing.T) {
	d := Deps{
		Live: func() (map[string]claudecode.LiveSession, error) {
			return map[string]claudecode.LiveSession{"x": {PID: 1}}, &claudecode.IncompleteError{}
		},
		Sleep: func(time.Duration) {},
	}
	o := []Outcome{
		{Ref: launch.SessionRef{ID: "s-x"}, waitFor: "x"},
		{Ref: launch.SessionRef{ID: "s-y"}, waitFor: "y"},
	}
	waitStarted(context.Background(), d, o)
	if o[0].Ref.Warning != "" || !strings.Contains(o[1].Ref.Warning, "did not start") {
		t.Errorf("warnings %q and %q, want only the second", o[0].Ref.Warning, o[1].Ref.Warning)
	}
	d.Live = func() (map[string]claudecode.LiveSession, error) { return nil, errors.New("ps failed") }
	waitStarted(context.Background(), d, o[:1])
	if !strings.Contains(o[0].Ref.Warning, "did not start") {
		t.Errorf("no read at all: warning %q", o[0].Ref.Warning)
	}
}

// Incomplete data reserves nothing: a fresh start's session is saved only
// after the running sessions were all read.
func TestUpFreshStartNotReservedOnIncompleteSessions(t *testing.T) {
	w, steps, d, _ := capWorld(t)
	for i := range steps {
		if steps[i].Name == "w-warm" {
			steps[i].Action = Fresh
		}
	}
	d.Live = func() (map[string]claudecode.LiveSession, error) { return nil, &claudecode.IncompleteError{} }
	outs := Apply(context.Background(), &w.project, steps, d)
	if o := outcomeOf(t, outs, "w-warm"); o.Err == nil || o.NewSessionID != "" || o.Script != "" {
		t.Errorf("outcome %+v, want nothing started", o)
	}
	if len(w.project.Starting) != 0 {
		t.Errorf("reserved %v", w.project.Starting)
	}
}

// A coordinator run by the path of its version is live, and its flags are
// read: the workers start, capped at them.
func TestUpWorkersCappedAtVersionedCoordinator(t *testing.T) {
	w, steps, d, _ := capWorld(t)
	procs := []claudecode.Proc{{PID: 301, Cmd: "/opt/share/claude/versions/2.1.0 --resume " + coordID + " --permission-mode plan"}}
	d.CoordinatorArgs = func(id string) ([]string, bool, error) {
		live, err := claudecode.LiveComplete(context.Background(), t.TempDir(), procs)
		if err != nil {
			return nil, false, err
		}
		return LiveArgs(id, live, procs)
	}
	outs := Apply(context.Background(), &w.project, steps, d)
	for _, name := range []string{"w-warm", "w-hand"} {
		o := outcomeOf(t, outs, name)
		if o.Err != nil {
			t.Fatalf("%s outcome = %+v", name, o)
		}
		if script := readFile(t, o.Script); !strings.Contains(script, " claude --permission-mode plan ") {
			t.Errorf("%s not capped at the coordinator's plan mode:\n%s", name, script)
		}
	}
}
