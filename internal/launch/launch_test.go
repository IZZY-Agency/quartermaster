package launch

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLauncherAutoDetect(t *testing.T) {
	cases := []struct {
		name, choice string
		env          map[string]string
		want         string // agterm | tmux-inside | tmux-outside
	}{
		{"agterm only", "auto", map[string]string{"AGTERM_SESSION_ID": "s1"}, "agterm"},
		{"tmux only", "auto", map[string]string{"TMUX": "/tmp/tmux-1/default,1,0"}, "tmux-inside"},
		{"both", "auto", map[string]string{"AGTERM_SESSION_ID": "s1", "TMUX": "/tmp/t,1,0"}, "agterm"},
		{"empty choice is auto", "", map[string]string{"TMUX": "/tmp/t,1,0"}, "tmux-inside"},
		{"config tmux wins over agterm", "tmux", map[string]string{"AGTERM_SESSION_ID": "s1"}, "tmux-outside"},
		{"config agterm", "agterm", map[string]string{}, "agterm"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l, err := Pick(c.choice, envOf(c.env))
			if err != nil {
				t.Fatal(err)
			}
			got := ""
			switch v := l.(type) {
			case Agterm:
				got = "agterm"
			case Tmux:
				got = "tmux-outside"
				if v.Inside {
					got = "tmux-inside"
				}
			}
			if got != c.want {
				t.Errorf("Pick(%q) = %s, want %s", c.choice, got, c.want)
			}
		})
	}
}

func TestNoLauncherClearError(t *testing.T) {
	l, err := Pick("auto", envOf(nil))
	if !errors.Is(err, ErrNoLauncher) || l != nil {
		t.Fatalf("Pick = %v, %v; want nil, ErrNoLauncher", l, err)
	}
	for _, w := range []string{"agterm", "tmux", `"launcher"`} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error %q does not name %s", err, w)
		}
	}
	if _, err := Pick("screen", envOf(nil)); err == nil {
		t.Error("Pick(screen) succeeded")
	}
}

// recorder is a Runner that records its call and prints out.
type recorder struct {
	bin   string
	calls [][]string
	out   string
	fail  func(args []string) bool // when set and true, the call fails
	err   string                   // the failure's text ("failed" when empty)
}

func (r *recorder) run(_ context.Context, bin string, args ...string) ([]byte, error) {
	r.bin = bin
	r.calls = append(r.calls, args)
	if r.fail != nil && r.fail(args) {
		if r.err != "" {
			return nil, errors.New(r.err)
		}
		return nil, errors.New("failed")
	}
	return []byte(r.out), nil
}

func (r *recorder) reset() { r.calls = nil }

// first is the first call since the last reset.
func (r *recorder) first() []string { return r.calls[0] }

func TestAgtermArgs(t *testing.T) {
	spec := LaunchSpec{Name: "task-7", Cwd: "/work/repo", Script: "/state/launch/task-7-1.sh"}
	r := &recorder{out: `{"ok":true,"result":{"id":"4f2a9c"}}` + "\n"}
	ref, err := Agterm{Bin: "agtermctl", Run: r.run}.Spawn(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"session", "new", "--cwd", "/work/repo", "--name", "task-7", "--no-select", "--command", "/bin/sh /state/launch/task-7-1.sh", "--json"}
	if r.bin != "agtermctl" || !slices.Equal(r.first(), want) {
		t.Errorf("ran %s %q\nwant agtermctl %q", r.bin, r.first(), want)
	}
	if ref != (SessionRef{Launcher: "agterm", ID: "4f2a9c", Name: "task-7"}) {
		t.Errorf("ref = %+v", ref)
	}

	spec.Workspace, spec.Script = "ws1", "/state dir/x.sh"
	r.reset()
	if _, err := (Agterm{Bin: "agtermctl", Run: r.run}).Spawn(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	want = []string{"session", "new", "--workspace", "ws1", "--cwd", "/work/repo", "--name", "task-7", "--no-select", "--command", "/bin/sh '/state dir/x.sh'", "--json"}
	if !slices.Equal(r.first(), want) {
		t.Errorf("ran %q\nwant %q", r.first(), want)
	}

	// Inside an agterm session with no workspace named: right after the
	// coordinator's session, so the worker lands in its workspace.
	spec.Workspace = ""
	r.reset()
	if _, err := (Agterm{Bin: "agtermctl", Run: r.run, Anchor: "coord-1"}).Spawn(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	want = []string{"session", "new", "--after", "coord-1", "--cwd", "/work/repo", "--name", "task-7", "--no-select", "--command", "/bin/sh '/state dir/x.sh'", "--json"}
	if !slices.Equal(r.first(), want) {
		t.Errorf("ran %q\nwant %q", r.first(), want)
	}
	// A named workspace wins over the anchor.
	spec.Workspace = "ws1"
	r.reset()
	if _, err := (Agterm{Bin: "agtermctl", Run: r.run, Anchor: "coord-1"}).Spawn(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(r.first(), "ws1") || slices.Contains(r.first(), "--after") {
		t.Errorf("ran %q, want --workspace ws1 and no --after", r.first())
	}

	for _, out := range []string{"", "4f2a9c", `{"ok":true,"result":{}}`, `{"ok":false,"error":"no such workspace"}`} {
		r.out = out
		r.reset()
		if _, err := (Agterm{Bin: "agtermctl", Run: r.run}).Spawn(context.Background(), spec); err == nil {
			t.Errorf("output %q: want an error", out)
		}
	}
}

func TestTmuxArgs(t *testing.T) {
	spec := LaunchSpec{Name: "task-7", Cwd: "/work/repo", Script: "/state/launch/task-7-1.sh", Workspace: "ignored"}
	r := &recorder{out: "@12\n"}
	ref, err := Tmux{Bin: "tmux", Run: r.run, Inside: true}.Spawn(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"new-window", "-d", "-P", "-F", "#{window_id}", "-n", "task-7", "-c", "/work/repo", "/bin/sh /state/launch/task-7-1.sh"}
	if r.bin != "tmux" || !slices.Equal(r.first(), want) {
		t.Errorf("inside: ran %s %q\nwant tmux %q", r.bin, r.first(), want)
	}
	if ref != (SessionRef{Launcher: "tmux", ID: "@12", Name: "task-7"}) {
		t.Errorf("ref = %+v", ref)
	}

	r.out = "$3\n"
	r.reset()
	ref, err = Tmux{Bin: "tmux", Run: r.run}.Spawn(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"new-session", "-d", "-P", "-F", "#{session_id}", "-s", "task-7", "-c", "/work/repo", "/bin/sh /state/launch/task-7-1.sh"}
	if !slices.Equal(r.first(), want) {
		t.Errorf("outside: ran %q\nwant %q", r.first(), want)
	}
	if ref.ID != "$3" {
		t.Errorf("ref = %+v", ref)
	}
}

func TestQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/a/b.sh": "/a/b.sh",
		"/a b":    "'/a b'",
		"it's":    `'it'\''s'`,
		"":        "''",
		"$(x)":    "'$(x)'",
	} {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %s, want %s", in, got, want)
		}
	}
}

const newOut = `{"ok":true,"result":{"id":"4f2a9c"}}`

func TestAgtermWorkspaceNameArgs(t *testing.T) {
	spec := LaunchSpec{Name: "coordinator", Cwd: "/work/repo", Script: "/s/c.sh", WorkspaceName: "proj"}
	r := &recorder{out: newOut}
	// The workspace by name wins over the anchor, as --workspace does.
	if _, err := (Agterm{Bin: "agtermctl", Run: r.run, Anchor: "coord-1"}).Spawn(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	want := []string{"session", "new", "--workspace-name", "proj", "--create-workspace", "--cwd", "/work/repo", "--name", "coordinator", "--no-select", "--command", "/bin/sh /s/c.sh", "--json"}
	if !slices.Equal(r.first(), want) {
		t.Errorf("ran %q\nwant %q", r.first(), want)
	}
}

func TestAgtermPinsNewSession(t *testing.T) {
	r := &recorder{out: newOut}
	ref, err := Agterm{Bin: "agtermctl", Run: r.run}.Spawn(context.Background(), LaunchSpec{Name: "n", Cwd: "/w", Script: "/s.sh"})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 2 {
		t.Fatalf("calls = %q, want new then restore", r.calls)
	}
	want := []string{"session", "restore", "--none", "--target", "4f2a9c", "--json"}
	if !slices.Equal(r.calls[1], want) {
		t.Errorf("pin ran %q\nwant %q", r.calls[1], want)
	}
	if ref.Warning != "" {
		t.Errorf("Warning = %q", ref.Warning)
	}
	r.reset()
	if err := (Agterm{Bin: "agtermctl", Run: r.run}).Pin(context.Background(), "x1"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.first(), []string{"session", "restore", "--none", "--target", "x1", "--json"}) {
		t.Errorf("Pin ran %q", r.first())
	}
}

func TestAgtermPinFailureWarns(t *testing.T) {
	spec := LaunchSpec{Name: "n", Cwd: "/w", Script: "/s.sh"}
	// pinReply is the pin call's reply; the new call always succeeds.
	spawn := func(pinReply string, pinErr error) (SessionRef, error) {
		run := func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if args[1] == "restore" {
				return []byte(pinReply), pinErr
			}
			return []byte(newOut), nil
		}
		return Agterm{Bin: "agtermctl", Run: run}.Spawn(context.Background(), spec)
	}
	for name, c := range map[string]struct {
		reply string
		err   error
		want  string
	}{
		"runner error":     {"", errors.New("boom"), "agterm restore pin failed: boom"},
		"ok false":         {`{"ok":false,"error":"no such session"}`, nil, "no such session"},
		"ok false no text": {`{"ok":false}`, nil, "agtermctl reported failure"},
	} {
		t.Run(name, func(t *testing.T) {
			ref, err := spawn(c.reply, c.err)
			if err != nil {
				t.Fatalf("err = %v, want none", err)
			}
			if ref.ID != "4f2a9c" || !strings.HasPrefix(ref.Warning, "agterm restore pin failed: ") || !strings.Contains(ref.Warning, c.want) {
				t.Errorf("ref = %+v, want a warning with %q", ref, c.want)
			}
		})
	}
}

func TestWorkspaceAndNameBothError(t *testing.T) {
	spec := LaunchSpec{Name: "n", Cwd: "/w", Script: "/s.sh", Workspace: "w1", WorkspaceName: "proj"}
	r := &recorder{out: newOut}
	if _, err := (Agterm{Bin: "agtermctl", Run: r.run}).Spawn(context.Background(), spec); err == nil {
		t.Error("agterm: want an error")
	}
	if _, err := (Tmux{Bin: "tmux", Run: r.run}).Spawn(context.Background(), spec); err == nil {
		t.Error("tmux: want an error")
	}
	if len(r.calls) != 0 {
		t.Errorf("ran %q, want nothing", r.calls)
	}
}

func TestTmuxNewSessionWhenMissing(t *testing.T) {
	spec := LaunchSpec{Name: "coordinator", Cwd: "/work/repo", Script: "/s/c.sh", WorkspaceName: "proj"}
	r := &recorder{out: "@1\n", fail: func(a []string) bool { return a[0] == "has-session" }}
	ref, err := Tmux{Bin: "tmux", Run: r.run, Inside: true}.Spawn(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.calls[0], []string{"has-session", "-t", "=proj"}) {
		t.Errorf("first call %q", r.calls[0])
	}
	want := []string{"new-session", "-d", "-P", "-F", "#{window_id}", "-s", "proj", "-n", "coordinator", "-c", "/work/repo", "/bin/sh /s/c.sh"}
	if len(r.calls) != 2 || !slices.Equal(r.calls[1], want) {
		t.Errorf("calls %q\nwant second %q", r.calls, want)
	}
	if ref != (SessionRef{Launcher: "tmux", ID: "@1", Name: "coordinator"}) {
		t.Errorf("ref = %+v", ref)
	}
}

func TestTmuxNewWindowInExistingSession(t *testing.T) {
	spec := LaunchSpec{Name: "task-1", Cwd: "/work/repo", Script: "/s/t.sh", WorkspaceName: "proj"}
	r := &recorder{out: "@2\n"}
	if _, err := (Tmux{Bin: "tmux", Run: r.run}).Spawn(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	want := []string{"new-window", "-d", "-P", "-F", "#{window_id}", "-t", "=proj:", "-n", "task-1", "-c", "/work/repo", "/bin/sh /s/t.sh"}
	if len(r.calls) != 2 || !slices.Equal(r.calls[1], want) {
		t.Errorf("calls %q\nwant second %q", r.calls, want)
	}
}

func TestAgtermSessionsInWorkspace(t *testing.T) {
	tree, err := os.ReadFile("testdata/agterm-tree.json")
	if err != nil {
		t.Fatal(err)
	}
	r := &recorder{out: string(tree)}
	a := Agterm{Bin: "agtermctl", Run: r.run}
	got, err := a.Sessions(context.Background(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	want := []SessionRef{{Launcher: "agterm", ID: "s1", Name: "coordinator", Cwd: "/work/repo", Foreground: "claude --resume 00000000-0000-4000-8000-000000000001"}, {Launcher: "agterm", ID: "s2", Name: "task-1", Idle: true, Cwd: "/work/wt"}}
	if !slices.Equal(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if !slices.Equal(r.first(), []string{"tree", "--json"}) {
		t.Errorf("ran %q", r.first())
	}
	if got, err := a.Sessions(context.Background(), "nope"); err != nil || len(got) != 0 {
		t.Errorf("missing workspace: %v, %v", got, err)
	}
}

func TestTmuxSessionsListed(t *testing.T) {
	const ps = "  101     1 Ss+  ttys001 -zsh\n" +
		"  301   101 S    ttys001 /opt/share/gitstatusd-linux-x86_64 -G v1\n" +
		"  102     1 Ss   ttys002 /bin/sh /state/launch/w.sh\n" +
		"  103     1 Ss+  ttys003 -zsh\n" +
		"  203   103 T    ttys003 claude --resume x\n" +
		"  105     1 Ss+  ttys005 -fish\n" +
		"  205   105 Z    ttys005 <defunct>\n" +
		"  106     1 Ss+  ttys006 bash -l\n" +
		"  206   106 Ss   ??      claude\n" +
		"  107     1 Ss+  ttys007 bash --norc\n" +
		"  207     1 S    ttys007 npm run dev\n" +
		"  108     1 Ss   ttys008 -bash\n" +
		"  109     1 Ss+  ttys009 gitstatusd\n"
	r := &recorder{out: "@1|qm|$1|qm|1|qm|0|qm|0|qm|100|qm|/dev/ttys000|qm|claude|qm|coordinator\n" +
		"@4|qm|$1|qm|1|qm|0|qm|0|qm|101|qm|/dev/ttys001|qm|-zsh|qm|task with space\n" +
		"@5|qm|$1|qm|1|qm|1|qm|0|qm|99|qm|/dev/ttys002|qm|sh|qm|Shop v2 · next\n" +
		"@6|qm|$1|qm|2|qm|0|qm|0|qm|101|qm|/dev/ttys001|qm|zsh|qm|split\n" +
		"@7|qm|$1|qm|1|qm|0|qm|1|qm|100|qm|/dev/ttys000|qm|node|qm|dev|qm|server\n" +
		"@8|qm|$1|qm|1|qm|0|qm|0|qm|102|qm|/dev/ttys002|qm|sh|qm|script\n" +
		"@9|qm|$1|qm|1|qm|0|qm|0|qm|103|qm|/dev/ttys003|qm|zsh|qm|suspended\n" +
		"@10|qm|$1|qm|1|qm|0|qm|0|qm|104|qm|/dev/ttys004|qm|zsh|qm|not in ps\n" +
		"@11|qm|$1|qm|1|qm|0|qm|0|qm|105|qm|/dev/ttys005|qm|fish|qm|ended job\n" +
		"@12|qm|$1|qm|1|qm|0|qm|0|qm|106|qm|/dev/ttys006|qm|bash|qm|job on no terminal\n" +
		"@13|qm|$1|qm|1|qm|0|qm|0|qm|107|qm|/dev/ttys007|qm|bash|qm|another's job\n" +
		"@14|qm|$1|qm|1|qm|0|qm|0|qm|108|qm|/dev/ttys008|qm|bash|qm|hidden job\n" +
		"@15|qm|$1|qm|1|qm|0|qm|0|qm|109|qm|/dev/ttys009|qm|sh|qm|helper alone\n"}
	run := func(ctx context.Context, bin string, args ...string) ([]byte, error) {
		if bin == "ps" {
			return []byte(ps), nil
		}
		return r.run(ctx, bin, args...)
	}
	got, err := Tmux{Bin: "tmux", Run: run}.Sessions(context.Background(), "proj")
	if err != nil {
		t.Fatal(err)
	}
	want := []SessionRef{
		{Launcher: "tmux", ID: "@1", Name: "coordinator", Workspace: "proj", Panes: 1, PanePID: 100, tmuxSession: "$1"},
		{Launcher: "tmux", ID: "@4", Name: "task with space", Workspace: "proj", Idle: true, Panes: 1, PanePID: 101, tmuxSession: "$1"},
		{Launcher: "tmux", ID: "@5", Name: "Shop v2 · next", Workspace: "proj", Idle: true, Panes: 1, PanePID: 99, tmuxSession: "$1"},
		{Launcher: "tmux", ID: "@6", Name: "split", Workspace: "proj", Panes: 2, PanePID: 101, tmuxSession: "$1"},
		{Launcher: "tmux", ID: "@7", Name: "dev|qm|server", Workspace: "proj", Panes: 1, PanePID: 100, tmuxSession: "$1", Linked: true},
		{Launcher: "tmux", ID: "@8", Name: "script", Workspace: "proj", Panes: 1, PanePID: 102, tmuxSession: "$1"},
		{Launcher: "tmux", ID: "@9", Name: "suspended", Workspace: "proj", Panes: 1, PanePID: 103, tmuxSession: "$1"},
		{Launcher: "tmux", ID: "@10", Name: "not in ps", Workspace: "proj", Panes: 1, PanePID: 104, tmuxSession: "$1"},
		{Launcher: "tmux", ID: "@11", Name: "ended job", Workspace: "proj", Idle: true, Panes: 1, PanePID: 105, tmuxSession: "$1"},
		{Launcher: "tmux", ID: "@12", Name: "job on no terminal", Workspace: "proj", Panes: 1, PanePID: 106, tmuxSession: "$1"},
		{Launcher: "tmux", ID: "@13", Name: "another's job", Workspace: "proj", Panes: 1, PanePID: 107, tmuxSession: "$1"},
		{Launcher: "tmux", ID: "@14", Name: "hidden job", Workspace: "proj", Panes: 1, PanePID: 108, tmuxSession: "$1"},
		{Launcher: "tmux", ID: "@15", Name: "helper alone", Workspace: "proj", Panes: 1, PanePID: 109, tmuxSession: "$1"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if !slices.Equal(r.first(), []string{"-u", "list-windows", "-t", "=proj", "-F", "#{window_id}|qm|#{session_id}|qm|#{window_panes}|qm|#{pane_dead}|qm|#{window_linked}|qm|#{pane_pid}|qm|#{pane_tty}|qm|#{pane_current_command}|qm|#{window_name}"}) {
		t.Errorf("ran %q", r.first())
	}
	// Processes not read: an error, never "nothing runs there".
	noPS := func(ctx context.Context, bin string, args ...string) ([]byte, error) {
		if bin == "ps" {
			return nil, errors.New("ps: not found")
		}
		return r.run(ctx, bin, args...)
	}
	if got, err := (Tmux{Bin: "tmux", Run: noPS}).Sessions(context.Background(), "proj"); err == nil {
		t.Errorf("ps failed: %+v, want an error", got)
	}
}

func TestTmuxSessionsUnreadable(t *testing.T) {
	for _, out := range []string{"x|qm|$1|qm|1|qm|0|qm|0|qm|1|qm|/dev/pts/0|qm|sh|qm|w", "@1;kill|qm|$1|qm|1|qm|0|qm|0|qm|1|qm|/dev/pts/0|qm|sh|qm|w", "@1", "@1|qm|w|qm|1|qm|0|qm|0|qm|1|qm|/dev/pts/0|qm|sh|qm|w", "@1|qm|$1|qm|?|qm|0|qm|0|qm|1|qm|/dev/pts/0|qm|sh|qm|w", "@1|qm|$1|qm|1|qm|0|qm|0|qm||qm|/dev/pts/0|qm|sh|qm|w"} {
		r := &recorder{out: out + "\n"}
		if got, err := (Tmux{Bin: "tmux", Run: r.run}).Sessions(context.Background(), "proj"); err == nil {
			t.Errorf("%q: %+v, want an error", out, got)
		}
	}
}

func TestWaitingShell(t *testing.T) {
	for args, want := range map[string]bool{
		"-zsh": true, "zsh": true, "/bin/bash": true, "bash -l": true, "fish --login": true, "zsh -il": true, "bash --norc -i": true, "bash +m": true,
		"": false, "claude": false, "sh x.sh": false, "/bin/sh /s/x.sh": false, "bash -c sleep": false, "bash -lc sleep": false, "fish --command=ls": false, "fish -C ls": false, "bash -": false, "bash --rcfile f": false, "-zsh x": false,
	} {
		if got := waitingShell(strings.Fields(args)); got != want {
			t.Errorf("waitingShell(%q) = %v, want %v", args, got, want)
		}
	}
}

// What tmux prints for an id is checked to be one before it is used again;
// so is a pane id from the environment.
func TestTmuxIDsChecked(t *testing.T) {
	ctx := context.Background()
	spec := LaunchSpec{Name: "w", Cwd: "/w", Script: "/s.sh"}
	for _, out := range []string{"", "@", "@1 @2", "%1", "$1", "1", "@1x", "note\n@1"} {
		r := &recorder{out: out}
		if ref, err := (Tmux{Bin: "tmux", Run: r.run, Inside: true}).Spawn(ctx, spec); !errors.Is(err, ErrMaybeStarted) {
			t.Errorf("window %q: %+v, %v, want ErrMaybeStarted", out, ref, err)
		}
	}
	r := &recorder{out: "@1"}
	if ref, err := (Tmux{Bin: "tmux", Run: r.run}).Spawn(ctx, spec); !errors.Is(err, ErrMaybeStarted) {
		t.Errorf("session: %+v, %v, want ErrMaybeStarted", ref, err)
	}
	r = &recorder{out: "never"}
	tm := Tmux{Bin: "tmux", Run: r.run}
	for _, id := range []string{"", "@", "1", "w", "$1", "{last}", "@1.", "=ws:w", "=ws:%1", "=a.b:@1", "=$1:@1", "-t"} {
		if n, err := tm.WindowPanes(ctx, id); err == nil {
			t.Errorf("WindowPanes(%q) = %d", id, n)
		}
		if tty, err := tm.PaneTTY(ctx, id); err == nil {
			t.Errorf("PaneTTY(%q) = %q", id, tty)
		}
		if _, _, _, err := tm.PaneOf(ctx, id); err == nil {
			t.Errorf("PaneOf(%q): no error", id)
		}
		if w, err := tm.WindowOf(ctx, id); err == nil {
			t.Errorf("WindowOf(%q) = %q", id, w)
		}
		if err := tm.RenameWindow(ctx, id, "n"); err == nil {
			t.Errorf("RenameWindow(%q): no error", id)
		}
		// A session id, as Spawn gives, names no window: none, no error.
		if places, err := tm.Locate(ctx, SessionRef{ID: id}); len(places) != 0 || (err == nil) != (id == "$1") {
			t.Errorf("Locate(%q): %v, %v", id, places, err)
		}
		if err := tm.Close(ctx, SessionRef{ID: id, Workspace: "ws"}); err == nil || errors.Is(err, ErrGone) {
			t.Errorf("Close(%q): %v", id, err)
		}
	}
	if err := tm.RenameWindow(ctx, "@1", "#(id)"); err == nil {
		t.Error("RenameWindow to a command: no error")
	}
	if len(r.calls) != 0 {
		t.Errorf("tmux ran %q", r.calls)
	}
}

func TestIsShell(t *testing.T) {
	for c, want := range map[string]bool{"zsh": true, "-zsh": true, "/bin/bash": true, "fish": true, "sh": true, "claude": false, "node": false, "vim": false, "": false} {
		if got := IsShell(c); got != want {
			t.Errorf("IsShell(%q) = %v, want %v", c, got, want)
		}
	}
}

func TestTmuxSessionsMissing(t *testing.T) {
	for _, msg := range []string{"tmux: can't find session: proj", "tmux: no server running on /tmp/tmux-1/default", "error connecting to /tmp/tmux-1/default (No such file or directory)"} {
		r := &recorder{fail: func([]string) bool { return true }, err: msg}
		got, err := Tmux{Bin: "tmux", Run: r.run}.Sessions(context.Background(), "proj")
		if err != nil || len(got) != 0 {
			t.Errorf("%q: got %v, %v; want none, no error", msg, got, err)
		}
	}
}

func TestTmuxSessionsOtherFailure(t *testing.T) {
	r := &recorder{fail: func([]string) bool { return true }, err: "exec: tmux: executable file not found in $PATH"}
	if _, err := (Tmux{Bin: "tmux", Run: r.run}).Sessions(context.Background(), "proj"); err == nil {
		t.Error("want an error, not an empty list")
	}
}

func TestAgtermSessionsBadAnswer(t *testing.T) {
	for _, out := range []string{`{"ok":false,"error":"busy"}`, "not json", `{"ok":true,"result":"x"}`} {
		r := &recorder{out: out}
		if got, err := (Agterm{Bin: "agtermctl", Run: r.run}).Sessions(context.Background(), "alpha"); err == nil {
			t.Errorf("%q: got %v, want an error", out, got)
		}
	}
}

func TestCloseArgs(t *testing.T) {
	r := &recorder{out: `{"ok":true,"result":{}}`}
	if err := (Agterm{Bin: "agtermctl", Run: r.run}).Close(context.Background(), SessionRef{Launcher: "agterm", ID: "s2"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.first(), []string{"session", "close", "--target", "s2", "--json"}) {
		t.Errorf("agterm ran %q", r.first())
	}
	r = &recorder{out: "@4|qm|$1|qm|1|qm|0|qm|0|qm|100|qm|/dev/pts/1|qm|claude|qm|w\n"}
	if err := (Tmux{Bin: "tmux", Run: r.run}).Close(context.Background(), SessionRef{Launcher: "tmux", ID: "@4", Workspace: "proj"}); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 2 || r.calls[0][1] != "list-windows" || !slices.Equal(r.calls[1], []string{"if-shell", "-F", "-t", "=proj:@4", "#{&&:#{==:#{window_id},@4},#{&&:#{==:#{session_id},$1},#{&&:#{==:#{window_panes},1},#{&&:#{==:#{window_linked},0},#{==:#{pane_pid},100}}}}}", "kill-window -t @4", "display-message -p changed"}) {
		t.Errorf("tmux ran %q", r.calls)
	}
}

func TestTmuxCloseNeedsSession(t *testing.T) {
	// A window id alone may name a window of another tmux session (ids
	// restart with the server): it is refused, and tmux is not run.
	r := &recorder{out: ""}
	if err := (Tmux{Bin: "tmux", Run: r.run}).Close(context.Background(), SessionRef{Launcher: "tmux", ID: "@4"}); err == nil || errors.Is(err, ErrGone) {
		t.Errorf("err %v, want a plain error", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("tmux ran %v", r.calls)
	}
}

func TestCloseGone(t *testing.T) {
	ag := func(msg string) error {
		r := &recorder{out: msg}
		return Agterm{Bin: "agtermctl", Run: r.run}.Close(context.Background(), SessionRef{ID: "s9"})
	}
	if err := ag(`{"ok":false,"error":"no such session: s9"}`); !errors.Is(err, ErrGone) {
		t.Errorf("agterm no such session: %v, want ErrGone", err)
	}
	if err := ag(`{"ok":false,"error":"busy"}`); err == nil || errors.Is(err, ErrGone) {
		t.Errorf("agterm other failure: %v, want a plain error", err)
	}
	tm := func(msg string) error {
		r := &recorder{fail: func([]string) bool { return true }, err: msg}
		return Tmux{Bin: "tmux", Run: r.run}.Close(context.Background(), SessionRef{ID: "@4", Workspace: "proj"})
	}
	for _, msg := range []string{"tmux: can't find window: @4", "can't find session: proj", "no server running on /tmp/tmux-1/default"} {
		if err := tm(msg); !errors.Is(err, ErrGone) {
			t.Errorf("tmux %q: %v, want ErrGone", msg, err)
		}
	}
	for _, msg := range []string{"exec: tmux: command not found", "tmux: executable file not found in $PATH", "tmux: permission denied"} {
		if err := tm(msg); err == nil || errors.Is(err, ErrGone) {
			t.Errorf("tmux %q: %v, want a plain error", msg, err)
		}
	}
	// agtermctl's own words for a failed command that never reached the app.
	if err := ag(`agtermctl: command not found`); err == nil || errors.Is(err, ErrGone) {
		t.Errorf("agterm unreadable answer: %v, want a plain error", err)
	}
}

func TestPickUpOrder(t *testing.T) {
	cases := []struct {
		name     string
		env      map[string]string
		up, tmux bool
		want     string // agterm | tmux-inside | tmux-outside | none
	}{
		{"agterm session wins", map[string]string{"AGTERM_SESSION_ID": "s1", "TMUX": "x"}, true, true, "agterm"},
		{"inside tmux", map[string]string{"TMUX": "x"}, true, true, "tmux-inside"},
		{"agterm running", nil, true, true, "agterm"},
		{"tmux installed", nil, false, true, "tmux-outside"},
		{"nothing", nil, false, false, "none"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l, err := PickUp("auto", envOf(c.env), func() bool { return c.up }, func(string) bool { return c.tmux })
			got := "none"
			switch v := l.(type) {
			case Agterm:
				got = "agterm"
			case Tmux:
				got = "tmux-outside"
				if v.Inside {
					got = "tmux-inside"
				}
			}
			if got != c.want {
				t.Errorf("got %s (%v), want %s", got, err, c.want)
			}
			if c.want == "none" && !errors.Is(err, ErrNoLauncher) {
				t.Errorf("err = %v, want ErrNoLauncher", err)
			}
		})
	}
	// A configured launcher is not second-guessed.
	l, err := PickUp("tmux", envOf(nil), func() bool { return true }, func(string) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := l.(Tmux); !ok {
		t.Errorf("got %T, want Tmux", l)
	}
	l, err = PickUp("agterm", envOf(nil), func() bool { return false }, func(string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := l.(Agterm); !ok {
		t.Errorf("got %T, want Agterm", l)
	}
}

func readTree(t *testing.T) string {
	t.Helper()
	tree, err := os.ReadFile("testdata/agterm-tree.json")
	if err != nil {
		t.Fatal(err)
	}
	return string(tree)
}

func TestAgtermWorkspaces(t *testing.T) {
	r := &recorder{out: readTree(t)}
	got, err := Agterm{Bin: "agtermctl", Run: r.run}.Workspaces(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Workspace{
		{ID: "w1", Name: "alpha", Sessions: []SessionRef{
			{Launcher: "agterm", ID: "s1", Name: "coordinator", Cwd: "/work/repo", Foreground: "claude --resume 00000000-0000-4000-8000-000000000001"},
			{Launcher: "agterm", ID: "s2", Name: "task-1", Idle: true, Cwd: "/work/wt"},
		}},
		{ID: "w2", Name: "beta", Sessions: []SessionRef{{Launcher: "agterm", ID: "s3", Name: "other"}}},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].ID != want[i].ID || got[i].Name != want[i].Name || !slices.Equal(got[i].Sessions, want[i].Sessions) {
			t.Errorf("workspace %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if len(r.calls) != 1 || !slices.Equal(r.first(), []string{"tree", "--json"}) {
		t.Errorf("ran %q, want one tree --json", r.calls)
	}
}

func TestAgtermSessionsIdle(t *testing.T) {
	r := &recorder{out: readTree(t)}
	got, err := Agterm{Bin: "agtermctl", Run: r.run}.Sessions(context.Background(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Idle || !got[1].Idle {
		t.Errorf("got %+v, want only the second idle", got)
	}
}

func TestFindSession(t *testing.T) {
	ws := []Workspace{
		{ID: "w1", Name: "alpha", Sessions: []SessionRef{{ID: "s1"}}},
		{ID: "w2", Name: "beta", Sessions: []SessionRef{{ID: "s3"}}},
	}
	if w, ok := FindSession(ws, "s3"); !ok || w.ID != "w2" {
		t.Errorf("FindSession(s3) = %+v, %v", w, ok)
	}
	if w, ok := FindSession(ws, "nope"); ok {
		t.Errorf("FindSession(nope) = %+v, true", w)
	}
}

func TestAgtermMoveArgs(t *testing.T) {
	r := &recorder{out: `{"ok":true,"result":{}}`}
	if err := (Agterm{Bin: "agtermctl", Run: r.run}).Move(context.Background(), "s-1", "w-9"); err != nil {
		t.Fatal(err)
	}
	want := []string{"session", "move", "w-9", "--target", "s-1", "--json"}
	if len(r.calls) != 1 || !slices.Equal(r.first(), want) {
		t.Errorf("ran %q\nwant %q", r.calls, want)
	}
	r = &recorder{out: `{"ok":false,"error":"no such workspace"}`}
	if err := (Agterm{Bin: "agtermctl", Run: r.run}).Move(context.Background(), "s-1", "w-9"); err == nil {
		t.Error("Move succeeded on ok:false")
	}
}

func TestAgtermWorkspaceNew(t *testing.T) {
	r := &recorder{out: `{"ok":true,"result":{"id":"W-1"}}`}
	id, err := (Agterm{Bin: "agtermctl", Run: r.run}).WorkspaceNew(context.Background(), "repo")
	want := []string{"workspace", "new", "--json", "--", "repo"}
	if err != nil || id != "W-1" || !slices.Equal(r.first(), want) {
		t.Errorf("id %q err %v, ran %q, want %q", id, err, r.calls, want)
	}
	for _, out := range []string{`{"ok":false,"error":"refused"}`, `{"ok":true,"result":{}}`} {
		r = &recorder{out: out}
		if _, err := (Agterm{Bin: "agtermctl", Run: r.run}).WorkspaceNew(context.Background(), "repo"); err == nil {
			t.Errorf("WorkspaceNew succeeded on %s", out)
		}
	}
}

func TestAgtermReuseArgs(t *testing.T) {
	r := &recorder{out: `{"ok":true,"result":{}}`}
	ref := SessionRef{Launcher: "agterm", ID: "s-1", Name: "w-one", Idle: true}
	got, err := Agterm{Bin: "agtermctl", Run: r.run}.Reuse(context.Background(), ref, "/tmp/a b/x.sh")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 2 {
		t.Fatalf("calls = %q, want type then restore", r.calls)
	}
	// Ctrl-U (0x15) first: leftover input on the line never runs with it.
	wantType := []string{"session", "type", "--target", "s-1", "--json", "\x15/bin/sh '/tmp/a b/x.sh'\n"}
	if !slices.Equal(r.calls[0], wantType) {
		t.Errorf("type ran %q\nwant %q", r.calls[0], wantType)
	}
	wantPin := []string{"session", "restore", "--none", "--target", "s-1", "--json"}
	if !slices.Equal(r.calls[1], wantPin) {
		t.Errorf("pin ran %q\nwant %q", r.calls[1], wantPin)
	}
	if got.Launcher != "agterm" || got.ID != "s-1" || got.Name != "w-one" || got.Warning != "" {
		t.Errorf("ref = %+v", got)
	}
}

func TestAgtermReuseNotReadyRetriesSelect(t *testing.T) {
	n := 0
	r := &recorder{}
	run := func(ctx context.Context, bin string, args ...string) ([]byte, error) {
		r.calls = append(r.calls, args)
		if args[1] == "type" {
			n++
			if n == 1 {
				return []byte(`{"ok":false,"error":"session not realized"}`), nil
			}
		}
		return []byte(`{"ok":true,"result":{}}`), nil
	}
	if _, err := (Agterm{Bin: "agtermctl", Run: run}).Reuse(context.Background(), SessionRef{ID: "s-1", Name: "n"}, "/x.sh"); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 3 {
		t.Fatalf("calls = %q, want type, type --select, restore", r.calls)
	}
	want := []string{"session", "type", "--select", "--target", "s-1", "--json", "\x15/bin/sh /x.sh\n"}
	if !slices.Equal(r.calls[1], want) {
		t.Errorf("retry ran %q\nwant %q", r.calls[1], want)
	}
}

func TestAgtermReuseFails(t *testing.T) {
	// A failure that is not "not ready" is not retried.
	r := &recorder{out: `{"ok":false,"error":"no such session"}`}
	if _, err := (Agterm{Bin: "agtermctl", Run: r.run}).Reuse(context.Background(), SessionRef{ID: "s-1"}, "/x.sh"); !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	if len(r.calls) != 1 {
		t.Errorf("calls = %q, want one type call", r.calls)
	}
	// Not ready twice: both calls fail, no pin.
	r = &recorder{out: `{"ok":false,"error":"session not realized"}`}
	if _, err := (Agterm{Bin: "agtermctl", Run: r.run}).Reuse(context.Background(), SessionRef{ID: "s-1"}, "/x.sh"); !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	if len(r.calls) != 2 {
		t.Fatalf("calls = %q, want two type calls", r.calls)
	}
	for _, c := range r.calls {
		if c[1] == "restore" {
			t.Errorf("pin called after failure: %q", c)
		}
	}
}

func TestAgtermReuseRealRefusals(t *testing.T) {
	// agterm's own texts (its reference.md): a target that matches nothing
	// is notFound, not retried; a surface that never comes up is "session
	// not realized", retried once with --select. Both are ErrRefused.
	for _, tc := range []struct {
		answer string
		calls  int
	}{{`{"ok":false,"error":"notFound"}`, 1}, {`{"ok":false,"error":"session not realized"}`, 2}, {`{"ok":false,"error":"no such session: s-1"}`, 1}} {
		r := &recorder{out: tc.answer}
		_, err := (Agterm{Bin: "agtermctl", Run: r.run}).Reuse(context.Background(), SessionRef{ID: "s-1"}, "/x.sh")
		if !errors.Is(err, ErrRefused) || len(r.calls) != tc.calls {
			t.Errorf("%s: err %v after %d calls, want ErrRefused after %d", tc.answer, err, len(r.calls), tc.calls)
		}
	}
}

func TestAgtermReuseOtherErrorNotRefused(t *testing.T) {
	// A runner failure or unreadable output is not a definite refusal: the
	// tab may have received the line (finding 9).
	for _, r := range []*recorder{
		{fail: func([]string) bool { return true }, err: "agtermctl: signal: killed"},
		{out: "garbage"},
		{out: `{"ok":false,"error":"internal error"}`},
	} {
		_, err := (Agterm{Bin: "agtermctl", Run: r.run}).Reuse(context.Background(), SessionRef{ID: "s-1"}, "/x.sh")
		if err == nil || errors.Is(err, ErrRefused) {
			t.Errorf("err = %v, want an error that is not ErrRefused", err)
		}
	}
}

func TestAgtermReusePinWarns(t *testing.T) {
	run := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if args[1] == "restore" {
			return []byte(`{"ok":false,"error":"pin broke"}`), nil
		}
		return []byte(`{"ok":true,"result":{}}`), nil
	}
	ref, err := Agterm{Bin: "agtermctl", Run: run}.Reuse(context.Background(), SessionRef{ID: "s-1", Name: "n"}, "/x.sh")
	if err != nil {
		t.Fatalf("err = %v, want none", err)
	}
	if ref.ID != "s-1" || !strings.Contains(ref.Warning, "pin broke") {
		t.Errorf("ref = %+v, want a pin warning", ref)
	}
}

func TestTmuxNotReuser(t *testing.T) {
	if _, ok := any(Tmux{}).(Reuser); ok {
		t.Error("Tmux implements Reuser")
	}
	if _, ok := any(Agterm{}).(Reuser); !ok {
		t.Error("Agterm does not implement Reuser")
	}
}

func TestTmuxPaneOf(t *testing.T) {
	// A non-ASCII session name with spaces comes back whole: -u keeps a
	// client that is not UTF-8 from printing it with "_" (finding 15).
	r := &recorder{out: "%3|qm|@4|qm|0|qm||qm|Shop v2 · next|qm|w one\n"}
	sess, win, name, err := Tmux{Bin: "tmux", Run: r.run}.PaneOf(context.Background(), "%3")
	if err != nil {
		t.Fatal(err)
	}
	if sess != "Shop v2 · next" || win != "@4" || name != "w one" {
		t.Errorf("got %q %q %q", sess, win, name)
	}
	want := []string{"-u", "display-message", "-p", "-t", "%3", "#{pane_id}|qm|#{window_id}|qm|#{window_linked}|qm|#{session_group_size}|qm|#{session_name}|qm|#{window_name}"}
	if !slices.Equal(r.first(), want) {
		t.Errorf("ran %q\nwant %q", r.first(), want)
	}
	r = &recorder{out: "%3|qm|@4|qm|1|qm||qm|proj|qm|w\n"}
	if _, _, _, err := (Tmux{Bin: "tmux", Run: r.run}).PaneOf(context.Background(), "%3"); !errors.Is(err, ErrLinked) {
		t.Errorf("linked window: %v, want ErrLinked", err)
	}
	for _, bad := range []string{"", "|qm||qm||qm||qm||qm|\n", "@4|qm|proj\n", "proj\t@4\tw one\n", "%3|qm|@4|qm|0|qm||qm|a|qm|b|qm|c\n", "%3|qm|x|qm|0|qm||qm|a|qm|b\n", "%3|qm|@4|qm|0|qm||qm|$1|qm|b\n", "%1|qm|@4|qm|0|qm||qm|a|qm|b\n", "%3|qm|@4|qm|0|qm|2|qm|a|qm|b\n"} {
		r := &recorder{out: bad}
		if _, _, _, err := (Tmux{Bin: "tmux", Run: r.run}).PaneOf(context.Background(), "%3"); err == nil {
			t.Errorf("PaneOf accepted %q", bad)
		}
	}
	r = &recorder{fail: func([]string) bool { return true }}
	if _, _, _, err := (Tmux{Bin: "tmux", Run: r.run}).PaneOf(context.Background(), "%3"); err == nil {
		t.Error("PaneOf ignored a runner error")
	}
}

// A name tmux would change, expand or run is never given to a new session or
// window; one a target would not name is refused before tmux runs; a folder's
// is escaped.
func TestTmuxNames(t *testing.T) {
	ctx := context.Background()
	for _, n := range []string{"", "a#(id)b", "#S", `a\b`, "tab\tx", "\x1b[31m", "bad\xff", "end;", "a|qm|b", "my.site", "a:b", "$1"} {
		r := &recorder{out: "@1\n", fail: func(a []string) bool { return a[0] == "has-session" }}
		tm := Tmux{Bin: "tmux", Run: r.run}
		if n != "" { // no workspace asked for
			if _, err := tm.Spawn(ctx, LaunchSpec{Name: "w", Cwd: "/w", Script: "/s.sh", WorkspaceName: n}); err == nil || !strings.Contains(err.Error(), "tmux session") {
				t.Errorf("Spawn in %q: %v", n, err)
			}
		}
		if _, err := tm.Spawn(ctx, LaunchSpec{Name: n, Cwd: "/w", Script: "/s.sh"}); err == nil {
			t.Errorf("Spawn of session %q: no error", n)
		}
		for _, c := range r.calls {
			if c[0] != "has-session" {
				t.Errorf("%q: tmux ran %q", n, c)
			}
		}
	}
	for _, n := range []string{"", "end;", "a|qm|b", "my.site", "a:b", "$1"} {
		r := &recorder{out: "@1\n"}
		tm := Tmux{Bin: "tmux", Run: r.run}
		if _, err := tm.Sessions(ctx, n); err == nil {
			t.Errorf("Sessions(%q): no error", n)
		}
		if err := tm.Close(ctx, SessionRef{ID: "@1", Name: "w", Workspace: n}); err == nil || errors.Is(err, ErrGone) {
			t.Errorf("Close in %q: %v", n, err)
		}
		if len(r.calls) != 0 {
			t.Errorf("%q: tmux ran %q", n, r.calls)
		}
	}
	// A session that exists is listed, and given windows, whatever tmux would
	// make of its name in a new one.
	for _, n := range []string{"issue #12", "a\u200db", `a\b`} {
		r := &recorder{out: "@4|qm|$1|qm|1|qm|0|qm|0|qm|9|qm|/dev/pts/1|qm|claude|qm|w\n"}
		if refs, err := (Tmux{Bin: "tmux", Run: r.run}).Sessions(ctx, n); err != nil || len(refs) != 1 {
			t.Errorf("Sessions(%q) = %v, %v", n, refs, err)
		}
		r = &recorder{out: "@1\n"}
		if _, err := (Tmux{Bin: "tmux", Run: r.run}).Spawn(ctx, LaunchSpec{Name: "w", Cwd: "/w", Script: "/s.sh", WorkspaceName: n}); err != nil {
			t.Errorf("Spawn in existing %q: %v", n, err)
		}
	}
	r := &recorder{out: "@1\n"}
	if _, err := (Tmux{Bin: "tmux", Run: r.run}).Spawn(ctx, LaunchSpec{Name: "w", Cwd: "/w/#(id)/end;", Script: "/s.sh", WorkspaceName: "Shop v2 · next"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(r.calls[1], `/w/##(id)/end\;`) {
		t.Errorf("ran %q, want the folder escaped", r.calls[1])
	}
}

var _ Locator = Tmux{}

// fakeTmux answers each tmux command, and ps, with a fixed text.
func fakeTmux(answers map[string]string, errs map[string]string, ran *[]string) Tmux {
	return Tmux{Bin: "tmux", Run: func(_ context.Context, bin string, args ...string) ([]byte, error) {
		cmd := bin
		if bin == "tmux" {
			if cmd = args[0]; cmd == "-u" {
				cmd = args[1]
			}
			if cmd == "list-windows" && slices.Contains(args, "-a") {
				cmd = "list-all"
			}
		}
		*ran = append(*ran, cmd)
		if msg, ok := errs[cmd]; ok {
			return nil, errors.New(msg)
		}
		return []byte(answers[cmd]), nil
	}}
}

// Close kills only the window it was given, as it was listed.
func TestTmuxCloseReadsAgain(t *testing.T) {
	const line = "@4|qm|$1|qm|1|qm|0|qm|0|qm|100|qm|/dev/pts/1|qm|bash|qm|w\n"
	ref := SessionRef{Launcher: "tmux", ID: "@4", Name: "w", Workspace: "proj", Panes: 1, PanePID: 100}
	idle := ref
	idle.Idle = true
	for _, c := range []struct {
		name    string
		ref     SessionRef
		answers map[string]string
		errs    map[string]string
		why     string // the ChangedError's, "gone", or "" for a kill
		ps      bool
	}{
		{"as listed", ref, map[string]string{"list-windows": line}, nil, "", false},
		{"still idle", idle, map[string]string{"list-windows": line, "ps": "100 1 Ss+ pts/1 -bash\n"}, nil, "", true},
		{"in use", idle, map[string]string{"list-windows": line, "ps": "100 1 Ss pts/1 -bash\n200 100 S+ pts/1 vim\n"}, nil, "runs something now", true},
		{"renamed", ref, map[string]string{"list-windows": strings.Replace(line, "|w\n", "|vim\n", 1)}, nil, `named "vim" now`, false},
		{"linked", ref, map[string]string{"list-windows": strings.Replace(line, "|0|qm|100", "|1|qm|100", 1)}, nil, LinkedWhy, false},
		{"split", ref, map[string]string{"list-windows": strings.Replace(line, "$1|qm|1", "$1|qm|2", 1)}, nil, "has 2 panes", false},
		{"respawned", ref, map[string]string{"list-windows": strings.Replace(line, "|100|", "|101|", 1)}, nil, "started anew", false},
		{"moved", ref, map[string]string{"list-all": "@40|qm|x|qm|w\n@4|qm|mine|qm|w\n"}, nil, "now in tmux session mine", false},
		{"moved and renamed", ref, map[string]string{"list-all": "@4|qm|mine|qm|vim|qm|x\n"}, nil, `now "vim|qm|x" in tmux session mine`, false},
		{"gone", ref, map[string]string{"list-all": "@5|qm|mine|qm|w\n"}, nil, "gone", false},
		{"its session gone", ref, nil, map[string]string{"list-windows": "can't find session: proj", "list-all": "no server running on /s"}, "gone", false},
		{"gone at the kill", ref, map[string]string{"list-windows": line}, map[string]string{"if-shell": "can't find window: @4"}, "gone", false},
		{"changed at the kill", ref, map[string]string{"list-windows": line, "if-shell": "changed\n"}, nil, "right before its close", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			var ran []string
			err := fakeTmux(c.answers, c.errs, &ran).Close(context.Background(), c.ref)
			var ch *ChangedError
			switch {
			case c.why == "":
				if err != nil {
					t.Errorf("close: %v", err)
				}
			case c.why == "gone":
				if !errors.Is(err, ErrGone) {
					t.Errorf("close: %v, want ErrGone", err)
				}
			case !errors.As(err, &ch) || !strings.Contains(ch.Why, c.why):
				t.Errorf("close: %v, want it left open: %s", err, c.why)
			}
			kills := c.why == "" || strings.HasSuffix(c.name, "at the kill")
			if slices.Contains(ran, "if-shell") != kills || slices.Contains(ran, "ps") != c.ps {
				t.Errorf("ran %q", ran)
			}
		})
	}
}

// What tmux says when a window is gone is told from what it says of a socket
// that may not be used.
func TestTmuxNotThere(t *testing.T) {
	ctx := context.Background()
	var ran []string
	denied := fakeTmux(nil, map[string]string{"list-windows": "error connecting to /s (Permission denied)", "list-all": "error connecting to /s (Permission denied)", "list-panes": "error connecting to /s (Permission denied)"}, &ran)
	if refs, err := denied.Sessions(ctx, "proj"); err == nil {
		t.Errorf("Sessions: %v", refs)
	}
	if refs, err := denied.Locate(ctx, SessionRef{ID: "@4"}); err == nil {
		t.Errorf("Locate: %v", refs)
	}
	if n, err := denied.WindowPanes(ctx, "@4"); err == nil || errors.Is(err, ErrGone) {
		t.Errorf("WindowPanes: %d, %v", n, err)
	}
	for _, msg := range []string{"can't find window: @4", "can't find pane: %4", "can't find session: proj", "no server running on /s"} {
		gone := fakeTmux(nil, map[string]string{"list-panes": msg}, &ran)
		if tty, err := gone.PaneTTY(ctx, "=proj:@4"); !errors.Is(err, ErrGone) {
			t.Errorf("%s: %q, %v, want ErrGone", msg, tty, err)
		}
	}
}

func TestTmuxPanesRead(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct{ out, target, tty string }{
		{"@4|qm|%3|qm|0|qm|/dev/pts/7\n", "@4", "pts/7"},
		{"@4|qm|%3|qm|0|qm|/dev/pts/7\n", "=proj:@4", "pts/7"},
		{"@4|qm|%3|qm|1|qm|/dev/pts/7\n", "@4", ""},                                   // its command ended
		{"@4|qm|%3|qm|0|qm|/dev/pts/7\n@4|qm|%5|qm|0|qm|/dev/pts/8\n", "@4", ""},      // two panes
		{"@4|qm|%3|qm|0|qm|/dev/pts/7\n@4|qm|%5|qm|0|qm|/dev/pts/8\n", "%5", "pts/8"}, // one of them
		{"@6|qm|%3|qm|0|qm|/dev/pts/7\n", "@4", "error"},                              // another window
		{"@4|qm|%3|qm|0|qm|/dev/pts/7\n", "%9", "error"},                              // another pane
		{"", "@4", "error"},
		{"junk\n", "@4", "error"},
	} {
		var ran []string
		got, err := fakeTmux(map[string]string{"list-panes": c.out}, nil, &ran).PaneTTY(ctx, c.target)
		if err != nil {
			got = "error"
		}
		if got != c.tty {
			t.Errorf("PaneTTY(%s) of %q = %q, %v, want %q", c.target, c.out, got, err, c.tty)
		}
	}
	var ran []string
	two := fakeTmux(map[string]string{"list-panes": "@4|qm|%3|qm|0|qm|/dev/pts/7\n@4|qm|%5|qm|1|qm|/dev/pts/8\n", "display-message": "%3 @4\n"}, nil, &ran)
	if n, err := two.WindowPanes(ctx, "@4"); err != nil || n != 2 {
		t.Errorf("WindowPanes = %d, %v", n, err)
	}
	if w, err := two.WindowOf(ctx, "%3"); err != nil || w != "@4" {
		t.Errorf("WindowOf = %q, %v", w, err)
	}
	if w, err := two.WindowOf(ctx, "%9"); err == nil {
		t.Errorf("WindowOf answered for another pane: %q", w)
	}
}
