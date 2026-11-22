package hooks

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/config"
	"github.com/IZZY-Agency/quartermaster/internal/declog"
	"github.com/IZZY-Agency/quartermaster/internal/route"
)

// setup isolates state and config in temp folders and clears the worker
// environment.
func setup(t *testing.T) config.Config {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	for _, k := range []string{"QM_LSP", "QM_AUTOCOMPACT", "QM_SESSION_NAME", "QM_ROLE", "QM_PROJECT_ID"} {
		t.Setenv(k, "")
	}
	return config.Defaults()
}

// fire runs hook name on a PreToolUse input for tool and returns the raw output.
func fire(t *testing.T, cfg config.Config, name, tool string, input map[string]any) []byte {
	t.Helper()
	return fireEvent(t, cfg, name, "PreToolUse", tool, input, "")
}

func fireEvent(t *testing.T, cfg config.Config, name, event, tool string, input map[string]any, transcript string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"session_id":      "test-session",
		"transcript_path": transcript,
		"cwd":             t.TempDir(),
		"hook_event_name": event,
		"tool_name":       tool,
		"tool_input":      input,
	})
	if err != nil {
		t.Fatal(err)
	}
	return Handle(name, bytes.NewReader(b), cfg)
}

// verdict is pass, deny, note or rewrite, read from a hook's output.
func verdict(t *testing.T, out []byte) string {
	t.Helper()
	if len(out) == 0 {
		return "pass"
	}
	h := decode(t, out).HookSpecificOutput
	switch {
	case h["permissionDecision"] == "deny":
		return "deny"
	case h["updatedInput"] != nil:
		return "rewrite"
	case h["additionalContext"] != nil:
		return "note"
	}
	t.Fatalf("unknown output %s", out)
	return ""
}

func want(t *testing.T, out []byte, v string) {
	t.Helper()
	if got := verdict(t, out); got != v {
		t.Errorf("got %s (%s), want %s", got, out, v)
	}
}

func records(t *testing.T, kind string) []route.Record {
	t.Helper()
	all, err := declog.Read(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var out []route.Record
	for _, r := range all {
		if r.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}

// countCommands replaces the notifier runner with a counter.
func countCommands(t *testing.T) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	old := runCommand
	runCommand = func(_ []string) error { n.Add(1); return nil }
	t.Cleanup(func() { runCommand = old })
	return &n
}

func grep(pattern string) map[string]any { return map[string]any{"pattern": pattern} }
func bash(cmd string) map[string]any     { return map[string]any{"command": cmd} }

func TestIsExploration(t *testing.T) {
	cases := []struct {
		tool  string
		input map[string]any
		want  bool
	}{
		{"Read", map[string]any{"file_path": "/a.go"}, true},
		{"Read", map[string]any{"file_path": "/a.go", "limit": 200}, true},
		{"Read", map[string]any{"file_path": "/a.go", "offset": 10, "limit": 80}, false},
		{"Grep", grep("x"), true},
		{"Glob", map[string]any{"pattern": "*.go"}, false},
		{"Bash", bash("cat main.go"), true},
		{"Bash", bash("cd internal && grep -rn foo ."), true},
		{"Bash", bash("cd a && cd b && rg foo"), true},
		{"Bash", bash("head -50 x | wc -l"), true},
		{"Bash", bash("/usr/bin/find . -name '*.go'"), true},
		{"Bash", bash("tail -f log"), true},
		{"Bash", bash("sed -n '10,89p' a.go"), false}, // 80 lines
		{"Bash", bash("sed -n '10,90p' a.go"), true},  // 81 lines
		{"Bash", bash("sed -i 's/a/b/' a.go"), true},
		{"Bash", bash("go test ./... | grep FAIL"), false},
		{"Bash", bash("git status"), false},
		{"Bash", bash("cat > notes.md <<'EOF'\nhello\nEOF"), false},
		{"Bash", bash("cat <<EOF > notes.md\nhello\nEOF"), false},
		{"Bash", bash("cat >> notes.md"), false},
		{"Bash", bash("cat a.go 2>/dev/null"), true},
		{"Bash", bash("cat a.go 2>&1 | head"), true},
		{"Bash", bash("cat a.go > /dev/null"), true},
		{"Bash", bash("cat a.go 1> out.txt"), false},
		{"Bash", bash("cat a.go b.go 2>/dev/null > both.txt"), false},
	}
	for _, c := range cases {
		if got := isExploration(c.tool, c.input); got != c.want {
			t.Errorf("%s %v: got %v, want %v", c.tool, c.input, got, c.want)
		}
	}
}

func TestReadWithSmallLimitAllowed(t *testing.T) {
	cfg := setup(t)
	for i := 0; i < 20; i++ {
		want(t, fire(t, cfg, "read-budget", "Read", map[string]any{"file_path": "/a.go", "offset": i * 40, "limit": 40}), "pass")
	}
}

func TestHeredocNotExploration(t *testing.T) {
	cfg := setup(t)
	for i := 0; i < 20; i++ {
		want(t, fire(t, cfg, "read-budget", "Bash", bash("cat > out.txt <<EOF\nline\nEOF")), "pass")
	}
}

func TestReadBudgetDeniesNinth(t *testing.T) {
	cfg := setup(t)
	for i := 0; i < 8; i++ {
		want(t, fire(t, cfg, "read-budget", "Grep", grep("foo")), "pass")
	}
	out := fire(t, cfg, "read-budget", "Grep", grep("foo"))
	want(t, out, "deny")
	if r := decode(t, out).HookSpecificOutput["permissionDecisionReason"]; r != readBudgetReason {
		t.Errorf("reason %q", r)
	}
}

func TestPhaseResetOnEdit(t *testing.T) {
	cfg := setup(t)
	for i := 0; i < 8; i++ {
		fire(t, cfg, "read-budget", "Read", map[string]any{"file_path": "/a.go"})
	}
	want(t, fire(t, cfg, "read-budget", "Read", map[string]any{"file_path": "/a.go"}), "deny")
	want(t, fire(t, cfg, "read-budget", "Edit", map[string]any{"file_path": "/a.go"}), "pass")
	for i := 0; i < 8; i++ {
		want(t, fire(t, cfg, "read-budget", "Read", map[string]any{"file_path": "/a.go"}), "pass")
	}
	want(t, fire(t, cfg, "read-budget", "Read", map[string]any{"file_path": "/a.go"}), "deny")
	want(t, fire(t, cfg, "read-budget", "Write", map[string]any{"file_path": "/b.go"}), "pass")
	want(t, fire(t, cfg, "read-budget", "Grep", grep("x")), "pass")
}

func TestReadBudgetSoftensAfterThree(t *testing.T) {
	cfg := setup(t)
	cfg.Notify = []string{"notifier", "--title", "quartermaster"}
	var got [][]string
	old := runCommand
	runCommand = func(argv []string) error { got = append(got, argv); return nil }
	t.Cleanup(func() { runCommand = old })
	t.Setenv("QM_SESSION_NAME", "task-a")

	for i := 0; i < 8; i++ {
		fire(t, cfg, "read-budget", "Grep", grep("x"))
	}
	for i := 0; i < 3; i++ {
		want(t, fire(t, cfg, "read-budget", "Grep", grep("x")), "deny")
	}
	for i := 0; i < 3; i++ {
		want(t, fire(t, cfg, "read-budget", "Grep", grep("x")), "note")
	}
	recs := records(t, "guard")
	if len(recs) != 1 || recs[0].Rule != "read-budget" || recs[0].Action != "softened" || recs[0].Name != "task-a" || recs[0].Session != "test-session" {
		t.Fatalf("guard records %+v", recs)
	}
	wantArgv := []string{"notifier", "--title", "quartermaster", "task-a: read-budget softened after 3 denials"}
	if len(got) != 1 || strings.Join(got[0], "|") != strings.Join(wantArgv, "|") {
		t.Errorf("notify argv %q, want %q", got, wantArgv)
	}
	// A new phase is strict again.
	fire(t, cfg, "read-budget", "Edit", map[string]any{"file_path": "/a.go"})
	for i := 0; i < 8; i++ {
		fire(t, cfg, "read-budget", "Grep", grep("x"))
	}
	want(t, fire(t, cfg, "read-budget", "Grep", grep("x")), "deny")
}

func TestLaunchGate(t *testing.T) {
	cases := []struct {
		cmd  string
		deny bool
	}{
		{"codex exec 'review the diff'", true},
		{"cd repo && codex review --base main", true},
		{"agtermctl session new --command 'claude'", true},
		{"agtermctl session new --name w --cwd /repo --command 'claude --model opus \"$(cat b.md)\"'", true},
		{"agtermctl session new --command=codex", true},
		{"agtermctl session new --command 'codex exec x' --wait", true},
		{"agtermctl session new --workspace-name dev --command '/usr/local/bin/claude -p hi'", true},
		{"agtermctl session new --command 'env FOO=1 claude'", true},
		{"agtermctl session new --command 'cd /repo && claude'", true},
		{"agtermctl session new --command 'bash -lc \"claude -p x\"'", true},
		{"agtermctl session new --tint red --command claude", true},
		{"agtermctl session new --window active --command 'claude -p x'", true},
		{"agtermctl session new --socket /tmp/s --command claude", true},
		{"agtermctl session new --unknown-flag v --command claude", true},
		{"agtermctl session new --command htop --command claude", true},
		{"agtermctl session new --command=htop --command='codex exec x'", true},
		{"agtermctl session new --command htop --command 'npm run dev'", false},
		{"agtermctl session new --tint red --name w", false},
		{"agtermctl session new --command 'npm run dev'", false},
		{"agtermctl session new --command 'tail -f /tmp/app.log' --wait", false},
		{"agtermctl session new --name claude-notes --cwd /repo", false},
		{"agtermctl session new", false},
		{"agtermctl session new --name codex --command htop", false},
		{"agtermctl session new --command 'grep -rn claude docs'", false},
		{"agtermctl session list", false},
		{"tmux new-window codex", true},
		{"tmux new-window 'claude -p hi'", true},
		{"tmux new-session -d -s w claude --model opus", true},
		{"tmux new-window htop", false},
		{"claude -p 'do the task'", true},
		{"claude --model opus 'brief'", true},
		{"claude -n worker", true},
		{"claude 'fix the bug'", true},
		{"/usr/local/bin/claude -p hi", true},
		{"env FOO=1 claude -p hi", true},
		{"echo hi && claude -p x", true},
		{"claude plugin list", false},
		{"claude mcp list", false},
		{"claude --version", false},
		{"claude -v", false},
		{"claude --help", false},
		{"claude doctor", false},
		{"claude update", false},
		{"claude config list", false},
		{"claude", false},
		{"which claude", false},
		{"grep -rn claude docs", false},
		{"ls ~/.claude/plugins", false},
		{"cat > notes.md <<EOF\nclaude -p 'quoted in a heredoc'\nEOF", false},
		{"codex --version", false},
		{"qm spawn worker --name a brief.md", false},
		{"qm spawn review --base main", false},
		{"cd repo && qm spawn review", false},
		{"qm version; codex exec x", true},
		{"qm spawn worker --name claude-fix brief.md && claude -p x", true},
		{"claude --dangerously-skip-permissions 'task'", true},
		{"claude --permission-mode acceptEdits 'task'", true},
		{"claude --permission-mode acceptEdits", false},
		{"claude --resume abc123", false},
		{"claude --settings=s.json 'task'", true},
		{"tmux new-window -n claude htop", false},
		{"tmux new-window -n w -c /repo claude", true},
		{"tmux new-window 'codex exec x'", true},
		{"tmux send-keys -t w 'claude -p x' Enter", true},
		{"tmux send-keys -t claude 'ls' Enter", false},
		{"bash -lc 'claude -p x'", true},
		{"sh -ec 'codex review'", true},
		{"bash -l script.sh", false},
		{"echo \"$(claude -p summarise)\"", true},
		{"echo \"result: `claude -p x`\"", true},
		{"echo \"$(git rev-parse HEAD)\"", false},
		{"cat a.go 2>&1 | claude -p review", true},
	}
	for _, c := range cases {
		if got := rawLaunch(c.cmd); got != c.deny {
			t.Errorf("%q: got %v, want %v", c.cmd, got, c.deny)
		}
	}
}

func TestClaudePluginCmdAllowed(t *testing.T) {
	cfg := setup(t)
	want(t, fire(t, cfg, "raw-launch-gate", "Bash", bash("claude plugin list")), "pass")
}

func TestQmSpawnAllowed(t *testing.T) {
	cfg := setup(t)
	want(t, fire(t, cfg, "raw-launch-gate", "Bash", bash("qm spawn worker --name fix-a --hint sonnet brief.md")), "pass")
	want(t, fire(t, cfg, "raw-launch-gate", "Bash", bash("qm spawn review --base main")), "pass")
}

func TestTmuxClaudeDenied(t *testing.T) {
	cfg := setup(t)
	out := fire(t, cfg, "raw-launch-gate", "Bash", bash("tmux new-window -n w 'claude --model opus \"$(cat brief.md)\"'"))
	want(t, out, "deny")
	if r := decode(t, out).HookSpecificOutput["permissionDecisionReason"]; r != launchGateReason {
		t.Errorf("reason %q", r)
	}
}

func TestLaunchGateSoftensAfterThree(t *testing.T) {
	cfg := setup(t)
	n := countCommands(t)
	for i := 0; i < 3; i++ {
		want(t, fire(t, cfg, "raw-launch-gate", "Bash", bash("codex exec review")), "deny")
	}
	want(t, fire(t, cfg, "raw-launch-gate", "Bash", bash("codex exec review")), "note")
	recs := records(t, "guard")
	if len(recs) != 1 || recs[0].Rule != "raw-launch-gate" || recs[0].Action != "softened" {
		t.Fatalf("guard records %+v", recs)
	}
	if n.Load() != 0 {
		t.Errorf("notifier ran %d times with no notify config", n.Load())
	}
	// With no new phase (a coordinator), softening ends after softenFor.
	clock = func() time.Time { return time.Now().Add(softenFor + time.Minute) }
	t.Cleanup(func() { clock = time.Now })
	want(t, fire(t, cfg, "raw-launch-gate", "Bash", bash("codex exec review")), "deny")
	want(t, fire(t, cfg, "raw-launch-gate", "Bash", bash("codex exec review")), "deny")
}

func TestExplorerRewrite(t *testing.T) {
	cfg := setup(t)
	for _, typ := range []string{"Explore", "explorer", "qm-worker:explorer"} {
		in := map[string]any{"description": "find callers", "prompt": "where is X used", "subagent_type": typ, "model": "opus"}
		out := fire(t, cfg, "explorer-model", "Agent", in)
		want(t, out, "rewrite")
		u, _ := decode(t, out).HookSpecificOutput["updatedInput"].(map[string]any)
		if u["model"] != "haiku" || u["prompt"] != "where is X used" || u["description"] != "find callers" || u["subagent_type"] != typ {
			t.Errorf("%s: updated %v", typ, u)
		}
	}
	want(t, fire(t, cfg, "explorer-model", "Agent", map[string]any{"subagent_type": "explorer", "model": "haiku"}), "pass")
	want(t, fire(t, cfg, "explorer-model", "Agent", map[string]any{"subagent_type": "general-purpose", "prompt": "x"}), "pass")
}

func TestNoDescriptionAgentPasses(t *testing.T) {
	cfg := setup(t)
	want(t, fire(t, cfg, "explorer-model", "Agent", map[string]any{"prompt": "do it"}), "pass")
	want(t, fire(t, cfg, "explorer-model", "Agent", map[string]any{}), "pass")
}

func TestLSPNote(t *testing.T) {
	cfg := setup(t)
	t.Setenv("QM_LSP", "go")
	for _, p := range []string{"DecideWorker", "func Load", "def parse", "class Foo", "type Config"} {
		out := fire(t, cfg, "lsp-note", "Grep", grep(p))
		want(t, out, "note")
		if c := decode(t, out).HookSpecificOutput["additionalContext"]; c != lspNoteText {
			t.Errorf("%q: note %q", p, c)
		}
	}
	for _, p := range []string{"ab", "foo bar", "TODO.*fix", "err != nil"} {
		want(t, fire(t, cfg, "lsp-note", "Grep", grep(p)), "pass")
	}
}

func TestLSPNoteBashSearch(t *testing.T) {
	cfg := setup(t)
	t.Setenv("QM_LSP", "go")
	for _, c := range []struct {
		cmd  string
		note bool
	}{
		{"rg DecideWorker", true},
		{"rg -n DecideWorker internal/", true},
		{"cd repo && grep -rn DecideWorker .", true},
		{"grep -n 'func Load' internal/config/config.go", true},
		{"rg -t go -e Handle", true},
		{"grep --include=*.go -n Handle -r", true},
		{"grep -n Handle app.log", false},
		{"grep DecideWorker", false},
		{"git log | grep DecideWorker", false},
		{"rg 'TODO.*fix'", false},
		{"rg -n ab", false},
		{"ls internal", false},
	} {
		got := verdict(t, fire(t, cfg, "lsp-note", "Bash", bash(c.cmd)))
		if (got == "note") != c.note {
			t.Errorf("%q: got %s, want note=%v", c.cmd, got, c.note)
		}
	}
	t.Setenv("QM_LSP", "")
	want(t, fire(t, cfg, "lsp-note", "Bash", bash("rg DecideWorker")), "pass")
}

func TestLSPNoteOffWithoutLSP(t *testing.T) {
	cfg := setup(t)
	want(t, fire(t, cfg, "lsp-note", "Grep", grep("DecideWorker")), "pass")
}

func TestWriteNote(t *testing.T) {
	cfg := setup(t)
	dir := t.TempDir()
	big := filepath.Join(dir, "big.go")
	small := filepath.Join(dir, "small.go")
	if err := os.WriteFile(big, bytes.Repeat([]byte("x"), 2001), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(small, bytes.Repeat([]byte("x"), 2000), 0o600); err != nil {
		t.Fatal(err)
	}
	out := fire(t, cfg, "write-note", "Write", map[string]any{"file_path": big, "content": "y"})
	want(t, out, "note")
	if c := decode(t, out).HookSpecificOutput["additionalContext"]; c != writeNoteText {
		t.Errorf("note %q", c)
	}
	want(t, fire(t, cfg, "write-note", "Write", map[string]any{"file_path": small, "content": "y"}), "pass")
	want(t, fire(t, cfg, "write-note", "Write", map[string]any{"file_path": filepath.Join(dir, "new.go"), "content": "y"}), "pass")
}

// transcript writes a minimal hand-made transcript whose last assistant
// message has the given input-side token counts.
func usageTranscript(t *testing.T, input, cacheWrite, cacheRead int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "t.jsonl")
	lines := []string{
		`{"type":"user","message":{"role":"user","content":"hi"}}`,
		`{"type":"assistant","message":{"usage":{"input_tokens":1,"cache_creation_input_tokens":1,"cache_read_input_tokens":1,"output_tokens":1}}}`,
	}
	last, err := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"usage": map[string]int{
		"input_tokens": input, "cache_creation_input_tokens": cacheWrite, "cache_read_input_tokens": cacheRead, "output_tokens": 500,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	lines = append(lines, string(last),
		`{"type":"assistant","isSidechain":true,"message":{"usage":{"input_tokens":900000}}}`,
		`{"type":"user","message":{"role":"user","content":"tool result"}}`)
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestContextLimit(t *testing.T) {
	cfg := setup(t)
	t.Setenv("QM_AUTOCOMPACT", "250000")
	t.Setenv("QM_SESSION_NAME", "task-a")
	post := func(tr string) []byte {
		return fireEvent(t, cfg, "context-limit", "PostToolUse", "Bash", bash("ls"), tr)
	}
	want(t, post(usageTranscript(t, 100, 1000, 170000)), "pass") // 171,100 is under 70% of 250,000
	out := post(usageTranscript(t, 100, 4900, 170000))           // 175,000
	want(t, out, "note")
	state := os.Getenv("XDG_STATE_HOME")
	wantNote := "Context is at 175000 tokens. Finish the current step, write the handoff note to " +
		filepath.Join(state, "quartermaster", "handoff", "task-a.md") + ", and report to the coordinator."
	if c := decode(t, out).HookSpecificOutput["additionalContext"]; c != wantNote {
		t.Errorf("note %q\nwant %q", c, wantNote)
	}
	if fi, err := os.Stat(filepath.Join(state, "quartermaster", "handoff")); err != nil || !fi.IsDir() {
		t.Errorf("handoff folder: %v", err)
	}
	want(t, post(usageTranscript(t, 0, 0, 224999)), "pass") // under 50,000 above the last note
	want(t, post(usageTranscript(t, 0, 0, 225000)), "note")
}

func TestContextLimitProjectFolder(t *testing.T) {
	// A worker of a qm up project writes its note in the project's folder,
	// where qm up reads it.
	cfg := setup(t)
	t.Setenv("QM_AUTOCOMPACT", "250000")
	t.Setenv("QM_SESSION_NAME", "task-a")
	t.Setenv("QM_PROJECT_ID", "proj-12345678")
	out := fireEvent(t, cfg, "context-limit", "PostToolUse", "Bash", bash("ls"), usageTranscript(t, 100, 4900, 170000))
	dir := filepath.Join(os.Getenv("XDG_STATE_HOME"), "quartermaster", "handoff", "proj-12345678")
	if c, _ := decode(t, out).HookSpecificOutput["additionalContext"].(string); !strings.Contains(c, filepath.Join(dir, "task-a.md")) {
		t.Errorf("note %q, want the path under %s", c, dir)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Errorf("project handoff folder: %v", err)
	}
}

func TestContextLimitNeedsWindow(t *testing.T) {
	cfg := setup(t)
	want(t, fireEvent(t, cfg, "context-limit", "PostToolUse", "Bash", bash("ls"), usageTranscript(t, 0, 0, 900000)), "pass")
}

func TestContextLimitReadsTail(t *testing.T) {
	cfg := setup(t)
	t.Setenv("QM_AUTOCOMPACT", "250000")
	p := usageTranscript(t, 0, 0, 200000)
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	old, _ := os.ReadFile(p)
	// A 3 MiB line before the real transcript must not be read.
	pad := `{"type":"user","message":{"content":"` + strings.Repeat("a", 3<<20) + `"}}` + "\n"
	if _, err := f.WriteAt(append([]byte(pad), old...), 0); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	want(t, fireEvent(t, cfg, "context-limit", "PostToolUse", "Bash", bash("ls"), p), "note")
}

func TestRuleOffSwitch(t *testing.T) {
	cfg := setup(t)
	t.Setenv("QM_LSP", "go")
	cfg.Rules = map[string]string{"raw-launch-gate": "off", "explorer-model": "note", "lsp-note": "off"}
	want(t, fire(t, cfg, "raw-launch-gate", "Bash", bash("codex exec x")), "pass")
	want(t, fire(t, cfg, "lsp-note", "Grep", grep("DecideWorker")), "pass")
	want(t, fire(t, cfg, "explorer-model", "Agent", map[string]any{"subagent_type": "Explore", "model": "opus"}), "note")

	cfg.Rules = map[string]string{"read-budget": "note"}
	for i := 0; i < 8; i++ {
		fire(t, cfg, "read-budget", "Grep", grep("x"))
	}
	for i := 0; i < 4; i++ {
		out := fire(t, cfg, "read-budget", "Grep", grep("x"))
		want(t, out, "note")
		if c := decode(t, out).HookSpecificOutput["additionalContext"]; c != readBudgetReason {
			t.Errorf("note %q", c)
		}
	}
	if recs := records(t, "guard"); len(recs) != 0 {
		t.Errorf("note mode softened: %+v", recs)
	}
}

func TestNotifyOptional(t *testing.T) {
	cfg := setup(t)
	n := countCommands(t)
	if err := Notify(cfg, "task-a", "hello"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 11; i++ {
		fire(t, cfg, "read-budget", "Grep", grep("x"))
	}
	if n.Load() != 0 {
		t.Errorf("notifier ran %d times", n.Load())
	}
	if recs := records(t, "guard"); len(recs) != 1 {
		t.Errorf("guard records %+v", recs)
	}
}

func TestNotifyRunsCommand(t *testing.T) {
	cfg := setup(t)
	out := filepath.Join(t.TempDir(), "out")
	cfg.Notify = []string{"sh", "-c", "printf %s \"$1\" > " + out, "notify"}
	if err := Notify(cfg, "task-a", "softened"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil || string(b) != "task-a: softened" {
		t.Errorf("got %q, %v", b, err)
	}
	cfg.Notify = []string{"sh", "-c", "exit 3"}
	if err := Notify(cfg, "", "x"); err == nil {
		t.Error("want an error from a failing notifier")
	}
}

func TestNotifyFailureLogged(t *testing.T) {
	cfg := setup(t)
	cfg.Notify = []string{"sh", "-c", "exit 3"}
	for i := 0; i < 11; i++ {
		fire(t, cfg, "read-budget", "Grep", grep("x"))
	}
	if recs := records(t, "guard"); len(recs) != 1 {
		t.Errorf("guard records %+v", recs)
	}
	if recs := records(t, "error"); len(recs) != 1 || recs[0].Rule != "read-budget" {
		t.Errorf("error records %+v", recs)
	}
}

func TestHookTimeoutPasses(t *testing.T) {
	cfg := setup(t)
	oldTimeout := hookTimeout
	hookTimeout = 50 * time.Millisecond
	rules["test-sleep"] = rule{run: func(*call) (result, error) {
		time.Sleep(2 * time.Second)
		return result{deny: "too late"}, nil
	}}
	t.Cleanup(func() { hookTimeout = oldTimeout; delete(rules, "test-sleep") })
	start := time.Now()
	out := fire(t, cfg, "test-sleep", "Bash", bash("ls"))
	if len(out) != 0 {
		t.Errorf("got %s, want no output", out)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("took %v", d)
	}
	if recs := records(t, "error"); len(recs) != 1 || recs[0].Rule != "test-sleep" {
		t.Errorf("error records %+v", recs)
	}
}

func TestBadInputPasses(t *testing.T) {
	cfg := setup(t)
	if out := Handle("read-budget", strings.NewReader("{"), cfg); len(out) != 0 {
		t.Errorf("got %s", out)
	}
	if out := Handle("no-such-hook", strings.NewReader("{}"), cfg); len(out) != 0 {
		t.Errorf("got %s", out)
	}
	if recs := records(t, "error"); len(recs) != 2 {
		t.Errorf("error records %+v", recs)
	}
}

func TestStateRejectsBadSessionID(t *testing.T) {
	setup(t)
	for _, id := range []string{"", "../x", "a/b", "."} {
		if err := withState(id, func(*State) error { return nil }); err == nil {
			t.Errorf("%q: want an error", id)
		}
	}
}

func TestStateBadFileReplaced(t *testing.T) {
	setup(t)
	if err := withState("s1", func(s *State) error { s.Reads = 5; return nil }); err != nil {
		t.Fatal(err)
	}
	state := os.Getenv("XDG_STATE_HOME")
	p := filepath.Join(state, "quartermaster", "sessions", "s1.json")
	if err := os.WriteFile(p, []byte(`{"reads":5,"pha`), 0o600); err != nil {
		t.Fatal(err)
	}
	var got State
	if err := withState("s1", func(s *State) error { s.Reads++; got = *s; return nil }); err != nil {
		t.Fatalf("bad state file: %v", err)
	}
	if got.Reads != 1 || got.Denials == nil || got.Soft == nil {
		t.Errorf("state %+v, want a fresh one", got)
	}
	if recs := records(t, "error"); len(recs) != 1 || recs[0].Rule != "session-state" {
		t.Errorf("error records %+v", recs)
	}
	if err := withState("s1", func(s *State) error { got = *s; return nil }); err != nil || got.Reads != 1 {
		t.Errorf("after replace: %+v, %v", got, err)
	}
}

func TestStateSweepsOldSessions(t *testing.T) {
	setup(t)
	if err := withState("old-session", func(*State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(os.Getenv("XDG_STATE_HOME"), "quartermaster", "sessions")
	old := filepath.Join(dir, "old-session.json")
	past := time.Now().Add(-stateAge - time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	// An existing session's call sweeps nothing; a new session's does.
	if err := withState("old-session", func(*State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	if err := withState("new-session", func(*State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("old session state kept: %v", err)
	}
}

func TestStateConcurrent(t *testing.T) {
	setup(t)
	done := make(chan error)
	for i := 0; i < 20; i++ {
		go func() { done <- withState("s1", func(s *State) error { s.Reads++; return nil }) }()
	}
	for i := 0; i < 20; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	var got int
	if err := withState("s1", func(s *State) error { got = s.Reads; return nil }); err != nil {
		t.Fatal(err)
	}
	if got != 20 {
		t.Errorf("reads %d, want 20", got)
	}
}
