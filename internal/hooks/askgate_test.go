package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IZZY-Agency/quartermaster/internal/policy"
)

const transcriptFixture = "testdata/coordinator-transcript.jsonl"

func askInput(question string) map[string]any {
	return map[string]any{"questions": []any{map[string]any{
		"question": question, "header": "Retry",
		"options": []any{map[string]any{"label": "Three", "description": "three tries"}, map[string]any{"label": "Five", "description": "five tries"}},
	}}}
}

func askCall(question string) map[string]any {
	return map[string]any{"hook_event_name": "PreToolUse", "tool_name": "AskUserQuestion", "transcript_path": transcriptFixture, "tool_input": askInput(question)}
}

func stopCall(msg string, active bool) map[string]any {
	return map[string]any{"hook_event_name": "Stop", "transcript_path": transcriptFixture, "last_assistant_message": msg, "stop_hook_active": active}
}

func askJev(answerable, cheap, product float64) *fakeAsker {
	return &fakeAsker{nouls: map[string]float64{"answerable_from_context": answerable, "cheap_to_undo": cheap, "product_ruling": product}}
}

func TestAskGateDenies(t *testing.T) {
	cfg := setup(t)
	ask := askJev(0.9, 0.9, 0.1)
	useJev(t, ask, &fakeGate{})
	out := fireInput(t, cfg, "ask-gate", askCall("Should the retry count be three?"))
	want(t, out, "deny")
	if r := decode(t, out).HookSpecificOutput["permissionDecisionReason"]; r != askGateReason {
		t.Errorf("reason %q, want %q", r, askGateReason)
	}
	if len(ask.states) != 1 {
		t.Fatalf("Jev asked %d times, want 1", len(ask.states))
	}
	st := ask.states[0].(map[string]any)
	q, _ := st[policy.AskQuestionField].(string)
	conv, _ := st[policy.AskConversationField].(string)
	if !strings.Contains(q, "Should the retry count be three?") || !strings.Contains(q, "Three") {
		t.Errorf("question %q lacks the question or its options", q)
	}
	for _, s := range []string{"user: Add a retry to the upload client.", "assistant: I will read the client first.", "assistant: The client has no retry yet."} {
		if !strings.Contains(conv, s) {
			t.Errorf("conversation lacks %q:\n%s", s, conv)
		}
	}
	for _, s := range []string{"package client", "hidden", "Subagent", "not a turn"} {
		if strings.Contains(conv, s) {
			t.Errorf("conversation holds %q:\n%s", s, conv)
		}
	}
}

func TestAskGatePassesProductRuling(t *testing.T) {
	cfg := setup(t)
	for _, a := range []*fakeAsker{askJev(0.9, 0.9, 0.8), askJev(0.9, 0.9, 0.5), askJev(0.5, 0.9, 0.1), askJev(0.9, 0.6, 0.1)} {
		useJev(t, a, &fakeGate{})
		if out := fireInput(t, cfg, "ask-gate", askCall("Which price tier?")); len(out) != 0 {
			t.Errorf("%v: got %s, want pass", a.nouls, out)
		}
	}
}

func TestAskGateNoteMode(t *testing.T) {
	cfg := setup(t)
	cfg.Rules["ask-gate"] = "note"
	useJev(t, askJev(0.9, 0.9, 0.1), &fakeGate{})
	want(t, fireInput(t, cfg, "ask-gate", askCall("Three?")), "note")
}

func TestStopGateNoteModeLetsStop(t *testing.T) {
	cfg := setup(t)
	cfg.Rules["ask-gate"] = "note"
	useJev(t, askJev(0.9, 0.9, 0.1), &fakeGate{})
	if out := fireInput(t, cfg, "ask-gate", stopCall("Should the retry count be three?", false)); len(out) != 0 {
		t.Errorf("got %s, want no output: any Stop output makes Claude continue", out)
	}
	recs := records(t, "guard")
	if len(recs) != 1 || recs[0].Rule != "ask-gate" || recs[0].Action != "stop-allowed" {
		t.Errorf("guard records %+v, want one ask-gate stop-allowed", recs)
	}
}

func TestStopGateBlocks(t *testing.T) {
	cfg := setup(t)
	useJev(t, askJev(0.9, 0.9, 0.1), &fakeGate{})
	out := fireInput(t, cfg, "ask-gate", stopCall("Done with the client.\n\nShould the retry count be three?\n\n", false))
	o := decode(t, out)
	if o.Decision != "block" || o.Reason != askGateReason {
		t.Errorf("got %s, want a block with the ask-gate reason", out)
	}
}

func TestStopGateIgnoresWhenActive(t *testing.T) {
	cfg := setup(t)
	ask := askJev(0.9, 0.9, 0.1)
	useJev(t, ask, &fakeGate{})
	if out := fireInput(t, cfg, "ask-gate", stopCall("Should the retry count be three?", true)); len(out) != 0 {
		t.Errorf("got %s, want pass", out)
	}
	if ask.calls.Load() != 0 {
		t.Errorf("Jev asked %d times, want 0", ask.calls.Load())
	}
}

func TestStopGateOnlyQuestions(t *testing.T) {
	cfg := setup(t)
	ask := askJev(0.9, 0.9, 0.1)
	useJev(t, ask, &fakeGate{})
	for _, msg := range []string{"", "Done.", "Is it done? Yes, it is.", "Should I go on?\nThe tests pass."} {
		if out := fireInput(t, cfg, "ask-gate", stopCall(msg, false)); len(out) != 0 {
			t.Errorf("%q: got %s, want pass", msg, out)
		}
	}
	if ask.calls.Load() != 0 {
		t.Errorf("Jev asked %d times, want 0", ask.calls.Load())
	}
}

func TestPrivateContextSkipsJev(t *testing.T) {
	cfg := setup(t)
	ask := askJev(0.9, 0.9, 0.1)
	gate := &fakeGate{local: true}
	useJev(t, ask, gate)
	if out := fireInput(t, cfg, "ask-gate", askCall("Three?")); len(out) != 0 {
		t.Errorf("got %s, want pass", out)
	}
	if gate.calls.Load() == 0 || ask.calls.Load() != 0 {
		t.Errorf("gate %d, Jev %d calls; want the gate only", gate.calls.Load(), ask.calls.Load())
	}
}

func TestAskGateOffWithoutKey(t *testing.T) {
	cfg := setup(t)
	useJev(t, nil, nil)
	if out := fireInput(t, cfg, "ask-gate", askCall("Three?")); len(out) != 0 {
		t.Errorf("got %s, want pass", out)
	}
	if out := fireInput(t, cfg, "ask-gate", stopCall("Three?", false)); len(out) != 0 {
		t.Errorf("Stop: got %s, want pass", out)
	}
	if recs := records(t, "error"); len(recs) != 0 {
		t.Errorf("error records %v, want none", recs)
	}
}

func TestRecentConversationKeepsLatest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	var b strings.Builder
	for i := 0; i < policy.AskTurns+5; i++ {
		b.WriteString(`{"type":"user","message":{"role":"user","content":"turn ` + strings.Repeat("x", i) + `"}}` + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	conv, err := recentConversation(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(conv, "user: turn"); n != policy.AskTurns {
		t.Errorf("%d turns, want %d", n, policy.AskTurns)
	}
	if !strings.HasSuffix(conv, "user: turn "+strings.Repeat("x", policy.AskTurns+4)) {
		t.Errorf("conversation does not end with the latest turn:\n%s", conv)
	}

	long := filepath.Join(t.TempDir(), "long.jsonl")
	big := strings.Repeat("y", policy.AskContextChars)
	data := `{"type":"user","message":{"role":"user","content":"old turn"}}` + "\n" +
		`{"type":"user","message":{"role":"user","content":"` + big + `"}}` + "\n"
	if err := os.WriteFile(long, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	conv, err = recentConversation(long)
	if err != nil {
		t.Fatal(err)
	}
	if len(conv) > policy.AskContextChars || strings.Contains(conv, "old turn") {
		t.Errorf("conversation of %d characters (cap %d) or still holding the old turn", len(conv), policy.AskContextChars)
	}
}
