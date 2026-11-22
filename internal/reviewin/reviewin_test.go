package reviewin

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/declog"
	"github.com/IZZY-Agency/quartermaster/internal/route"
)

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	base := []string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}
	if out, err := exec.Command("git", append(base, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func put(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// repo has main with two files, and branch work that edits one (2 lines
// out, 1 in), adds a binary file and a file whose name has a tab, and
// renames the other.
func repo(t *testing.T) string {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q", "-b", "main")
	put(t, filepath.Join(dir, "a.txt"), []byte("one\ntwo\n"))
	put(t, filepath.Join(dir, "old.txt"), []byte("same\n"))
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-q", "-m", "first")
	gitRun(t, dir, "checkout", "-q", "-b", "work")
	put(t, filepath.Join(dir, "a.txt"), []byte("three\n"))
	put(t, filepath.Join(dir, "img.bin"), []byte{0, 1, 2, 0, 3})
	put(t, filepath.Join(dir, "sub", "tab\tname.txt"), []byte("x\n"))
	gitRun(t, dir, "mv", "old.txt", "new.txt")
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", "second")
	return dir
}

func TestForDiff(t *testing.T) {
	dir := repo(t)
	prompt := filepath.Join(t.TempDir(), "p.md")
	put(t, prompt, []byte("review this"))

	in, err := For(filepath.Join(dir, "sub"), "main", prompt, 0)
	if err != nil {
		t.Fatal(err)
	}
	top, _, err := Repo(dir)
	if err != nil {
		t.Fatal(err)
	}
	// a.txt 1+2, img.bin binary, new.txt +1, old.txt -1, tab file +1.
	want := []string{"a.txt", "img.bin", "new.txt", "old.txt", "sub/tab\tname.txt"}
	slices.Sort(in.Paths)
	if in.Prompt != "review this" || in.Round != 1 || in.DiffLines != 6 || in.DiffFiles != 5 ||
		!slices.Equal(in.Paths, want) || in.Repo != top {
		t.Errorf("got %+v, want 6 lines in %v, round 1, repo %s", in, want, top)
	}
}

func TestForGivenRound(t *testing.T) {
	dir := repo(t)
	prompt := filepath.Join(t.TempDir(), "p.md")
	put(t, prompt, []byte("p"))
	if in, err := For(dir, "main", prompt, 4); err != nil || in.Round != 4 {
		t.Errorf("round %d, err %v; want 4", in.Round, err)
	}
	if _, err := For(dir, "main", prompt, -1); err == nil {
		t.Error("negative round: no error")
	}
}

func TestForErrors(t *testing.T) {
	dir := repo(t)
	prompt := filepath.Join(t.TempDir(), "p.md")
	put(t, prompt, []byte("p"))
	if _, err := For(dir, "main", filepath.Join(dir, "missing.md"), 0); err == nil {
		t.Error("missing prompt: no error")
	}
	if _, err := For(dir, "no-such-ref", prompt, 0); err == nil {
		t.Error("unknown base: no error")
	}
	if _, err := For(t.TempDir(), "main", prompt, 0); err == nil {
		t.Error("not a repository: no error")
	}
	out := filepath.Join(t.TempDir(), "out")
	for _, base := range []string{"--output=" + out, "-p"} {
		if _, err := For(dir, base, prompt, 0); err == nil {
			t.Errorf("base %q: no error", base)
		}
	}
	if _, err := os.Stat(out + "...HEAD"); err == nil {
		t.Error("a base starting with - reached git as an option")
	}
}

func TestRoundCountsBranchReviews(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for _, r := range []route.Record{
		{Kind: "review", Project: "/r", Branch: "b"},
		{Kind: "review", Project: "/r", Branch: "b"},
		{Kind: "review", Project: "/r", Branch: "c"},
		{Kind: "review", Project: "/s", Branch: "b"},
		{Kind: "worker", Project: "/r", Branch: "b"},
	} {
		r.Time = time.Now().UTC()
		if err := declog.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		project, branch string
		want            int
	}{{"/r", "b", 3}, {"/r", "c", 2}, {"/r", "d", 1}, {"/r", "", 1}} {
		if got, err := Round(c.project, c.branch, ""); err != nil || got != c.want {
			t.Errorf("Round(%s, %s) = %d, %v; want %d", c.project, c.branch, got, err, c.want)
		}
	}
}

func TestRepoDetached(t *testing.T) {
	dir := repo(t)
	gitRun(t, dir, "checkout", "-q", "--detach")
	if _, branch, err := Repo(dir); err != nil || branch != "" {
		t.Errorf("branch %q, err %v; want none", branch, err)
	}
}

func TestBaseFollowsOriginHead(t *testing.T) {
	dir := repo(t)
	if b := Base(dir); b != DefaultBase {
		t.Errorf("no origin/HEAD: base %q, want %q", b, DefaultBase)
	}
	gitRun(t, dir, "update-ref", "refs/remotes/origin/master", "main")
	gitRun(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/master")
	if b, br := Base(dir), DefaultBranch(dir); b != "origin/master" || br != "master" {
		t.Errorf("base %q, branch %q; want origin/master, master", b, br)
	}
	// For with no base diffs against origin/master.
	prompt := filepath.Join(t.TempDir(), "p.md")
	put(t, prompt, []byte("p"))
	if in, err := For(dir, "", prompt, 0); err != nil || in.DiffFiles != 5 {
		t.Errorf("For: %+v, %v; want the 5 files of work against origin/master", in, err)
	}
}

// TestRoundSinceFork: reviews logged for an earlier branch of the same name,
// before this branch forked, do not count.
func TestRoundSinceFork(t *testing.T) {
	dir := repo(t)
	top, _, err := Repo(dir)
	if err != nil {
		t.Fatal(err)
	}
	fork := ForkTime(top, "main")
	if fork.IsZero() {
		t.Fatal("no fork time")
	}
	for _, at := range []time.Time{fork.Add(-48 * time.Hour), fork.Add(-time.Hour), fork.Add(time.Minute)} {
		if err := declog.Append(route.Record{Time: at, Kind: "review", Project: top, Branch: "work"}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := Round(top, "work", "main"); err != nil || n != 2 {
		t.Errorf("round %d, %v; want 2: only the review after the fork counts", n, err)
	}
	if n, err := Round(top, "work", "no-such-ref"); err != nil || n != 4 {
		t.Errorf("no merge base: round %d, %v; want 4 (every record)", n, err)
	}
}

// gitAt runs git in dir with author and committer dates set to at.
func gitAt(t *testing.T, dir string, at time.Time, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	d := at.Format(time.RFC3339)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+d, "GIT_COMMITTER_DATE="+d)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// TestRoundSurvivesRebase: rebasing the branch onto a newer main moves the
// merge base forward; the reviews since the branch's worker still count,
// and those of an earlier branch of the same name still do not.
func TestRoundSurvivesRebase(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	gitAt(t, dir, t0, "init", "-q", "-b", "main")
	put(t, filepath.Join(dir, "a.txt"), []byte("one\n"))
	gitAt(t, dir, t0, "add", ".")
	gitAt(t, dir, t0, "commit", "-q", "-m", "first")
	gitAt(t, dir, t0, "checkout", "-q", "-b", "work")
	put(t, filepath.Join(dir, "b.txt"), []byte("b\n"))
	gitAt(t, dir, t0.Add(time.Hour), "add", ".")
	gitAt(t, dir, t0.Add(time.Hour), "commit", "-q", "-m", "work")
	top, _, err := Repo(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []route.Record{
		{Time: t0.Add(-240 * time.Hour), Kind: "worker", Project: top, Branch: "work"}, // an earlier branch "work"
		{Time: t0.Add(-239 * time.Hour), Kind: "review", Project: top, Branch: "work"},
		{Time: t0.Add(30 * time.Minute), Kind: "worker", Project: top, Branch: "work"},
		{Time: t0.Add(2 * time.Hour), Kind: "review", Project: top, Branch: "work"},
		{Time: t0.Add(3 * time.Hour), Kind: "review", Project: top, Branch: "work"},
	} {
		if err := declog.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := Round(top, "work", "main"); err != nil || n != 3 {
		t.Fatalf("before the rebase: round %d, %v; want 3", n, err)
	}
	// main moves on; work is rebased onto it.
	gitAt(t, dir, t0.Add(4*time.Hour), "checkout", "-q", "main")
	put(t, filepath.Join(dir, "a.txt"), []byte("one\ntwo\n"))
	gitAt(t, dir, t0.Add(4*time.Hour), "commit", "-q", "-am", "main moves")
	gitAt(t, dir, t0.Add(5*time.Hour), "checkout", "-q", "work")
	gitAt(t, dir, t0.Add(5*time.Hour), "rebase", "-q", "main")
	if f := ForkTime(top, "main"); !f.Equal(t0.Add(4 * time.Hour)) {
		t.Fatalf("fork time %v, want the new main commit", f)
	}
	if n, err := Round(top, "work", "main"); err != nil || n != 3 {
		t.Errorf("after the rebase: round %d, %v; want 3", n, err)
	}
}

// TestRoundCountsReviewerSessionRecord: when a --type review session's worker
// record is the branch's first, its own review record, written 1 µs before,
// still counts.
func TestRoundCountsReviewerSessionRecord(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	gitAt(t, dir, t0, "init", "-q", "-b", "main")
	put(t, filepath.Join(dir, "a.txt"), []byte("one\n"))
	gitAt(t, dir, t0, "add", ".")
	gitAt(t, dir, t0, "commit", "-q", "-m", "first")
	gitAt(t, dir, t0, "checkout", "-q", "-b", "work")
	put(t, filepath.Join(dir, "b.txt"), []byte("b\n"))
	gitAt(t, dir, t0.Add(time.Hour), "add", ".")
	gitAt(t, dir, t0.Add(time.Hour), "commit", "-q", "-m", "work")
	// main moves on and work is rebased, so the fork time is later than both records.
	gitAt(t, dir, t0.Add(4*time.Hour), "checkout", "-q", "main")
	put(t, filepath.Join(dir, "a.txt"), []byte("one\ntwo\n"))
	gitAt(t, dir, t0.Add(4*time.Hour), "commit", "-q", "-am", "main moves")
	gitAt(t, dir, t0.Add(5*time.Hour), "checkout", "-q", "work")
	gitAt(t, dir, t0.Add(5*time.Hour), "rebase", "-q", "main")
	top, _, err := Repo(dir)
	if err != nil {
		t.Fatal(err)
	}
	at := t0.Add(2 * time.Hour)
	for _, r := range []route.Record{
		{Time: at, Kind: "review", Project: top, Branch: "work"},
		{Time: at.Add(time.Microsecond), Kind: "worker", Project: top, Branch: "work", Type: "review"},
		{Time: at.Add(-time.Hour), Kind: "review", Project: top, Branch: "work"}, // before the branch began
	} {
		if err := declog.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := Round(top, "work", "main"); err != nil || n != 2 {
		t.Errorf("round %d, %v; want 2: the reviewer session's own record counts", n, err)
	}
}
