package reviewin

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func noPR(t *testing.T) {
	t.Helper()
	old := pullRequest
	pullRequest = func(string, string) (string, string, bool) { return "", "", false }
	t.Cleanup(func() { pullRequest = old })
}

// build is BuildChange as a map from file name to content, in order.
func build(t *testing.T, top string, rng Range) (map[string]string, []string) {
	t.Helper()
	c, err := BuildChange(top, rng)
	if err != nil {
		t.Fatal(err)
	}
	m, names := map[string]string{}, []string{}
	for _, f := range c.Files {
		m[f.Name] = string(f.Data)
		names = append(names, f.Name)
	}
	return m, names
}

func TestBuildChange(t *testing.T) {
	noPR(t)
	dir := repo(t)
	rng, err := ParseRange(dir, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	if rng.Spec != "main...HEAD" || rng.Base != revParse(t, dir, "main") || rng.Left != rng.Base || rng.Right != revParse(t, dir, "HEAD") || rng.Head != rng.Right || rng.Mode != "..." {
		t.Errorf("default range %+v", rng)
	}
	got, names := build(t, dir, rng)
	if strings.Join(names, " ") != "change.diff commits.txt files.txt" {
		t.Errorf("files %v, want no pr.md without a pull request", names)
	}
	diff := got[DiffFile]
	for _, want := range []string{"diff --git a/a.txt b/a.txt", "+three", "rename from old.txt", "Binary files"} {
		if !strings.Contains(diff, want) {
			t.Errorf("change.diff lacks %q:\n%s", want, diff)
		}
	}
	commits := got[CommitsFile]
	if !strings.HasPrefix(commits, "commit ") || !strings.Contains(commits, "\n\nsecond\n") || strings.Contains(commits, "first") {
		t.Errorf("commits.txt:\n%s", commits)
	}
	for _, want := range []string{"added\tremoved\tpath\n", "1\t2\ta.txt\n", "-\t-\timg.bin\n", "1\t0\tnew.txt\n", "0\t1\told.txt\n", `1	0	"sub/tab\tname.txt"`} {
		if !strings.Contains(got[FilesFile], want) {
			t.Errorf("files.txt lacks %q:\n%s", want, got[FilesFile])
		}
	}
}

func TestBuildChangeWithPullRequest(t *testing.T) {
	old := pullRequest
	t.Cleanup(func() { pullRequest = old })
	dir := repo(t)
	head := revParse(t, dir, "HEAD")
	var asked string
	pullRequest = func(_, branch string) (string, string, bool) {
		asked = branch
		return "# #7 Title\n\nBody\n", head, true
	}
	rng, _ := ParseRange(dir, "main", "")
	rng.Branch = "work"
	got, names := build(t, dir, rng)
	if asked != "work" {
		t.Errorf("asked gh for the pull request of %q, want the range's branch", asked)
	}
	if names[len(names)-1] != "pr.md" || got[PRFile] != "# #7 Title\n\nBody\n" {
		t.Errorf("files %v", names)
	}
	out := t.TempDir()
	for n, d := range got {
		put(t, filepath.Join(out, n), []byte(d))
	}
	if sec := Section(out, "", rng.Summary()); !strings.Contains(sec, filepath.Join(out, "pr.md")) || !strings.Contains(sec, filepath.Join(out, "change.diff")) {
		t.Errorf("section:\n%s", sec)
	}

	// A pull request whose head is not the range's end is another commit's
	// (the branch moved, or the range ends elsewhere): no pr.md.
	put(t, filepath.Join(dir, "a.txt"), []byte("four\n"))
	gitRun(t, dir, "commit", "-q", "-am", "third")
	for _, spec := range []string{"main..HEAD~1", ""} {
		rng, err := ParseRange(dir, "main", spec)
		if err != nil {
			t.Fatal(err)
		}
		rng.Branch = "work"
		// head is the pull request's head: HEAD~1 now, which ends the first
		// range but is not where the branch is.
		if _, names := build(t, dir, rng); slices.Contains(names, PRFile) != (spec != "") {
			t.Errorf("%q: files %v, want pr.md only when the pull request's head ends the range", spec, names)
		}
	}
}

// revParse is the commit id rev names in dir.
func revParse(t *testing.T, dir, rev string) string {
	t.Helper()
	id, err := commitID(dir, rev)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestGhPullRequestWithoutGh(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if s, _, ok := ghPullRequest(t.TempDir(), "work"); ok || s != "" {
		t.Errorf("got %q, %v with no gh on PATH", s, ok)
	}
}

func TestGhPullRequest(t *testing.T) {
	bin := t.TempDir()
	script := func(body string) {
		put(t, filepath.Join(bin, "gh"), []byte("#!/bin/sh\n"+body))
		if err := os.Chmod(filepath.Join(bin, "gh"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	// gh is asked for the branch's pull request, by name.
	script(`[ "$3" = work ] || exit 1
printf '%s' '{"number":12,"title":"Fix it","body":"Why.\n","state":"OPEN","headRefOid":"abc"}'`)
	if s, head, ok := ghPullRequest(t.TempDir(), "work"); !ok || s != "# #12 Fix it\n\nWhy.\n" || head != "abc" {
		t.Errorf("open: %q, %q, %v", s, head, ok)
	}
	for _, branch := range []string{"", "-x"} {
		if _, _, ok := ghPullRequest(t.TempDir(), branch); ok {
			t.Errorf("branch %q was looked up", branch)
		}
	}
	script(`echo '{"number":12,"title":"Fix it","body":"Why.","state":"MERGED"}'`)
	if _, _, ok := ghPullRequest(t.TempDir(), "work"); ok {
		t.Error("a merged pull request was taken")
	}
	// GH_REPO must not reach gh.
	t.Setenv("GH_REPO", "owner/other")
	script(`[ -z "$GH_REPO" ] || exit 1
printf '%s' '{"number":3,"title":"T","body":"","state":"OPEN"}'`)
	if _, _, ok := ghPullRequest(t.TempDir(), "work"); !ok {
		t.Error("GH_REPO reached gh")
	}
	script(`echo "no pull requests found" >&2; exit 1`)
	if _, _, ok := ghPullRequest(t.TempDir(), "work"); ok {
		t.Error("a gh error gave a pull request")
	}
}

func TestBuildChangeEmptyRange(t *testing.T) {
	noPR(t)
	dir := repo(t)
	rng, err := ParseRange(dir, "main", "HEAD..HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if c, err := BuildChange(dir, rng); !errors.Is(err, ErrNoChange) || c != nil {
		t.Fatalf("%v, %v, want ErrNoChange", c, err)
	}
}

func TestParseRange(t *testing.T) {
	dir := repo(t)
	gitRun(t, dir, "tag", "second")
	put(t, filepath.Join(dir, "a.txt"), []byte("four\n"))
	gitRun(t, dir, "commit", "-q", "-am", "third")

	for _, tc := range []struct {
		spec, diff, log string
		atHead          bool
	}{
		{"second..HEAD", "..", "..", true},
		{"second...HEAD", "...", "..", true},
		{"main..work", "..", "..", true},
		{"main..second", "..", "..", false},
	} {
		r, err := ParseRange(dir, "main", tc.spec)
		// Both ends are resolved to commit ids, so a ref that moves later changes nothing.
		ids := regexp.MustCompile(`^[0-9a-f]{40}(\.\.\.?)[0-9a-f]{40}$`).FindStringSubmatch(r.Diff)
		if err != nil || r.Spec != tc.spec || r.Base != revParse(t, dir, "main") || ids == nil || ids[1] != tc.diff || (r.Right == r.Head) != tc.atHead || r.Mode != tc.diff ||
			!regexp.MustCompile(`^[0-9a-f]{40}\.\.[0-9a-f]{40}$`).MatchString(r.Log) {
			t.Errorf("%q: %+v, %v", tc.spec, r, err)
		}
	}
	for _, spec := range []string{"second", "..HEAD", "main..", "...", "--output=x..HEAD", "main..--output=x", "main...-x",
		"nosuch..HEAD", "main..nosuch", "a..b..c", "main....HEAD"} {
		if r, err := ParseRange(dir, "main", spec); err == nil {
			t.Errorf("%q accepted: %+v", spec, r)
		}
	}
	if _, err := ParseRange(dir, "-x", ""); err == nil {
		t.Error("a base that looks like an option was accepted")
	}
	// A base is one revision, and errors about it name no --diff when none
	// was given.
	if _, err := ParseRange(dir, "main..HEAD", ""); err == nil || !strings.Contains(err.Error(), "one revision") {
		t.Errorf("a base that is a range: %v", err)
	}
	if _, err := ParseRange(dir, "nosuch", ""); err == nil || strings.Contains(err.Error(), "--diff") || !strings.Contains(err.Error(), "not a commit") {
		t.Errorf("an unknown base: %v", err)
	}
	// A misspelled --base is refused with --diff too: the rounds are counted
	// against it.
	if _, err := ParseRange(dir, "mian", "second..HEAD"); err == nil || !strings.Contains(err.Error(), `"mian"`) {
		t.Errorf("a misspelled base with --diff: %v", err)
	}
	// Git's own message is kept when it is not an unknown revision.
	if _, err := commitID(t.TempDir(), "HEAD"); err == nil || errors.Is(err, errUnknownRev) || !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("commitID outside a repository: %v", err)
	}

	// :/text names the youngest commit whose message matches; it must not be
	// broken by the peel to a commit.
	if id := revParse(t, dir, ":/second"); id != revParse(t, dir, "HEAD~1") {
		t.Errorf(":/second is %s", id)
	}
	if r, err := ParseRange(dir, "main", ":/second..HEAD"); err != nil || r.Left != revParse(t, dir, "HEAD~1") {
		t.Errorf(":/second..HEAD: %+v, %v", r, err)
	}
	// A tag is peeled to the commit it tags.
	gitRun(t, dir, "tag", "-a", "-m", "annotated", "ann", "HEAD~1")
	if id := revParse(t, dir, "ann"); id != revParse(t, dir, "HEAD~1") {
		t.Errorf("annotated tag gives %s", id)
	}

	// A fix round: only the last commit.
	noPR(t)
	r, _ := ParseRange(dir, "main", "second..HEAD")
	got, _ := build(t, dir, r)
	if d := got[DiffFile]; !strings.Contains(d, "+four") || strings.Contains(d, "img.bin") {
		t.Errorf("fix-round diff:\n%s", d)
	}
	if c := got[CommitsFile]; !strings.Contains(c, "third") || strings.Contains(c, "second") {
		t.Errorf("fix-round commits:\n%s", c)
	}
}

func TestForRangeSizesTheRange(t *testing.T) {
	dir := repo(t)
	gitRun(t, dir, "tag", "second")
	put(t, filepath.Join(dir, "a.txt"), []byte("four\n"))
	gitRun(t, dir, "commit", "-q", "-am", "third")
	prompt := filepath.Join(t.TempDir(), "p.md")
	put(t, prompt, []byte("p"))

	whole, _, err := ForRange(dir, "main", "", prompt, 0)
	if err != nil {
		t.Fatal(err)
	}
	part, rng, err := ForRange(dir, "main", "second..HEAD", prompt, 0)
	if err != nil {
		t.Fatal(err)
	}
	// The round is counted against the base either way; the size follows the range.
	if part.DiffLines != 2 || part.DiffFiles != 1 || whole.DiffFiles != 5 || part.Round != whole.Round || rng.Spec != "second..HEAD" || rng.Base != revParse(t, dir, "main") || rng.Branch != "work" {
		t.Errorf("whole %+v, part %+v, range %+v", whole, part, rng)
	}
}

func TestSection(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{DiffFile, CommitsFile, FilesFile} {
		put(t, filepath.Join(dir, n), []byte("x"))
	}
	at := Section(dir, "", Summary{Spec: "main...HEAD", Mode: "...", Left: "aaa", Right: "bbb", Head: "bbb"})
	for _, want := range []string{"since it forked from aaa", "`git diff aaa...bbb`", "the code at the end of the range"} {
		if !strings.Contains(at, want) {
			t.Errorf("section lacks %q:\n%s", want, at)
		}
	}
	if strings.Contains(at, "pr.md") || strings.Contains(at, "not the end") {
		t.Errorf("section:\n%s", at)
	}
	off := Section(dir, "", Summary{Spec: "aaa..bbb", Mode: "..", Left: "aaa", Right: "bbb", Head: "ccc"})
	for _, want := range []string{"from aaa to bbb", "`git diff aaa..bbb`", "The checked-out commit is ccc, not the end of that range"} {
		if !strings.Contains(off, want) {
			t.Errorf("section off the range's end lacks %q:\n%s", want, off)
		}
	}
}

func TestSectionNamesTheRuleFiles(t *testing.T) {
	dir, repo, outside := t.TempDir(), t.TempDir(), t.TempDir()
	for _, n := range []string{DiffFile, CommitsFile, FilesFile} {
		put(t, filepath.Join(dir, n), []byte("x"))
	}
	sum := Summary{Mode: "...", Left: "aaa", Right: "bbb", Head: "bbb"}
	if sec := Section(dir, repo, sum); strings.Contains(sec, "rule files") {
		t.Errorf("a repository with no rule file got the list:\n%s", sec)
	}
	put(t, filepath.Join(repo, "CLAUDE.md"), []byte("rules"))
	put(t, filepath.Join(repo, ".claude", "CLAUDE.md"), []byte("more rules"))
	// A link out of the repository is not followed, nor is a folder.
	put(t, filepath.Join(outside, "AGENTS.md"), []byte("not the project's"))
	if err := os.Symlink(filepath.Join(outside, "AGENTS.md"), filepath.Join(repo, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	sec := Section(dir, repo, sum)
	for _, want := range []string{
		filepath.Join(repo, "CLAUDE.md") + ": the project's rules for its code: read them first.",
		filepath.Join(repo, ".claude", "CLAUDE.md") + ": the project's rules for its code: read them first.",
	} {
		if !strings.Contains(sec, want) {
			t.Errorf("section lacks %q:\n%s", want, sec)
		}
	}
	if strings.Contains(sec, "AGENTS.md") {
		t.Errorf("a symlinked AGENTS.md was named:\n%s", sec)
	}
	// A .claude that is a link to a folder elsewhere is not followed either.
	if err := os.RemoveAll(filepath.Join(repo, ".claude")); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(outside, "dot", "CLAUDE.md"), []byte("elsewhere"))
	if err := os.Symlink(filepath.Join(outside, "dot"), filepath.Join(repo, ".claude")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repo, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repo, "AGENTS.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := ruleFiles(repo); len(got) != 1 || got[0] != filepath.Join(repo, "CLAUDE.md") {
		t.Errorf("rule files %v, want CLAUDE.md only", got)
	}
}

func TestCheckSize(t *testing.T) {
	old := maxOutput
	t.Cleanup(func() { maxOutput = old })
	dir := repo(t)
	rng, err := ParseRange(dir, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckSize(dir, rng); err != nil {
		t.Errorf("under the cap: %v", err)
	}
	maxOutput = 64
	if err := CheckSize(dir, rng); !errors.Is(err, ErrTooLarge) || !strings.Contains(err.Error(), "--diff") {
		t.Errorf("over the cap: %v", err)
	}
}

func TestParseRangeRefusesARebasedTwoDotRange(t *testing.T) {
	dir := repo(t)
	gitRun(t, dir, "tag", "old")
	// The branch is rebased onto a main that moved: old is not on it any more.
	gitRun(t, dir, "checkout", "-q", "main")
	put(t, filepath.Join(dir, "m.txt"), []byte("m\n"))
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-q", "-m", "main moves")
	gitRun(t, dir, "checkout", "-q", "work")
	gitRun(t, dir, "rebase", "-q", "main")
	_, err := ParseRange(dir, "main", "old..HEAD")
	if err == nil || !strings.Contains(err.Error(), "not an ancestor") || !strings.Contains(err.Error(), "rebase") {
		t.Errorf("a rebased A..B: %v", err)
	}
	// A...B stays fine: it is taken from the merge base.
	if _, err := ParseRange(dir, "main", "old...HEAD"); err != nil {
		t.Errorf("old...HEAD: %v", err)
	}
}

func TestParseRangeWithoutARemote(t *testing.T) {
	dir := repo(t) // no origin: origin/main falls back to main
	r, err := ParseRange(dir, "", "")
	if err != nil || r.Base != revParse(t, dir, "main") || r.Spec != "main...HEAD" {
		t.Errorf("%+v, %v", r, err)
	}
	// No default branch at all: BASE...HEAD has no base, a given range still
	// works, and its rounds cannot be counted.
	gitRun(t, dir, "branch", "-q", "-m", "main", "trunk")
	if _, err := ParseRange(dir, "", ""); err == nil || !strings.Contains(err.Error(), "origin/main") {
		t.Errorf("no base: %v", err)
	}
	r, err = ParseRange(dir, "", "trunk..HEAD")
	if err != nil || r.Base != "" {
		t.Errorf("trunk..HEAD with no base: %+v, %v", r, err)
	}
	prompt := filepath.Join(t.TempDir(), "p.md")
	put(t, prompt, []byte("p"))
	if _, _, err := ForRange(dir, "", "trunk..HEAD", prompt, 0); err == nil || !strings.Contains(err.Error(), "--round") {
		t.Errorf("rounds counted with no base: %v", err)
	}
	if in, _, err := ForRange(dir, "", "trunk..HEAD", prompt, 2); err != nil || in.Round != 2 {
		t.Errorf("--round given: %+v, %v", in, err)
	}
	// A detached HEAD has no rounds to count: round 1, base or not.
	gitRun(t, dir, "checkout", "-q", "--detach")
	if in, _, err := ForRange(dir, "", "trunk..HEAD", prompt, 0); err != nil || in.Round != 1 {
		t.Errorf("detached, no base: %+v, %v", in, err)
	}
}

func TestBuildChangeRefusesATooLargeChange(t *testing.T) {
	noPR(t)
	old := maxOutput
	t.Cleanup(func() { maxOutput = old })
	maxOutput = 64
	dir := repo(t)
	rng, err := ParseRange(dir, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	c, err := BuildChange(dir, rng)
	if !errors.Is(err, ErrTooLarge) || c != nil || !strings.Contains(err.Error(), "--diff") || !strings.Contains(err.Error(), "64 bytes") {
		t.Errorf("%v, %v, want ErrTooLarge suggesting --diff", c, err)
	}
	maxOutput = 1 << 20
	if _, err := BuildChange(dir, rng); err != nil {
		t.Errorf("under the cap: %v", err)
	}
}

func TestBuildChangeKeepsASubmodulePointerChange(t *testing.T) {
	noPR(t)
	dir := repo(t)
	sub := t.TempDir()
	gitRun(t, sub, "init", "-q", "-b", "main")
	put(t, filepath.Join(sub, "f.txt"), []byte("1\n"))
	gitRun(t, sub, "add", ".")
	gitRun(t, sub, "commit", "-q", "-m", "sub one")
	gitRun(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "vendor")
	gitRun(t, dir, "commit", "-q", "-m", "add submodule")
	put(t, filepath.Join(sub, "f.txt"), []byte("2\n"))
	gitRun(t, sub, "commit", "-q", "-am", "sub two")
	gitRun(t, filepath.Join(dir, "vendor"), "pull", "-q")
	gitRun(t, dir, "commit", "-q", "-am", "bump submodule")
	// A config that would hide the pointer change must not hide it from the reviewer.
	gitRun(t, dir, "config", "diff.ignoreSubmodules", "all")
	rng, err := ParseRange(dir, "main", "HEAD~1..HEAD")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := build(t, dir, rng)
	if !strings.Contains(got[FilesFile], "vendor") || !strings.Contains(got[DiffFile], "Subproject commit") {
		t.Errorf("a committed submodule pointer change is missing:\n%s\n%s", got[FilesFile], got[DiffFile])
	}
}
