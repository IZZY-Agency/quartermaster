package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/config"
	"github.com/IZZY-Agency/quartermaster/internal/privacy"
	"github.com/IZZY-Agency/quartermaster/internal/route"
	"github.com/IZZY-Agency/quartermaster/internal/typesafe"
)

// fakeAsker answers every noul question from a table and records what it
// was sent.
type fakeAsker struct {
	nouls  map[string]float64
	calls  atomic.Int32
	states []any
}

func (f *fakeAsker) Ask(_ context.Context, state any, qs map[string]typesafe.Question) (map[string]typesafe.Answer, error) {
	f.calls.Add(1)
	f.states = append(f.states, state)
	out := make(map[string]typesafe.Answer, len(qs))
	for id, q := range qs {
		out[id] = typesafe.Answer{Type: q.Type, Noul: f.nouls[id]}
	}
	return out, nil
}

// fakeGate passes text through, or keeps it local: all of it, or the texts
// localIf picks.
type fakeGate struct {
	local   bool
	localIf func(string) bool
	calls   atomic.Int32
}

func (g *fakeGate) Gate(_ context.Context, text string) (privacy.Result, error) {
	g.calls.Add(1)
	if g.local || (g.localIf != nil && g.localIf(text)) {
		return privacy.Result{Local: true, Reason: "test"}, nil
	}
	return privacy.Result{Text: text}, nil
}

// useJev replaces the Jev asker and gate the coordinator hooks build. A nil
// ask means Jev is off.
func useJev(t *testing.T, ask *fakeAsker, gate *fakeGate) {
	t.Helper()
	old := jev
	jev = func(config.Config) (route.Asker, route.Gater) {
		if ask == nil {
			return nil, nil
		}
		return ask, gate
	}
	t.Cleanup(func() { jev = old })
}

// fireInput runs hook name on a hook input given as a map.
func fireInput(t *testing.T, cfg config.Config, name string, in map[string]any) []byte {
	t.Helper()
	if _, ok := in["session_id"]; !ok {
		in["session_id"] = "test-session"
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	return Handle(name, bytes.NewReader(b), cfg)
}

// reviewRepo makes a repository whose branch is ahead of origin/main by a
// small change.
func reviewRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.name", "Test")
	run("config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "one")
	run("update-ref", "refs/remotes/origin/main", "HEAD")
	run("checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("commit", "-q", "-am", "two")
	return dir
}

func agent(cwd string, input map[string]any) map[string]any {
	return map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Agent", "cwd": cwd, "tool_input": input, "tool_use_id": "toolu_test1"}
}

// ran fires the PostToolUse of an Agent call made with agent.
func ran(t *testing.T, cfg config.Config, call map[string]any) {
	t.Helper()
	post := map[string]any{}
	for k, v := range call {
		post[k] = v
	}
	post["hook_event_name"] = "PostToolUse"
	if out := fireInput(t, cfg, "reviewer-model", post); len(out) != 0 {
		t.Errorf("PostToolUse: got %s, want nothing", out)
	}
}

func TestReviewerModelRewrite(t *testing.T) {
	cfg := setup(t)
	ask := &fakeAsker{nouls: map[string]float64{"security": 0.9, "data_risk": 0.1}}
	useJev(t, ask, &fakeGate{})
	repo := reviewRepo(t)
	out := fireInput(t, cfg, "reviewer-model", agent(repo, map[string]any{
		"description": "Review the login change", "prompt": "Review the diff for bugs.", "subagent_type": "general-purpose",
	}))
	want(t, out, "rewrite")
	got := decode(t, out).HookSpecificOutput["updatedInput"].(map[string]any)
	if got["model"] != "opus" || got["effort"] != "high" || got["prompt"] != "Review the diff for bugs." || got["subagent_type"] != "general-purpose" {
		t.Errorf("updated input %v, want opus/high with the other fields kept", got)
	}
	if ask.calls.Load() != 1 {
		t.Errorf("Jev asked %d times, want 1", ask.calls.Load())
	}

	// Jev off: the review table runs on the diff size alone.
	useJev(t, nil, nil)
	out = fireInput(t, cfg, "reviewer-model", agent(reviewRepo(t), map[string]any{"description": "code review", "prompt": "Check the diff."}))
	got = decode(t, out).HookSpecificOutput["updatedInput"].(map[string]any)
	if got["model"] != "sonnet" || got["effort"] != "high" {
		t.Errorf("Jev off: %v, want sonnet/high (row small)", got)
	}
}

func TestReviewerModelPasses(t *testing.T) {
	cfg := setup(t)
	ask := &fakeAsker{}
	useJev(t, ask, &fakeGate{})
	repo := reviewRepo(t)
	for _, in := range []map[string]any{
		{"description": "Implement the parser", "prompt": "x"},
		{"description": "Previewer fix", "prompt": "x"},
		{"description": "Review the diff", "prompt": "x", "model": "sonnet"},
		{"description": "Review the code", "prompt": "x", "subagent_type": "Explore"},
		{"description": "Review the code", "prompt": "x", "subagent_type": "qm-worker:explorer"},
	} {
		if out := fireInput(t, cfg, "reviewer-model", agent(repo, in)); len(out) != 0 {
			t.Errorf("%v: got %s, want pass", in, out)
		}
	}
	if ask.calls.Load() != 0 {
		t.Errorf("Jev asked %d times, want 0", ask.calls.Load())
	}
}

func TestReviewerModelNoteMode(t *testing.T) {
	cfg := setup(t)
	cfg.Rules["reviewer-model"] = "note"
	useJev(t, nil, nil)
	out := fireInput(t, cfg, "reviewer-model", agent(reviewRepo(t), map[string]any{"description": "Review", "prompt": "x"}))
	want(t, out, "note")
}

func TestWorkerRoleShortCircuits(t *testing.T) {
	for _, role := range []string{"worker", "reviewer"} {
		t.Run(role, func(t *testing.T) {
			cfg := setup(t)
			t.Setenv("QM_ROLE", role)
			ask := &fakeAsker{nouls: map[string]float64{"answerable_from_context": 0.9, "cheap_to_undo": 0.9, "lasting_rule": 0.9}}
			gate := &fakeGate{}
			useJev(t, ask, gate)
			started := countStarts(t)
			writePendingNote(t, "test-session", "a note")

			ins := map[string]map[string]any{
				"ask-gate":         {"hook_event_name": "PreToolUse", "tool_name": "AskUserQuestion", "tool_input": askInput("Which name?")},
				"feedback":         {"hook_event_name": "UserPromptSubmit", "prompt": "From now on always run the linter."},
				"feedback-deliver": {"hook_event_name": "PostToolUse", "tool_name": "Bash"},
			}
			for name, in := range ins {
				if out := fireInput(t, cfg, name, in); len(out) != 0 {
					t.Errorf("%s: got %s, want pass", name, out)
				}
			}
			stop := map[string]any{"hook_event_name": "Stop", "last_assistant_message": "Which name should I use?"}
			if out := fireInput(t, cfg, "ask-gate", stop); len(out) != 0 {
				t.Errorf("ask-gate Stop: got %s, want pass", out)
			}
			if ask.calls.Load() != 0 || gate.calls.Load() != 0 || started.Load() != 0 {
				t.Errorf("Jev %d, gate %d, analyzer %d calls; want none", ask.calls.Load(), gate.calls.Load(), started.Load())
			}

			// reviewer-model is shared with qm-worker: it still routes.
			out := fireInput(t, cfg, "reviewer-model", agent(reviewRepo(t), map[string]any{"description": "Review", "prompt": "x"}))
			want(t, out, "rewrite")
		})
	}
}

func TestReviewerModelCountsRounds(t *testing.T) {
	cfg := setup(t)
	useJev(t, nil, nil)
	repo := reviewRepo(t)
	call := agent(repo, map[string]any{"description": "Review the change", "prompt": "Check the diff."})
	first := decode(t, fireInput(t, cfg, "reviewer-model", call)).HookSpecificOutput["updatedInput"].(map[string]any)
	// The round is logged only once the Agent call has run.
	if recs := records(t, "review"); len(recs) != 0 {
		t.Errorf("review records %+v before the Agent call ran, want none", recs)
	}
	ran(t, cfg, call)
	ran(t, cfg, call) // a second PostToolUse for the same call logs nothing
	second := decode(t, fireInput(t, cfg, "reviewer-model", call)).HookSpecificOutput["updatedInput"].(map[string]any)
	ran(t, cfg, call)
	if first["model"] != "sonnet" || first["effort"] != "high" {
		t.Errorf("first review %v, want sonnet/high (row small)", first)
	}
	if second["model"] != "sonnet" || second["effort"] != "medium" {
		t.Errorf("second review %v, want sonnet/medium (round 2, row fix-check)", second)
	}
	recs := records(t, "review")
	if len(recs) != 2 || recs[0].Round != 1 || recs[1].Round != 2 || recs[1].Branch != "feature" ||
		recs[1].Detail != agentReviewDetail || recs[1].Decision == nil || recs[1].Decision.Row != "fix-check" {
		t.Errorf("review records %+v, want rounds 1 and 2 on branch feature", recs)
	}
}

func TestReviewerModelQuietWithoutDiff(t *testing.T) {
	cfg := setup(t)
	useJev(t, nil, nil)
	noBase := reviewRepo(t)
	cmd := exec.Command("git", "-C", noBase, "update-ref", "-d", "refs/remotes/origin/main")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for _, dir := range []string{t.TempDir(), noBase} {
		if out := fireInput(t, cfg, "reviewer-model", agent(dir, map[string]any{"description": "Review", "prompt": "x"})); len(out) != 0 {
			t.Errorf("%s: got %s, want pass", dir, out)
		}
	}
	if recs := records(t, "error"); len(recs) != 0 {
		t.Errorf("error records %+v, want none", recs)
	}
	if recs := records(t, "review"); len(recs) != 0 {
		t.Errorf("review records %+v, want none", recs)
	}
}

// TestReviewerModelPassesOnDefaultBranch: a coordinator in the main checkout
// starts a reviewer for work on another branch; qm cannot size it from there,
// so the call passes untouched and no round is logged, whatever was logged
// before.
func TestReviewerModelPassesOnDefaultBranch(t *testing.T) {
	cfg := setup(t)
	useJev(t, nil, nil)
	repo := reviewRepo(t)
	gitC := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	gitC("checkout", "-q", "main")
	call := agent(repo, map[string]any{"description": "Review the whole feature", "prompt": "Review it all."})
	for range 2 {
		if out := fireInput(t, cfg, "reviewer-model", call); len(out) != 0 {
			t.Errorf("on main: got %s, want pass", out)
		}
		ran(t, cfg, call)
	}
	// Detached HEAD, and a branch with no change against the base.
	gitC("checkout", "-q", "--detach", "feature")
	if out := fireInput(t, cfg, "reviewer-model", call); len(out) != 0 {
		t.Errorf("detached: got %s, want pass", out)
	}
	gitC("checkout", "-q", "-b", "empty", "main")
	if out := fireInput(t, cfg, "reviewer-model", call); len(out) != 0 {
		t.Errorf("empty diff: got %s, want pass", out)
	}
	if recs := records(t, "review"); len(recs) != 0 {
		t.Errorf("review records %+v, want none", recs)
	}
	// Back on the feature branch, the first review is round 1, row small.
	gitC("checkout", "-q", "feature")
	got := decode(t, fireInput(t, cfg, "reviewer-model", call)).HookSpecificOutput["updatedInput"].(map[string]any)
	if got["model"] != "sonnet" || got["effort"] != "high" {
		t.Errorf("feature branch: %v, want sonnet/high (round 1, row small)", got)
	}
}

func TestPendingReviewSwept(t *testing.T) {
	setup(t)
	dir, err := pendingDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "toolu_old.json")
	if err := os.WriteFile(old, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * pendingReviewAge)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	if err := keepPendingReview("toolu_new", route.Decision{Kind: "review"}, route.Record{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("old pending review not swept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "toolu_new.json")); err != nil {
		t.Errorf("new pending review: %v", err)
	}
}

// An inherited GIT_DIR must not make a hook name another repository as a
// worker's project.
func TestGitTopIgnoresInheritedGitVariables(t *testing.T) {
	dir, other := reviewRepo(t), reviewRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	if got, want := gitTop(dir), func() string { w, _ := filepath.EvalSymlinks(dir); return w }(); got != want {
		t.Errorf("gitTop = %q, want %q", got, want)
	}
}
