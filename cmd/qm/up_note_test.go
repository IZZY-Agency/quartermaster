package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
	"github.com/IZZY-Agency/quartermaster/internal/declog"
	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/route"
	"github.com/IZZY-Agency/quartermaster/internal/up"
)

// noteMachine is an upMachine with one project whose coordinator and worker
// do not run, and an agtermctl that records notify.
func noteMachine(t *testing.T) (*upMachine, *[]string) {
	t.Helper()
	m := newUpMachine(t)
	m.worker(t, m.project(t), "w-1")
	var notified []string
	swap(t, &upAgtermCtl, launch.Agterm{Bin: "agtermctl", Run: func(_ context.Context, bin string, args ...string) ([]byte, error) {
		if args[0] != "notify" {
			t.Errorf("ran %s %v", bin, args)
		}
		notified = append(notified, strings.Join(args, "|"))
		return []byte(`{"ok":true,"result":{}}`), nil
	}})
	t.Setenv("AGT_SOCKET", "")
	t.Setenv("AGTERM_SOCKET", "")
	// A note works from any folder: it must not ask for one.
	swap(t, &upWorkDir, func() (string, error) {
		t.Error("the working folder was read")
		return "", errors.New("no folder")
	})
	return m, &notified
}

func coordProc(pid int) claudecode.Proc {
	return claudecode.Proc{PID: pid, Cmd: "claude --resume " + upCoordID}
}

func workerProc(pid int) claudecode.Proc {
	return claudecode.Proc{PID: pid, Cmd: "claude --resume " + upWorkerID}
}

func TestUpNoteLists(t *testing.T) {
	m, notified := noteMachine(t)
	code, out, errb := runQM("up", "note")
	if code != 0 || errb != "" {
		t.Fatalf("exit %d, stderr %q", code, errb)
	}
	for _, want := range []string{m.root, "coordinator and up to 1 worker(s) not running", "last active", "cd " + launch.Quote(m.root) + " && qm up"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q: want %q in it", out, want)
		}
	}
	if len(*notified) != 0 || len(m.term.calls) != 0 {
		t.Errorf("ran agtermctl: %v %v", *notified, m.term.calls)
	}
}

func TestUpNotePrintsNothingWhenNothingIsLeft(t *testing.T) {
	m, notified := noteMachine(t)
	swapProcs(t, func() ([]claudecode.Proc, error) { return []claudecode.Proc{coordProc(11), workerProc(12)}, nil })
	for _, args := range [][]string{{"up", "note"}, {"up", "note", "--notify"}} {
		if code, out, errb := runQM(args...); code != 0 || out != "" || errb != "" {
			t.Errorf("%v: exit %d, %q, %q", args, code, out, errb)
		}
	}
	// No project at all.
	if err := os.RemoveAll(filepath.Join(os.Getenv("XDG_STATE_HOME"), "quartermaster")); err != nil {
		t.Fatal(err)
	}
	swapProcs(t, func() ([]claudecode.Proc, error) { return nil, nil })
	if code, out, errb := runQM("up", "note"); code != 0 || out != "" || errb != "" {
		t.Errorf("no project: exit %d, %q, %q", code, out, errb)
	}
	if len(*notified) != 0 || len(m.term.calls) != 0 {
		t.Errorf("ran agtermctl: %v %v", *notified, m.term.calls)
	}
}

func TestUpNoteNotifies(t *testing.T) {
	m, notified := noteMachine(t)
	t.Setenv("AGT_SOCKET", "/tmp/hook.sock")
	t.Setenv("AGTERM_SOCKET", "/tmp/other.sock")
	code, out, errb := runQM("up", "note", "--notify")
	if code != 0 || out != "" || errb != "" || len(*notified) != 1 {
		t.Fatalf("exit %d, %q, %q, notified %q", code, out, errb, *notified)
	}
	got := (*notified)[0]
	for _, want := range []string{filepath.Base(m.root), "qm up", "|--socket|/tmp/hook.sock"} {
		if !strings.Contains(got, want) {
			t.Errorf("notified %q: want %q in it", got, want)
		}
	}
	// No path and no session's text.
	if strings.Contains(got, m.root) || strings.Contains(got, "w-1") || strings.Contains(got, "proj-coord") {
		t.Errorf("notified %q: a path or a session in it", got)
	}
}

// Without the hook's socket, the socket of the agterm qm runs in is used.
func TestUpNoteNotifiesTheAgtermItRunsIn(t *testing.T) {
	_, notified := noteMachine(t)
	t.Setenv("AGTERM_SOCKET", "/tmp/own.sock")
	if code, _, errb := runQM("up", "note", "--notify"); code != 0 || len(*notified) != 1 || !strings.Contains((*notified)[0], "|--socket|/tmp/own.sock") {
		t.Errorf("exit %d, %q, notified %q", code, errb, *notified)
	}
}

func TestUpNoteNotifyFails(t *testing.T) {
	noteMachine(t)
	swap(t, &upAgtermCtl, launch.Agterm{Bin: "agtermctl", Run: func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("connect failed")
	}})
	if code, _, errb := runQM("up", "note", "--notify"); code != 1 || !strings.Contains(errb, "connect failed") {
		t.Errorf("exit %d, %q", code, errb)
	}
}

// Sessions agterm starts again by itself, one pane after another, are not in
// the note: it waits until no claude process came or went for a while.
func TestUpNoteWaitsForSessionsThatStartAgain(t *testing.T) {
	for _, c := range []struct {
		name string
		// procs is the process list at read n (0 is the first one).
		procs func(n int) ([]claudecode.Proc, error)
		want  bool // one notification, of the coordinator's project
	}{
		{"both, one after another", func(n int) ([]claudecode.Proc, error) {
			var ps []claudecode.Proc
			if n >= 2 {
				ps = append(ps, coordProc(11))
			}
			if n >= 2+noteQuiet {
				ps = append(ps, workerProc(12))
			}
			return ps, nil
		}, false},
		{"the coordinator is back long after the other sessions", func(n int) ([]claudecode.Proc, error) {
			ps := []claudecode.Proc{workerProc(12)}
			for i := range min(n, 20) { // the panes of other projects
				ps = append(ps, claudecode.Proc{PID: 100 + i, Cmd: "claude"})
			}
			if n >= 20 {
				ps = append(ps, coordProc(11))
			}
			return ps, nil
		}, false},
		{"one only", func(n int) ([]claudecode.Proc, error) {
			if n >= 2 {
				return []claudecode.Proc{workerProc(12)}, nil
			}
			return nil, nil
		}, true},
		{"never quiet", func(n int) ([]claudecode.Proc, error) {
			return []claudecode.Proc{{PID: 100 + n, Cmd: "claude"}}, nil
		}, false},
		{"unreadable at first", func(n int) ([]claudecode.Proc, error) {
			if n < 3 {
				return nil, errors.New("ps: no")
			}
			return nil, nil
		}, true},
		{"unreadable at the end", func(n int) ([]claudecode.Proc, error) {
			if n >= 3 {
				return nil, errors.New("ps: no")
			}
			return nil, nil
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, notified := noteMachine(t)
			n := 0
			swapProcs(t, func() ([]claudecode.Proc, error) {
				ps, err := c.procs(n)
				n++
				return ps, err
			})
			var slept time.Duration
			swap(t, &upSleep, func(d time.Duration) { slept += d })
			code, out, errb := runQM("up", "note", "--notify")
			if code != 0 || out != "" || (len(*notified) == 1) != c.want || len(*notified) > 1 {
				t.Errorf("exit %d, %q, %q, notified %q, want one: %v", code, out, errb, *notified, c.want)
			}
			if slept > noteMax*noteStep {
				t.Errorf("waited %v, more than %v", slept, noteMax*noteStep)
			}
		})
	}
}

// A project whose state changes while the note waits is being worked on (a
// qm up): it is left out.
func TestUpNoteLeavesOutAProjectBeingRestored(t *testing.T) {
	m, notified := noteMachine(t)
	n := 0
	swapProcs(t, func() ([]claudecode.Proc, error) {
		if n++; n == 3 {
			p, _, err := up.Load(m.root)
			if err == nil {
				err = up.Save(p)
			}
			if err != nil {
				t.Error(err)
			}
		}
		return nil, nil
	})
	if code, _, errb := runQM("up", "note", "--notify"); code != 0 || len(*notified) != 0 {
		t.Errorf("exit %d, %q, notified %q", code, errb, *notified)
	}
}

// What qm cannot judge gives no note.
func TestUpNoteNotJudged(t *testing.T) {
	for _, c := range []struct {
		name string
		set  func(t *testing.T, m *upMachine)
		want string
		slow bool // each read retries the file: not read noteMax times
	}{
		{"no process list", func(t *testing.T, _ *upMachine) {
			swapProcs(t, func() ([]claudecode.Proc, error) { return nil, errors.New("ps: no") })
		}, "ps: no", false},
		{"a running session's file unreadable", func(t *testing.T, m *upMachine) {
			swapProcs(t, func() ([]claudecode.Proc, error) { return []claudecode.Proc{{PID: 301, Cmd: "claude"}}, nil })
			if err := os.Mkdir(filepath.Join(m.claude, "sessions", "301.json"), 0o700); err != nil {
				t.Fatal(err)
			}
		}, "not all known", true},
		{"no sessions folder", func(t *testing.T, m *upMachine) {
			if err := os.Remove(filepath.Join(m.claude, "sessions")); err != nil {
				t.Fatal(err)
			}
		}, "sessions", false},
		{"trust unreadable", func(t *testing.T, m *upMachine) {
			if err := os.WriteFile(filepath.Join(m.claude, ".claude.json"), []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, ".claude.json", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, notified := noteMachine(t)
			c.set(t, m)
			code, out, errb := runQM("up", "note")
			if code != 1 || out != "" || !strings.Contains(errb, c.want) {
				t.Errorf("exit %d, %q, %q: want exit 1 and %q", code, out, errb, c.want)
			}
			if c.slow {
				return
			}
			// A hook that fails shows a banner: not for this.
			code, out, errb = runQM("up", "note", "--notify")
			if code != 0 || out != "" || len(*notified) != 0 || !strings.Contains(errb, c.want) {
				t.Errorf("--notify: exit %d, %q, %q, notified %q", code, out, errb, *notified)
			}
		})
	}
}

func TestUpNoteUsage(t *testing.T) {
	noteMachine(t)
	for _, args := range [][]string{{"up", "note", "x"}, {"up", "note", "--yes"}} {
		if code, _, errb := runQM(args...); code != 2 || !strings.Contains(errb, "qm up note") {
			t.Errorf("%v: exit %d, %q", args, code, errb)
		}
	}
}

func TestNoteBody(t *testing.T) {
	left := func(roots ...string) []up.Left {
		var ls []up.Left
		for _, r := range roots {
			ls = append(ls, up.Left{Project: up.Project{Root: r}})
		}
		return ls
	}
	for _, c := range []struct {
		ls   []up.Left
		want string
	}{
		{left("/w/one"), "Sessions not running: one. qm up in the project folder restores them."},
		{left("/w/one", "/w/two"), "Sessions not running: one, two. qm up in each project folder restores them; qm up note lists them."},
		{left("/w/one", "/v/one"), "Sessions not running: one (2 folders). qm up in each project folder restores them; qm up note lists them."},
		{left("/1", "/2", "/3", "/4", "/5", "/6", "/7"), "Sessions not running: 1, 2, 3, 4, 5 and 2 more. qm up in each project folder restores them; qm up note lists them."},
		{left("/w/a\x1b[31mb\nc"), "Sessions not running: a[31mbc. qm up in the project folder restores them."},
		{left("/w/" + strings.Repeat("é", 60)), "Sessions not running: " + strings.Repeat("é", 40) + "…. qm up in the project folder restores them."},
	} {
		if got := noteBody(c.ls); got != c.want {
			t.Errorf("body %q\nwant %q", got, c.want)
		}
	}
}

func TestRenderNote(t *testing.T) {
	now := time.Now()
	got := renderNote([]up.Left{
		{Project: up.Project{Root: "/w/a b"}, Coordinator: true, Workers: 2, LastAt: now.Add(-3 * time.Hour)},
		{Project: up.Project{Root: "/w/c"}, Workers: 1},
		{Project: up.Project{Root: "/w/\x1b[31md\ne"}, Coordinator: true},
	}, now)
	want := "/w/a b: coordinator and up to 2 worker(s) not running, last active 3 h ago\n  cd '/w/a b' && qm up\n" +
		"/w/c: 1 worker(s) not running\n  cd /w/c && qm up\n" +
		"/w/[31mde: coordinator not running\n  run qm up in that folder (its name holds characters that are not shown)\n"
	if got != want {
		t.Errorf("note %q\nwant %q", got, want)
	}
}

// qm down closes the sessions, and qm up takes that back: the note follows.
func TestUpNoteAfterDownAndUp(t *testing.T) {
	m, _ := noteMachine(t)
	swap(t, &upWorkDir, func() (string, error) { return m.root, nil })
	if code, out, errb := runQM("down", "--yes"); code != 0 {
		t.Fatalf("qm down: exit %d, %s%s", code, out, errb)
	}
	if _, out, errb := runQM("up", "note"); out != "" || errb != "" {
		t.Errorf("after qm down: %q, %q", out, errb)
	}
	// A dry run takes nothing back.
	if code, _, errb := runQM("up", "--dry-run"); code != 0 || loadProject(t, m.root).Closed == nil {
		t.Errorf("a dry run: exit %d, %q, closed %v", code, errb, loadProject(t, m.root).Closed)
	}
	// qm up tints its tabs; the notes' agtermctl takes notify only.
	ctl := upAgtermCtl
	upAgtermCtl = launch.Agterm{Bin: "agtermctl", Run: m.term.run}
	code, out, errb := runQM("up", "--yes")
	upAgtermCtl = ctl
	if code != 0 {
		t.Fatalf("qm up: exit %d, %s%s", code, out, errb)
	}
	if p := loadProject(t, m.root); p.Closed != nil || !p.DownAt.IsZero() {
		t.Errorf("after qm up: closed %v, down at %v", p.Closed, p.DownAt)
	}
	if _, out, _ := runQM("up", "note"); !strings.Contains(out, m.root) {
		t.Errorf("after qm up, and its sessions ended: %q, want the project", out)
	}
}

// A restore that only leaves running sessions takes qm down's record back
// too.
func TestUpTakesDownBackWithNothingToStart(t *testing.T) {
	m, _ := noteMachine(t)
	swap(t, &upWorkDir, func() (string, error) { return m.root, nil })
	if code, _, errb := runQM("down", "--yes"); code != 0 {
		t.Fatal(errb)
	}
	swapProcs(t, func() ([]claudecode.Proc, error) { return []claudecode.Proc{coordProc(11), workerProc(12)}, nil })
	if code, out, errb := runQM("up", "--yes"); code != 0 || loadProject(t, m.root).Closed != nil {
		t.Errorf("exit %d, %s%s: closed %v", code, out, errb, loadProject(t, m.root).Closed)
	}
}

// A note is quick: twenty projects of five workers take far less than this bound.
func TestUpNoteTwentyProjects(t *testing.T) {
	m, _ := noteMachine(t)
	if err := os.RemoveAll(filepath.Join(os.Getenv("XDG_STATE_HOME"), "quartermaster")); err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		root := filepath.Join(m.root, fmt.Sprintf("p%02d", i))
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		id := fmt.Sprintf("%08d-0000-4000-8000-000000000000", i)
		p := up.Project{
			ID: up.ProjectID(root), Root: root, Workspace: filepath.Base(root), Launcher: "agterm",
			Coordinator: up.Session{ID: id, Name: "c", Started: time.Now().Add(-time.Hour)},
		}
		if err := up.Save(p); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(m.claude, "projects", fmt.Sprintf("p%d", i))
		m.fixture(t, "coord.jsonl", filepath.Join(dir, id+".jsonl"))
		for j := range 5 {
			wid := fmt.Sprintf("%08d-0000-4000-8000-%012d", i, j+1)
			r := route.Record{
				Time: time.Now().UTC().Add(-time.Hour), Kind: "worker", Name: fmt.Sprintf("w-%d", j),
				Cwd: root, UpProject: p.ID, ClaudeSession: wid,
				Decision: &route.Decision{Profile: &route.Profile{}},
			}
			if err := declog.Append(r); err != nil {
				t.Fatal(err)
			}
			m.fixture(t, "coord.jsonl", filepath.Join(dir, wid+".jsonl"))
		}
	}
	for i := range 200 { // the folders of other projects' transcripts
		if err := os.Mkdir(filepath.Join(m.claude, "projects", fmt.Sprintf("other%d", i)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	code, out, errb := runQM("up", "note")
	took := time.Since(start)
	if n := strings.Count(out, "coordinator and up to 5 worker(s)"); code != 0 || n != 20 {
		t.Errorf("exit %d, %q, %d projects listed, want 20", code, errb, n)
	}
	t.Logf("twenty projects: %v", took)
	if took > 5*time.Second {
		t.Errorf("took %v", took)
	}
}
