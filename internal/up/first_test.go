package up

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/route"
)

func TestUpFirstRunStartsCoordinator(t *testing.T) {
	var logged []route.Record
	f := &fakeAgterm{}
	d := upDeps(t, f, &logged)
	d.Coordinator = []string{"--permission-mode", "acceptEdits"}
	root := t.TempDir()
	p, o, err := Start(context.Background(), root, nil, d)
	if err != nil {
		t.Fatal(err)
	}
	ws := filepath.Base(root)
	if p.Workspace != ws || p.Coordinator.Name != ws+"-coord" || p.Coordinator.ID == "" || p.Launcher != "agterm" {
		t.Errorf("project = %+v", p)
	}
	if !slices.Equal(p.ClaudeArgs, d.Coordinator) {
		t.Errorf("claude args = %v, want the config's", p.ClaudeArgs)
	}
	saved, ok, err := Load(root)
	if err != nil || !ok || saved.Coordinator.ID != p.Coordinator.ID || saved.Sessions[p.Coordinator.ID].ID != "s-1" {
		t.Fatalf("saved state = %+v, %v, %v", saved, ok, err)
	}
	script := readFile(t, o.Script)
	for _, want := range []string{"QM_PROJECT_ID=" + p.ID, "QM_WORKSPACE=" + ws, " claude --permission-mode acceptEdits -n " + ws + "-coord --session-id " + p.Coordinator.ID + "\n"} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q:\n%s", want, script)
		}
	}
	new := f.calls[0]
	if i := slices.Index(new, "--workspace-name"); i < 0 || new[i+1] != ws {
		t.Errorf("session new = %v, want --workspace-name %s", new, ws)
	}
	if got := f.verbs(); !slices.Equal(got, []string{"session new " + ws + "-coord", "session restore s-1"}) {
		t.Errorf("agtermctl calls = %v, want the session started and pinned", got)
	}
	if r := upRecords(logged)[ws+"-coord"]; r.Action != "fresh" || r.ClaudeSession != p.Coordinator.ID || r.UpProject != p.ID {
		t.Errorf("up Record = %+v", r)
	}
}

func TestUpFirstRunRefusesResumeArg(t *testing.T) {
	var logged []route.Record
	f := &fakeAgterm{}
	d := upDeps(t, f, &logged)
	for _, args := range [][]string{
		{"--resume", "x"}, {"-r", "x"}, {"--continue"}, {"-c"}, {"--session-id=x"},
		{"-n", "x"}, {"--name", "x"}, {"-p"}, {"--print"}, {"--model", "opus", "--resume=x"},
	} {
		if _, _, err := Start(context.Background(), t.TempDir(), args, d); !errors.Is(err, ErrInput) {
			t.Errorf("%v: err %v, want ErrInput", args, err)
		}
	}
	if len(f.calls) != 0 || len(logged) != 0 {
		t.Errorf("refused runs started %v, logged %d", f.calls, len(logged))
	}
	if err := CheckClaudeArgs([]string{"--permission-mode", "acceptEdits", "--model", "opus"}); err != nil {
		t.Errorf("plain arguments refused: %v", err)
	}
}

func TestUpFirstRunUntrusted(t *testing.T) {
	var logged []route.Record
	f := &fakeAgterm{}
	d := upDeps(t, f, &logged)
	d.Trusted = func(string) (bool, error) { return false, nil }
	if _, _, err := Start(context.Background(), t.TempDir(), nil, d); err == nil || !strings.Contains(err.Error(), "not trusted") {
		t.Errorf("err = %v, want not trusted", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("started %v", f.calls)
	}
}

func TestUpFirstRunEmptyArgsMeansNone(t *testing.T) {
	var logged []route.Record
	f := &fakeAgterm{}
	d := upDeps(t, f, &logged)
	d.Coordinator = []string{"--permission-mode", "acceptEdits"}
	p, _, err := Start(context.Background(), t.TempDir(), []string{}, d)
	if err != nil {
		t.Fatal(err)
	}
	if p.ClaudeArgs == nil || len(p.ClaudeArgs) != 0 {
		t.Errorf("claude args = %#v, want explicitly none", p.ClaudeArgs)
	}
}

func TestUpFirstRunNoLauncherSavesTheProject(t *testing.T) {
	var logged []route.Record
	f := &fakeAgterm{}
	d := upDeps(t, f, &logged)
	d.Launcher, d.Lister, d.Closer, d.Pin = nil, nil, nil, nil
	root := t.TempDir()
	p, o, err := Start(context.Background(), root, nil, d)
	if !errors.Is(err, launch.ErrNoLauncher) || o.Script == "" {
		t.Fatalf("err %v, outcome %+v", err, o)
	}
	saved, ok, err := Load(root)
	if err != nil || !ok || saved.Coordinator.ID != p.Coordinator.ID || len(saved.Sessions) != 0 {
		t.Errorf("saved = %+v, %v, %v; want the coordinator's id and no terminal session", saved, ok, err)
	}
	if !strings.Contains(readFile(t, o.Script), "--session-id "+p.Coordinator.ID) || len(logged) != 0 {
		t.Errorf("script or Records wrong: %d Records", len(logged))
	}
}

// lockProbe is a launcher that reads, as it starts a session, whether the
// adoption lock is held.
type lockProbe struct {
	launch.Launcher
	held *bool
}

func (l lockProbe) Spawn(ctx context.Context, s launch.LaunchSpec) (launch.SessionRef, error) {
	unlock, err := AdoptLock()
	if err == nil {
		unlock()
	}
	*l.held = errors.Is(err, ErrAdoptBusy)
	return l.Launcher.Spawn(ctx, s)
}

// A first run starts its coordinator and saves its project under the
// adoption lock: an adoption in another project, which rechecks under it
// what the saved projects hold, never sees the coordinator running before
// it is saved, so never takes it as a worker. The lock is free afterwards.
func TestUpFirstRunStartsUnderTheAdoptionLock(t *testing.T) {
	var logged []route.Record
	d := upDeps(t, &fakeAgterm{}, &logged)
	held := false
	d.Launcher = lockProbe{d.Launcher, &held}
	old := adoptLockWait
	adoptLockWait = 20 * time.Millisecond
	t.Cleanup(func() { adoptLockWait = old })
	if _, _, err := Start(context.Background(), t.TempDir(), nil, d); err != nil {
		t.Fatal(err)
	}
	if !held {
		t.Error("the coordinator started without the adoption lock held")
	}
	unlock, err := AdoptLock()
	if err != nil {
		t.Fatalf("the lock was left held after the run: %v", err)
	}
	unlock()
}

// saveProbe is a launcher that reads, as it starts a session, what the
// project's state file holds.
type saveProbe struct {
	launch.Launcher
	root  string
	saved *Project
	ok    *bool
}

func (l saveProbe) Spawn(ctx context.Context, s launch.LaunchSpec) (launch.SessionRef, error) {
	*l.saved, *l.ok, _ = Load(l.root)
	return l.Launcher.Spawn(ctx, s)
}

// A first run saves the project, with the coordinator's id reserved and no
// terminal session, before it starts the coordinator: a crash or a failed
// save after the start leaves a project that knows the live coordinator's id.
func TestUpFirstRunSavesTheProjectBeforeStartingTheCoordinator(t *testing.T) {
	var logged []route.Record
	d := upDeps(t, &fakeAgterm{}, &logged)
	root := t.TempDir()
	var before Project
	var ok bool
	d.Launcher = saveProbe{d.Launcher, root, &before, &ok}
	p, _, err := Start(context.Background(), root, nil, d)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || before.Coordinator.ID != p.Coordinator.ID || len(before.Sessions) != 0 {
		t.Errorf("state when the coordinator started = %+v, %v; want the id saved with no terminal session", before, ok)
	}
	saved, _, _ := Load(root)
	if saved.Sessions[p.Coordinator.ID].ID != "s-1" {
		t.Errorf("saved after the start = %+v, want the terminal session pinned", saved.Sessions)
	}
}

// A coordinator that fails to start leaves its project saved as never run: no
// terminal session, the id reserved, and the next Gather reads it as a
// session that is not running and has no transcript.
func TestUpFirstRunFailedStartLeavesTheProjectNeverRun(t *testing.T) {
	var logged []route.Record
	f := &fakeAgterm{}
	d := upDeps(t, f, &logged)
	root := t.TempDir()
	f.fail = map[string]bool{filepath.Base(root) + "-coord": true}
	p, _, err := Start(context.Background(), root, nil, d)
	if err == nil {
		t.Fatal("a failed start returned no error")
	}
	saved, ok, err := Load(root)
	if err != nil || !ok || saved.Coordinator.ID != p.Coordinator.ID || len(saved.Sessions) != 0 || len(logged) != 0 {
		t.Fatalf("saved = %+v, %v, %v, %d Records; want the id saved, no session, nothing logged", saved, ok, err, len(logged))
	}
	cs, err := Gather(&saved, nil, Env{Live: map[string]claudecode.LiveSession{}, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].SessionID != p.Coordinator.ID || cs[0].Live || cs[0].Transcript != "" {
		t.Errorf("candidates = %+v, want the never-run coordinator", cs)
	}
}
