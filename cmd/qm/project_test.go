package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
	"github.com/IZZY-Agency/quartermaster/internal/up"
)

const (
	spawnCoordID = "00000000-0000-4000-8000-000000000511"
	spawnSideID  = "00000000-0000-4000-8000-000000000512"
)

// spawnFrom makes qm (pid 52) run under a claude process (pid 51) resuming
// session id, or under no claude when id is "".
func spawnFrom(t *testing.T, id string) {
	t.Helper()
	procs := []claudecode.Proc{{PID: 52, PPID: 51, Cmd: "qm spawn worker"}}
	if id != "" {
		procs = append(procs, claudecode.Proc{PID: 51, PPID: 1, Cmd: "claude --resume " + id})
	}
	swapProcs(t, func() ([]claudecode.Proc, error) { return procs, nil })
	swap(t, &upSelfPID, func([]claudecode.Proc) int { return 52 })
}

// linkedWorktree makes a repository and a linked worktree of it, symlinks
// resolved, and returns both.
func linkedWorktree(t *testing.T) (main, linked string) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	main = filepath.Join(dir, "main")
	if err := os.Mkdir(main, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", main,
			"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("commit", "-q", "--allow-empty", "-m", "first")
	linked = filepath.Join(dir, "linked")
	git("worktree", "add", "-q", "-b", "feature", linked)
	return main, linked
}

func TestUpDoneFromLinkedWorktree(t *testing.T) {
	m := newUpMachine(t)
	main, linked := linkedWorktree(t)
	m.root = main
	p := m.project(t)
	m.worker(t, p, "w1")
	swap(t, &upWorkDir, func() (string, error) { return linked, nil })
	var out, errb bytes.Buffer
	if code := run([]string{"up", "done", "w1"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if _, ok := loadProject(t, main).Done[upWorkerID]; !ok {
		t.Errorf("w1 not marked done")
	}
}

func TestDownFindsProjectLikeUpDone(t *testing.T) {
	// From a linked worktree of the project, and from any folder with
	// QM_PROJECT_ID set, qm down finds the project as qm up done does.
	m := newUpMachine(t)
	main, linked := linkedWorktree(t)
	m.root = main
	p := m.project(t)
	for _, tc := range []struct{ name, wd, env string }{
		{"linked worktree", linked, ""},
		{"QM_PROJECT_ID", t.TempDir(), p.ID},
	} {
		if err := up.Save(p); err != nil {
			t.Fatal(err)
		}
		t.Setenv("QM_PROJECT_ID", tc.env)
		swap(t, &upWorkDir, func() (string, error) { return tc.wd, nil })
		var out, errb bytes.Buffer
		if code := run([]string{"down", "--keep-open"}, &out, &errb); code != 0 {
			t.Fatalf("%s: exit %d, stderr %q", tc.name, code, errb.String())
		}
		if loadProject(t, main).DownAt.IsZero() {
			t.Errorf("%s: project not marked down", tc.name)
		}
	}
}

// spawnDry runs a dry-run worker spawn and returns stdout and stderr.
func spawnDry(t *testing.T) (string, string) {
	t.Helper()
	t.Setenv("AGTERM_SESSION_ID", "")
	t.Setenv("TMUX", "")
	t.Setenv("CLAUDE_PID", "")
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "claude"))
	t.Setenv("QM_WORKER_PLUGIN", t.TempDir())
	t.Setenv("QM_PROJECT_ID", "")
	t.Setenv("QM_WORKSPACE", "")
	repo := routeRepo(t)
	brief := routeFile(t, t.TempDir(), "brief.md", "Fix the typo in the readme.\n")
	var out, errb bytes.Buffer
	args := []string{"spawn", "worker", "--brief", brief, "--type", "docs", "--size", "S", "--hint", "sonnet/low", "--name", "w1", "--cwd", repo, "--dry-run"}
	if code := run(args, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	return out.String(), errb.String()
}

func TestSpawnWorkerTiedByFolder(t *testing.T) {
	routeHermetic(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := up.Project{ID: up.ProjectID(root), Root: root, Workspace: "proj", Launcher: "agterm", Coordinator: up.Session{ID: spawnCoordID, Name: "proj-coord"}}
	if err := up.Save(p); err != nil {
		t.Fatal(err)
	}
	swap(t, &upWorkDir, func() (string, error) { return root, nil })
	spawnFrom(t, spawnCoordID)
	out, _ := spawnDry(t)
	if want := " QM_PROJECT_ID=" + p.ID + " QM_WORKSPACE=proj "; !strings.Contains(out, want) {
		t.Errorf("output lacks %q:\n%s", want, out)
	}
}

func TestSpawnWorkerFromSideSessionUntied(t *testing.T) {
	// A session in the project's folder that the project does not hold (a
	// side session the user did not adopt), or no claude above qm at all:
	// the worker spawns untied, with a note.
	routeHermetic(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := up.Project{ID: up.ProjectID(root), Root: root, Workspace: "proj", Launcher: "agterm", Coordinator: up.Session{ID: spawnCoordID, Name: "proj-coord"}}
	if err := up.Save(p); err != nil {
		t.Fatal(err)
	}
	swap(t, &upWorkDir, func() (string, error) { return root, nil })
	for _, id := range []string{spawnSideID, ""} {
		spawnFrom(t, id)
		out, errb := spawnDry(t)
		if strings.Contains(out, "QM_PROJECT_ID") {
			t.Errorf("%q: output holds a project tie:\n%s", id, out)
		}
		if !strings.Contains(errb, "qm spawn: note: ") || !strings.Contains(errb, "worker not tied to project "+p.ID) {
			t.Errorf("%q: stderr = %q, want the untied note", id, errb)
		}
	}
}

func TestSpawnWorkerUntiedOutsideProject(t *testing.T) {
	routeHermetic(t)
	swap(t, &upWorkDir, func() (string, error) { return t.TempDir(), nil })
	out, errb := spawnDry(t)
	if strings.Contains(out, "QM_PROJECT_ID") || strings.Contains(out, "QM_WORKSPACE") {
		t.Errorf("output holds a project tie:\n%s", out)
	}
	if errb != "" {
		t.Errorf("stderr = %q, want none", errb)
	}
}

func TestSpawnWorkerProjectUnreadableWarns(t *testing.T) {
	routeHermetic(t)
	swap(t, &upWorkDir, func() (string, error) { return "", os.ErrPermission })
	out, errb := spawnDry(t)
	if !strings.Contains(errb, "qm spawn: warning: project not read: ") || !strings.Contains(errb, "worker not tied to a project") {
		t.Errorf("stderr = %q, want the not-read warning", errb)
	}
	if strings.Contains(out, "QM_PROJECT_ID") {
		t.Errorf("output holds a project tie:\n%s", out)
	}
}
