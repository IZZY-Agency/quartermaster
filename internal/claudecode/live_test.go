package claudecode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // America/New_York without the system's zone files
)

const (
	id101 = "00000000-0000-4000-8000-000000000101"
	id202 = "00000000-0000-4000-8000-000000000202"
)

// cet is the zone ps prints lstart in for these tests: one hour ahead of the
// UTC procStart in 101.json ("Fri Nov 20 08:00:00 2026").
var cet = time.FixedZone("CET", 3600)

func TestLiveMatchesProcStart(t *testing.T) {
	dir, root := claudeDir(t)
	procs := []Proc{{PID: 101, Start: "Fri Nov 20 09:00:01 2026", Cmd: "claude --dangerously-skip-permissions"}}
	got, err := live(context.Background(), dir, procs, cet)
	if err != nil {
		t.Fatal(err)
	}
	want := LiveSession{PID: 101, SessionID: id101, Cwd: filepath.Join(root, "alpha"), Name: "coordinator"}
	if got[id101] != want {
		t.Errorf("Live()[101] = %+v, want %+v", got[id101], want)
	}
	if len(got) != 1 {
		t.Errorf("Live() = %+v, want only 101 (202 has no process)", got)
	}
}

func TestLiveRejectsReusedPID(t *testing.T) {
	dir, _ := claudeDir(t)
	for _, start := range []string{
		"Fri Nov 20 09:00:05 2026", // five seconds later
		"Fri Nov 20 08:00:00 2026", // the procStart text, read in the local zone
		"Sat Nov 21 09:00:00 2026", // a day later, after a reboot
	} {
		procs := []Proc{{PID: 101, Start: start, Cmd: "claude"}}
		got, err := live(context.Background(), dir, procs, cet)
		if err != nil {
			t.Fatal(err)
		}
		if s, ok := got[id101]; ok {
			t.Errorf("start %q: Live()[101] = %+v, want not live", start, s)
		}
	}
}

func TestSameStartRepeatedHour(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	// Daylight saving ends on Sun Nov 1 2026 at 02:00 EDT: 01:30 happens
	// twice, at 05:30 UTC (EDT) and 06:30 UTC (EST).
	lstart := "Sun Nov  1 01:30:00 2026"
	for _, tc := range []struct {
		procStart string
		want      bool
	}{
		{"Sun Nov  1 05:30:00 2026", true},
		{"Sun Nov  1 06:30:01 2026", true},
		{"Sun Nov  1 07:30:00 2026", false},
		{"Sun Nov  1 04:30:00 2026", false},
	} {
		if got := sameStart(tc.procStart, lstart, ny); got != tc.want {
			t.Errorf("sameStart(%q, %q) = %v, want %v", tc.procStart, lstart, got, tc.want)
		}
	}
	// An hour away from the change has one reading only.
	if !sameStart("Sun Nov  1 08:00:00 2026", "Sun Nov  1 03:00:00 2026", ny) ||
		sameStart("Sun Nov  1 07:00:00 2026", "Sun Nov  1 03:00:00 2026", ny) {
		t.Error("sameStart after the change: want only the EST reading")
	}
	// A reading skipped when daylight saving starts still parses.
	if sameStart("Sun Mar  8 12:00:00 2026", "Sun Mar  8 02:30:00 2026", ny) {
		t.Error("sameStart(skipped hour) matched an unrelated instant")
	}
}

func TestLiveRejectsNonClaudePID(t *testing.T) {
	dir, _ := claudeDir(t)
	procs := []Proc{
		{PID: 101, Start: "Fri Nov 20 09:00:00 2026", Cmd: "/bin/zsh"},
		{PID: 202, Start: "Fri Nov 20 09:00:00 2026", Cmd: "/bin/sh -c claude --resume " + id202},
	}
	got, err := live(context.Background(), dir, procs, cet)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("Live() = %+v, want nothing live", got)
	}
}

func TestLiveWithoutProcStart(t *testing.T) {
	dir, root := claudeDir(t)
	procs := []Proc{{PID: 202, Start: "Thu Nov 19 10:00:00 2026", Cmd: "/opt/bin/claude -n w1"}}
	got, err := Live(dir, procs)
	if err != nil {
		t.Fatal(err)
	}
	want := LiveSession{PID: 202, SessionID: id202, Cwd: filepath.Join(root, "alpha", ".claude", "worktrees", "w1"), Name: "w1"}
	if got[id202] != want {
		t.Errorf("Live()[202] = %+v, want %+v", got[id202], want)
	}
}

func TestLiveFromArgs(t *testing.T) {
	dir := t.TempDir() // a Claude Code folder with an empty sessions folder
	if err := os.Mkdir(filepath.Join(dir, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	start := "Fri Nov 20 09:00:00 2026"
	procs := []Proc{
		{PID: 1, Start: start, Cmd: "claude --resume id-1 -n w1"},
		{PID: 2, Start: start, Cmd: "claude --model m --session-id id-2"},
		{PID: 3, Start: start, Cmd: "/usr/local/bin/claude -r id-3"},
		{PID: 4, Start: start, Cmd: "claude --resume=id-4 --name=w4"},
		{PID: 5, Start: start, Cmd: "claude --session-id=id-5"},
		// Not claude itself.
		{PID: 6, Start: start, Cmd: "/bin/zsh -c claude --resume id-6"},
		// A prompt quoting the flag after "--" or past its first line.
		{PID: 7, Start: start, Cmd: "claude -n w7 -- Do not run claude --resume id-7"},
		{PID: 8, Start: start, Cmd: `claude -n w8 First line.\012Then claude --resume id-8`},
		// --resume with no id opens the picker.
		{PID: 9, Start: start, Cmd: "claude --resume --model m"},
	}
	got, err := Live(dir, procs)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]LiveSession{
		"id-1": {PID: 1, SessionID: "id-1", Name: "w1"},
		"id-2": {PID: 2, SessionID: "id-2"},
		"id-3": {PID: 3, SessionID: "id-3"},
		"id-4": {PID: 4, SessionID: "id-4", Name: "w4"},
		"id-5": {PID: 5, SessionID: "id-5"},
	}
	if len(got) != len(want) {
		t.Errorf("Live() = %+v, want %+v", got, want)
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("Live()[%s] = %+v, want %+v", id, got[id], w)
		}
	}
}

func TestSessionsNote(t *testing.T) {
	dir := t.TempDir()
	if n := SessionsNote(dir); !strings.Contains(n, "missing") {
		t.Errorf("no sessions folder: note %q", n)
	}
	if err := os.WriteFile(filepath.Join(dir, "sessions"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if n := SessionsNote(dir); !strings.Contains(n, "not read") {
		t.Errorf("sessions is a file: note %q", n)
	}
	if err := os.Remove(filepath.Join(dir, "sessions")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if n := SessionsNote(dir); n != "" {
		t.Errorf("readable folder: note %q", n)
	}
}

func TestLiveNoSessionsDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	procs := []Proc{{PID: 1, Start: "Fri Nov 20 09:00:00 2026", Cmd: "claude --resume id-1"}}
	got, err := Live(dir, procs)
	if err != nil {
		t.Fatalf("Live() error = %v, want none without a sessions folder", err)
	}
	if want := (LiveSession{PID: 1, SessionID: "id-1"}); len(got) != 1 || got["id-1"] != want {
		t.Errorf("Live() = %+v, want only %+v", got, want)
	}
}

func TestLiveSkipsUnreadableFile(t *testing.T) {
	dir, _ := claudeDir(t)
	if err := os.WriteFile(filepath.Join(dir, "sessions", "303.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	procs := []Proc{
		{PID: 101, Start: "Fri Nov 20 09:00:00 2026", Cmd: "claude"},
		{PID: 303, Start: "Fri Nov 20 09:00:00 2026", Cmd: "claude"},
	}
	got, err := live(context.Background(), dir, procs, cet)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[id101].PID != 101 {
		t.Errorf("Live() = %+v, want 101 only", got)
	}
}

func TestProcsParse(t *testing.T) {
	out := []byte("  101     1 Fri Nov 20 09:00:00 2026     claude --resume id-1 -n w1\n" +
		"    7     1 Sun Nov  1 08:05:09 2026     /bin/zsh -l\n" +
		"12345   101 Mon Nov 16 21:44:17 2026     /Applications/My Tools/claude  --model m\n" +
		"garbage line\n" +
		"  9 x Mon Nov 16 21:44:17 2026 bad ppid\n")
	want := []Proc{
		{PID: 101, PPID: 1, Start: "Fri Nov 20 09:00:00 2026", Cmd: "claude --resume id-1 -n w1"},
		{PID: 7, PPID: 1, Start: "Sun Nov  1 08:05:09 2026", Cmd: "/bin/zsh -l"},
		{PID: 12345, PPID: 101, Start: "Mon Nov 16 21:44:17 2026", Cmd: "/Applications/My Tools/claude  --model m"},
	}
	if got := ParseProcs(out); !slices.Equal(got, want) {
		t.Errorf("ParseProcs() =\n%+v\nwant\n%+v", got, want)
	}
}

func TestAncestorFindsClaude(t *testing.T) {
	procs := []Proc{
		{PID: 500, PPID: 400, Cmd: "/bin/sh"},
		{PID: 400, PPID: 300, Cmd: "zsh"},
		{PID: 300, PPID: 1, Cmd: "claude -n c"},
		{PID: 1, PPID: 0, Cmd: "/sbin/launchd"},
	}
	got, ok := Ancestor(procs, 500)
	if !ok || got.PID != 300 {
		t.Errorf("Ancestor(500) = %+v, %v, want pid 300", got, ok)
	}
}

func TestAncestorSelfIsClaude(t *testing.T) {
	procs := []Proc{{PID: 300, PPID: 1, Cmd: "claude -n c"}, {PID: 1, Cmd: "launchd"}}
	if got, ok := Ancestor(procs, 300); !ok || got.PID != 300 {
		t.Errorf("Ancestor(300) = %+v, %v, want itself", got, ok)
	}
}

func TestAncestorNone(t *testing.T) {
	procs := []Proc{
		{PID: 500, PPID: 400, Cmd: "/bin/sh"},
		{PID: 400, PPID: 1, Cmd: "zsh"},
		{PID: 1, PPID: 0, Cmd: "launchd"},
	}
	if got, ok := Ancestor(procs, 500); ok {
		t.Errorf("Ancestor(500) = %+v, true, want false", got)
	}
	if _, ok := Ancestor(procs, 999); ok {
		t.Error("Ancestor of an unlisted pid = true, want false")
	}
}

func TestAncestorLoopBounded(t *testing.T) {
	procs := []Proc{{PID: 10, PPID: 11, Cmd: "a"}, {PID: 11, PPID: 10, Cmd: "b"}}
	if got, ok := Ancestor(procs, 10); ok {
		t.Errorf("Ancestor in a loop = %+v, true, want false", got)
	}
}

func TestIsClaudePathWithSpaces(t *testing.T) {
	old := statFile
	statFile = func(p string) bool { return p == "/Applications/My Tools/claude" }
	t.Cleanup(func() { statFile = old })
	if args, ok := IsClaude("/Applications/My Tools/claude --model m"); !ok || !slices.Equal(args, []string{"--model", "m"}) {
		t.Errorf("IsClaude() = %q, %v, want [--model m], true", args, ok)
	}
	if _, ok := IsClaude("/bin/sh -c /Applications/My Tools/claude --model m"); ok {
		t.Error("IsClaude(a shell running claude) = true, want false")
	}
}

// ProcsCtx stops once its context ends, and lists the processes otherwise.
func TestProcsCtxStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ProcsCtx(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, want context.Canceled", err)
	}
	procs, err := ProcsCtx(context.Background())
	if err != nil || len(procs) == 0 {
		t.Fatalf("procs %d, err %v", len(procs), err)
	}
}

// LiveCtx stops between session files once its context ends.
func TestLiveCtxStopsWhenCancelled(t *testing.T) {
	dir := filepath.Join("testdata", "cancel") // one session file, pid 7
	procs := []Proc{{PID: 7, Cmd: "claude"}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := LiveCtx(ctx, dir, procs); !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	if got, err := LiveCtx(context.Background(), dir, procs); err != nil || got["00000000-0000-4000-8000-000000000007"].PID != 7 {
		t.Fatalf("got %v err %v", got, err)
	}
}

// parseForeground reads ps's tty, pgid and tpgid: a process is in its
// terminal's foreground when its group is the terminal's foreground group.
func TestParseForeground(t *testing.T) {
	for _, c := range []struct {
		out  string
		tty  string
		fg   bool
		fail bool
	}{
		{"ttys012  4242  4242\n", "ttys012", true, false},
		{"ttys012  4242  5151\n", "ttys012", false, false},
		{"??      4242     0\n", "", false, false},
		{"pts/3 77 77", "pts/3", true, false},
		{"", "", false, true},
		{"ttys012 x 1", "", false, true},
	} {
		tty, fg, err := parseForeground([]byte(c.out))
		if (err != nil) != c.fail || tty != c.tty || fg != c.fg {
			t.Errorf("parseForeground(%q) = %q, %v, %v", c.out, tty, fg, err)
		}
	}
}
