//go:build tmuxlive

// qm down and qm up's cleanup against a real tmux, on a private socket (see
// internal/launch/tmux_live_test.go).
package up

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
	"github.com/IZZY-Agency/quartermaster/internal/launch"
)

// liveTerms is qm's terminals over a tmux server of its own. A session's
// process is the one tmux started in the window it was spawned in.
type liveTerms struct {
	launch.Tmux
	t   *testing.T
	pid map[string]int // by session id
}

func (l *liveTerms) Sessions(ctx context.Context, _, ws string) ([]launch.SessionRef, error) {
	return l.Tmux.Sessions(ctx, ws)
}

// PaneRuns is as qm down's own (cmd/qm): a session that does not run is not
// run by the pane.
func (l *liveTerms) PaneRuns(ctx context.Context, ref launch.SessionRef, id string) (bool, error) {
	if syscall.Kill(l.pid[id], 0) != nil {
		return false, nil
	}
	pane, err := l.PaneTTY(ctx, "="+ref.Workspace+":"+ref.ID)
	if err != nil {
		return false, err
	}
	tty, fg, err := claudecode.ForegroundOf(ctx, l.pid[id])
	return fg && tty != "" && tty == pane, err
}

func newLiveTerms(t *testing.T) *liveTerms {
	t.Helper()
	dir, err := os.MkdirTemp("", "qm") // a socket path must stay short
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("SHELL", "/bin/bash")
	t.Setenv("LC_ALL", "C.UTF-8")
	l := &liveTerms{t: t, pid: map[string]int{}, Tmux: launch.Tmux{Bin: "tmux", Run: func(ctx context.Context, bin string, args ...string) ([]byte, error) {
		if bin == "tmux" {
			args = append([]string{"-f", "/dev/null", "-S", filepath.Join(dir, "s")}, args...)
		}
		return launch.Exec(ctx, bin, args...)
	}}}
	t.Cleanup(func() {
		_, _ = l.Run(context.Background(), "tmux", "kill-server")
		_ = os.RemoveAll(dir)
	})
	return l
}

func (l *liveTerms) do(args ...string) string {
	l.t.Helper()
	out, err := l.Run(context.Background(), "tmux", args...)
	if err != nil {
		l.t.Fatalf("tmux %q: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// session spawns session id in workspace ws and records it in p.
func (l *liveTerms) session(p *Project, ws, id, body string) launch.SessionRef {
	l.t.Helper()
	script := filepath.Join(l.t.TempDir(), id+".sh")
	if err := os.WriteFile(script, []byte(body+"\n"), 0o600); err != nil {
		l.t.Fatal(err)
	}
	ref, err := l.Spawn(context.Background(), launch.LaunchSpec{Name: id, Cwd: "/tmp", Script: script, WorkspaceName: ws})
	if err != nil {
		l.t.Fatal(err)
	}
	if l.pid[id], err = strconv.Atoi(l.do("display-message", "-p", "-t", ref.ID, "#{pane_pid}")); err != nil {
		l.t.Fatal(err)
	}
	if p.Sessions == nil {
		p.Sessions = map[string]launch.SessionRef{}
	}
	p.Sessions[id] = ref
	return ref
}

func (l *liveTerms) exists(id string) bool {
	l.t.Helper()
	return slices.Contains(strings.Fields(l.do("list-windows", "-a", "-F", "#{window_id}")), id)
}

func (l *liveTerms) eventually(what string, ok func() bool) {
	l.t.Helper()
	for range 50 {
		if ok() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	l.t.Fatalf("%s: never so", what)
}

// fg waits until the window's foreground command is cmd.
func (l *liveTerms) fg(id, cmd string) {
	l.t.Helper()
	l.eventually(id+" runs "+cmd, func() bool {
		return l.do("display-message", "-p", "-t", id, "#{pane_current_command}") == cmd
	})
}

// Down ends the project's windows and leaves every window that is not, or no
// longer, its own alone.
func TestLiveDown(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ctx := context.Background()
	const ws = "Shop v2 · next"
	l := newLiveTerms(t)
	l.do("new-session", "-d", "-s", "mine", "-n", "mine")
	p := &Project{ID: "proj-277ae841", Root: "/work/proj", Workspace: ws, Launcher: "tmux"}
	var steps []Step
	add := func(id string, a Action, body string) string {
		ref := l.session(p, ws, id, body)
		role := RoleWorker
		if id == "coord" {
			role = RoleCoordinator
		}
		steps = append(steps, Step{Candidate: Candidate{Role: role, SessionID: id, Name: id, Cwd: "/tmp"}, Action: a})
		return ref.ID
	}
	const runs = "exec sleep 600"
	want := map[string]string{} // session id: the warning it is left open with, "" when closed
	win := map[string]string{}
	run := func(id, body, warning string) string {
		win[id], want[id] = add(id, Leave, body), warning
		return win[id]
	}
	run("coord", runs, "")
	run("runs", runs, "")
	// Its process is a child of the launch script's shell.
	run("child", "sleep 601; exit 0", "")
	l.do("move-window", "-d", "-s", run("moved", runs, "left open: it is now in tmux session mine"), "-t", "=mine:")
	l.do("link-window", "-d", "-s", run("linked", runs, "left open: "+launch.LinkedWhy), "-t", "=mine:")
	l.do("rename-window", "-t", run("renamed", runs, `left open: that window is named "notes" now, not "renamed"`), "notes")
	l.do("split-window", "-d", "-t", run("split", runs, LeftOpen))
	// Another command in the same window.
	l.do("respawn-window", "-k", "-t", run("respawned", runs, LeftOpen), "exec sleep 602")
	// The session suspended, a shell's other job in the foreground.
	job := l.do("new-window", "-d", "-P", "-F", "#{window_id}", "-t", "="+ws+":", "-n", "job")
	l.do("send-keys", "-t", job, "sleep 603", "Enter")
	l.fg(job, "sleep")
	l.do("send-keys", "-t", job, "C-z")
	l.fg(job, "bash")
	l.do("send-keys", "-t", job, "sleep 604", "Enter")
	l.fg(job, "sleep")
	win["job"], want["job"] = job, LeftOpen
	p.Sessions["job"] = launch.SessionRef{Launcher: "tmux", ID: job, Name: "job"}
	tty, err := l.PaneTTY(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	out, err := l.Run(ctx, "ps", "-A", "-ww", "-o", "pid=", "-o", "tty=", "-o", "args=")
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for line := range strings.Lines(string(out)) {
		if f := strings.Fields(line); len(f) == 4 && f[1] == tty && f[2] == "sleep" && f[3] == "603" {
			l.pid["job"], _ = strconv.Atoi(f[0])
			found++
		}
	}
	if found != 1 {
		t.Fatalf("%d suspended sleeps on %s", found, tty)
	}
	steps = append(steps, Step{Candidate: Candidate{Role: RoleWorker, SessionID: "job", Name: "job", Cwd: "/tmp"}, Action: Leave})
	// Not running any more: its window is a shell, or one with a suspended job.
	shell := l.do("new-window", "-d", "-P", "-F", "#{window_id}", "-t", "="+ws+":", "-n", "shell")
	p.Sessions["shell"], win["shell"] = launch.SessionRef{Launcher: "tmux", ID: shell, Name: "shell"}, shell
	want["shell"] = ""
	susp := l.do("new-window", "-d", "-P", "-F", "#{window_id}", "-t", "="+ws+":", "-n", "susp")
	l.do("send-keys", "-t", susp, "sleep 605", "Enter")
	l.fg(susp, "sleep")
	l.do("send-keys", "-t", susp, "C-z")
	l.fg(susp, "bash")
	p.Sessions["susp"], win["susp"], want["susp"] = launch.SessionRef{Launcher: "tmux", ID: susp, Name: "susp"}, susp, LeftOpen
	for _, id := range []string{"shell", "susp"} {
		steps = append(steps, Step{Candidate: Candidate{Role: RoleWorker, SessionID: id, Name: id, Cwd: "/tmp"}, Action: Resume})
	}
	// A window of the user's own in the project's session, not recorded.
	own := l.do("new-window", "-d", "-P", "-F", "#{window_id}", "-t", "="+ws+":", "-n", "own")
	for id, w := range win {
		if id == "shell" || id == "susp" || id == "child" || id == "split" {
			continue
		}
		l.fg(w, "sleep")
	}
	l.fg(win["child"], "sh")
	l.eventually("the shell is idle", func() bool {
		refs, err := l.Tmux.Sessions(ctx, ws)
		return err == nil && slices.ContainsFunc(refs, func(r launch.SessionRef) bool { return r.ID == shell && r.Idle })
	})

	outs := Down(ctx, p, steps, l, false)
	for _, o := range outs {
		id := o.Step.SessionID
		if o.Err != nil {
			t.Errorf("%s: %v", id, o.Err)
		}
		if o.Ref.Warning != want[id] {
			t.Errorf("%s: warning %q, want %q", id, o.Ref.Warning, want[id])
		}
		if _, closed := p.Closed[id]; closed && want[id] != "" && o.Step.Action == Leave {
			t.Errorf("%s: left open, and recorded as closed", id)
		}
	}
	if len(outs) != len(want) {
		t.Errorf("%d outcomes, want %d", len(outs), len(want))
	}
	for id, w := range win {
		if l.exists(w) != (want[id] != "") {
			t.Errorf("%s: window exists %v, want %v", id, l.exists(w), want[id] != "")
		}
	}
	if !l.exists(own) {
		t.Error("a window qm never recorded was closed")
	}
	if len(p.Sessions) != 0 {
		t.Errorf("still recorded: %v", p.Sessions)
	}
}

// A server started again numbers its windows from the start: a recorded id
// that names another window by now is left alone.
func TestLiveDownServerRestarted(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ctx := context.Background()
	l := newLiveTerms(t)
	p := &Project{ID: "proj-277ae841", Root: "/work/proj", Workspace: "proj", Launcher: "tmux"}
	old := l.session(p, "proj", "w1", "exec sleep 600")
	l.do("kill-server")
	l.eventually("the server ended", func() bool {
		refs, err := l.Tmux.Sessions(ctx, "proj")
		return err == nil && len(refs) == 0
	})
	step := []Step{{Candidate: Candidate{Role: RoleWorker, SessionID: "w1", Name: "w1", Cwd: "/tmp"}, Action: Resume}}
	q := &Project{}
	again := l.session(q, "proj", "other", "exec sleep 600")
	if again.ID != old.ID {
		t.Fatalf("ids %s and %s, want the same", old.ID, again.ID)
	}
	outs := Down(ctx, p, step, l, false)
	if !l.exists(again.ID) {
		t.Error("another window was closed by its id")
	}
	if len(outs) != 1 || outs[0].Err != nil || outs[0].Ref.Warning != `left open: that window is named "other" now, not "w1"` {
		t.Errorf("outcomes %+v", outs)
	}
}

// qm up closes a stale window of its own, an idle one, and says why it leaves
// any other; what changed while it asked is read again.
func TestLiveUpStaleWindows(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		before, after func(l *liveTerms, w string)
		planned       bool
		closed        bool
		say           string
	}{
		"idle": {planned: true, closed: true},
		"moved": {before: func(l *liveTerms, w string) { l.do("move-window", "-d", "-s", w, "-t", "=mine:") },
			say: "it is now in tmux session mine"},
		"linked": {before: func(l *liveTerms, w string) { l.do("link-window", "-d", "-s", w, "-t", "=mine:") },
			say: launch.LinkedWhy},
		"renamed": {before: func(l *liveTerms, w string) { l.do("rename-window", "-t", w, "notes") },
			say: `named "notes" now`},
		"moved while asked": {after: func(l *liveTerms, w string) { l.do("move-window", "-d", "-s", w, "-t", "=mine:") },
			planned: true, say: "it is now in tmux session mine"},
		"linked while asked": {after: func(l *liveTerms, w string) { l.do("link-window", "-d", "-s", w, "-t", "=mine:") },
			planned: true, say: launch.LinkedWhy},
		"renamed while asked": {after: func(l *liveTerms, w string) { l.do("rename-window", "-t", w, "notes") },
			planned: true, say: `named "notes" now`},
		"used while asked": {after: func(l *liveTerms, w string) {
			l.do("send-keys", "-t", w, "sleep 600", "Enter")
			l.fg(w, "sleep")
		}, planned: true, say: "it runs something now"},
		"suspended while asked": {after: func(l *liveTerms, w string) {
			l.do("send-keys", "-t", w, "sleep 600", "Enter")
			l.fg(w, "sleep")
			l.do("send-keys", "-t", w, "C-z")
			l.fg(w, "bash")
		}, planned: true, say: "it runs something now"},
	} {
		t.Run(name, func(t *testing.T) {
			l := newLiveTerms(t)
			l.do("new-session", "-d", "-s", "mine", "-n", "mine")
			p := &Project{ID: "proj-277ae841", Workspace: "proj"}
			l.session(p, "proj", "keep", "exec sleep 600")
			w := l.do("new-window", "-d", "-P", "-F", "#{window_id}", "-t", "=proj:", "-n", "w1")
			p.Sessions["w1"] = launch.SessionRef{Launcher: "tmux", ID: w, Name: "w1"}
			idle := func() bool {
				refs, err := l.Tmux.Sessions(ctx, "proj")
				return err == nil && slices.ContainsFunc(refs, func(r launch.SessionRef) bool { return r.ID == w && r.Idle })
			}
			l.eventually("idle", idle)
			if c.before != nil {
				c.before(l, w)
			}
			d := Deps{Lister: l.Tmux, Closer: l.Tmux}
			steps := []Step{{Candidate: Candidate{Role: RoleWorker, SessionID: "w1", Name: "w1"}, Action: Resume}}
			sp := PlanStale(ctx, p, steps, d)
			if (len(sp.Close) == 1) != c.planned {
				t.Errorf("planned %+v, notes %q", sp.Close, sp.Notes)
			}
			if c.after != nil {
				c.after(l, w)
			}
			said := strings.Join(append(sp.Notes, closeStale(ctx, sp, d)["w1"]...), "\n")
			if l.exists(w) == c.closed {
				t.Errorf("window exists %v, said %q", l.exists(w), said)
			}
			if (said == "") != (c.say == "") || !strings.Contains(said, c.say) {
				t.Errorf("said %q, want %q", said, c.say)
			}
		})
	}
}
