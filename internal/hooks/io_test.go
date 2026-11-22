package hooks

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestReadPreToolUse(t *testing.T) {
	f, err := os.Open("testdata/pretooluse.json")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	in, err := Read(f)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if in.HookEventName != "PreToolUse" || in.ToolName != "Bash" {
		t.Errorf("event %q tool %q", in.HookEventName, in.ToolName)
	}
	if got := in.ToolInput["command"]; got != "ls" {
		t.Errorf("tool_input.command = %v, want ls", got)
	}
	if in.SessionID != "test-session" || in.ToolUseID != "toolu_test" || in.CWD != "/work/example" {
		t.Errorf("unexpected fields: %+v", in)
	}
}

func TestReadRejectsBadJSON(t *testing.T) {
	if _, err := Read(strings.NewReader("{")); err == nil {
		t.Fatal("want an error for truncated JSON")
	}
}

type hookOut struct {
	HookSpecificOutput map[string]any `json:"hookSpecificOutput"`
	Decision           string         `json:"decision"`
	Reason             string         `json:"reason"`
}

func decode(t *testing.T, b []byte) hookOut {
	t.Helper()
	var o hookOut
	if err := json.Unmarshal(b, &o); err != nil {
		t.Fatalf("unmarshal %s: %v", b, err)
	}
	return o
}

func TestDenyShape(t *testing.T) {
	o := decode(t, Deny("PreToolUse", "use qm spawn worker"))
	h := o.HookSpecificOutput
	if h["hookEventName"] != "PreToolUse" || h["permissionDecision"] != "deny" || h["permissionDecisionReason"] != "use qm spawn worker" {
		t.Errorf("got %v", h)
	}
	for _, k := range []string{"updatedInput", "additionalContext"} {
		if _, ok := h[k]; ok {
			t.Errorf("%s set on a deny", k)
		}
	}
}

func TestNoteShape(t *testing.T) {
	h := decode(t, Note("PostToolUse", "use Edit")).HookSpecificOutput
	if h["hookEventName"] != "PostToolUse" || h["additionalContext"] != "use Edit" {
		t.Errorf("got %v", h)
	}
	if _, ok := h["permissionDecision"]; ok {
		t.Error("permissionDecision set on a note")
	}
}

func TestRewriteShape(t *testing.T) {
	b, err := Rewrite("PreToolUse", map[string]any{"model": "sonnet"}, "routed")
	if err != nil {
		t.Fatal(err)
	}
	h := decode(t, b).HookSpecificOutput
	u, ok := h["updatedInput"].(map[string]any)
	if !ok || u["model"] != "sonnet" || h["additionalContext"] != "routed" {
		t.Errorf("got %v", h)
	}
	if _, ok := h["permissionDecision"]; ok {
		t.Error("permissionDecision set on a rewrite")
	}
	b, err = Rewrite("PreToolUse", map[string]any{"a": 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	h = decode(t, b).HookSpecificOutput
	if _, ok := h["additionalContext"]; ok {
		t.Error("empty note written")
	}
}

func TestRewriteEncodeError(t *testing.T) {
	b, err := Rewrite("PreToolUse", map[string]any{"bad": make(chan int)}, "")
	if err == nil || b != nil {
		t.Errorf("got %s, %v; want nil and an error", b, err)
	}
}

func TestBlockStopShape(t *testing.T) {
	o := decode(t, BlockStop("finish the task"))
	if o.Decision != "block" || o.Reason != "finish the task" || o.HookSpecificOutput != nil {
		t.Errorf("got %+v", o)
	}
}
