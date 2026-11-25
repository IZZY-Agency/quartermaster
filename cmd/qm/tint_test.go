package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
	"github.com/IZZY-Agency/quartermaster/internal/config"
	"github.com/IZZY-Agency/quartermaster/internal/declog"
	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/route"
	"github.com/IZZY-Agency/quartermaster/internal/up"
	"github.com/IZZY-Agency/quartermaster/internal/watch"
)

const (
	purple = "purple icon,name"
	teal   = "teal icon"
	orange = "orange icon,name"
	yellow = "yellow icon,name"
	red    = "red icon,name"
)

// tintTab shows tab id and has qm tint it color ON, so qm remembers it.
func (m *upMachine) tintTab(t *testing.T, id string, tint launch.Tint) {
	t.Helper()
	m.term.show(id)
	if res, err := upAgtermCtl.ApplyTint(context.Background(), id, tint); err != nil || res != launch.TintApplied {
		t.Fatalf("tint %s: %v, %v", id, res, err)
	}
	m.term.calls = nil
}

// tabs records tabs for the test project's sessions.
func (m *upMachine) tabs(t *testing.T, p up.Project, coord, worker string) {
	t.Helper()
	p.Sessions = map[string]launch.SessionRef{}
	if coord != "" {
		p.Sessions[upCoordID] = launch.SessionRef{Launcher: "agterm", ID: coord}
	}
	if worker != "" {
		p.Sessions[upWorkerID] = launch.SessionRef{Launcher: "agterm", ID: worker}
	}
	if err := up.Save(p); err != nil {
		t.Fatal(err)
	}
}

func noteLines(out string) []string {
	var notes []string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "tinted") || strings.Contains(l, "the tint of") || strings.HasPrefix(l, "note: config:") {
			notes = append(notes, l)
		}
	}
	return notes
}

func TestUpTintsTheTabsItOpens(t *testing.T) {
	m := newUpMachine(t)
	m.term.showNew = true
	m.worker(t, m.project(t), "w1")
	code, out, errs := runQM("up", "--yes")
	if code != 0 {
		t.Fatalf("exit %d, out %q, err %q", code, out, errs)
	}
	p := loadProject(t, m.root)
	if c, w := m.term.tint[p.Sessions[upCoordID].ID], m.term.tint[p.Sessions[upWorkerID].ID]; c != purple || w != teal {
		t.Errorf("coordinator %q, worker %q", c, w)
	}
	// Every start comes before the first tint.
	last, first := -1, -1
	for i, c := range m.term.calls {
		if strings.HasPrefix(c, "session new") {
			last = i
		}
		if first < 0 && strings.HasPrefix(c, "session tint") {
			first = i
		}
	}
	if first < last {
		t.Errorf("a tint before a start: %q", m.term.calls)
	}
	if n := noteLines(out + errs); len(n) != 0 {
		t.Errorf("notes %q", n)
	}
}

func TestUpFirstRunTintsTheCoordinator(t *testing.T) {
	m := newUpMachine(t)
	m.term.showNew = true
	if code, out, errs := runQM("up"); code != 0 || m.term.tint["s-1"] != purple {
		t.Fatalf("exit %d, tint %q, out %q, err %q", code, m.term.tint["s-1"], out, errs)
	}
}

// A reused tab gets the role's tint back, whatever qm left on it; one the
// owner tinted keeps its own.
func TestUpTintsTheTabsItReuses(t *testing.T) {
	m := newUpMachine(t)
	p := m.project(t)
	m.worker(t, p, "w1")
	m.tabs(t, p, "t-c", "t-w")
	m.tintTab(t, "t-w", launch.AttentionTint("red"))
	m.term.showIdle("t-c", "t-w")
	m.term.setTint("t-c", "pink icon")
	code, out, errs := runQM("up", "--yes")
	if code != 0 || !strings.Contains(out, "t-w") {
		t.Fatalf("exit %d, out %q, err %q", code, out, errs)
	}
	if m.term.tint["t-w"] != teal || m.term.tint["t-c"] != "pink icon" {
		t.Errorf("worker %q, hand-tinted coordinator %q", m.term.tint["t-w"], m.term.tint["t-c"])
	}
	if n := noteLines(out + errs); len(n) != 0 {
		t.Errorf("notes %q", n)
	}
}

// A tab qm up lets go of, as it runs something else by now, loses qm's tint.
func TestUpClearsATabItNoLongerRecords(t *testing.T) {
	m := newUpMachine(t)
	m.term.showNew = true
	p := m.project(t)
	m.worker(t, p, "w1")
	m.tabs(t, p, "", "t-w")
	m.tintTab(t, "t-w", launch.AttentionTint("red"))
	m.term.tab("t-w", m.term.tint["t-w"], "npm", "run", "dev")
	if code, out, errs := runQM("up", "--yes"); code != 0 {
		t.Fatalf("exit %d, out %q, err %q", code, out, errs)
	}
	if id := loadProject(t, m.root).Sessions[upWorkerID].ID; id == "t-w" || m.term.tint[id] != teal {
		t.Errorf("worker's tab %s tinted %q", id, m.term.tint[id])
	}
	if m.term.tint["t-w"] != "" {
		t.Errorf("the tab let go of is still %q", m.term.tint["t-w"])
	}
}

func TestUpGoesOnWhenTintsFail(t *testing.T) {
	m := newUpMachine(t)
	m.term.showNew = true
	m.term.failTint = "session tint: Error: Unexpected argument 'tint'"
	writeConfig(t, `{"tint":{"worker":"beige"}}`)
	m.worker(t, m.project(t), "w1")
	code, out, errs := runQM("up", "--yes")
	if code != 0 || !strings.Contains(out, "resumed w1") {
		t.Fatalf("exit %d, out %q, err %q", code, out, errs)
	}
	n := noteLines(out + errs)
	if len(n) != 1 || !strings.Contains(n[0], "the tint of 2 of 2 agterm tabs not changed") || !strings.Contains(n[0], "tint.worker") {
		t.Errorf("notes %q, want one with both", n)
	}
}

// An agterm that does not answer holds no start up, and the run only for
// tintTimeout.
func TestUpTintsAreBounded(t *testing.T) {
	m := newUpMachine(t)
	m.term.showNew = true
	m.worker(t, m.project(t), "w1")
	swap(t, &tintTimeout, 50*time.Millisecond)
	swap(t, &upAgtermCtl, launch.Agterm{Bin: "agtermctl", Run: func(ctx context.Context, _ string, args ...string) ([]byte, error) {
		if args[0] == "window" || args[0] == "tree" {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return m.term.run(ctx, "agtermctl", args...)
	}})
	code, out, errs := runQM("up", "--yes")
	if code != 0 || len(noteLines(out+errs)) != 1 || !hasCall(m.term.calls, "session new") {
		t.Fatalf("exit %d, out %q, err %q", code, out, errs)
	}
}

func TestUpHereTintsThisTabBeforeItsCoordinatorStarts(t *testing.T) {
	for _, first := range []bool{true, false} {
		m, _ := hereMachine(t)
		if !first {
			m.project(t)
		}
		atExec := ""
		swap(t, &upExec, func(string) error { atExec = m.term.tint["t-here"]; return nil })
		if code, out, errs := runQM("up", "--yes", "--here"); code != 0 || atExec != purple {
			t.Errorf("first %v: exit %d, tint at exec %q, out %q, err %q", first, code, atExec, out, errs)
		}
	}
}

func TestUpHereFailedExecTakesTheTintBack(t *testing.T) {
	m, _ := hereMachine(t)
	m.project(t)
	swap(t, &upExec, func(string) error { return errors.New("exec refused") })
	if code, _, errs := runQM("up", "--yes", "--here"); code == 0 || m.term.tint["t-here"] != "" || len(m.term.tintCalls()) != 2 {
		t.Errorf("exit %d, tint %q, calls %q, err %q", code, m.term.tint["t-here"], m.term.tintCalls(), errs)
	}
}

func TestUpAdoptTintsTheTabsByRole(t *testing.T) {
	m := newAdoptMachine(t)
	swap(t, &upTerminal, func() bool { return true })
	swap[io.Reader](t, &upStdin, strings.NewReader("1\n2\ny\n"))
	if code, out, errs := runQM("up"); code != 0 {
		t.Fatalf("exit %d, out %q, err %q", code, out, errs)
	}
	want := []string{"session tint purple --on icon,name --target s-c --json", "session tint teal --on icon --target s-h --json"}
	if got := m.agterm.tints(); !slices.Equal(got, want) {
		t.Errorf("tints %q, want %q", got, want)
	}
}

func TestUpAdoptLaterTintsTheWorkersOnly(t *testing.T) {
	m := newLaterMachine(t)
	swap(t, &upTerminal, func() bool { return true })
	swap[io.Reader](t, &upStdin, strings.NewReader("a\ny\n"))
	if code, out, errs := runQM("up", "--adopt"); code != 0 {
		t.Fatalf("exit %d, out %q, err %q", code, out, errs)
	}
	if got := m.agterm.tints(); !slices.Equal(got, []string{"session tint teal --on icon --target s-h --json"}) {
		t.Errorf("tints %q", got)
	}
}

func TestUpDoneTakesTheTintOff(t *testing.T) {
	for _, hand := range []bool{false, true} {
		m := newUpMachine(t)
		p := m.project(t)
		m.worker(t, p, "w1")
		m.tabs(t, p, "t-c", "t-w")
		m.tintTab(t, "t-c", launch.CoordinatorTint("purple"))
		m.tintTab(t, "t-w", launch.WorkerTint("teal"))
		want := ""
		if hand {
			m.term.setTint("t-w", "pink icon")
			want = "pink icon"
		}
		code, out, errs := runQM("up", "done", "w1")
		if code != 0 || m.term.tint["t-w"] != want || m.term.tint["t-c"] != purple || len(noteLines(out+errs)) != 0 {
			t.Errorf("hand %v: exit %d, worker %q, coordinator %q, out %q, err %q", hand, code, m.term.tint["t-w"], m.term.tint["t-c"], out, errs)
		}
	}
}

func TestDownLeavesNoTint(t *testing.T) {
	for _, args := range [][]string{{"down", "--keep-open"}, {"down", "--yes"}} {
		m := newUpMachine(t)
		p := m.project(t)
		m.worker(t, p, "w1")
		m.tabs(t, p, "t-c", "t-w")
		m.tintTab(t, "t-c", launch.CoordinatorTint("purple"))
		m.tintTab(t, "t-w", launch.AttentionTint("red"))
		m.term.showIdle("t-c")
		// t-w runs something else by now, so it stays open either way.
		m.term.tab("t-w", m.term.tint["t-w"], "npm", "run", "dev")
		code, out, errs := runQM(args...)
		if code != 0 || m.term.tint["t-w"] != "" || m.term.tint["t-c"] != "" || len(noteLines(out+errs)) != 0 {
			t.Errorf("%q: exit %d, tints %v, out %q, err %q", args, code, m.term.tint, out, errs)
		}
	}
}

func TestDownClosesWhenTintsFail(t *testing.T) {
	m := newUpMachine(t)
	p := m.project(t)
	m.worker(t, p, "w1")
	m.tabs(t, p, "t-c", "t-w")
	m.tintTab(t, "t-c", launch.CoordinatorTint("purple"))
	m.term.showIdle("t-c", "t-w")
	m.term.failTint = "session tint failed"
	code, out, errs := runQM("down", "--yes")
	if code != 0 || !hasCall(m.term.calls, "session close --target t-c") || len(noteLines(out+errs)) != 1 {
		t.Errorf("exit %d, calls %q, out %q, err %q", code, m.term.calls, out, errs)
	}
}

// tmux tabs are not tinted: no word to agterm, no tint memory, no note.
func TestTmuxSessionsAreNotTinted(t *testing.T) {
	m := newUpMachine(t)
	p := m.project(t)
	m.worker(t, p, "w1")
	p.Sessions = map[string]launch.SessionRef{upWorkerID: {Launcher: "tmux", ID: "@3", Name: "w1"}, upCoordID: {Launcher: "tmux", ID: "@4", Name: "c"}}
	if err := up.Save(p); err != nil {
		t.Fatal(err)
	}
	swap(t, &upPick, func(string) (launch.Launcher, error) {
		return launch.Tmux{Bin: "tmux", Run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			return []byte("@9\n"), nil
		}}, nil
	})
	for _, args := range [][]string{{"up", "--yes"}, {"up", "done", "w1"}, {"down", "--yes", "--keep-open"}} {
		code, out, errs := runQM(args...)
		if len(noteLines(out+errs)) != 0 || len(m.term.calls) != 0 {
			t.Errorf("%q: exit %d, calls %q, out %q, err %q", args, code, m.term.calls, out, errs)
		}
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("XDG_STATE_HOME"), "quartermaster", "tints.json.lock")); err == nil {
		t.Error("the tint memory was opened")
	}
}

// watched is a machine with worker w in tab t-w, tinted as qm left it.
func watched(t *testing.T) *upMachine {
	t.Helper()
	m := newUpMachine(t)
	p := m.project(t)
	m.worker(t, p, "w")
	m.tabs(t, p, "t-c", "t-w")
	m.tintTab(t, "t-c", launch.CoordinatorTint("purple"))
	m.tintTab(t, "t-w", launch.WorkerTint("teal"))
	m.term.tab("t-w", teal, "claude", "--session-id", upWorkerID)
	return m
}

func TestWatchTintsByStateAndBack(t *testing.T) {
	m := watched(t)
	liveWorker(t)
	turnHook(t, "turn-start", "UserPromptSubmit")
	fakeClock(t, func(n int) {
		if n == 2 {
			turnHook(t, "turn-stop", "Stop")
		}
	})
	runWatch := func(extra ...string) {
		t.Helper()
		code, out, errs := runQM(append([]string{"watch", "--silent", "100000h", "--interval", "1s"}, extra...)...)
		if code != 0 || len(noteLines(out+errs)) != 0 {
			t.Fatalf("exit %d, out %q, err %q", code, out, errs)
		}
	}
	runWatch()
	// Set once, when it stopped; the checks while it worked set nothing.
	if m.term.tint["t-w"] != orange || len(m.term.tintCalls()) != 1 {
		t.Fatalf("stopped: %q, calls %q", m.term.tint["t-w"], m.term.tintCalls())
	}
	// The next watch ends at its timeout: the stop is reported, and the colour
	// stays.
	fakeClock(t, nil)
	runWatch("--timeout", "3s")
	if m.term.tint["t-w"] != orange || len(m.term.tintCalls()) != 1 {
		t.Fatalf("still stopped: %q, calls %q", m.term.tint["t-w"], m.term.tintCalls())
	}
	// Working again, then waiting on its own background task: the role's.
	turnHook(t, "turn-start", "UserPromptSubmit")
	fakeClock(t, func(n int) {
		if n == 2 {
			turnHook(t, "turn-stop", "Stop", "shell")
		}
	})
	runWatch("--timeout", "5s")
	if m.term.tint["t-w"] != teal || len(m.term.tintCalls()) != 2 {
		t.Fatalf("working again, waiting: %q, calls %q", m.term.tint["t-w"], m.term.tintCalls())
	}
	if m.term.tint["t-c"] != purple {
		t.Errorf("the coordinator's tab is %q", m.term.tint["t-c"])
	}
}

func TestWatchSilentTint(t *testing.T) {
	m := watched(t)
	liveWorker(t)
	turnHook(t, "turn-start", "UserPromptSubmit")
	if code, out, errs := runQM("watch", "--silent", "1ms"); code != 0 || m.term.tint["t-w"] != yellow {
		t.Fatalf("exit %d, tint %q, out %q, err %q", code, m.term.tint["t-w"], out, errs)
	}
}

func TestWatchGoneTint(t *testing.T) {
	other := "00000000-0000-4000-8000-0000000000ee"
	for _, c := range []struct {
		name, want string
		set        func(m *upMachine)
	}{
		{"its shell", red, func(m *upMachine) { m.term.fg = nil; m.term.showIdle("t-w") }},
		{"another program", red, func(m *upMachine) { m.term.tab("t-w", teal, "less") }},
		{"another session", teal, func(m *upMachine) { m.term.tab("t-w", teal, "claude", "--resume", other) }},
		{"an unnamed session", teal, func(m *upMachine) { m.term.tab("t-w", teal, "claude") }},
		{"closed", teal, func(m *upMachine) { delete(m.term.open, "t-w") }},
		{"tinted by hand", "pink icon", func(m *upMachine) { m.term.tab("t-w", "pink icon", "less") }},
	} {
		m := watched(t)
		c.set(m)
		code, out, errs := runQM("watch")
		if code != 0 || !strings.Contains(out, "w gone") || m.term.tint["t-w"] != c.want || len(noteLines(out+errs)) != 0 {
			t.Errorf("%s: exit %d, tint %q, want %q, out %q, err %q", c.name, code, m.term.tint["t-w"], c.want, out, errs)
		}
		// Another session's tab is decided on again; the others are settled.
		marks, err := fileMarks{path: tintMarksPath(up.ProjectID(m.root))}.Load()
		if _, marked := marks[upWorkerID]; err != nil || marked == strings.HasSuffix(c.name, " session") {
			t.Errorf("%s: marks %+v, %v", c.name, marks, err)
		}
	}
}

func TestWatchUnknownStateTintsNothing(t *testing.T) {
	m := watched(t)
	swapProcs(t, func() ([]claudecode.Proc, error) { return nil, errors.New("no ps") })
	fakeClock(t, nil)
	code, out, errs := runQM("watch", "--timeout", "3s", "--interval", "1s")
	if code != 0 || len(m.term.calls) != 0 {
		t.Errorf("exit %d, calls %q, out %q, err %q", code, m.term.calls, out, errs)
	}
}

// A qm up done, or qm down --keep-open, that runs while the watch is about to
// tint has the last word.
func TestWatchDoesNotTintAfterAQmThatTookTheTintOff(t *testing.T) {
	for name, change := range map[string]func(p *up.Project){
		"done":     func(p *up.Project) { p.Done = map[string]time.Time{upWorkerID: time.Now()} },
		"down":     func(p *up.Project) { p.DownAt = time.Now() },
		"dropped":  func(p *up.Project) { delete(p.Sessions, upWorkerID) },
		"replaced": func(p *up.Project) { p.Sessions[upWorkerID] = launch.SessionRef{Launcher: "agterm", ID: "t-2"} },
	} {
		m := watched(t)
		changed := false
		swap(t, &upAgtermCtl, launch.Agterm{Bin: "agtermctl", Run: func(ctx context.Context, bin string, args ...string) ([]byte, error) {
			if args[0] == "tree" && !changed {
				changed = true
				p := loadProject(t, m.root)
				change(&p)
				if err := up.Save(p); err != nil {
					t.Error(err)
				}
			}
			return m.term.run(ctx, bin, args...)
		}})
		if code, out, errs := runQM("watch"); code != 0 || !changed || m.term.tint["t-w"] != teal {
			t.Errorf("%s: exit %d, changed %v, tint %q, out %q, err %q", name, code, changed, m.term.tint["t-w"], out, errs)
		}
	}
}

// onTree runs change once, when a tint reads agterm's tabs: after the watch
// read the worker, before it tints.
func (m *upMachine) onTree(t *testing.T, change func()) {
	t.Helper()
	done := false
	swap(t, &upAgtermCtl, launch.Agterm{Bin: "agtermctl", Run: func(ctx context.Context, bin string, args ...string) ([]byte, error) {
		if args[0] == "tree" && !done {
			done = true
			change()
		}
		return m.term.run(ctx, bin, args...)
	}})
}

// A worker resumed between the watch's reading and its tint keeps the colour
// qm up gave it.
func TestWatchDoesNotTintAWorkerResumedMeanwhile(t *testing.T) {
	for name, change := range map[string]func(m *upMachine, p up.Project){
		"its session runs in the tab": func(m *upMachine, _ up.Project) {
			m.term.tab("t-w", teal, "claude", "--resume", upWorkerID)
		},
		"a newer Record": func(m *upMachine, p up.Project) {
			m.term.tab("t-w", teal, "/bin/sh")
			if err := declog.Append(route.Record{Time: time.Now().Add(time.Second), Kind: "up", Action: "resume", UpProject: p.ID, ClaudeSession: upWorkerID, Name: "w"}); err != nil {
				t.Error(err)
			}
		},
	} {
		m := watched(t)
		p := loadProject(t, m.root)
		m.term.fg = nil
		m.term.showIdle("t-w")
		m.onTree(t, func() { change(m, p) })
		if code, out, errs := runQM("watch"); code != 0 || !strings.Contains(out, "w gone") || m.term.tint["t-w"] != teal {
			t.Errorf("%s: exit %d, tint %q, out %q, err %q", name, code, m.term.tint["t-w"], out, errs)
		}
	}
}

// A qm up that left the worker as it is gave its tab no tint: its Record stands
// in no tint's way.
func TestWatchTintsAWorkerAQmUpLeftAsItIs(t *testing.T) {
	m := watched(t)
	p := loadProject(t, m.root)
	m.term.fg = nil
	m.onTree(t, func() {
		if err := declog.Append(route.Record{Time: time.Now().Add(time.Second), Kind: "up", Action: string(up.Leave), UpProject: p.ID, ClaudeSession: upWorkerID, Name: "w"}); err != nil {
			t.Error(err)
		}
	})
	if code, out, errs := runQM("watch"); code != 0 || m.term.tint["t-w"] != red {
		t.Errorf("exit %d, tint %q, out %q, err %q", code, m.term.tint["t-w"], out, errs)
	}
}

// A watch from before qm down --keep-open tints none of its tabs again, at a
// later check either.
func TestWatchFromBeforeTheDownTintsNothingAfterIt(t *testing.T) {
	m := watched(t)
	liveWorker(t)
	turnHook(t, "turn-start", "UserPromptSubmit")
	fakeClock(t, func(n int) {
		switch n {
		case 2:
			if code, out, errs := runQM("down", "--keep-open"); code != 0 || m.term.tint["t-w"] != "" {
				t.Errorf("down: exit %d, tint %q, out %q, err %q", code, m.term.tint["t-w"], out, errs)
			}
		case 60:
			turnHook(t, "turn-stop", "Stop")
		}
	})
	code, out, errs := runQM("watch", "--silent", "100000h", "--interval", "1s")
	if code != 0 || !strings.Contains(out, "w stopped ") || m.term.tint["t-w"] != "" {
		t.Errorf("exit %d, tint %q, out %q, err %q", code, m.term.tint["t-w"], out, errs)
	}
}

// Another watch can claim and report while this one tints.
func TestWatchTintDoesNotHoldTheEventMarks(t *testing.T) {
	m := watched(t)
	id := loadProject(t, m.root).ID
	tried := false
	m.term.onTint = func(string) {
		tried = true
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		if err := (fileMarks{path: marksPath(id)}).Update(ctx, func(watch.Marks) bool { return false }); err != nil {
			t.Errorf("the event marks are locked: %v", err)
		}
	}
	m.term.fg = nil
	if code, out, errs := runQM("watch"); code != 0 || !tried || m.term.tint["t-w"] != red {
		t.Errorf("exit %d, tint %q, out %q, err %q", code, m.term.tint["t-w"], out, errs)
	}
}

// The tab of a fresh restart that never ran is recorded for the new session,
// which qm up done marks with the one restored in its place.
func TestUpDoneTakesTheTintOffAFreshRestartThatNeverRan(t *testing.T) {
	m := newUpMachine(t)
	p := m.project(t)
	m.worker(t, p, "w1")
	recs, err := declog.Read(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	r := recs[len(recs)-1]
	r.Time, r.ClaudeSession, r.Replaces = time.Now().UTC(), upFreshID, upWorkerID
	if err := declog.Append(r); err != nil {
		t.Fatal(err)
	}
	p.Sessions = map[string]launch.SessionRef{upFreshID: {Launcher: "agterm", ID: "t-f", Name: "w1"}}
	p.Dropped = map[string]bool{upWorkerID: true}
	if err := up.Save(p); err != nil {
		t.Fatal(err)
	}
	m.tintTab(t, "t-f", launch.WorkerTint("teal"))
	// The project's lock is free while agterm is asked.
	m.term.onTint = func(string) {
		unlock, err := up.Lock(p.ID)
		if err != nil {
			t.Errorf("project locked: %v", err)
			return
		}
		unlock()
	}
	code, out, errs := runQM("up", "done", "w1")
	if code != 0 || len(loadProject(t, m.root).Done) != 2 || m.term.tint["t-f"] != "" {
		t.Errorf("exit %d, done %v, tint %q, out %q, err %q", code, loadProject(t, m.root).Done, m.term.tint["t-f"], out, errs)
	}
}

// Without agterm as the launcher a tab let go of is no word: agterm may not
// run.
func TestTintStartedIsSilentAboutReleasedTabsWithoutAgterm(t *testing.T) {
	m := newUpMachine(t)
	m.tintTab(t, "t-w", launch.WorkerTint("teal"))
	m.term.failTint = "connect(/x): Connection refused"
	before := map[string]launch.SessionRef{upWorkerID: {Launcher: "agterm", ID: "t-w"}}
	for _, l := range []launch.Launcher{nil, launch.Tmux{}} {
		if n := tintStarted(config.Defaults(), l, nil, before, nil); len(n) != 0 {
			t.Errorf("%T: notes %q", l, n)
		}
	}
	if n := tintStarted(config.Defaults(), launch.Agterm{}, nil, before, nil); len(n) != 1 {
		t.Errorf("agterm: notes %q", n)
	}
}

func TestWatchTintFailureIsNotedOnceAndReportsAllTheSame(t *testing.T) {
	m := watched(t)
	liveWorker(t)
	turnHook(t, "turn-start", "UserPromptSubmit")
	m.term.setTint("t-w", "")
	m.term.failTint = "session tint failed"
	fakeClock(t, func(n int) {
		if n == 200 {
			turnHook(t, "turn-stop", "Stop")
		}
	})
	code, out, errs := runQM("watch", "--silent", "100000h", "--interval", "1s")
	if code != 0 || !strings.Contains(out, "w stopped ") || len(noteLines(out+errs)) != 1 {
		t.Errorf("exit %d, out %q, err %q", code, out, errs)
	}
	if n := len(m.term.tintCalls()); n < 2 || n > 5 {
		t.Errorf("%d tries in 200 checks of a second, want one a minute", n)
	}
}
