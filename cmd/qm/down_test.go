package main

import (
	"bytes"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/declog"
	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/route"
	"github.com/IZZY-Agency/quartermaster/internal/up"
)

func TestDownCLI(t *testing.T) {
	m := newUpMachine(t)
	p := m.project(t)
	m.worker(t, p, "w1")
	p.Sessions = map[string]launch.SessionRef{
		upCoordID:  {Launcher: "agterm", ID: "t-c"},
		upWorkerID: {Launcher: "agterm", ID: "t-w"},
	}
	if err := up.Save(p); err != nil {
		t.Fatal(err)
	}
	m.term.showIdle("t-c", "t-w")
	var out, errb bytes.Buffer
	if code := run([]string{"down", "--yes"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	var closes []string
	for _, c := range m.term.calls {
		if strings.HasPrefix(c, "session close") {
			closes = append(closes, c)
		}
	}
	if len(closes) != 2 || !strings.Contains(closes[0], "t-w") || !strings.Contains(closes[1], "t-c") {
		t.Errorf("closes %v, want the worker then the coordinator", closes)
	}
	after := loadProject(t, m.root)
	if after.DownAt.IsZero() || len(after.Sessions) != 0 {
		t.Errorf("state %+v", after)
	}
	if got := upKinds(t); !strings.Contains(strings.Join(got, ";"), "w1 down") {
		t.Errorf("up records %v", got)
	}
}

func TestDownClosesSpawnedWorker(t *testing.T) {
	// A worker qm spawn worker started is in no project state, only in its
	// Record: qm down closes it all the same.
	m := newUpMachine(t)
	p := m.project(t)
	r := route.Record{
		Time: time.Now().UTC().Add(-time.Hour), Kind: "worker", Name: "w1", Cwd: m.root,
		UpProject: p.ID, ClaudeSession: upWorkerID, TermLauncher: "agterm", TermID: "t-w",
	}
	if err := declog.Append(r); err != nil {
		t.Fatal(err)
	}
	m.term.showIdle("t-w")
	var out, errb bytes.Buffer
	if code := run([]string{"down", "--yes"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if !slices.Contains(m.term.calls, "session close --target t-w --json") || !strings.Contains(out.String(), "(agterm t-w)") {
		t.Errorf("the spawned worker was not closed: calls %v, stdout %q", m.term.calls, out.String())
	}
}

func TestDownLeavesTabThatRunsSomethingElse(t *testing.T) {
	// Finding 3: a worker's tab outlived its session (typed-in launch, or
	// adopted) and now runs something else: qm down shows it, leaves it open
	// and drops its ref. The idle one is closed.
	m := newUpMachine(t)
	p := m.project(t)
	m.worker(t, p, "w1")
	p.Sessions = map[string]launch.SessionRef{
		upCoordID:  {Launcher: "agterm", ID: "t-c"},
		upWorkerID: {Launcher: "agterm", ID: "t-w"},
	}
	if err := up.Save(p); err != nil {
		t.Fatal(err)
	}
	m.term.show("t-w")
	m.term.showIdle("t-c")
	var out, errb bytes.Buffer
	if code := run([]string{"down", "--yes"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if slices.Contains(m.term.calls, "session close --target t-w --json") || !slices.Contains(m.term.calls, "session close --target t-c --json") {
		t.Errorf("calls %v, want t-c closed and t-w left open", m.term.calls)
	}
	if !strings.Contains(out.String(), "worker w1: agterm t-w (left open: it now runs something else)") ||
		!strings.Contains(out.String(), "recorded w1; its agterm t-w left open: it now runs something else") {
		t.Errorf("stdout %q", out.String())
	}
	if len(loadProject(t, m.root).Sessions) != 0 {
		t.Errorf("refs kept: %v", loadProject(t, m.root).Sessions)
	}
}

func TestDownSkipsWindowNotInWorkspace(t *testing.T) {
	// A recorded session the workspace no longer lists is never closed by
	// id, and its ref is dropped.
	m := newUpMachine(t)
	p := m.project(t)
	p.Sessions = map[string]launch.SessionRef{upCoordID: {Launcher: "agterm", ID: "t-gone"}}
	if err := up.Save(p); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := run([]string{"down", "--yes"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if len(m.term.acted()) != 0 {
		t.Errorf("closed %v", m.term.calls)
	}
	if len(loadProject(t, m.root).Sessions) != 0 {
		t.Errorf("the unlisted ref was kept")
	}
}

func TestDownCLIKeepOpen(t *testing.T) {
	m := newUpMachine(t)
	m.project(t)
	var out, errb bytes.Buffer
	if code := run([]string{"down", "--keep-open"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	for _, c := range m.term.calls {
		if strings.HasPrefix(c, "session close") {
			t.Errorf("closed with --keep-open: %s", c)
		}
	}
	if loadProject(t, m.root).DownAt.IsZero() {
		t.Errorf("DownAt not set")
	}
}

func TestDownNoTerminalNeedsYes(t *testing.T) {
	m := newUpMachine(t)
	m.project(t)
	var out, errb bytes.Buffer
	if code := run([]string{"down"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "--yes") {
		t.Errorf("exit %d, stderr %q", code, errb.String())
	}
	if !loadProject(t, m.root).DownAt.IsZero() {
		t.Errorf("state changed without an answer")
	}
}

func TestDownNoProject(t *testing.T) {
	newUpMachine(t)
	var out, errb bytes.Buffer
	if code := run([]string{"down", "--yes"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "no qm up project here") {
		t.Errorf("exit %d, stderr %q", code, errb.String())
	}
}

func downWithTwoSessions(t *testing.T) *upMachine {
	t.Helper()
	m := newUpMachine(t)
	p := m.project(t)
	p.Sessions = map[string]launch.SessionRef{upCoordID: {Launcher: "agterm", ID: "t-c"}}
	if err := up.Save(p); err != nil {
		t.Fatal(err)
	}
	m.term.showIdle("t-c")
	return m
}

func TestDownAsksOnATerminal(t *testing.T) {
	for _, tc := range []struct {
		answer string
		closed bool
	}{{"y\n", true}, {"yes\n", true}, {"n\n", false}, {"\n", false}, {"", false}} {
		m := downWithTwoSessions(t)
		swap(t, &upTerminal, func() bool { return true })
		swap(t, &upStdin, io.Reader(strings.NewReader(tc.answer)))
		var out, errb bytes.Buffer
		if code := run([]string{"down"}, &out, &errb); code != 0 {
			t.Fatalf("%q: exit %d, stderr %q", tc.answer, code, errb.String())
		}
		after := loadProject(t, m.root)
		if got := !after.DownAt.IsZero(); got != tc.closed {
			t.Errorf("%q: state saved %v, want %v", tc.answer, got, tc.closed)
		}
		if !strings.Contains(out.String(), "Close them? [y/N] ") || strings.Contains(out.String(), "nothing closed") == tc.closed {
			t.Errorf("%q: output %q", tc.answer, out.String())
		}
		if tc.closed && !slices.ContainsFunc(m.term.calls, func(c string) bool { return strings.HasPrefix(c, "session close") }) {
			t.Errorf("%q: nothing closed: %v", tc.answer, m.term.calls)
		}
	}
}

func TestDownClosesFinishedSessionToo(t *testing.T) {
	m := newUpMachine(t)
	p := m.project(t)
	m.worker(t, p, "w1")
	p.Done = map[string]time.Time{upWorkerID: time.Now()}
	p.Sessions = map[string]launch.SessionRef{upWorkerID: {Launcher: "agterm", ID: "t-w"}}
	if err := up.Save(p); err != nil {
		t.Fatal(err)
	}
	m.term.showIdle("t-w")
	var out, errb bytes.Buffer
	if code := run([]string{"down", "--yes"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if !slices.ContainsFunc(m.term.calls, func(c string) bool { return strings.Contains(c, "session close") && strings.Contains(c, "t-w") }) {
		t.Errorf("the finished worker's shell was not closed: %v", m.term.calls)
	}
	if len(loadProject(t, m.root).Sessions) != 0 {
		t.Errorf("its ref was kept")
	}
	for _, k := range upKinds(t) {
		if k == "w1 down" {
			t.Errorf("a finished worker got a down Record")
		}
	}
}
