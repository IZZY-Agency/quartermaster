package claudecode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// fakeGit makes git see the repositories in common (a folder to its
// --git-common-dir); every other folder is outside git.
func fakeGit(t *testing.T, common map[string]string) {
	t.Helper()
	old := runGit
	t.Cleanup(func() { runGit = old })
	runGit = func(dir string, args ...string) ([]byte, error) {
		if c, ok := common[dir]; ok {
			return []byte(c + "\n"), nil
		}
		return nil, errors.New("fatal: not a git repository")
	}
}

// claudeJSON writes testdata/claude.json under a temporary root and returns
// its path and the root.
func claudeJSON(t *testing.T) (path, root string) {
	t.Helper()
	fakeGit(t, nil)
	root = t.TempDir()
	path = filepath.Join(root, ".claude.json")
	fixture(t, "claude.json", root, path)
	return path, root
}

func trusted(t *testing.T, file, dir string) bool {
	t.Helper()
	ok, err := Trusted(file, dir)
	if err != nil {
		t.Fatalf("Trusted(%q) error = %v", dir, err)
	}
	return ok
}

func TestTrustedExact(t *testing.T) {
	f, root := claudeJSON(t)
	for _, d := range []string{filepath.Join(root, "trusted"), filepath.Join(root, "trusted") + "/", filepath.Join(root, "slash")} {
		if !trusted(t, f, d) {
			t.Errorf("Trusted(%q) = false, want true", d)
		}
	}
}

func TestTrustedByParent(t *testing.T) {
	f, root := claudeJSON(t)
	for _, d := range []string{
		filepath.Join(root, "trusted", ".claude", "worktrees", "w1"),
		// A false entry below a trusted folder does not undo the parent's trust.
		filepath.Join(root, "trusted", "sub", "declined"),
	} {
		if !trusted(t, f, d) {
			t.Errorf("Trusted(%q) = false, want true by its parent", d)
		}
	}
}

func TestUntrusted(t *testing.T) {
	f, root := claudeJSON(t)
	for _, d := range []string{
		filepath.Join(root, "other"),
		filepath.Join(root, "other", "child"),
		filepath.Join(root, "trusted-sibling"),
		root,
		"/",
	} {
		if trusted(t, f, d) {
			t.Errorf("Trusted(%q) = true, want false", d)
		}
	}
}

func TestTrustMissingFile(t *testing.T) {
	ok, err := Trusted(filepath.Join(t.TempDir(), ".claude.json"), "/any")
	if ok || err != nil {
		t.Errorf("Trusted(missing) = %v, %v, want false, nil", ok, err)
	}
}

func TestTrustBadFile(t *testing.T) {
	waits := 0
	old := trustWait
	t.Cleanup(func() { trustWait = old })
	trustWait = func() { waits++ }
	f := filepath.Join(t.TempDir(), ".claude.json")
	if err := os.WriteFile(f, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Trusted(f, "/any"); err == nil {
		t.Error("Trusted(bad JSON) error = nil, want one")
	}
	if waits != 1 {
		t.Errorf("read again after %d waits, want 1", waits)
	}
}

func TestTrustRetriesFileMidWrite(t *testing.T) {
	// Claude Code caught writing the file: the first read does not parse
	// (empty, or cut short), the second, 200 ms later, does.
	f, root := claudeJSON(t)
	good, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	old := trustWait
	t.Cleanup(func() { trustWait = old })
	for _, partial := range [][]byte{nil, good[:len(good)/2]} {
		if err := os.WriteFile(f, partial, 0o600); err != nil {
			t.Fatal(err)
		}
		trustWait = func() {
			if err := os.WriteFile(f, good, 0o600); err != nil {
				t.Error(err)
			}
		}
		tr, err := ReadTrust(f)
		if err != nil || !tr.Trusted(filepath.Join(root, "trusted")) {
			t.Errorf("partial %q: trust %v, %v; want the second read's", partial, tr, err)
		}
	}
	// A read that fails (not a parse error) is not retried.
	trustWait = func() { t.Error("retried a read error") }
	if _, err := ReadTrust(root); err == nil {
		t.Error("reading a folder: want an error")
	}
}

func TestTrustedWorktreeOutsideMainRoot(t *testing.T) {
	f, root := claudeJSON(t)
	wt := filepath.Join(root, "worktrees", "feat")
	fakeGit(t, map[string]string{wt: filepath.Join(root, "trusted", ".git")})
	if !trusted(t, f, wt) {
		t.Errorf("a worktree of a trusted repository, outside its folder, reads untrusted")
	}
}

func TestTrustedFolderInsideTrustedRepo(t *testing.T) {
	f, root := claudeJSON(t)
	sub := filepath.Join(root, "trusted", "pkg", "parser")
	fakeGit(t, map[string]string{sub: filepath.Join(root, "trusted", ".git")})
	if !trusted(t, f, sub) {
		t.Errorf("a folder of a trusted repository reads untrusted")
	}
}

func TestNestedRepoNotTrusted(t *testing.T) {
	f, root := claudeJSON(t)
	nested := filepath.Join(root, "trusted", "vendor", "lib")
	fakeGit(t, map[string]string{nested: filepath.Join(nested, ".git")})
	if trusted(t, f, nested) {
		t.Errorf("a repository nested in a trusted one reads trusted: Claude Code shows it the dialog")
	}
}

func TestTrustedNonGitChild(t *testing.T) {
	f, root := claudeJSON(t)
	fakeGit(t, nil)
	if !trusted(t, f, filepath.Join(root, "trusted", "notes")) {
		t.Errorf("a folder outside git under a trusted folder reads untrusted")
	}
}

func TestTrustedExactInsideRepo(t *testing.T) {
	f, root := claudeJSON(t)
	dir := filepath.Join(root, "slash")
	fakeGit(t, map[string]string{dir: filepath.Join(root, "elsewhere", ".git")})
	if !trusted(t, f, dir) {
		t.Errorf("a folder whose own entry is true reads untrusted")
	}
}

// realGit runs git for a test's throwaway repositories, with no user or
// system config.
func realGit(t *testing.T) func(dir string, args ...string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git missing")
	}
	return func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// TestTrustedRealWorktree runs git on throwaway repositories: a linked
// worktree outside the trusted main folder is trusted, a repository nested
// in it is not.
func TestTrustedRealWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git missing")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(root, ".claude.json")
	fixture(t, "claude.json", root, f)
	main := filepath.Join(root, "trusted")
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(filepath.Join(main, "vendor", "lib"), 0o700); err != nil {
		t.Fatal(err)
	}
	git(main, "init", "-q", "-b", "main")
	git(main, "-c", "user.name=t", "-c", "user.email=t", "commit", "-q", "--allow-empty", "-m", "init")
	wt := filepath.Join(root, "wt")
	git(main, "worktree", "add", "-q", "-b", "feat", wt)
	git(filepath.Join(main, "vendor", "lib"), "init", "-q")
	if !trusted(t, f, wt) {
		t.Errorf("worktree %s reads untrusted", wt)
	}
	if trusted(t, f, filepath.Join(main, "vendor", "lib")) {
		t.Errorf("nested repository reads trusted")
	}
}

func TestTrustedOnlyExactWhenCommonDirIsNotDotGit(t *testing.T) {
	f, root := claudeJSON(t)
	sub := filepath.Join(root, "trusted", "modsub")
	bareWT := filepath.Join(root, "trusted", "wt")
	sep := filepath.Join(root, "other", "sep")
	fakeGit(t, map[string]string{
		sub:    filepath.Join(root, "trusted", ".git", "modules", "modsub"), // submodule
		bareWT: filepath.Join(root, "trusted", "proj.git"),                  // worktree of a bare repository
		sep:    filepath.Join(root, "trusted", "gitdirs", "sep"),            // --separate-git-dir
	})
	for _, d := range []string{sub, bareWT, sep} {
		if trusted(t, f, d) {
			t.Errorf("Trusted(%q) = true: a common dir that is not .git names no main folder", d)
		}
	}
	// Its own entry still counts.
	slash := filepath.Join(root, "slash")
	fakeGit(t, map[string]string{slash: filepath.Join(root, "trusted", "proj.git")})
	if !trusted(t, f, slash) {
		t.Errorf("exact entry ignored under a bare common dir")
	}
}

// An inherited GIT_DIR (qm run from a git hook) must not make git answer for
// another repository.
func TestTrustedIgnoresInheritedGitDir(t *testing.T) {
	git := realGit(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(root, ".claude.json")
	fixture(t, "claude.json", root, f)
	main, other := filepath.Join(root, "trusted"), filepath.Join(root, "other")
	for _, d := range []string{main, other} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		git(d, "init", "-q", "-b", "main")
	}
	git(main, "-c", "user.name=t", "-c", "user.email=t", "commit", "-q", "--allow-empty", "-m", "init")
	wt := filepath.Join(root, "wt")
	git(main, "worktree", "add", "-q", "-b", "feat", wt)
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	if !trusted(t, f, wt) {
		t.Errorf("with GIT_DIR of an untrusted repository inherited, the worktree reads untrusted")
	}
}

// How git fails decides the fallback: missing git and "not a repository"
// read as outside git (the parents count); any other failure, a timeout or a
// git too old for --path-format, reads as an unknown repository (only the
// exact entry counts).
func TestTrustedGitFailures(t *testing.T) {
	f, root := claudeJSON(t)
	dir := filepath.Join(root, "trusted", "vendor", "lib")
	for _, tc := range []struct {
		name string
		out  string
		err  error
		want bool
	}{
		{"git missing", "", fmt.Errorf("exec: %w", exec.ErrNotFound), true},
		{"git older than 2.31", "--path-format=absolute\n.git\n", nil, false},
		{"not a repository", "", errors.New("fatal: not a git repository (or any of the parent directories): .git"), true},
		{"timeout", "", fmt.Errorf("git: %w", context.DeadlineExceeded), false},
		{"other failure", "", errors.New("fatal: unsafe repository"), false},
	} {
		old := runGit
		runGit = func(string, ...string) ([]byte, error) { return []byte(tc.out), tc.err }
		if got := trusted(t, f, dir); got != tc.want {
			t.Errorf("%s: Trusted = %v, want %v", tc.name, got, tc.want)
		}
		runGit = old
	}
}

func TestRunGitMissingFolderIsOutsideGit(t *testing.T) {
	_, err := runGit(filepath.Join(t.TempDir(), "gone"), "rev-parse", "--git-common-dir")
	if !notARepository(err) {
		t.Errorf("err = %v, want it read as outside git", err)
	}
}

// An inherited GIT_CEILING_DIRECTORIES must not make git answer "not a
// repository" for a folder of a repository nested in a trusted one: the
// parents would then count, and the nested repository would read trusted.
func TestTrustedIgnoresInheritedGitCeiling(t *testing.T) {
	git := realGit(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(root, ".claude.json")
	fixture(t, "claude.json", root, f)
	nested := filepath.Join(root, "trusted", "vendor", "lib")
	sub := filepath.Join(nested, "pkg")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	git(nested, "init", "-q", "-b", "main")
	if trusted(t, f, sub) {
		t.Fatalf("a folder of a nested repository reads trusted without the variable")
	}
	t.Setenv("GIT_CEILING_DIRECTORIES", nested)
	if trusted(t, f, sub) {
		t.Errorf("with GIT_CEILING_DIRECTORIES inherited, a nested repository reads trusted")
	}
}
