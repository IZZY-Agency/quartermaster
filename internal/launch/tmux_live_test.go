//go:build tmuxlive

// Tests of the tmux launcher against a real tmux, on a private socket. They
// run in a container (make tmux-live):
//
//	docker run --rm --init -v "$PWD":/src:ro -w /src golang:1.27 sh -c \
//	  'apt-get update -qq && apt-get install -y -qq tmux >/dev/null && go test -tags tmuxlive ./internal/launch ./internal/up -count=1'
package launch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// liveTmux is a tmux server of its own for one test.
type liveTmux struct {
	Tmux
	t *testing.T
}

func newLiveTmux(t *testing.T) *liveTmux {
	t.Helper()
	// Not t.TempDir: a socket path must stay short.
	dir, err := os.MkdirTemp("", "qm")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("SHELL", "/bin/bash")
	t.Setenv("LC_ALL", "C.UTF-8")
	sock := filepath.Join(dir, "s")
	l := &liveTmux{t: t}
	l.Tmux = Tmux{Bin: "tmux", Run: func(ctx context.Context, bin string, args ...string) ([]byte, error) {
		if bin == "tmux" {
			args = append([]string{"-f", "/dev/null", "-S", sock}, args...)
		}
		return Exec(ctx, bin, args...)
	}}
	t.Cleanup(func() {
		_, _ = l.Run(context.Background(), "tmux", "kill-server")
		_ = os.RemoveAll(dir)
	})
	return l
}

// do runs a tmux command by hand, as a user would.
func (l *liveTmux) do(args ...string) string {
	l.t.Helper()
	out, err := l.Run(context.Background(), "tmux", args...)
	if err != nil {
		l.t.Fatalf("tmux %q: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// script writes a /bin/sh script.
func (l *liveTmux) script(body string) string {
	l.t.Helper()
	f, err := os.CreateTemp(l.t.TempDir(), "*.sh")
	if err != nil {
		l.t.Fatal(err)
	}
	if _, err := f.WriteString(body + "\n"); err != nil {
		l.t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		l.t.Fatal(err)
	}
	return f.Name()
}

// spawn starts a window of workspace ws.
func (l *liveTmux) spawn(ws, name, body string) SessionRef {
	l.t.Helper()
	ref, err := l.Spawn(context.Background(), LaunchSpec{Name: name, Cwd: "/tmp", Script: l.script(body), WorkspaceName: ws})
	if err != nil {
		l.t.Fatalf("spawn %s: %v", name, err)
	}
	return ref
}

// listed is the window id of workspace ws, as listed.
func (l *liveTmux) listed(ws, id string) (SessionRef, bool) {
	l.t.Helper()
	refs, err := l.Sessions(context.Background(), ws)
	if err != nil {
		l.t.Fatalf("list %s: %v", ws, err)
	}
	i := slices.IndexFunc(refs, func(r SessionRef) bool { return r.ID == id })
	if i < 0 {
		return SessionRef{}, false
	}
	return refs[i], true
}

// exists says whether tmux lists window id in any session.
func (l *liveTmux) exists(id string) bool {
	l.t.Helper()
	return slices.Contains(strings.Fields(l.do("list-windows", "-a", "-F", "#{window_id}")), id)
}

// eventually waits up to five seconds for ok.
func (l *liveTmux) eventually(what string, ok func() bool) {
	l.t.Helper()
	for range 50 {
		if ok() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	l.t.Errorf("%s: never so; windows:\n%s", what, l.do("list-windows", "-a", "-F", "#{session_name} #{window_id} #{window_name} dead=#{pane_dead} #{pane_current_command}"))
}

// typed sends line to window id, then Enter.
func (l *liveTmux) typed(id, line string) {
	l.t.Helper()
	l.do("send-keys", "-t", id, line, "Enter")
}

// fg waits until the foreground command of window id is cmd.
func (l *liveTmux) fg(id, cmd string) {
	l.t.Helper()
	l.eventually(id+" runs "+cmd, func() bool {
		return l.do("display-message", "-p", "-t", id, "#{pane_current_command}") == cmd
	})
}

// always fails when the window is ever idle over a second.
func (l *liveTmux) always(what string, ok func() bool) {
	l.t.Helper()
	for range 10 {
		if !ok() {
			l.t.Errorf("%s: not so", what)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// A window is idle when a shell alone runs in it, or its command has ended;
// never while anything else runs in it, in its foreground or not.
func TestLiveTmuxIdle(t *testing.T) {
	l := newLiveTmux(t)
	idle := func(id string) bool {
		r, ok := l.listed("ws", id)
		return ok && r.Idle
	}
	busy := func(what, id string) {
		t.Helper()
		l.always(what+" is not idle", func() bool { return !idle(id) })
	}

	// A launch script's command replaces its shell, as qm's scripts do.
	execd := l.spawn("ws", "execd", "exec sleep 600")
	l.fg(execd.ID, "sleep")
	busy("a script that exec'd its command", execd.ID)

	// A script that did not exec runs its command as a child: tmux shows sh.
	child := l.spawn("ws", "child", "sleep 600; exit 0")
	l.fg(child.ID, "sh")
	busy("a script with a child", child.ID)

	// A script alone, waiting in a builtin.
	alone := l.spawn("ws", "alone", "read x")
	l.fg(alone.ID, "sh")
	busy("a script with no child", alone.ID)

	shell := l.do("new-window", "-d", "-P", "-F", "#{window_id}", "-t", "=ws:", "-n", "shell")
	l.eventually("an interactive shell is idle", func() bool { return idle(shell) })
	nested := l.do("new-window", "-d", "-P", "-F", "#{window_id}", "-t", "=ws:", "-n", "nested")
	l.typed(nested, "bash --norc")
	l.eventually("a shell in a shell is idle", func() bool { return idle(nested) })

	// The shell's own jobs: in the foreground, suspended, in the background.
	l.typed(shell, "sleep 601")
	l.fg(shell, "sleep")
	busy("a foreground job", shell)
	l.do("send-keys", "-t", shell, "C-z")
	l.fg(shell, "bash")
	busy("a suspended job", shell)
	l.typed(shell, "kill %1; wait; clear")
	l.eventually("the job ended", func() bool { return idle(shell) })
	l.typed(shell, "sleep 602 &")
	time.Sleep(500 * time.Millisecond)
	busy("a background job", shell)
	l.typed(shell, "kill %1; wait; clear")
	l.eventually("the background job ended", func() bool { return idle(shell) })
	// Off the pane's terminal, but the shell's own.
	l.typed(shell, "setsid -f true; (setsid sleep 603; true)")
	time.Sleep(500 * time.Millisecond)
	busy("a job on no terminal", shell)

	// A split window runs two things.
	l.do("split-window", "-d", "-t", nested)
	busy("a split window", nested)

	// A command that ended in a window that stays.
	l.do("set-option", "-g", "remain-on-exit", "on")
	dead := l.spawn("ws", "dead", "exit 3")
	l.eventually("a finished command is idle", func() bool { return idle(dead.ID) })
	// A finished window's terminal name may be another pane's by now.
	next := l.spawn("ws", "next", "exec sleep 604")
	l.fg(next.ID, "sleep")
	busy("a window after a finished one", next.ID)
	if !idle(dead.ID) {
		t.Error("the finished window is no longer idle")
	}
}

// A name tmux would change, expand or resolve otherwise is refused before
// tmux runs; every name that is not comes back as given.
func TestLiveTmuxNames(t *testing.T) {
	ctx := context.Background()
	l := newLiveTmux(t)
	l.spawn("base", "base", "exec sleep 600")
	pwned := filepath.Join(t.TempDir(), "pwned")
	for i, ws := range []string{"plain-1_x", "Shop v2 · next", "@2", "%1", "*", "a,b}", "a%Y", "-x", "~", "{marked}", "=x", "+1", "a|b", "!", "^", "ünï"} {
		name := fmt.Sprintf("w%d", i)
		for _, n := range []string{name + "a", name + "b"} { // a new session, then a window in it
			if ref := l.spawn(ws, n, "exec sleep 600"); ref.ID == "" {
				t.Errorf("%q: no id", ws)
			} else if r, ok := l.listed(ws, ref.ID); !ok || r.Name != n || r.Workspace != ws {
				t.Errorf("%q: listed %+v, %v", ws, r, ok)
			}
		}
		if refs, err := l.Sessions(ctx, ws); err != nil || len(refs) != 2 {
			t.Errorf("%q: %v, %v, want its two windows", ws, refs, err)
		}
	}
	before := l.do("list-windows", "-a", "-F", "#{window_id}")
	targets := []string{"my.site", "a:b", "$0", "$1", "end;"} // no target names these
	for _, ws := range []string{"my.site", "a:b", "x#Sy", "a#(touch " + pwned + ")b", "$0", "$1", `a\b`, "tab\tx", "nl\nx", "end;", "bad\xff", "\x1b[31m"} {
		if ref, err := l.Spawn(ctx, LaunchSpec{Name: "w", Cwd: "/tmp", Script: l.script("exec sleep 600"), WorkspaceName: ws}); err == nil || errors.Is(err, ErrMaybeStarted) {
			t.Errorf("spawn in %q: %+v, %v, want a refusal", ws, ref, err)
		}
		if refs, err := l.Sessions(ctx, ws); len(refs) != 0 || (err == nil) == slices.Contains(targets, ws) {
			t.Errorf("list %q: %v, %v", ws, refs, err)
		}
		if err := l.Close(ctx, SessionRef{Launcher: "tmux", ID: "@0", Name: "base", Workspace: ws}); err == nil || errors.Is(err, ErrGone) {
			t.Errorf("close in %q: %v, want a refusal", ws, err)
		}
	}
	for _, name := range []string{"x#Sy", "a#(touch " + pwned + ")b", `a\b`, "tab\tx", "end;", ""} {
		for _, ws := range []string{"base", "new", ""} {
			if ref, err := l.Spawn(ctx, LaunchSpec{Name: name, Cwd: "/tmp", Script: l.script("exec sleep 600"), WorkspaceName: ws}); err == nil || errors.Is(err, ErrMaybeStarted) {
				t.Errorf("window %q in %q: %+v, %v, want a refusal", name, ws, ref, err)
			}
		}
	}
	if after := l.do("list-windows", "-a", "-F", "#{window_id}"); after != before {
		t.Errorf("a refused launch started windows: %q, before %q", after, before)
	}

	// A session that exists is used whatever its name would become in a new one.
	for _, ws := range []string{"issue #12", "a\u200db"} {
		l.do("-u", "new-session", "-d", "-s", strings.ReplaceAll(ws, "#", "##"), "-n", "first")
		ref := l.spawn(ws, "w", "exec sleep 600")
		l.fg(ref.ID, "sleep")
		if r, ok := l.listed(ws, ref.ID); !ok {
			t.Errorf("%q: not listed", ws)
		} else if tty, err := l.PaneTTY(ctx, "="+ws+":"+ref.ID); err != nil || tty == "" {
			t.Errorf("%q: PaneTTY = %q, %v", ws, tty, err)
		} else if err := l.Close(ctx, r); err != nil || l.exists(ref.ID) {
			t.Errorf("%q: close: %v", ws, err)
		}
	}

	// A folder's name is used as it is, never expanded or run.
	for _, d := range []string{"c#S", "a#(touch " + filepath.Base(pwned) + ")b", "end;", "#{session_name}", "##x"} {
		dir := filepath.Join(filepath.Dir(pwned), d)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		ref, err := l.Spawn(ctx, LaunchSpec{Name: "cwd", Cwd: dir, Script: l.script("exec sleep 600"), WorkspaceName: "base"})
		if err != nil {
			t.Errorf("folder %q: %v", d, err)
			continue
		}
		l.eventually("folder "+d, func() bool {
			return l.do("display-message", "-p", "-t", ref.ID, "#{pane_current_path}") == dir
		})
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(pwned); err == nil {
		t.Error("a name was run as a command")
	}
}

// A window is closed only in its own session; the ids of others, the ids of
// no window and a server that is not running are told apart.
func TestLiveTmuxCloseAndGone(t *testing.T) {
	ctx := context.Background()
	l := newLiveTmux(t)
	if refs, err := l.Sessions(ctx, "ws"); err != nil || len(refs) != 0 {
		t.Errorf("no server: %v, %v, want no windows", refs, err)
	}
	if err := l.Close(ctx, SessionRef{Launcher: "tmux", ID: "@1", Name: "x", Workspace: "ws"}); !errors.Is(err, ErrGone) {
		t.Errorf("close with no server: %v, want ErrGone", err)
	}
	other := l.spawn("other", "other", "exec sleep 600")
	w := l.spawn("ws", "w", "exec sleep 600")
	if refs, err := l.Sessions(ctx, "nope"); err != nil || len(refs) != 0 {
		t.Errorf("no such session: %v, %v", refs, err)
	}
	// Session names are matched whole, never as a prefix or a pattern.
	for _, ws := range []string{"w", "*", "w?"} {
		if refs, err := l.Sessions(ctx, ws); err != nil || len(refs) != 0 {
			t.Errorf("%q: %v, %v, want none", ws, refs, err)
		}
	}
	if err := l.Close(ctx, SessionRef{Launcher: "tmux", ID: "@99", Name: "w", Workspace: "ws"}); !errors.Is(err, ErrGone) {
		t.Errorf("close of no window: %v, want ErrGone", err)
	}
	if err := l.Close(ctx, SessionRef{Launcher: "tmux", ID: other.ID, Name: "w", Workspace: "ws"}); err == nil {
		t.Error("closed another session's window")
	}
	if !l.exists(other.ID) {
		t.Error("another session's window is gone")
	}
	ref, _ := l.listed("ws", w.ID)
	if err := l.Close(ctx, ref); err != nil {
		t.Errorf("close: %v", err)
	}
	if l.exists(w.ID) {
		t.Error("the window is still there")
	}
	// A socket that may not be used is an error, never "nothing runs".
	denied := l.Tmux
	denied.Run = func(ctx context.Context, bin string, args ...string) ([]byte, error) {
		return nil, errors.New("tmux: error connecting to /tmp/x/s (Permission denied): exit status 1")
	}
	if refs, err := denied.Sessions(ctx, "ws"); err == nil {
		t.Errorf("permission denied: %v, want an error", refs)
	}
}

// Closing reads the window again: one that is not as it was listed is left.
func TestLiveTmuxCloseReadsAgain(t *testing.T) {
	ctx := context.Background()
	for name, change := range map[string]func(l *liveTmux, ref SessionRef){
		"renamed":   func(l *liveTmux, r SessionRef) { l.do("rename-window", "-t", r.ID, "notes") },
		"split":     func(l *liveTmux, r SessionRef) { l.do("split-window", "-d", "-t", r.ID) },
		"respawned": func(l *liveTmux, r SessionRef) { l.do("respawn-window", "-k", "-t", r.ID, "exec sleep 700") },
		"moved": func(l *liveTmux, r SessionRef) {
			l.do("move-window", "-d", "-s", r.ID, "-t", "=other:")
		},
		"linked": func(l *liveTmux, r SessionRef) { l.do("link-window", "-d", "-s", r.ID, "-t", "=other:") },
	} {
		t.Run(name, func(t *testing.T) {
			l := newLiveTmux(t)
			l.spawn("other", "other", "exec sleep 600")
			l.spawn("ws", "keep", "exec sleep 600")
			w := l.spawn("ws", "w", "exec sleep 600")
			l.fg(w.ID, "sleep")
			ref, ok := l.listed("ws", w.ID)
			if !ok {
				t.Fatal("not listed")
			}
			change(l, ref)
			err := l.Close(ctx, ref)
			if err == nil || errors.Is(err, ErrGone) {
				t.Errorf("close: %v, want a refusal", err)
			}
			t.Logf("refused: %v", err)
			if !l.exists(w.ID) {
				t.Error("the window was closed")
			}
		})
	}
	t.Run("idle shell taken into use", func(t *testing.T) {
		l := newLiveTmux(t)
		l.spawn("ws", "keep", "exec sleep 600")
		w := l.do("new-window", "-d", "-P", "-F", "#{window_id}", "-t", "=ws:", "-n", "w")
		var ref SessionRef
		l.eventually("idle", func() bool { ref, _ = l.listed("ws", w); return ref.Idle })
		l.typed(w, "sleep 605")
		l.fg(w, "sleep")
		if err := l.Close(ctx, ref); err == nil || errors.Is(err, ErrGone) {
			t.Errorf("close: %v, want a refusal", err)
		}
		if !l.exists(w) {
			t.Error("the window was closed")
		}
	})
}

// What qm reads of a pane and a window: also of one that is gone, dead,
// split, or in several sessions.
func TestLiveTmuxPanes(t *testing.T) {
	ctx := context.Background()
	l := newLiveTmux(t)
	w := l.spawn("Shop v2 · next", "w", "exec sleep 600")
	first := l.spawn("Shop v2 · next", "first", "exec sleep 600")
	l.do("select-window", "-t", first.ID)
	l.spawn("other", "other", "exec sleep 600")
	tty := strings.TrimPrefix(l.do("display-message", "-p", "-t", w.ID, "#{pane_tty}"), "/dev/")
	pane := l.do("display-message", "-p", "-t", w.ID, "#{pane_id}")
	for _, target := range []string{w.ID, pane, "=Shop v2 · next:" + w.ID} {
		if got, err := l.PaneTTY(ctx, target); err != nil || got != tty {
			t.Errorf("PaneTTY(%s) = %q, %v, want %q", target, got, err, tty)
		}
	}
	if s, id, name, err := l.PaneOf(ctx, pane); err != nil || s != "Shop v2 · next" || id != w.ID || name != "w" {
		t.Errorf("PaneOf = %q %q %q, %v", s, id, name, err)
	}
	if n, err := l.WindowPanes(ctx, w.ID); err != nil || n != 1 {
		t.Errorf("WindowPanes = %d, %v", n, err)
	}
	// Where tmux's display-message would answer for another window.
	for _, target := range []string{"@99", "%99", "=Shop v2 · next:@99", "=other:" + w.ID, "=nope:" + w.ID} {
		if got, err := l.PaneTTY(ctx, target); err == nil {
			t.Errorf("PaneTTY(%s) = %q, want an error", target, got)
		}
	}
	if s, id, name, err := l.PaneOf(ctx, "%99"); err == nil {
		t.Errorf("PaneOf of no pane = %q %q %q", s, id, name)
	}
	if id, err := l.WindowOf(ctx, "%99"); err == nil {
		t.Errorf("WindowOf no pane = %q", id)
	}

	l.do("split-window", "-d", "-t", w.ID)
	if n, err := l.WindowPanes(ctx, w.ID); err != nil || n != 2 {
		t.Errorf("split: WindowPanes = %d, %v", n, err)
	}
	if got, err := l.PaneTTY(ctx, w.ID); err != nil || got != "" {
		t.Errorf("split: PaneTTY of the window = %q, %v, want none", got, err)
	}
	if got, err := l.PaneTTY(ctx, pane); err != nil || got != tty {
		t.Errorf("split: PaneTTY of the pane = %q, %v", got, err)
	}

	// A finished pane's terminal may be another pane's by now.
	l.do("set-option", "-g", "remain-on-exit", "on")
	dead := l.spawn("other", "dead", "exit 0")
	l.eventually("finished", func() bool { r, ok := l.listed("other", dead.ID); return ok && r.Idle })
	if got, err := l.PaneTTY(ctx, dead.ID); err != nil || got != "" {
		t.Errorf("finished: PaneTTY = %q, %v, want none", got, err)
	}

	// By hand: linked into another session, then moved there.
	l.do("link-window", "-d", "-s", first.ID, "-t", "=other:")
	fp := l.do("display-message", "-p", "-t", first.ID, "#{pane_id}")
	if s, _, _, err := l.PaneOf(ctx, fp); !errors.Is(err, ErrLinked) {
		t.Errorf("linked: PaneOf = %q, %v, want ErrLinked", s, err)
	}
	if id, err := l.WindowOf(ctx, fp); err != nil || id != first.ID {
		t.Errorf("linked: WindowOf = %q, %v", id, err)
	}
	for _, ws := range []string{"Shop v2 · next", "other"} {
		if r, ok := l.listed(ws, first.ID); !ok || !r.Linked {
			t.Errorf("linked: listed in %s: %+v, %v", ws, r, ok)
		}
	}
	l.do("unlink-window", "-t", "=other:"+first.ID)
	if r, ok := l.listed("Shop v2 · next", first.ID); !ok || r.Linked {
		t.Errorf("unlinked: %+v, %v", r, ok)
	}
	l.do("move-window", "-d", "-s", first.ID, "-t", "=other:")
	if _, ok := l.listed("Shop v2 · next", first.ID); ok {
		t.Error("moved: still listed")
	}
	places, err := l.Locate(ctx, first)
	first.Workspace = "Shop v2 · next"
	if why := Moved(first, places); err != nil || why != "it is now in tmux session other" {
		t.Errorf("moved: %q (%+v, %v)", why, places, err)
	}
	if s, _, _, err := l.PaneOf(ctx, fp); err != nil || s != "other" {
		t.Errorf("moved: PaneOf = %q, %v", s, err)
	}
	// The same windows seen through a session group are no link.
	l.do("new-session", "-d", "-t", "=other", "-s", "other-2")
	if r, ok := l.listed("other", first.ID); !ok || r.Linked {
		t.Errorf("session group: %+v, %v", r, ok)
	}
	if s, _, _, err := l.PaneOf(ctx, fp); err == nil || !strings.Contains(err.Error(), "group") {
		t.Errorf("session group: PaneOf = %q, %v, want a refusal", s, err)
	}
	if err := l.RenameWindow(ctx, first.ID, "renamed"); err != nil {
		t.Error(err)
	}
	if places, err := l.Locate(ctx, first); err != nil || Moved(SessionRef{ID: first.ID, Name: "first", Workspace: "other"}, places) != `that window is named "renamed" now, not "first"` {
		t.Errorf("renamed: %+v, %v", places, err)
	}
}

// tmux itself checks the window in the command that kills it: one moved, and
// made current in its new session, between the reading and the kill is left.
func TestLiveTmuxChangedAtTheKill(t *testing.T) {
	for name, change := range map[string]func(l *liveTmux, id string){
		"moved": func(l *liveTmux, id string) {
			l.do("move-window", "-s", id, "-t", "=other:")
			l.do("select-window", "-t", "=other:"+id)
		},
		"moved, its session gone": func(l *liveTmux, id string) {
			l.do("move-window", "-s", id, "-t", "=other:")
			l.do("kill-session", "-t", "=ws")
		},
		"split":     func(l *liveTmux, id string) { l.do("split-window", "-d", "-t", id) },
		"linked":    func(l *liveTmux, id string) { l.do("link-window", "-d", "-s", id, "-t", "=other:") },
		"respawned": func(l *liveTmux, id string) { l.do("respawn-window", "-k", "-t", id, "exec sleep 700") },
	} {
		t.Run(name, func(t *testing.T) {
			l := newLiveTmux(t)
			l.spawn("other", "other", "exec sleep 600")
			l.spawn("ws", "keep", "exec sleep 600")
			w := l.spawn("ws", "w", "exec sleep 600")
			l.fg(w.ID, "sleep")
			ref, _ := l.listed("ws", w.ID)
			late := l.Tmux
			late.Run = func(ctx context.Context, bin string, args ...string) ([]byte, error) {
				if args[0] == "if-shell" {
					change(l, w.ID)
				}
				return l.Run(ctx, bin, args...)
			}
			if err := late.Close(context.Background(), ref); err == nil {
				t.Error("close: no error")
			}
			if !l.exists(w.ID) {
				t.Error("the window was killed")
			}
		})
	}
}
