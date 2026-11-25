package spawn

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/declog"
	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/route"
)

// launcherFunc is a launcher that runs f.
type launcherFunc func(launch.LaunchSpec) (launch.SessionRef, error)

func (f launcherFunc) Spawn(_ context.Context, s launch.LaunchSpec) (launch.SessionRef, error) {
	return f(s)
}

func starting(t *testing.T) []Starting {
	t.Helper()
	st, err := ListStarting()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// recoverAll recovers what a stopped qm left, as qm up does.
func recoverAll(t *testing.T) []string {
	t.Helper()
	notes, err := RecoverStarting(declog.Append)
	if err != nil {
		t.Fatal(err)
	}
	return notes
}

// TestWorkerHeldBeforeItIsVisible: while the launcher starts the session its
// Record is reserved, with its project; afterwards it is logged once and
// nothing is reserved.
func TestWorkerHeldBeforeItIsVisible(t *testing.T) {
	for _, project := range []string{"", "try-00000000"} {
		hermetic(t)
		var seen []Starting
		var held []route.Record
		l := launcherFunc(func(s launch.LaunchSpec) (launch.SessionRef, error) {
			seen = starting(t)
			held = AddStarting(records(t), seen)
			if n := recoverAll(t); len(n) != 0 || len(starting(t)) != 1 {
				t.Errorf("a running spawn was recovered: %q", n)
			}
			return launch.SessionRef{Launcher: "fake", ID: "s-1", Name: s.Name}, nil
		})
		req := request(t, repoWith(t, "README.md"), "docs")
		req.UpProject = project
		res, err := Worker(context.Background(), req, testDeps(l))
		if err != nil {
			t.Fatal(err)
		}
		if len(seen) != 1 || seen[0].SessionID != res.SessionID || !seen[0].Running {
			t.Fatalf("reserved during the launch: %+v", seen)
		}
		if len(held) != 1 || held[0].Kind != "worker" || held[0].ClaudeSession != res.SessionID || held[0].UpProject != project || held[0].Decision == nil {
			t.Errorf("records during the launch: %+v", held)
		}
		if recs := records(t); len(recs) != 1 || recs[0].TermID != "s-1" || recs[0].Detail != "fake s-1" {
			t.Errorf("logged %+v", recs)
		}
		if st := starting(t); len(st) != 0 {
			t.Errorf("still reserved: %+v", st)
		}
	}
}

func TestWorkerRefusedIsWithdrawn(t *testing.T) {
	hermetic(t)
	l := launcherFunc(func(launch.LaunchSpec) (launch.SessionRef, error) {
		return launch.SessionRef{}, errors.New("no such workspace")
	})
	_, err := Worker(context.Background(), request(t, repoWith(t, "README.md"), "docs"), testDeps(l))
	if err == nil || !strings.Contains(err.Error(), "no such workspace") {
		t.Fatalf("err %v", err)
	}
	if st, recs := starting(t), records(t); len(st) != 0 || len(recs) != 0 || len(launchDirEntries(t)) != 0 {
		t.Errorf("left: reserved %+v, records %+v, files %v", st, recs, launchDirEntries(t))
	}
}

func TestWorkerMaybeStartedStaysHeld(t *testing.T) {
	hermetic(t)
	l := launcherFunc(func(launch.LaunchSpec) (launch.SessionRef, error) {
		return launch.SessionRef{}, fmt.Errorf("%w: no answer", launch.ErrMaybeStarted)
	})
	res, err := Worker(context.Background(), request(t, repoWith(t, "README.md"), "docs"), testDeps(l))
	if !errors.Is(err, launch.ErrMaybeStarted) {
		t.Fatalf("err %v", err)
	}
	recs := records(t)
	if len(recs) != 1 || recs[0].ClaudeSession != res.SessionID || recs[0].Detail != "launch failed, may have started: "+res.ScriptPath || recs[0].TermID != "" {
		t.Errorf("records %+v", recs)
	}
	if len(starting(t)) != 0 || len(launchDirEntries(t)) != 2 {
		t.Errorf("reserved %+v, files %v; want none, and the script and the brief", starting(t), launchDirEntries(t))
	}
}

// TestTwoSpawnsAtOnce: each holds its own worker while both start.
func TestTwoSpawnsAtOnce(t *testing.T) {
	hermetic(t)
	repo := repoWith(t, "README.md")
	var both, done sync.WaitGroup
	both.Add(2)
	var mu sync.Mutex
	most := 0
	l := launcherFunc(func(s launch.LaunchSpec) (launch.SessionRef, error) {
		both.Done()
		both.Wait()
		st, err := ListStarting()
		mu.Lock()
		defer mu.Unlock()
		if err == nil {
			most = max(most, len(st))
		}
		return launch.SessionRef{Launcher: "fake", ID: s.Name, Name: s.Name}, err
	})
	for _, name := range []string{"task-a", "task-b"} {
		req := request(t, repo, "docs")
		req.Name = name
		done.Go(func() {
			if _, err := Worker(context.Background(), req, testDeps(l)); err != nil {
				t.Error(err)
			}
		})
	}
	done.Wait()
	if most != 2 || len(records(t)) != 2 || len(starting(t)) != 0 {
		t.Errorf("held at once %d, records %d, left %d; want 2, 2, 0", most, len(records(t)), len(starting(t)))
	}
}

// TestSpawnKilledBeforeTheLaunch: the process that reserved is killed in its
// launch; the next qm up logs the Record, once.
func TestSpawnKilledBeforeTheLaunch(t *testing.T) {
	hermetic(t)
	repo, brief := repoWith(t, "README.md"), writeFile(t, filepath.Join(t.TempDir(), "b.md"), "Do it.\n")
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperSpawnAndHang$")
	cmd.Env = append(os.Environ(), "QM_TEST_HANG_REPO="+repo, "QM_TEST_HANG_BRIEF="+brief)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || line != "launching\n" {
		_ = cmd.Process.Kill()
		t.Fatalf("helper said %q, %v", line, err)
	}
	if st := starting(t); len(st) != 1 || !st[0].Running {
		t.Errorf("while it launches: %+v", st)
	}
	if n := recoverAll(t); len(n) != 0 {
		t.Errorf("recovered while it launches: %q", n)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	st := starting(t)
	if len(st) != 1 || st[0].Running || len(AddStarting(records(t), st)) != 1 || len(records(t)) != 0 {
		t.Fatalf("after the kill: %+v", st)
	}
	if n := recoverAll(t); len(n) != 1 || !strings.Contains(n[0], "worker task-a: an earlier qm spawn worker stopped while starting session ") {
		t.Errorf("notes %q", n)
	}
	recs := records(t)
	if len(recs) != 1 || recs[0].ClaudeSession != st[0].SessionID || !strings.HasPrefix(recs[0].Detail, "launch not confirmed: ") {
		t.Errorf("records %+v", recs)
	}
	if n := recoverAll(t); len(n) != 0 || len(records(t)) != 1 || len(starting(t)) != 0 || briefOf(st[0].SessionID) == "" {
		t.Errorf("recovered twice, or the brief is gone: %q", n)
	}
}

// TestHelperSpawnAndHang is the process TestSpawnKilledBeforeTheLaunch kills.
func TestHelperSpawnAndHang(t *testing.T) {
	repo := os.Getenv("QM_TEST_HANG_REPO")
	if repo == "" {
		t.Skip("run by TestSpawnKilledBeforeTheLaunch")
	}
	l := launcherFunc(func(launch.LaunchSpec) (launch.SessionRef, error) {
		fmt.Println("launching")
		time.Sleep(time.Minute)
		return launch.SessionRef{}, errors.New("not killed")
	})
	_, _ = Worker(context.Background(), WorkerRequest{
		Brief: os.Getenv("QM_TEST_HANG_BRIEF"), Type: "docs", Size: "S", Name: "task-a", Cwd: repo,
		Hint: &route.Tier{Model: "sonnet", Effort: "medium"},
	}, testDeps(l))
}

// TestRecordNotLogged: a started worker whose Records cannot be logged stays
// reserved, its tab with it, for the next qm up to log.
func TestRecordNotLogged(t *testing.T) {
	hermetic(t)
	path, err := declog.Path()
	if err != nil {
		t.Fatal(err)
	}
	mkdir(t, path) // a folder: nothing is appended
	_, err = Worker(context.Background(), request(t, repoWith(t, "README.md"), "docs"), testDeps(&fakeLauncher{}))
	if err == nil || !strings.Contains(err.Error(), "session s-1 started, but the decision was not logged") {
		t.Fatalf("err %v", err)
	}
	st := starting(t)
	if len(st) != 1 || st[0].Running || st[0].Records[0].TermID != "s-1" {
		t.Fatalf("reserved %+v", st)
	}
	if _, err := RecoverStarting(func(route.Record) error { return errors.New("disk full") }); err == nil || len(starting(t)) != 1 {
		t.Errorf("err %v, reserved %d; want an error and 1", err, len(starting(t)))
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if n := recoverAll(t); len(n) != 1 || len(records(t)) != 1 || records(t)[0].TermID != "s-1" {
		t.Errorf("notes %q, records %+v", n, records(t))
	}
}

// TestRecoverReviewWorker: a review session's Records are each logged once,
// whichever of them its qm had logged when it stopped.
func TestRecoverReviewWorker(t *testing.T) {
	hermetic(t)
	req := WorkerRequest{Type: "review", Name: "rv", Cwd: "/w", Size: "S"}
	for logged := range 3 {
		sid, err := NewSessionID()
		if err != nil {
			t.Fatal(err)
		}
		recs := workerRecords(route.Decision{Kind: "review"}, req, "/w", "b", 1, sid, time.Now().UTC())
		r, err := reserveWorker(sid, recs)
		if err != nil {
			t.Fatal(err)
		}
		for _, rec := range recs[:logged] {
			if err := declog.Append(rec); err != nil {
				t.Fatal(err)
			}
		}
		_ = r.f.Close() // its qm ends
		recoverAll(t)
		if got := len(records(t)); got != 2*(logged+1) {
			t.Errorf("%d logged before: %d records, want %d", logged, got, 2*(logged+1))
		}
	}
}

// TestUnconfirmedReviewIsNoRound: a reviewer session whose launch is not
// confirmed (no answer, or its qm killed) does not count as a review round;
// one that started does.
func TestUnconfirmedReviewIsNoRound(t *testing.T) {
	hermetic(t)
	repo := repoWith(t, "README.md")
	git(t, repo, "update-ref", "refs/remotes/origin/main", "main")
	git(t, repo, "checkout", "-q", "-b", "feature")
	writeFile(t, filepath.Join(repo, "notes.txt"), "one\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "second")
	kinds := func() (out []string) {
		for _, r := range records(t) {
			out = append(out, r.Kind)
		}
		return out
	}
	var inLaunch []route.Record
	fail := true
	l := launcherFunc(func(s launch.LaunchSpec) (launch.SessionRef, error) {
		inLaunch = starting(t)[0].Records
		if fail {
			return launch.SessionRef{}, launch.ErrMaybeStarted
		}
		return launch.SessionRef{Launcher: "fake", ID: "s-1", Name: s.Name}, nil
	})
	if _, err := Worker(context.Background(), request(t, repo, "review"), testDeps(l)); !errors.Is(err, launch.ErrMaybeStarted) {
		t.Fatalf("err %v", err)
	}
	if len(inLaunch) != 1 || inLaunch[0].Kind != "worker" || strings.Join(kinds(), " ") != "worker" {
		t.Errorf("reserved %+v, logged %v; want the worker Record alone", inLaunch, kinds())
	}
	fail = false
	res, err := Worker(context.Background(), request(t, repo, "review"), testDeps(l))
	if err != nil {
		t.Fatal(err)
	}
	if recs := records(t); strings.Join(kinds(), " ") != "worker review worker" || recs[1].Round != 1 || res.Decision.Row == "fix-check" {
		t.Errorf("after the retry: %v, row %s", kinds(), res.Decision.Row)
	}
}

func TestNotReservedNotStarted(t *testing.T) {
	hermetic(t)
	dir, err := startingDir()
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "in the way")
	l := &fakeLauncher{}
	_, err = Worker(context.Background(), request(t, repoWith(t, "README.md"), "docs"), testDeps(l))
	if err == nil || len(l.specs) != 0 || len(launchDirEntries(t)) != 0 {
		t.Errorf("err %v, launched %d, files %v", err, len(l.specs), launchDirEntries(t))
	}
}

// TestUnreadableReservation: the error names the file, and the others are
// still recovered. Other files in the folder are no reservations.
func TestUnreadableReservation(t *testing.T) {
	hermetic(t)
	recs := workerRecords(route.Decision{Kind: "worker"}, WorkerRequest{Type: "docs", Name: "task-a"}, "/w", "b", 0, testSession, time.Now().UTC())
	r, err := reserveWorker(testSession, recs)
	if err != nil {
		t.Fatal(err)
	}
	_ = r.f.Close()
	dir, _ := startingDir()
	writeFile(t, filepath.Join(dir, "notes.json"), "x")
	writeFile(t, filepath.Join(dir, ".1.tmp"), "x")
	if st := starting(t); len(st) != 1 {
		t.Fatalf("listed %+v", st)
	}
	bad := writeFile(t, filepath.Join(dir, "00000000-0000-4000-8000-000000000000.json"), "{")
	if _, err := ListStarting(); err == nil || !strings.Contains(err.Error(), bad) {
		t.Errorf("err %v, want it to name %s", err, bad)
	}
	if _, err := RecoverStarting(declog.Append); err == nil || !strings.Contains(err.Error(), bad) || len(records(t)) != 1 {
		t.Errorf("err %v, records %d", err, len(records(t)))
	}
}
