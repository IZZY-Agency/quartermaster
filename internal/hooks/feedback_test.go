package hooks

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/paths"
	"github.com/IZZY-Agency/quartermaster/internal/policy"
)

// countStarts replaces the analyzer start with a counter.
func countStarts(t *testing.T) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	old := startDetached
	startDetached = func([]string) (*os.Process, error) { n.Add(1); return nil, nil }
	t.Cleanup(func() { startDetached = old })
	return &n
}

func sessionsDir(t *testing.T) string {
	t.Helper()
	dir, err := paths.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "sessions")
}

func writePendingNote(t *testing.T, session, text string) {
	t.Helper()
	dir := sessionsDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, session+feedbackNoteSuffix), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func promptFiles(t *testing.T) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(sessionsDir(t), "*-fb-*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func userPrompt(text string) map[string]any {
	return map[string]any{"hook_event_name": "UserPromptSubmit", "prompt": text, "transcript_path": "/nowhere/project/s.jsonl"}
}

func TestFeedbackReturnsFast(t *testing.T) {
	cfg := setup(t)
	useJev(t, &fakeAsker{}, &fakeGate{})
	oldArgv, oldStart := analyzerArgv, startDetached
	var args []string
	analyzerArgv = func(file, memory string) ([]string, error) {
		args = []string{file, memory}
		return []string{"/bin/sleep", "5"}, nil
	}
	var pid atomic.Int32
	startDetached = func(argv []string) (*os.Process, error) {
		p, err := oldStart(argv)
		if p != nil {
			pid.Store(int32(p.Pid))
		}
		return p, err
	}
	t.Cleanup(func() {
		analyzerArgv, startDetached = oldArgv, oldStart
		if p := pid.Load(); p != 0 {
			_ = syscall.Kill(-int(p), syscall.SIGKILL)
		}
	})

	start := time.Now()
	out := fireInput(t, cfg, "feedback", userPrompt("From now on, run the linter before every commit."))
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Errorf("feedback took %v, want under 200ms", d)
	}
	if len(out) != 0 {
		t.Errorf("got %s, want no output", out)
	}
	if pid.Load() == 0 {
		t.Fatal("analyzer not started")
	}
	files := promptFiles(t)
	if len(files) != 1 || len(args) != 2 || args[0] != files[0] || args[1] != "/nowhere/project/memory/MEMORY.md" {
		t.Fatalf("prompt files %v, analyzer args %v", files, args)
	}
	b, err := os.ReadFile(files[0])
	if err != nil || string(b) != "From now on, run the linter before every commit." {
		t.Errorf("prompt file holds %q (%v)", b, err)
	}
	if fi, err := os.Stat(files[0]); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("prompt file mode %v (%v), want 0600", fi.Mode().Perm(), err)
	}
}

func TestFeedbackOffWithoutKey(t *testing.T) {
	cfg := setup(t)
	useJev(t, nil, nil)
	started := countStarts(t)
	if out := fireInput(t, cfg, "feedback", userPrompt("Always run the linter.")); len(out) != 0 {
		t.Errorf("got %s, want pass", out)
	}
	if started.Load() != 0 || len(promptFiles(t)) != 0 {
		t.Errorf("analyzer started %d times, prompt files %v; want none", started.Load(), promptFiles(t))
	}
	writePendingNote(t, "test-session", "a note")
	if out := fireInput(t, cfg, "feedback-deliver", map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Bash"}); len(out) != 0 {
		t.Errorf("feedback-deliver without a key: got %s, want pass", out)
	}
}

func TestFeedbackDeliveredOnce(t *testing.T) {
	cfg := setup(t)
	useJev(t, &fakeAsker{}, &fakeGate{})
	writePendingNote(t, "test-session", feedbackNoteText)
	post := map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Bash"}
	out := fireInput(t, cfg, "feedback-deliver", post)
	want(t, out, "note")
	if got := decode(t, out).HookSpecificOutput["additionalContext"]; got != feedbackNoteText {
		t.Errorf("note %q, want %q", got, feedbackNoteText)
	}
	if out := fireInput(t, cfg, "feedback-deliver", post); len(out) != 0 {
		t.Errorf("second delivery: got %s, want pass", out)
	}
	// A subagent's tool call does not take the coordinator's note.
	writePendingNote(t, "test-session", feedbackNoteText)
	if out := fireInput(t, cfg, "feedback-deliver", map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Bash", "agent_id": "a1"}); len(out) != 0 {
		t.Errorf("subagent: got %s, want pass", out)
	}
	out = fireInput(t, cfg, "feedback-deliver", map[string]any{"hook_event_name": "UserPromptSubmit", "prompt": "next"})
	want(t, out, "note")
}

// analyzeSetup writes a prompt file as the feedback hook does, a CLAUDE.md
// with n bullets and a memory index, and returns the prompt file and the
// memory index path.
func analyzeSetup(t *testing.T, message string, n int) (string, string) {
	t.Helper()
	claudeDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	var b strings.Builder
	b.WriteString("# Rules\n\nSome prose about linter commits that is not a bullet.\n\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "- rule %d about the linter and commits\n", i)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, "CLAUDE.md"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	memory := filepath.Join(t.TempDir(), "MEMORY.md")
	if err := os.WriteFile(memory, []byte("- [Tests](tests.md) — run the tests before a commit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := sessionsDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "test-session-fb-1.txt")
	if err := os.WriteFile(file, []byte(message), 0o600); err != nil {
		t.Fatal(err)
	}
	return file, memory
}

func pendingNote(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(sessionsDir(t), "test-session"+feedbackNoteSuffix))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestFeedbackNewRuleNoted(t *testing.T) {
	cfg := setup(t)
	ask := &fakeAsker{nouls: map[string]float64{"lasting_rule": 0.9, "covered": 0.1}}
	useJev(t, ask, &fakeGate{})
	file, memory := analyzeSetup(t, "From now on, run the linter before every commit.", 15)
	if err := AnalyzeFeedback(context.Background(), file, memory, cfg); err != nil {
		t.Fatal(err)
	}
	if got := pendingNote(t); got != feedbackNoteText {
		t.Errorf("pending note %q, want %q", got, feedbackNoteText)
	}
	if n := ask.calls.Load(); n != 1+policy.FeedbackCandidates {
		t.Errorf("Jev asked %d times, want %d", n, 1+policy.FeedbackCandidates)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("prompt file kept: %v", err)
	}
	rules := map[string]bool{}
	for _, s := range ask.states[1:] {
		rules[s.(map[string]any)[policy.FeedbackRuleField].(string)] = true
	}
	if !rules["[Tests](tests.md) — run the tests before a commit"] {
		t.Errorf("memory index line not among the candidates: %v", rules)
	}
}

func TestFeedbackCoveredNotNoted(t *testing.T) {
	cfg := setup(t)
	useJev(t, &fakeAsker{nouls: map[string]float64{"lasting_rule": 0.9, "covered": 0.8}}, &fakeGate{})
	file, memory := analyzeSetup(t, "Always run the linter before a commit.", 3)
	if err := AnalyzeFeedback(context.Background(), file, memory, cfg); err != nil {
		t.Fatal(err)
	}
	if got := pendingNote(t); got != "" {
		t.Errorf("pending note %q, want none", got)
	}
}

func TestFeedbackNotARule(t *testing.T) {
	cfg := setup(t)
	ask := &fakeAsker{nouls: map[string]float64{"lasting_rule": 0.5}}
	useJev(t, ask, &fakeGate{})
	file, memory := analyzeSetup(t, "Fix the failing test.", 3)
	if err := AnalyzeFeedback(context.Background(), file, memory, cfg); err != nil {
		t.Fatal(err)
	}
	if got := pendingNote(t); got != "" || ask.calls.Load() != 1 {
		t.Errorf("pending note %q after %d Jev calls; want none after 1", got, ask.calls.Load())
	}
}

func TestFeedbackPrivateSkipsJev(t *testing.T) {
	cfg := setup(t)
	ask := &fakeAsker{nouls: map[string]float64{"lasting_rule": 0.9}}
	gate := &fakeGate{local: true}
	useJev(t, ask, gate)
	file, memory := analyzeSetup(t, "Always run the linter.", 3)
	if err := AnalyzeFeedback(context.Background(), file, memory, cfg); err != nil {
		t.Fatal(err)
	}
	if gate.calls.Load() == 0 || ask.calls.Load() != 0 || pendingNote(t) != feedbackLocalNoteText {
		t.Errorf("gate %d, Jev %d calls, note %q; want the gate only and the local note", gate.calls.Load(), ask.calls.Load(), pendingNote(t))
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("prompt file kept: %v", err)
	}
}

func TestFeedbackAnalyzeRefusesOtherFiles(t *testing.T) {
	cfg := setup(t)
	useJev(t, &fakeAsker{}, &fakeGate{})
	other := filepath.Join(t.TempDir(), "test-session-fb-1.txt")
	if err := os.WriteFile(other, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AnalyzeFeedback(context.Background(), other, "", cfg); err == nil {
		t.Error("a file outside the sessions folder was accepted")
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("the other file was touched: %v", err)
	}
}

func TestFeedbackCandidates(t *testing.T) {
	rules := []string{"use tabs in go files", "run the linter before each commit", "never print", "the linter runs in ci"}
	got := candidates("Please run the linter before you commit", rules, 2)
	if len(got) != 2 || got[0] != rules[1] || got[1] != rules[3] {
		t.Errorf("got %v, want the two rules sharing the most words, best first", got)
	}
}

func TestFeedbackPrivateRuleSkipped(t *testing.T) {
	cfg := setup(t)
	ask := &fakeAsker{nouls: map[string]float64{"lasting_rule": 0.9, "covered": 0.1}}
	// Every CLAUDE.md bullet stays local; the memory line does not.
	useJev(t, ask, &fakeGate{localIf: func(s string) bool { return strings.HasPrefix(s, "rule ") }})
	file, memory := analyzeSetup(t, "From now on, run the tests before every commit.", 5)
	if err := AnalyzeFeedback(context.Background(), file, memory, cfg); err != nil {
		t.Fatal(err)
	}
	if n := ask.calls.Load(); n != 2 {
		t.Errorf("Jev asked %d times, want 2 (the message, then the memory line only)", n)
	}
	if got := pendingNote(t); got != feedbackNoteText {
		t.Errorf("pending note %q, want %q", got, feedbackNoteText)
	}
}

func TestFeedbackSweepsLeftovers(t *testing.T) {
	setup(t)
	dir := sessionsDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	files := map[string]time.Duration{
		"a-fb-1.txt":                    staleFeedbackFile + time.Second,  // dead analyzer: swept
		"b-fb-2.txt":                    time.Second,                      // in progress: kept
		"c" + feedbackNoteSuffix:        staleFeedbackFile + time.Second,  // pending, recent enough: kept
		"d" + feedbackNoteSuffix:        staleFeedbackNote + time.Second,  // session ended: swept
		"e" + feedbackNoteSuffix + ".9": staleFeedbackFile + time.Second,  // taken copy left behind: swept
		"test-session.json":             staleFeedbackNote + 24*time.Hour, // hook state: kept
	}
	for name, age := range files {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	sweepFeedback(dir, now)
	for name, kept := range map[string]bool{
		"a-fb-1.txt": false, "b-fb-2.txt": true, "c" + feedbackNoteSuffix: true,
		"d" + feedbackNoteSuffix: false, "e" + feedbackNoteSuffix + ".9": false, "test-session.json": true,
	} {
		_, err := os.Stat(filepath.Join(dir, name))
		if got := err == nil; got != kept {
			t.Errorf("%s: kept %v, want %v", name, got, kept)
		}
	}
}

// TestFeedbackSweepRemovesOnlyItsOwnFiles: the feedback sweep removes only
// the files the feedback hook names, whatever a session id holds.
func TestFeedbackSweepRemovesOnlyItsOwnFiles(t *testing.T) {
	setup(t)
	dir := sessionsDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	old := now.Add(-staleFeedbackNote - time.Hour)
	files := map[string]bool{
		"a-fb-1.txt":                        false,
		"a" + feedbackNoteSuffix:            false,
		"a" + feedbackNoteSuffix + ".12":    false,
		"a" + feedbackNoteSuffix + ".tmp-3": false,
		"x-fb-y.json":                       true, // session x-fb-y's state
		"x-fb-1.txt.json":                   true, // session x-fb-1.txt's state
		"x-fb-note.txt.json":                true,
		"x-fb-y.json.lock":                  true,
		"notes-fb-draft.md":                 true,
		"a-fb-1.txt.lock":                   true,
		".draft-fb-1.txt":                   true, // no session id starts with a dot
	}
	for name := range files {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	sweepFeedback(dir, now)
	for name, kept := range files {
		_, err := os.Stat(filepath.Join(dir, name))
		if got := err == nil; got != kept {
			t.Errorf("%s: kept %v, want %v", name, got, kept)
		}
	}
}
