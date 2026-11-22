package up

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

// realTempDir is a temporary folder with symlinks resolved (macOS's /var).
func realTempDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestWorktreesMain(t *testing.T) {
	root, a, b := realTempDir(t), realTempDir(t), realTempDir(t)
	f := &fakeDir{out: map[string]string{
		"git worktree list --porcelain": "worktree " + root + "\nHEAD aaa\nbranch refs/heads/main\n\n" +
			"worktree " + a + "\nHEAD bbb\ndetached\n\n" +
			"worktree " + b + "\nHEAD ccc\nbranch refs/heads/x\n",
	}}
	got := Worktrees(context.Background(), root, f.run)
	if want := []string{root, a, b}; !slices.Equal(got, want) {
		t.Errorf("Worktrees() = %q, want %q", got, want)
	}
	if len(f.calls) != 1 || f.calls[0] != root+": git worktree list --porcelain" {
		t.Errorf("calls = %q, want one git call in root", f.calls)
	}
}

func TestWorktreesFromLinked(t *testing.T) {
	main, linked := realTempDir(t), realTempDir(t)
	f := &fakeDir{out: map[string]string{
		"git worktree list --porcelain": "worktree " + main + "\n\nworktree " + linked + "\n",
	}}
	if got, want := Worktrees(context.Background(), linked, f.run), []string{linked}; !slices.Equal(got, want) {
		t.Errorf("Worktrees() = %q, want %q", got, want)
	}
}

func TestWorktreesGitFails(t *testing.T) {
	root := t.TempDir()
	if got, want := Worktrees(context.Background(), root, (&fakeDir{}).run), []string{root}; !slices.Equal(got, want) {
		t.Errorf("Worktrees() = %q, want %q", got, want)
	}
}

// projectRepo makes a repository with one commit and a linked worktree, and
// returns the main root and the linked worktree, symlinks resolved.
func projectRepo(t *testing.T) (main, linked string) {
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

func projectState(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
}

func saveAt(t *testing.T, root string) Project {
	t.Helper()
	p := Project{ID: ProjectID(root), Root: root, Workspace: "ws", Launcher: "agterm"}
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProjectForRoot(t *testing.T) {
	projectState(t)
	main, _ := projectRepo(t)
	want := saveAt(t, main)
	got, ok, err := ProjectFor(main)
	if err != nil || !ok || got.ID != want.ID {
		t.Errorf("ProjectFor(main) = %q, %v, %v, want %q", got.ID, ok, err, want.ID)
	}
}

func TestProjectForLinkedWorktree(t *testing.T) {
	projectState(t)
	main, linked := projectRepo(t)
	want := saveAt(t, main)
	got, ok, err := ProjectFor(linked)
	if err != nil || !ok || got.ID != want.ID {
		t.Errorf("ProjectFor(linked) = %q, %v, %v, want %q", got.ID, ok, err, want.ID)
	}
	if root, err := Root(linked); err != nil || root != linked {
		t.Errorf("Root(linked) = %q, %v, want it unchanged: %q", root, err, linked)
	}
}

func TestProjectForOwnStateWins(t *testing.T) {
	projectState(t)
	main, linked := projectRepo(t)
	saveAt(t, main)
	own := saveAt(t, linked)
	got, ok, err := ProjectFor(linked)
	if err != nil || !ok || got.ID != own.ID {
		t.Errorf("ProjectFor(linked) = %q, %v, %v, want its own %q", got.ID, ok, err, own.ID)
	}
}

func TestProjectForNone(t *testing.T) {
	projectState(t)
	main, linked := projectRepo(t)
	for _, dir := range []string{main, linked, t.TempDir()} {
		if got, ok, err := ProjectFor(dir); ok || err != nil {
			t.Errorf("ProjectFor(%q) = %q, %v, %v, want none", dir, got.ID, ok, err)
		}
	}
}
