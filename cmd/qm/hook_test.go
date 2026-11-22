package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestHookAlwaysExitsZero(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir+"/state")
	t.Setenv("XDG_CONFIG_HOME", dir+"/config")
	old := hookStdin
	t.Cleanup(func() { hookStdin = old })
	for _, c := range []struct {
		args  []string
		stdin string
	}{
		{[]string{"read-budget"}, "{"},
		{[]string{"no-such-hook"}, "{}"},
		{nil, ""},
	} {
		hookStdin = strings.NewReader(c.stdin)
		var out, errOut bytes.Buffer
		if code := run(append([]string{"hook"}, c.args...), &out, &errOut); code != 0 || out.Len() != 0 {
			t.Errorf("%v: exit %d, stdout %q", c.args, code, out.String())
		}
	}
}

func TestHookPrintsOutput(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir+"/state")
	t.Setenv("XDG_CONFIG_HOME", dir+"/config")
	old := hookStdin
	t.Cleanup(func() { hookStdin = old })
	hookStdin = strings.NewReader(`{"session_id":"s","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"codex exec x"}}`)
	var out, errOut bytes.Buffer
	if code := run([]string{"hook", "raw-launch-gate"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), `"permissionDecision":"deny"`) {
		t.Errorf("got %q", out.String())
	}
}
