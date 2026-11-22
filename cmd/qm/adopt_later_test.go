package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/declog"
	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/route"
	"github.com/IZZY-Agency/quartermaster/internal/up"
)

const laterNote = "running session(s) in the project's folders are not in the project: qm up --adopt lists them"

// newLaterMachine is an adoptMachine whose project already has state: the
// running session 411 is its coordinator (recorded in agterm tab s-c), a
// worker finished earlier is marked done. The hand-started session 413 and
// the side session 414 (added by the test) are the unheld ones.
func newLaterMachine(t *testing.T) *adoptMachine {
	t.Helper()
	m := newAdoptMachine(t)
	p := m.project(t)
	p.Coordinator = up.Session{ID: adoptCoordID, Name: "proj-coord", Started: time.Now().Add(-5 * time.Hour), Adopted: true}
	p.Sessions = map[string]launch.SessionRef{adoptCoordID: {Launcher: "agterm", ID: "s-c", Name: "proj-coord"}}
	p.Done = map[string]time.Time{upWorkerID: time.Now().Add(-time.Hour)}
	if err := up.Save(p); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestUpAdoptLaterFlagsUsage(t *testing.T) {
	newLaterMachine(t)
	for _, args := range [][]string{
		{"up", "--new"}, {"up", "--adopt-coordinator", "0000"},
		{"up", "--adopt", "--full"}, {"up", "--adopt", "--", "--model", "opus"},
	} {
		if code, _, errs := runQM(args...); code != 2 {
			t.Fatalf("%q: exit %d, stderr %q", args, code, errs)
		}
	}
	if code, _, errs := runQM("up", "--new"); !strings.Contains(errs, "first run") {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
}

func TestUpAdoptLaterAsksWorkersOnly(t *testing.T) {
	m := newLaterMachine(t)
	m.live(t, 414, adoptSideID, "side", "claude")
	swap(t, &upTerminal, func() bool { return true })
	swap(t, &upStdin, io.Reader(strings.NewReader("a\ny\n")))
	code, out, errs := runQM("up", "--adopt")
	if code != 0 {
		t.Fatalf("exit %d, out %q, err %q", code, out, errs)
	}
	for _, s := range []string{"w-hand", "side", "Workers: row numbers, a for all the rows", "adopt worker w-hand", "Proceed? [y/N]"} {
		if !strings.Contains(out, s) {
			t.Fatalf("output lacks %q:\n%s", s, out)
		}
	}
	if strings.Contains(out, "Coordinator:") || strings.Contains(out, "adopt coordinator") || strings.Contains(out, "proj-coord  ") {
		t.Fatalf("the coordinator was offered or asked about:\n%s", out)
	}
	last := strings.Split(strings.TrimSpace(out), "\n")
	if want := "project " + up.ProjectID(m.root) + ": adopted 2 worker(s); nothing restarted"; last[len(last)-1] != want {
		t.Fatalf("last line %q, want %q", last[len(last)-1], want)
	}
	p := loadProject(t, m.root)
	if p.Coordinator.ID != adoptCoordID || p.Workspace != "proj" || p.Launcher != "agterm" {
		t.Fatalf("project changed: %+v", p)
	}
	if p.Adopted[adoptHandID].Name != "w-hand" || p.Adopted[adoptSideID].Name != "side" {
		t.Fatalf("adopted %+v", p.Adopted)
	}
	if p.Sessions[adoptHandID].ID != "s-h" || p.Sessions[adoptCoordID].ID != "s-c" {
		t.Fatalf("sessions %+v", p.Sessions)
	}
	if _, ok := p.Done[upWorkerID]; !ok {
		t.Fatalf("done lost: %+v", p.Done)
	}
	if len(m.term.calls) != 0 {
		t.Fatalf("a session was started or typed into: %q", m.term.calls)
	}
	for _, c := range m.agterm.acted() {
		if strings.HasPrefix(c, "session move") || strings.HasPrefix(c, "session close") || strings.HasPrefix(c, "session type") {
			t.Fatalf("agterm acted: %q", c)
		}
	}
	kinds := upKinds(t)
	if len(kinds) != 2 || (kinds[0] != "w-hand adopt" && kinds[1] != "w-hand adopt") {
		t.Fatalf("records %q", kinds)
	}
}

func TestUpAdoptLaterPicksRows(t *testing.T) {
	m := newLaterMachine(t)
	m.live(t, 414, adoptSideID, "side", "claude")
	swap(t, &upTerminal, func() bool { return true })
	// Rows are sorted by name: 1 side, 2 w-hand.
	swap(t, &upStdin, io.Reader(strings.NewReader("2\ny\n")))
	if code, out, errs := runQM("up", "--adopt"); code != 0 || !strings.Contains(out, "adopted 1 worker(s)") {
		t.Fatalf("exit %d, out %q, err %q", code, out, errs)
	}
	p := loadProject(t, m.root)
	if _, ok := p.Adopted[adoptHandID]; !ok || len(p.Adopted) != 1 {
		t.Fatalf("adopted %+v", p.Adopted)
	}
}

func TestUpAdoptLaterEmptyOrDeclinedAdoptsNothing(t *testing.T) {
	for _, in := range []string{"\n", "1\nn\n"} {
		m := newLaterMachine(t)
		swap(t, &upTerminal, func() bool { return true })
		swap(t, &upStdin, io.Reader(strings.NewReader(in)))
		if code, out, errs := runQM("up", "--adopt"); code != 0 || !strings.Contains(out, "nothing adopted") {
			t.Fatalf("%q: exit %d, out %q, err %q", in, code, out, errs)
		}
		if p := loadProject(t, m.root); len(p.Adopted) != 0 {
			t.Fatalf("%q: adopted %+v", in, p.Adopted)
		}
		if recs, _ := declog.Read(time.Time{}); len(recs) != 0 {
			t.Fatalf("%q: logged %+v", in, recs)
		}
	}
}

func TestUpAdoptLaterYesAdoptsRecordedWorkersOnly(t *testing.T) {
	// --yes, and no terminal, adopt the sessions a worker Record names only.
	for _, args := range [][]string{{"up", "--adopt", "--yes"}, {"up", "--adopt"}} {
		m := newLaterMachine(t)
		m.live(t, 414, adoptSideID, "side", "claude")
		if err := declog.Append(route.Record{Time: time.Now(), Kind: "worker", ClaudeSession: adoptHandID, Name: "w-hand", Cwd: m.root}); err != nil {
			t.Fatal(err)
		}
		code, out, errs := runQM(args...)
		if code != 0 || !strings.Contains(out, "adopt worker w-hand") || !strings.Contains(out, "not adopted: side") || !strings.Contains(out, "run qm up --adopt in a terminal to pick it") {
			t.Fatalf("%q: exit %d, out %q, err %q", args, code, out, errs)
		}
		p := loadProject(t, m.root)
		if _, ok := p.Adopted[adoptHandID]; !ok || len(p.Adopted) != 1 {
			t.Fatalf("%q: adopted %+v", args, p.Adopted)
		}
	}
}

func TestUpAdoptLaterNothingAdoptableFails(t *testing.T) {
	m := newLaterMachine(t)
	for _, args := range [][]string{{"up", "--adopt", "--yes"}, {"up", "--adopt"}} {
		// 413 is hand-started: no Record names it, so a script adopts nothing.
		code, _, errs := runQM(args...)
		if code != 1 || !strings.Contains(errs, "no running session to adopt") || !strings.Contains(errs, "w-hand") || !strings.Contains(errs, "run qm up --adopt in a terminal to pick it") {
			t.Fatalf("%q: exit %d, err %q", args, code, errs)
		}
	}
	if p := loadProject(t, m.root); len(p.Adopted) != 0 {
		t.Fatalf("adopted %+v", p.Adopted)
	}
	// No candidate at all.
	m.procs = m.procs[:1]
	if err := os.Remove(filepath.Join(m.claude, "sessions", "413.json")); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := runQM("up", "--adopt", "--yes"); code != 1 || !strings.Contains(errs, "no running session to adopt") {
		t.Fatalf("exit %d, err %q", code, errs)
	}
}

func TestUpAdoptLaterHeldSessionsAreNotCandidates(t *testing.T) {
	m := newLaterMachine(t)
	p := loadProject(t, m.root)
	// 413 becomes adopted, 414 a worker a Record ties and marked done, 415 a
	// worker a Record ties that a later Record replaced.
	m.live(t, 414, adoptSideID, "side", "claude")
	const replacedID = "00000000-0000-4000-8000-000000000415"
	m.live(t, 415, replacedID, "old", "claude")
	p.Adopted = map[string]up.AdoptedSession{adoptHandID: {Name: "w-hand", Cwd: m.root}}
	p.Done = map[string]time.Time{adoptSideID: time.Now()}
	if err := up.Save(p); err != nil {
		t.Fatal(err)
	}
	for _, r := range []route.Record{
		{Time: time.Now().Add(-3 * time.Hour), Kind: "worker", UpProject: p.ID, ClaudeSession: adoptSideID, Name: "side", Cwd: m.root},
		{Time: time.Now().Add(-3 * time.Hour), Kind: "worker", UpProject: p.ID, ClaudeSession: replacedID, Name: "old", Cwd: m.root},
		{Time: time.Now().Add(-2 * time.Hour), Kind: "worker", UpProject: p.ID, ClaudeSession: upWorkerID, Name: "new", Cwd: m.root, Replaces: replacedID},
	} {
		if err := declog.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	code, _, errs := runQM("up", "--adopt", "--yes")
	if code != 1 || !strings.Contains(errs, "no running session to adopt") || strings.Contains(errs, "w-hand") || strings.Contains(errs, "side") || strings.Contains(errs, "old") {
		t.Fatalf("exit %d, err %q", code, errs)
	}
	// A restore offers none of them either.
	if _, out, _ := runQM("up", "--dry-run"); strings.Contains(out, laterNote) {
		t.Fatalf("restore notes held sessions:\n%s", out)
	}
}

func TestUpAdoptLaterHeldElsewhereIsNotCandidate(t *testing.T) {
	newLaterMachine(t)
	other := up.Project{ID: "other-1a2b3c4d", Root: "/elsewhere", Workspace: "else", Launcher: "agterm", Coordinator: up.Session{ID: "00000000-0000-4000-8000-0000000000ff"}, Adopted: map[string]up.AdoptedSession{adoptHandID: {}}}
	if err := up.Save(other); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := runQM("up", "--adopt", "--yes"); code != 1 || strings.Contains(errs, "w-hand") {
		t.Fatalf("exit %d, err %q", code, errs)
	}
}

func TestUpAdoptLaterCoordinatorRunsIt(t *testing.T) {
	m := newLaterMachine(t)
	m.inside(411)
	if err := declog.Append(route.Record{Time: time.Now(), Kind: "worker", ClaudeSession: adoptHandID, Name: "w-hand", Cwd: m.root}); err != nil {
		t.Fatal(err)
	}
	code, out, errs := runQM("up", "--adopt", "--yes")
	if code != 0 || !strings.Contains(out, "adopted 1 worker(s)") {
		t.Fatalf("exit %d, out %q, err %q", code, out, errs)
	}
	if p := loadProject(t, m.root); p.Coordinator.ID != adoptCoordID || len(p.Adopted) != 1 {
		t.Fatalf("state %+v", p)
	}
}

func TestUpAdoptLaterSelfIsListedNotPicked(t *testing.T) {
	m := newLaterMachine(t)
	m.inside(413)
	// 413 runs the command: listed as "this session", but a script does not
	// adopt a session nobody named.
	code, _, errs := runQM("up", "--adopt", "--yes")
	if code != 1 || !strings.Contains(errs, "this session") || !strings.Contains(errs, "no running session to adopt") {
		t.Fatalf("exit %d, err %q", code, errs)
	}
	swap(t, &upTerminal, func() bool { return true })
	swap(t, &upStdin, io.Reader(strings.NewReader("1\ny\n")))
	if code, out, errs := runQM("up", "--adopt"); code != 0 || !strings.Contains(out, "adopted 1 worker(s)") {
		t.Fatalf("exit %d, out %q, err %q", code, out, errs)
	}
}

func TestUpAdoptLaterDryRunWritesNothing(t *testing.T) {
	m := newLaterMachine(t)
	swap(t, &upTerminal, func() bool { return true })
	code, out, errs := runQM("up", "--adopt", "--dry-run")
	if code != 0 || !strings.Contains(out, "w-hand") || !strings.Contains(out, "would ask") || !strings.Contains(out, "dry run: nothing adopted") {
		t.Fatalf("exit %d, out %q, err %q", code, out, errs)
	}
	swap(t, &upTerminal, func() bool { return false })
	if err := declog.Append(route.Record{Time: time.Now(), Kind: "worker", ClaudeSession: adoptHandID, Name: "w-hand", Cwd: m.root}); err != nil {
		t.Fatal(err)
	}
	before, _ := declog.Read(time.Time{})
	code, out, errs = runQM("up", "--adopt", "--dry-run", "--yes")
	// Section 4.8: the dry run prints the rows, then what would be adopted.
	if code != 0 || !strings.Contains(out, "NAME") || !strings.Contains(out, "KNOWN") || !strings.Contains(out, "adopt worker w-hand") || !strings.Contains(out, "dry run: nothing adopted") {
		t.Fatalf("exit %d, out %q, err %q", code, out, errs)
	}
	if strings.Index(out, "KNOWN") > strings.Index(out, "adopt worker w-hand") {
		t.Fatalf("the rows come after the plan:\n%s", out)
	}
	if p := loadProject(t, m.root); len(p.Adopted) != 0 {
		t.Fatalf("dry run adopted %+v", p.Adopted)
	}
	if after, _ := declog.Read(time.Time{}); len(after) != len(before) {
		t.Fatalf("dry run logged %+v", after)
	}
	if len(m.term.calls) != 0 || len(m.agterm.acted()) != 0 {
		t.Fatalf("dry run acted: %q %q", m.term.calls, m.agterm.acted())
	}
}

func TestUpAdoptLaterBlindDetectionFails(t *testing.T) {
	m := newLaterMachine(t)
	if err := os.RemoveAll(filepath.Join(m.claude, "sessions")); err != nil {
		t.Fatal(err)
	}
	code, _, errs := runQM("up", "--adopt", "--yes")
	if code != 1 || !strings.Contains(errs, "sessions folder is missing") {
		t.Fatalf("exit %d, err %q", code, errs)
	}
	if p := loadProject(t, m.root); len(p.Adopted) != 0 {
		t.Fatalf("adopted %+v", p.Adopted)
	}
}

func TestUpRestoreNotesUnheldSessions(t *testing.T) {
	m := newLaterMachine(t)
	m.live(t, 414, adoptSideID, "side", "claude")
	for _, args := range [][]string{{"up", "--dry-run"}, {"up", "--yes"}} {
		code, out, errs := runQM(args...)
		if code != 0 || strings.Count(out, "2 "+laterNote) != 1 {
			t.Fatalf("%q: exit %d, out %q, err %q", args, code, out, errs)
		}
	}
	// A session in a subfolder cannot be adopted: it is not counted.
	m2 := newLaterMachine(t)
	sub := filepath.Join(m2.root, "sub")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	m2.procs = m2.procs[:1]
	if err := os.Remove(filepath.Join(m2.claude, "sessions", "413.json")); err != nil {
		t.Fatal(err)
	}
	m2.liveIn(t, 416, "00000000-0000-4000-8000-000000000416", "deep", "claude", sub)
	if _, out, _ := runQM("up", "--dry-run"); strings.Contains(out, laterNote) {
		t.Fatalf("a subfolder session was counted:\n%s", out)
	}
}

func TestUpRestoreNoteIsSilentWhenDetectionFails(t *testing.T) {
	m := newLaterMachine(t)
	if err := os.RemoveAll(filepath.Join(m.claude, "sessions")); err != nil {
		t.Fatal(err)
	}
	code, out, errs := runQM("up", "--dry-run")
	if code != 0 || strings.Contains(out, laterNote) || strings.Contains(errs, "not in the project") {
		t.Fatalf("exit %d, out %q, err %q", code, out, errs)
	}
}

func TestUpRestoreNoteAbsentWithoutUnheldSessions(t *testing.T) {
	m := newLaterMachine(t)
	m.procs = m.procs[:1]
	if err := os.Remove(filepath.Join(m.claude, "sessions", "413.json")); err != nil {
		t.Fatal(err)
	}
	if code, out, errs := runQM("up", "--dry-run"); code != 0 || strings.Contains(out, laterNote) {
		t.Fatalf("exit %d, out %q, err %q", code, out, errs)
	}
}

// An --adopt-coordinator that is given but empty (an unset shell variable) is
// a usage error everywhere; on a project with state it is a first-run flag
// like --new, whatever its value.
func TestUpAdoptCoordinatorGivenEmptyIsUsageError(t *testing.T) {
	m := newLaterMachine(t)
	if err := declog.Append(route.Record{Time: time.Now(), Kind: "worker", ClaudeSession: adoptHandID, Name: "w-hand", Cwd: m.root}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"up", "--adopt", "--yes", "--adopt-coordinator", ""},
		{"up", "--adopt-coordinator=", "--yes"},
	} {
		if code, _, errs := runQM(args...); code != 2 || !strings.Contains(errs, "--adopt-coordinator") {
			t.Fatalf("%q: exit %d, stderr %q", args, code, errs)
		}
	}
	if p := loadProject(t, m.root); len(p.Adopted) != 0 {
		t.Fatalf("adopted %+v", p.Adopted)
	}
	// A first run too: an empty id is no coordinator.
	m2 := newAdoptMachine(t)
	if code, _, errs := runQM("up", "--adopt", "--adopt-coordinator", ""); code != 2 || !strings.Contains(errs, "--adopt-coordinator") {
		t.Fatalf("first run: exit %d, stderr %q", code, errs)
	}
	if _, found, _ := up.Load(m2.root); found {
		t.Fatal("a first run saved state")
	}
}

// qm up --adopt </dev/null is a script: stdin is a character device but no
// terminal, so it adopts the recorded workers without asking.
func TestUpAdoptLaterStdinDevNullIsScripted(t *testing.T) {
	m := newLaterMachine(t)
	if err := declog.Append(route.Record{Time: time.Now(), Kind: "worker", ClaudeSession: adoptHandID, Name: "w-hand", Cwd: m.root}); err != nil {
		t.Fatal(err)
	}
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = null.Close() }()
	old := os.Stdin
	os.Stdin = null
	defer func() { os.Stdin = old }()
	// upTerminal is the real one here.
	code, out, errs := runQM("up", "--adopt")
	if code != 0 || !strings.Contains(out, "adopted 1 worker(s)") || strings.Contains(out, "Workers: row numbers") {
		t.Fatalf("exit %d, out %q, err %q", code, out, errs)
	}
}

// A restore must not wait for, or leak, a stalled detection: the note's
// subprocesses share its one deadline and are cancelled with it.
func TestUpRestoreNoteDetectionIsCancelledAtTheDeadline(t *testing.T) {
	m := newLaterMachine(t)
	m.live(t, 414, adoptSideID, "side", "claude")
	swap(t, &upNoteTimeout, 300*time.Millisecond)
	inner := upRun
	cancelled := make(chan bool, 4)
	swap(t, &upRun, up.DirRunner(func(ctx context.Context, dir, bin string, args ...string) ([]byte, error) {
		if bin == "git" && len(args) > 0 && args[0] == "worktree" {
			select {
			case <-ctx.Done():
				cancelled <- true
				return nil, ctx.Err()
			case <-time.After(30 * time.Second):
				cancelled <- false
				return nil, errors.New("stalled")
			}
		}
		return inner(ctx, dir, bin, args...)
	}))
	// Registered after the swaps, so it runs before they are put back: the
	// detection goes on after the deadline and reads them.
	t.Cleanup(upNoteRunning.Wait)
	start := time.Now()
	code, out, errs := runQM("up", "--dry-run")
	if code != 0 || strings.Contains(out, laterNote) {
		t.Fatalf("exit %d, out %q, err %q", code, out, errs)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("the restore waited %v for the note", time.Since(start))
	}
	select {
	case ok := <-cancelled:
		if !ok {
			t.Fatal("the stalled subprocess was not cancelled")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the note's detection never ran its worktree probe")
	}
}
