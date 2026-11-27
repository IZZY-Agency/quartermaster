package spawn

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/declog"
	"github.com/IZZY-Agency/quartermaster/internal/limits"
	"github.com/IZZY-Agency/quartermaster/internal/reviewin"
	"github.com/IZZY-Agency/quartermaster/internal/route"
)

// reviewHermetic points the state and Codex folders at temporary ones.
func reviewHermetic(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	return home
}

// fakeBin writes an executable shell script and returns its path.
func fakeBin(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

// fakeClaude is a claude that writes its arguments, one per line, to
// args.txt and its stdin to stdin.txt in dir, and replies with result as
// the result of a claude -p --output-format json run.
func fakeClaude(t *testing.T, dir, result string) string {
	t.Helper()
	reply, err := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": result})
	if err != nil {
		t.Fatal(err)
	}
	replyFile := filepath.Join(dir, "reply.json")
	if err := os.WriteFile(replyFile, reply, 0o600); err != nil {
		t.Fatal(err)
	}
	return fakeBin(t, "claude", "for a in \"$@\"; do printf '%s\\n' \"$a\"; done > '"+dir+"/args.txt'\ncat > '"+dir+"/stdin.txt'\ncat '"+replyFile+"'\n")
}

// limitError is the Codex CLI's usage-limit message, as it prints it on stderr.
const limitError = "ERROR: You\u2019ve hit your usage limit. Visit the usage page to purchase more credits or try again at Nov 23rd, 2099 9:02 AM."

const cleanFindings = `{"verdict":"mergeable","findings":[]}`

// reviewDecision routes a review with Jev off.
func reviewDecision(t *testing.T, round int, codex limits.CodexState) route.Decision {
	t.Helper()
	d, err := route.DecideReview(context.Background(), route.ReviewInput{Prompt: "Review the notes file.", Round: round, DiffLines: 3, DiffFiles: 1, Paths: []string{"notes.txt"}}, nil, nil, codex, limits.ClaudeState{})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func newJob(t *testing.T, d route.Decision, round int) string {
	t.Helper()
	return newJobPrompt(t, d, round, "Review the notes file.\n")
}

// testChange is a change as reviewin.BuildChange makes one: three files and
// the commits they cover, the checkout at the end of the range.
func testChange() *reviewin.Change {
	return &reviewin.Change{
		Summary: reviewin.Summary{Spec: "main...HEAD", Mode: "...", Left: "1111", Right: "2222", Head: "2222"},
		Files:   []reviewin.File{{Name: "change.diff", Data: []byte("diff\n")}, {Name: "commits.txt", Data: []byte("commit 2222\n")}, {Name: "files.txt", Data: []byte("added\tremoved\tpath\n")}},
	}
}

func newJobPrompt(t *testing.T, d route.Decision, round int, prompt string) string {
	t.Helper()
	repo := t.TempDir()
	dir, err := NewReviewJob(ReviewJob{Repo: repo, Decision: d, Record: route.Record{Project: repo, Branch: "feature", Round: round}}, []byte(prompt), testChange())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func readArgs(t *testing.T, dir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "args.txt"))
	if err != nil {
		t.Fatalf("claude was not run: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

func loggedReviews(t *testing.T) []route.Record {
	t.Helper()
	recs, err := declog.Read(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func TestCodexArgs(t *testing.T) {
	got := CodexArgs(route.CodexTier{Model: "gpt-6.2-sol", Effort: "medium"}, "/repo", "/jobs/1/last.txt", "/jobs/1/findings.schema.json")
	want := []string{"exec", "-m", "gpt-6.2-sol", "-c", `model_reasoning_effort="medium"`, "-s", "read-only", "--ephemeral", "--json",
		"-o", "/jobs/1/last.txt", "--output-schema", "/jobs/1/findings.schema.json", "-C", "/repo", "-"}
	if !slices.Equal(got, want) {
		t.Errorf("CodexArgs:\n got %q\nwant %q", got, want)
	}
}

func TestLimitErrorFallsBackToClaude(t *testing.T) {
	reviewHermetic(t)
	d := reviewDecision(t, 2, limits.CodexState{Available: true})
	if d.Codex == nil {
		t.Fatal("decision has no Codex tier")
	}
	dir := newJob(t, d, 2)
	codex := fakeBin(t, "codex", "echo '"+limitError+"' >&2\nexit 1\n")
	out := t.TempDir()
	claude := fakeClaude(t, out, cleanFindings)

	if err := (ReviewRunner{Codex: codex, Claude: claude}).Run(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	st, err := limits.Codex(func(string) (string, error) { return codex, nil }, limits.CodexSessions)
	if err != nil {
		t.Fatal(err)
	}
	if st.Available || st.ResetsAt.Year() != 2099 {
		t.Errorf("Codex state after the limit error: %+v, want unavailable until 2099", st)
	}
	args := readArgs(t, out)
	if i := slices.Index(args, "--model"); i < 0 || args[i+1] != "claude-sonnet-5-5" {
		t.Errorf("claude args %q lack --model claude-sonnet-5-5", args)
	}
	if i := slices.Index(args, "--effort"); i < 0 || args[i+1] != "medium" {
		t.Errorf("claude args %q lack --effort medium", args)
	}
	if s, _ := ReviewStatus(dir); s != StatusDone {
		t.Errorf("status %q, want %q", s, StatusDone)
	}
	recs := loggedReviews(t)
	if len(recs) != 1 || recs[0].Kind != "review" || recs[0].Decision == nil {
		t.Fatalf("records %+v, want one review record", recs)
	}
	if recs[0].Decision.Codex != nil || !slices.ContainsFunc(recs[0].Decision.Reasons, func(r string) bool { return strings.Contains(r, "usage limit") }) {
		t.Errorf("logged decision %+v should say Codex hit its usage limit and the review went to Claude", recs[0].Decision)
	}
}

func TestNoCodexRunsClaude(t *testing.T) {
	reviewHermetic(t)
	st, err := limits.Codex(func(string) (string, error) { return "", os.ErrNotExist }, limits.CodexSessions)
	if err != nil {
		t.Fatal(err)
	}
	d := reviewDecision(t, 1, st)
	dir := newJob(t, d, 1)
	marker := filepath.Join(t.TempDir(), "codex-ran")
	codex := fakeBin(t, "codex", "touch '"+marker+"'\n")
	out := t.TempDir()
	claude := fakeClaude(t, out, cleanFindings)

	if err := (ReviewRunner{Codex: codex, Claude: claude}).Run(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("codex ran although it is not installed")
	}
	readArgs(t, out)
	recs := loggedReviews(t)
	if len(recs) != 1 || recs[0].Decision == nil {
		t.Fatalf("records %+v, want one review record", recs)
	}
	if !slices.ContainsFunc(recs[0].Decision.Reasons, func(r string) bool { return strings.Contains(r, "codex not installed") }) {
		t.Errorf("reasons %q lack \"codex not installed\"", recs[0].Decision.Reasons)
	}
}

func TestClaudeReviewerLaunch(t *testing.T) {
	reviewHermetic(t)
	d := reviewDecision(t, 1, limits.CodexState{Reason: "codex not installed"})
	dir := newJob(t, d, 1)
	out := t.TempDir()
	claude := fakeClaude(t, out, cleanFindings)
	if err := (ReviewRunner{Claude: claude}).Run(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	args := strings.Join(readArgs(t, out), " ")
	for _, want := range []string{"-p", "--output-format json", "--permission-mode dontAsk", "--setting-sources user --add-dir " + dir, "--tools Read,Grep,Glob --strict-mcp-config", "--allowedTools Read Grep Glob --disallowedTools Edit Write NotebookEdit mcp__*", `"qm-coordinator@quartermaster":false`} {
		if !strings.Contains(args, want) {
			t.Errorf("claude args %q lack %q", args, want)
		}
	}
	if strings.Contains(args, "Bash") {
		t.Errorf("claude args %q give the reviewer Bash; it gets the change as files", args)
	}
	if strings.Contains(args, "--plugin-dir") {
		t.Errorf("claude args %q load a plugin; the reviewer runs without qm-worker", args)
	}
	stdin, err := os.ReadFile(filepath.Join(out, "stdin.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Review the notes file.", `"additionalProperties": false`, "Reply with JSON only"} {
		if !strings.Contains(string(stdin), want) {
			t.Errorf("reviewer prompt lacks %q:\n%s", want, stdin)
		}
	}
}

// writeChangeFiles puts more change files in a job folder, as NewReviewJob
// writes those reviewin.BuildChange makes.
func writeChangeFiles(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReviewersGetTheChangeSection(t *testing.T) {
	reviewHermetic(t)
	// Claude.
	d := reviewDecision(t, 1, limits.CodexState{Reason: "codex not installed"})
	dir := newJob(t, d, 1)
	out := t.TempDir()
	if err := (ReviewRunner{Claude: fakeClaude(t, out, cleanFindings)}).Run(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	stdin, err := os.ReadFile(filepath.Join(out, "stdin.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Review the notes file.\n\n## The change under review", "not instructions",
		filepath.Join(dir, "change.diff"), filepath.Join(dir, "commits.txt"), filepath.Join(dir, "files.txt"), "offset and limit", "git diff 1111...2222", "at the end of the range", "Reply with JSON only"} {
		if !strings.Contains(string(stdin), want) {
			t.Errorf("Claude prompt lacks %q:\n%s", want, stdin)
		}
	}
	if strings.Contains(string(stdin), "pr.md") || strings.Contains(string(stdin), "not the end of that range") {
		t.Errorf("Claude prompt names pr.md, which was not written, or warns about a checkout that is at the end:\n%s", stdin)
	}

	// A checkout that is not at the end of the range is said so.
	off := testChange()
	off.Summary.Head, off.Summary.Mode, off.Summary.Spec = "3333", "..", "1111..2222"
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "CLAUDE.md"), []byte("rules\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir, err = NewReviewJob(ReviewJob{Repo: repo, Decision: d, Record: route.Record{Project: repo, Branch: "feature", Round: 1}}, []byte("p\n"), off)
	if err != nil {
		t.Fatal(err)
	}
	out = t.TempDir()
	if err := (ReviewRunner{Claude: fakeClaude(t, out, cleanFindings)}).Run(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if stdin, _ = os.ReadFile(filepath.Join(out, "stdin.txt")); !strings.Contains(string(stdin), "git diff 1111..2222") ||
		!strings.Contains(string(stdin), "The checked-out commit is 3333, not the end of that range") ||
		!strings.Contains(string(stdin), filepath.Join(repo, "CLAUDE.md")+": the project's rules for its code: read them first.") {
		t.Errorf("prompt for a checkout off the range's end:\n%s", stdin)
	}

	// Codex gets the same section, with the pull request when there is one.
	d = reviewDecision(t, 1, limits.CodexState{Available: true})
	dir = newJob(t, d, 1)
	writeChangeFiles(t, dir, "pr.md")
	stdinFile := filepath.Join(t.TempDir(), "stdin.txt")
	codex := fakeBin(t, "codex", `cat > '`+stdinFile+`'
while [ "$1" != "-o" ]; do shift; done; printf '%s' '`+cleanFindings+`' > "$2"
`)
	if err := (ReviewRunner{Codex: codex, Claude: "/nonexistent/claude"}).Run(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(stdinFile)
	if err != nil || !strings.HasPrefix(string(got), "Review the notes file.\n\n## The change under review") || !strings.Contains(string(got), filepath.Join(dir, "pr.md")) {
		t.Errorf("Codex prompt (%v):\n%s", err, got)
	}
}

func TestReviewFailsWhenItsChangeFilesAreGone(t *testing.T) {
	reviewHermetic(t)
	d := reviewDecision(t, 1, limits.CodexState{Reason: "codex not installed"})
	for name, damage := range map[string]func(dir string) error{
		"missing": func(dir string) error { return os.Remove(filepath.Join(dir, "files.txt")) },
		"folder": func(dir string) error {
			return replaceWith(dir, "files.txt", func(p string) error { return os.Mkdir(p, 0o700) })
		},
		"symlink": func(dir string) error {
			return replaceWith(dir, "files.txt", func(p string) error { return os.Symlink("/etc/hosts", p) })
		},
		"fifo": func(dir string) error {
			return replaceWith(dir, "files.txt", func(p string) error { return syscall.Mkfifo(p, 0o600) })
		},
		"diff gone": func(dir string) error { return os.Remove(filepath.Join(dir, "change.diff")) },
	} {
		t.Run(name, func(t *testing.T) {
			dir := newJob(t, d, 1)
			for _, n := range []string{"change.diff", "commits.txt", "files.txt"} {
				if fi, err := os.Stat(filepath.Join(dir, n)); err != nil || fi.Mode().Perm() != 0o600 {
					t.Errorf("%s: %v %v, want 0600", n, fi, err)
				}
			}
			if err := damage(dir); err != nil {
				t.Fatal(err)
			}
			out := t.TempDir()
			err := (ReviewRunner{Claude: fakeClaude(t, out, cleanFindings)}).Run(context.Background(), dir)
			if err == nil || !strings.Contains(err.Error(), "change file") {
				t.Errorf("err %v, want a failure naming the change file", err)
			}
			if _, serr := os.Stat(filepath.Join(out, "args.txt")); serr == nil {
				t.Error("the reviewer ran without its change files")
			}
			if s, _ := ReviewStatus(dir); s != StatusFailed {
				t.Errorf("status %q, want failed", s)
			}
		})
	}
}

// replaceWith removes dir/name and makes it again with make.
func replaceWith(dir, name string, make func(path string) error) error {
	p := filepath.Join(dir, name)
	if err := os.Remove(p); err != nil {
		return err
	}
	return make(p)
}

func TestEmptyCommitsFileIsFine(t *testing.T) {
	reviewHermetic(t)
	d := reviewDecision(t, 1, limits.CodexState{Reason: "codex not installed"})
	c := testChange()
	c.Files[1].Data = nil // a range that has no commit of its own
	repo := t.TempDir()
	dir, err := NewReviewJob(ReviewJob{Repo: repo, Decision: d, Record: route.Record{Project: repo, Branch: "feature", Round: 1}}, []byte("p\n"), c)
	if err != nil {
		t.Fatal(err)
	}
	if err := (ReviewRunner{Claude: fakeClaude(t, t.TempDir(), cleanFindings)}).Run(context.Background(), dir); err != nil {
		t.Errorf("an empty commits.txt failed the review: %v", err)
	}
}

func TestJobFromAnOlderQmFails(t *testing.T) {
	reviewHermetic(t)
	d := reviewDecision(t, 1, limits.CodexState{Reason: "codex not installed"})
	repo := t.TempDir()
	dir, err := NewReviewJob(ReviewJob{Repo: repo, Decision: d, Record: route.Record{Project: repo, Branch: "feature", Round: 1}}, []byte("p\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	err = (ReviewRunner{Claude: fakeClaude(t, out, cleanFindings)}).Run(context.Background(), dir)
	if err == nil || !strings.Contains(err.Error(), "created by an older qm") || !strings.Contains(err.Error(), "start the review again") {
		t.Errorf("err %v", err)
	}
	if _, serr := os.Stat(filepath.Join(out, "args.txt")); serr == nil {
		t.Error("a job with no change files was reviewed")
	}
}

func TestNewReviewJobLeavesNothingWhenItFails(t *testing.T) {
	reviewHermetic(t)
	c := testChange()
	c.Files = append(c.Files, reviewin.File{Name: "no-such-folder/x", Data: []byte("x")}) // cannot be written
	repo := t.TempDir()
	dir, err := NewReviewJob(ReviewJob{Repo: repo}, []byte("p\n"), c)
	if err == nil || dir != "" {
		t.Fatalf("dir %q, err %v, want a failure and no folder", dir, err)
	}
	root, _ := ReviewsDir()
	if ents, _ := os.ReadDir(root); len(ents) != 0 {
		t.Errorf("a failed job left %d folders", len(ents))
	}
}

func TestCodexLastMessageIsPrivate(t *testing.T) {
	reviewHermetic(t)
	d := reviewDecision(t, 1, limits.CodexState{Available: true})
	dir := newJob(t, d, 1)
	// This codex makes the file anew, with the process's umask (here 022).
	codex := fakeBin(t, "codex", `cat > /dev/null
umask 022
while [ "$1" != "-o" ]; do shift; done; rm -f "$2"; printf '%s' '`+cleanFindings+`' > "$2"
`)
	if err := (ReviewRunner{Codex: codex, Claude: "/nonexistent/claude"}).Run(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if fi, err := e.Info(); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v, want 0600", e.Name(), fi.Mode(), err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "last.txt")); err != nil {
		t.Errorf("last.txt: %v", err)
	}
}

func TestFindingsValidated(t *testing.T) {
	valid := `{"verdict":"mergeable-after-fixes","findings":[{"severity":"P2","file":"a.go","line":3,"scenario":"s","fix":"f"}]}`
	if _, err := ParseFindings([]byte(valid)); err != nil {
		t.Errorf("valid findings rejected: %v", err)
	}
	if f, err := ParseFindings([]byte("```json\n" + cleanFindings + "\n```")); err != nil || f.Verdict != "mergeable" {
		t.Errorf("fenced findings: %+v, %v", f, err)
	}
	prose := "I found no regressions.\n\n```json\n" + cleanFindings + "\n```\n\nThe status of each finding:\n1. Fixed."
	if f, err := ParseFindings([]byte(prose)); err != nil || f.Verdict != "mergeable" {
		t.Errorf("findings inside prose: %+v, %v", f, err)
	}
	two := "```json\n" + cleanFindings + "\n```\nor\n```json\n" + cleanFindings + "\n```"
	if _, err := ParseFindings([]byte(two)); err == nil {
		t.Error("two fenced blocks accepted: the reply is ambiguous")
	}
	for name, bad := range map[string]string{
		"not json":         "Looks good to me.",
		"verdict unknown":  `{"verdict":"fine","findings":[]}`,
		"no findings":      `{"verdict":"mergeable"}`,
		"findings null":    `{"verdict":"mergeable","findings":null}`,
		"extra top field":  `{"verdict":"mergeable","findings":[],"summary":"x"}`,
		"severity unknown": `{"verdict":"mergeable","findings":[{"severity":"P4","file":"a.go","line":3,"scenario":"s","fix":"f"}]}`,
		"line missing":     `{"verdict":"mergeable","findings":[{"severity":"P1","file":"a.go","scenario":"s","fix":"f"}]}`,
		"line not integer": `{"verdict":"mergeable","findings":[{"severity":"P1","file":"a.go","line":1.5,"scenario":"s","fix":"f"}]}`,
		"extra item field": `{"verdict":"mergeable","findings":[{"severity":"P1","file":"a.go","line":1,"scenario":"s","fix":"f","note":"n"}]}`,
		"file not string":  `{"verdict":"mergeable","findings":[{"severity":"P1","file":7,"line":1,"scenario":"s","fix":"f"}]}`,
		"trailing data":    cleanFindings + ` {}`,
		"trailing brace":   cleanFindings + `}`,
		"trailing bracket": cleanFindings + `]`,
	} {
		if _, err := ParseFindings([]byte(bad)); err == nil {
			t.Errorf("%s: accepted %s", name, bad)
		}
	}

	reviewHermetic(t)
	dir := newJob(t, reviewDecision(t, 1, limits.CodexState{}), 1)
	out := t.TempDir()
	err := (ReviewRunner{Claude: fakeClaude(t, out, "Looks good to me.")}).Run(context.Background(), dir)
	if err == nil {
		t.Fatal("an invalid reply did not fail the review")
	}
	if s, _ := ReviewStatus(dir); s != StatusFailed {
		t.Errorf("status %q, want %q", s, StatusFailed)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "reply.txt"))
	if string(raw) != "Looks good to me." {
		t.Errorf("raw reply kept as %q", raw)
	}
	if _, err := os.Stat(filepath.Join(dir, "findings.json")); err == nil {
		t.Error("findings.json written for an invalid reply")
	}
	for _, r := range loggedReviews(t) {
		if r.Kind == "review" {
			t.Errorf("a failed review was logged as a review (it would count as a round): %+v", r)
		}
	}
}

func TestCodexFindings(t *testing.T) {
	reviewHermetic(t)
	d := reviewDecision(t, 1, limits.CodexState{Available: true})
	// Over 1 MB, and starting with "-": it must reach Codex intact, on stdin.
	prompt := "- check the notes file\n" + strings.Repeat("context line\n", 100_000)
	dir := newJobPrompt(t, d, 1, prompt)
	stdin := filepath.Join(t.TempDir(), "stdin.txt")
	// The fake codex keeps its stdin and writes the findings to the file after -o.
	codex := fakeBin(t, "codex", `cat > '`+stdin+`'
while [ "$1" != "-o" ]; do shift; done; printf '%s' '`+cleanFindings+`' > "$2"
echo '{"type":"turn.completed"}'
`)
	if err := (ReviewRunner{Codex: codex, Claude: "/nonexistent/claude"}).Run(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if s, _ := ReviewStatus(dir); s != StatusDone {
		t.Errorf("status %q, want %q", s, StatusDone)
	}
	// The prompt, intact, then the section about the change.
	if got, err := os.ReadFile(stdin); err != nil || !strings.HasPrefix(string(got), strings.TrimRight(prompt, "\n")+"\n\n## The change under review") {
		t.Errorf("codex stdin: %d bytes (%v), want the %d-byte prompt and the section", len(got), err, len(prompt))
	}
	recs := loggedReviews(t)
	if len(recs) != 1 || recs[0].Decision == nil || recs[0].Decision.Codex == nil {
		t.Errorf("records %+v, want one review record with the Codex tier", recs)
	}
}

func TestCountsLogged(t *testing.T) {
	reviewHermetic(t)
	d := reviewDecision(t, 1, limits.CodexState{})
	dir := newJob(t, d, 1)
	reply := `{"verdict":"not-mergeable","findings":[
		{"severity":"P1","file":"a.go","line":1,"scenario":"s","fix":"f"},
		{"severity":"P2","file":"a.go","line":2,"scenario":"s","fix":"f"},
		{"severity":"P2","file":"b.go","line":3,"scenario":"s","fix":"f"}]}`
	if err := (ReviewRunner{Claude: fakeClaude(t, t.TempDir(), reply)}).Run(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	recs := loggedReviews(t)
	if len(recs) != 1 {
		t.Fatalf("records %+v, want one", recs)
	}
	r := recs[0]
	if want := `{"verdict":"not-mergeable","p1":1,"p2":2,"p3":0}`; r.Kind != "review" || r.Detail != want {
		t.Errorf("record kind %q detail %q, want review %s", r.Kind, r.Detail, want)
	}
	if r.Project == "" || r.Branch != "feature" || r.Round != 1 {
		t.Errorf("record project %q branch %q round %d", r.Project, r.Branch, r.Round)
	}
	b, err := os.ReadFile(filepath.Join(dir, "findings.json"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := ParseFindings(b)
	if err != nil || len(f.Findings) != 3 {
		t.Errorf("findings.json: %+v, %v", f, err)
	}
}

func TestCodexFailureFallsBackToClaude(t *testing.T) {
	reviewHermetic(t)
	d := reviewDecision(t, 1, limits.CodexState{Available: true})
	dir := newJob(t, d, 1)
	codex := fakeBin(t, "codex", "echo 'ERROR: stream disconnected' >&2\nexit 1\n")
	out := t.TempDir()
	if err := (ReviewRunner{Codex: codex, Claude: fakeClaude(t, out, cleanFindings)}).Run(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	readArgs(t, out)
	if s, _ := ReviewStatus(dir); s != StatusDone {
		t.Errorf("status %q, want %q", s, StatusDone)
	}
	if st, err := limits.Codex(func(string) (string, error) { return codex, nil }, limits.CodexSessions); err != nil || !st.Available {
		t.Errorf("a Codex failure that is not the limit error marked Codex limited: %+v, %v", st, err)
	}
	recs := loggedReviews(t)
	if len(recs) != 1 || recs[0].Decision == nil || recs[0].Decision.Codex != nil ||
		!slices.ContainsFunc(recs[0].Decision.Reasons, func(r string) bool { return strings.Contains(r, "Codex failed") }) {
		t.Errorf("records %+v, want one review on Claude saying Codex failed", recs)
	}
}

func TestReviewTimeoutKillsGroup(t *testing.T) {
	reviewHermetic(t)
	dir := newJob(t, reviewDecision(t, 1, limits.CodexState{}), 1)
	// The shell's child, sleep, holds the output pipe: killing only the shell
	// would leave Run waiting for it.
	claude := fakeBin(t, "claude", "cat > /dev/null\nsleep 30\necho '{}'\n")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := (ReviewRunner{Claude: claude}).Run(ctx, dir)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Run: %v, want a deadline error", err)
	}
	if took := time.Since(start); took > 4*time.Second {
		t.Errorf("Run took %v after its deadline: the reviewer's process group was not killed", took)
	}
	if s, _ := ReviewStatus(dir); s != StatusFailed {
		t.Errorf("status %q, want %q", s, StatusFailed)
	}
}

func TestReviewerEnvDropsSessionVariables(t *testing.T) {
	got := reviewerEnv([]string{
		"PATH=/bin", "HOME=/h", "CLAUDE_CONFIG_DIR=/c", "ANTHROPIC_API_KEY=k", "CLAUDE_CODE_USE_BEDROCK=1",
		"CLAUDECODE=1", "CLAUDE_PID=42", "CLAUDE_EFFORT=max", "CLAUDE_CODE_SESSION_ID=s", "CLAUDE_CODE_ENTRYPOINT=cli",
		"CLAUDE_CODE_MESSAGING_SOCKET=/tmp/s", "CLAUDE_CODE_MESSAGING_TOKEN=t", "CLAUDE_CODE_CHILD_SESSION=1",
		"QM_SESSION_NAME=w", "QM_ROLE=worker", "QM_LSP=go",
	})
	want := []string{"PATH=/bin", "HOME=/h", "CLAUDE_CONFIG_DIR=/c", "ANTHROPIC_API_KEY=k", "CLAUDE_CODE_USE_BEDROCK=1"}
	if !slices.Equal(got, want) {
		t.Errorf("env = %q, want %q", got, want)
	}
}
