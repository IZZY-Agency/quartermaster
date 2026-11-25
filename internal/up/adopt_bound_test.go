package up

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
)

// adOther is a session started in an adopted session's tab.
const adOther = "00000000-0000-4000-8000-0000000004ff"

// bareWorld is an adoptWorld whose w-hand (process 413) runs a bare claude.
func bareWorld(t *testing.T) *adoptWorld {
	t.Helper()
	w := newAdoptWorld(t)
	w.procs[2].Cmd, w.agterm.fg["s-h"] = "claude", []string{"claude"}
	return w
}

// replaceHand starts another bare claude (process 415) in w-hand's tab and
// folder; process 413 goes on running.
func (w *adoptWorld) replaceHand() {
	w.procs = append(w.procs, claudecode.Proc{PID: 415, PPID: 1, Cmd: "claude"})
	w.term[415] = claudecode.TermEnv{AgtermSession: "s-h"}
	w.agterm.fg["s-h"] = []string{"claude"}
}

// wantTab fails unless w-hand is adopted, with tab s-h recorded and pinned
// or, when want is false, neither moved nor pinned and a note that says why.
func wantTab(t *testing.T, w *adoptWorld, ap AdoptPlan, want bool, why string) {
	t.Helper()
	if _, ok := ap.Project.Adopted[adHand]; !ok {
		t.Errorf("w-hand not adopted: %+v", ap.Project.Adopted)
	}
	if got := ap.Project.Sessions[adHand].ID == "s-h"; got != want {
		t.Errorf("tab recorded = %v, want %v (notes %q)", got, want, ap.Notes)
	}
	if got := w.agterm.has("session restore --none --target s-h --json"); got != want {
		t.Errorf("tab pinned = %v, want %v (calls %q)", got, want, w.agterm.calls)
	}
	if note := strings.Join(ap.Notes, "\n"); !want && (!strings.Contains(note, "s-h") || !strings.Contains(note, why) || !strings.Contains(note, "no terminal session recorded")) {
		t.Errorf("notes = %q, want one on tab s-h saying %q", ap.Notes, why)
	}
}

// After the confirmation, a tab whose bare claude was replaced by another
// one in the same folder is not the planned session's any more, though that
// session still runs.
func TestAdoptTabReplacedSinceThePlan(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(t *testing.T, w *adoptWorld)
		why    string
	}{
		"suspended in the tab": {func(_ *testing.T, w *adoptWorld) {
			w.replaceHand()
			w.bg[413] = true
		}, "foreground"},
		"in a terminal opened from the tab": {func(_ *testing.T, w *adoptWorld) {
			w.replaceHand()
		}, "another claude, process 415"},
		"with a session of its own": {func(t *testing.T, w *adoptWorld) {
			w.replaceHand()
			w.session(t, 415, adOther, w.wtHand, "other")
		}, "another claude, process 415"},
		"in no terminal any more": {func(_ *testing.T, w *adoptWorld) {
			w.replaceHand()
			w.tty[413] = ""
		}, "foreground"},
		"with its tab's variable cleared": {func(_ *testing.T, w *adoptWorld) {
			w.replaceHand()
			w.term[415] = claudecode.TermEnv{}
		}, "another claude, process 415"},
		"run under it, in one terminal": {func(_ *testing.T, w *adoptWorld) {
			w.replaceHand()
			w.procs[2].PPID = 415
			w.tty[413], w.tty[415] = "ttys001", "ttys001"
		}, "another claude, process 415"},
		"by another command": {func(_ *testing.T, w *adoptWorld) {
			w.replaceHand()
			w.agterm.fg["s-h"] = []string{"claude", "--model", "opus"}
			w.procs[3].Cmd = "claude --model opus"
		}, "foreground"},
		"in another tab": {func(_ *testing.T, w *adoptWorld) {
			w.replaceHand()
			w.term[413] = claudecode.TermEnv{AgtermSession: "s-y"}
		}, "foreground"},
	} {
		t.Run(name, func(t *testing.T) {
			w := bareWorld(t)
			ap, err := w.plan(t, allThree, nil, false)
			if err != nil {
				t.Fatal(err)
			}
			if ap.Project.Sessions[adHand].ID != "s-h" {
				t.Fatalf("planned sessions = %+v, notes %q", ap.Project.Sessions, ap.Notes)
			}
			tc.change(t, w)
			if err := Adopt(context.Background(), &ap, w.deps()); err != nil {
				t.Fatal(err)
			}
			wantTab(t, w, ap, false, tc.why)
			if !ap.Project.Dropped[adHand] {
				t.Error("w-hand's tab not marked dropped")
			}
			if ap.Project.Sessions[adCoord].ID != "s-c" || ap.Project.Sessions[adQM].ID != "s-q" {
				t.Errorf("the other tabs = %+v, want kept", ap.Project.Sessions)
			}
		})
	}
}

// What cannot be established keeps no reference.
func TestAdoptTabProcessNotEstablished(t *testing.T) {
	for name, change := range map[string]func(*adoptWorld, *AdoptDeps){
		"terminal not read": func(w *adoptWorld, _ *AdoptDeps) { w.fgErr[413] = errors.New("ps failed") },
		"processes not read": func(_ *adoptWorld, d *AdoptDeps) {
			d.Procs = func() ([]claudecode.Proc, error) { return nil, errors.New("ps failed") }
		},
		"no process list": func(_ *adoptWorld, d *AdoptDeps) { d.Procs = nil },
		"no foreground":   func(_ *adoptWorld, d *AdoptDeps) { d.Foreground = nil },
		"another claude's environment not read": func(w *adoptWorld, d *AdoptDeps) {
			w.replaceHand()
			termOf := d.TermOf
			d.TermOf = func(pid int) (claudecode.TermEnv, error) {
				if pid == 415 {
					return claudecode.TermEnv{}, errors.New("ps failed")
				}
				return termOf(pid)
			}
		},
		"another claude's terminal not read": func(w *adoptWorld, _ *AdoptDeps) {
			w.replaceHand()
			w.fgErr[415] = errors.New("ps failed")
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := bareWorld(t)
			ap, err := w.plan(t, allThree, nil, false)
			if err != nil {
				t.Fatal(err)
			}
			d := w.deps()
			change(w, &d)
			if err := Adopt(context.Background(), &ap, d); err != nil {
				t.Fatal(err)
			}
			wantTab(t, w, ap, false, "")
			if !ap.Project.Dropped[adHand] {
				t.Error("w-hand's tab not marked dropped")
			}
		})
	}
}

// Other claude processes that cannot be the tab's foreground change nothing.
func TestAdoptTabOtherClaudes(t *testing.T) {
	for name, change := range map[string]func(*testing.T, *adoptWorld, *AdoptDeps){
		"suspended earlier in the tab": func(_ *testing.T, w *adoptWorld, _ *AdoptDeps) {
			w.procs = append(w.procs, claudecode.Proc{PID: 415, PPID: 1, Cmd: "claude"})
			w.term[415] = claudecode.TermEnv{AgtermSession: "s-h"}
			w.bg[415] = true
		},
		"run by the session in its terminal": func(_ *testing.T, w *adoptWorld, _ *AdoptDeps) {
			w.procs = append(w.procs, claudecode.Proc{PID: 415, PPID: 413, Cmd: "claude -p x"})
			w.term[415] = claudecode.TermEnv{AgtermSession: "s-h"}
			w.tty[413], w.tty[415] = "ttys001", "ttys001"
		},
		"naming a session in its arguments": func(_ *testing.T, w *adoptWorld, _ *AdoptDeps) {
			w.procs = append(w.procs, claudecode.Proc{PID: 415, PPID: 1, Cmd: "claude --resume " + adOther})
			w.term[415] = claudecode.TermEnv{AgtermSession: "s-h"}
		},
		"in a tmux pane": func(_ *testing.T, w *adoptWorld, _ *AdoptDeps) {
			w.procs = append(w.procs, claudecode.Proc{PID: 415, PPID: 1, Cmd: "claude"})
			w.term[415] = claudecode.TermEnv{AgtermSession: "s-h", TmuxPane: "%1"}
		},
		"with another command, in a terminal opened from the tab": func(_ *testing.T, w *adoptWorld, _ *AdoptDeps) {
			w.procs = append(w.procs, claudecode.Proc{PID: 415, PPID: 1, Cmd: "claude --model opus"})
			w.term[415] = claudecode.TermEnv{AgtermSession: "s-h"}
		},
		"its terminal unread, in another tab": func(_ *testing.T, w *adoptWorld, _ *AdoptDeps) {
			w.procs = append(w.procs, claudecode.Proc{PID: 415, PPID: 1, Cmd: "claude"})
			w.term[415] = claudecode.TermEnv{AgtermSession: "s-x"}
			w.fgErr[415] = errors.New("ps failed")
		},
		"in another folder, in no agterm tab": func(t *testing.T, w *adoptWorld, _ *AdoptDeps) {
			w.procs = append(w.procs, claudecode.Proc{PID: 415, PPID: 1, Cmd: "claude"})
			w.session(t, 415, adOther, w.root, "other")
		},
		"unread, in another folder": func(t *testing.T, w *adoptWorld, d *AdoptDeps) {
			w.procs = append(w.procs, claudecode.Proc{PID: 415, PPID: 1, Cmd: "claude"})
			w.session(t, 415, adOther, w.root, "other")
			termOf := d.TermOf
			d.TermOf = func(pid int) (claudecode.TermEnv, error) {
				if pid == 415 {
					return claudecode.TermEnv{}, errors.New("ps failed")
				}
				return termOf(pid)
			}
		},
		"unread, exited": func(_ *testing.T, w *adoptWorld, d *AdoptDeps) {
			w.procs = append(w.procs, claudecode.Proc{PID: 415, PPID: 1, Cmd: "claude"})
			termOf := d.TermOf
			d.TermOf = func(pid int) (claudecode.TermEnv, error) {
				if pid == 415 {
					return claudecode.TermEnv{}, errors.New("ps failed")
				}
				return termOf(pid)
			}
			w.fgErr[415] = errors.New("ps failed")
			d.Running = func(pid int) bool { return pid != 415 }
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := bareWorld(t)
			d := w.deps()
			change(t, w, &d)
			ap, err := PlanAdopt(context.Background(), w.repo, w.all, w.detect(t), allThree, nil, false, d)
			if err != nil {
				t.Fatal(err)
			}
			if err := Adopt(context.Background(), &ap, d); err != nil {
				t.Fatal(err)
			}
			wantTab(t, w, ap, true, "")
		})
	}
}

// The plan itself binds the tab to the candidate's process.
func TestPlanAdoptTabOfAnotherProcess(t *testing.T) {
	for name, change := range map[string]func(*adoptWorld, *AdoptDeps){
		"not in the foreground": func(w *adoptWorld, _ *AdoptDeps) { w.bg[413] = true },
		"another claude":        func(w *adoptWorld, _ *AdoptDeps) { w.replaceHand() },
	} {
		t.Run(name, func(t *testing.T) {
			w := bareWorld(t)
			d := w.deps()
			change(w, &d)
			ap, err := PlanAdopt(context.Background(), w.repo, w.all, w.detect(t), allThree, nil, false, d)
			if err != nil {
				t.Fatal(err)
			}
			wantTab(t, w, ap, false, "")
		})
	}
}

// A tab whose claude names the session's id is bound too: the id may run in
// another process than the tab's (which went on to another session).
func TestAdoptTabNamingTheSessionInAnotherProcess(t *testing.T) {
	w := newAdoptWorld(t)
	w.procs[2].Cmd = "claude --resume " + adHand
	w.agterm.fg["s-h"] = []string{"claude", "--resume", adHand}
	ap, err := w.plan(t, allThree, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	w.procs = append(w.procs, claudecode.Proc{PID: 415, PPID: 1, Cmd: "claude --resume " + adHand})
	w.session(t, 415, adOther, w.wtHand, "other")
	w.term[415] = claudecode.TermEnv{AgtermSession: "s-h"}
	if err := Adopt(context.Background(), &ap, w.deps()); err != nil {
		t.Fatal(err)
	}
	wantTab(t, w, ap, false, "another claude, process 415")
}

// A worker Record's old tab does not come back for a worker whose tab was
// not established after the confirmation.
func TestAdoptTabNotEstablishedStaysDropped(t *testing.T) {
	w := newAdoptWorld(t)
	w.recs[0].TermLauncher, w.recs[0].TermID = "agterm", "s-q"
	ap, err := w.plan(t, allThree, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	w.recs[0].UpProject = ap.Project.ID
	d := w.deps()
	w.fgErr[412] = errors.New("ps failed")
	if err := Adopt(context.Background(), &ap, d); err != nil {
		t.Fatal(err)
	}
	p, ok, err := Load(w.repo)
	if err != nil || !ok {
		t.Fatalf("load: %v, %v", ok, err)
	}
	if ref, ok := effectiveSessions(p, w.recs)[adQM]; ok {
		t.Errorf("w-qm's tab came back from its Record: %+v", ref)
	}
}

// Incomplete session data plans no adoption, so a dry run does not show one.
func TestPlanAdoptRefusesOnIncompleteSessions(t *testing.T) {
	w := newAdoptWorld(t)
	cands := w.detect(t)
	d := w.deps()
	inc := &claudecode.IncompleteError{Unread: []claudecode.Unread{{Path: "/x/77.json", PID: 77, Err: errors.New("denied")}}}
	d.Live = func() (map[string]claudecode.LiveSession, error) { return nil, inc }
	_, err := PlanAdopt(context.Background(), w.repo, w.all, cands, allThree, nil, false, d)
	if !errors.Is(err, error(inc)) || !strings.Contains(err.Error(), "nothing adopted") {
		t.Errorf("first run: err = %v, want the incompleteness", err)
	}
	p := Project{ID: ProjectID(w.repo), Root: w.repo, Workspace: "proj", Launcher: "agterm"}
	if _, err = PlanAdoptWorkers(context.Background(), p, w.all, cands, []string{adHand}, d); !errors.Is(err, error(inc)) {
		t.Errorf("later: err = %v, want the incompleteness", err)
	}
}

// Two workspaces of one name matter only when the coordinator's tab is in
// one of them.
func TestPlanAdoptUnboundCoordinatorInDuplicateWorkspace(t *testing.T) {
	w := newAdoptWorld(t)
	w.agterm.ws[1].Name = "proj"
	if _, err := w.plan(t, allThree, nil, false); !errors.Is(err, ErrWorkspaceName) {
		t.Fatalf("bound: err = %v, want ErrWorkspaceName", err)
	}
	w.bg[411] = true
	ap, err := w.plan(t, allThree, nil, false)
	if err != nil || len(ap.Project.Sessions) != 0 {
		t.Errorf("unbound: sessions %+v, err %v, want it adopted without a tab", ap.Project.Sessions, err)
	}
}

func TestOtherCommand(t *testing.T) {
	for _, tc := range []struct {
		fg, cmd string
		other   bool
	}{
		{"claude", "claude", false},
		{"claude", "claude -p x", true},
		{"claude --model opus", "claude  --model opus", false},
		{"claude -n a fix it\nthen stop", `claude -n a fix it\012then stop`, false},
		{"claude -n a fix it\nthen stop", `claude -n b fix it\012then stop`, true},
		{"claude -n a fix it\nthen stop", "claude", false}, // a prefix: not certain
		{"claude caf\u00e9", `claude caf\303\251`, false},
		{"claude", "/opt/claude", true},
	} {
		if got := otherCommand(tc.fg, tc.cmd); got != tc.other {
			t.Errorf("otherCommand(%q, %q) = %v, want %v", tc.fg, tc.cmd, got, tc.other)
		}
	}
}

// A candidate run by the path of its version keeps its own flags, and one in
// print mode is no candidate.
func TestAdoptVersionedClaudeKeepsItsFlags(t *testing.T) {
	const exe = "/opt/share/claude/versions/2.1.0"
	w := newAdoptWorld(t)
	w.procs[0].Cmd = exe + " --permission-mode plan"
	w.procs[2].Cmd = exe + " -p x"
	live, err := claudecode.LiveComplete(context.Background(), w.claude, w.procs)
	if err != nil {
		t.Fatal(err)
	}
	det := Detect(w.repo, w.all, w.recs, DetectEnv{
		Procs: w.procs, Live: live, Worktrees: []string{w.repo, w.wtQM, w.wtHand}, TranscriptPath: w.transcriptPath,
	})
	if _, ok := candidateByID(det.Candidates, adHand); ok {
		t.Error("a print-mode session is a candidate")
	}
	ap, err := PlanAdopt(context.Background(), w.repo, w.all, det.Candidates, Choice{Coordinator: adCoord}, nil, false, w.deps())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ap.Project.ClaudeArgs, " "); got != "--permission-mode plan" {
		t.Errorf("coordinator's saved arguments = %q, want its own", got)
	}
}
