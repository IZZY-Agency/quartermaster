package spawn

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/declog"
	"github.com/IZZY-Agency/quartermaster/internal/limits"
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

func newJobPrompt(t *testing.T, d route.Decision, round int, prompt string) string {
	t.Helper()
	repo := t.TempDir()
	dir, err := NewReviewJob(ReviewJob{Repo: repo, Decision: d, Record: route.Record{Project: repo, Branch: "feature", Round: round}}, []byte(prompt))
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
	for _, want := range []string{"-p", "--output-format json", "--permission-mode dontAsk", "--tools Read,Grep,Glob,Bash", "--strict-mcp-config", "--allowedTools Read Grep Glob Bash(git diff *) Bash(git log *) Bash(gh pr view *) Bash(gh pr diff *)",
		"--disallowedTools Edit Write NotebookEdit mcp__*", `"qm-coordinator@quartermaster":false`} {
		if !strings.Contains(args, want) {
			t.Errorf("claude args %q lack %q", args, want)
		}
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
	if got, err := os.ReadFile(stdin); err != nil || string(got) != prompt {
		t.Errorf("codex stdin: %d bytes (%v), want the %d-byte prompt", len(got), err, len(prompt))
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
