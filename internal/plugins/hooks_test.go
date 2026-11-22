// Package plugins tests the plugin manifests and hook wiring under plugins/.
package plugins

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/IZZY-Agency/quartermaster/internal/hooks"
)

// hookEntry is one command hook from a plugin's hooks/hooks.json.
type hookEntry struct {
	Event   string
	Matcher string
	Type    string
	Command string
	Timeout int
}

// loadHooks reads plugins/<plugin>/hooks/hooks.json into a flat list.
func loadHooks(t *testing.T, plugin string) []hookEntry {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "plugins", plugin, "hooks", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("%s hooks.json: %v", plugin, err)
	}
	var out []hookEntry
	for event, groups := range f.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				out = append(out, hookEntry{event, g.Matcher, h.Type, h.Command, h.Timeout})
			}
		}
	}
	return out
}

// pathGuard is the prefix every hook command carries: without qm on PATH the
// hook exits 0 and prints nothing.
const pathGuard = "command -v qm >/dev/null 2>&1 || exit 0; exec qm hook "

var guardedCommand = regexp.MustCompile("^" + regexp.QuoteMeta(pathGuard) + "([a-z-]+)$")

// hookName returns the qm hook a command runs, failing the test when the
// command is not the guarded form or names an unknown hook.
func hookName(t *testing.T, h hookEntry) string {
	t.Helper()
	m := guardedCommand.FindStringSubmatch(h.Command)
	if m == nil {
		t.Errorf("%s %s: command %q is not %q<name>", h.Event, h.Matcher, h.Command, pathGuard)
		return ""
	}
	known := hooks.Names()
	if i := sort.SearchStrings(known, m[1]); i == len(known) || known[i] != m[1] {
		t.Errorf("%s %s: unknown hook %q", h.Event, h.Matcher, m[1])
	}
	return m[1]
}

// pluginNames are the plugins under plugins/.
var pluginNames = []string{"qm-coordinator", "qm-worker"}

func TestHooksJSONCommands(t *testing.T) {
	wants := map[string][]string{
		"qm-worker": {
			"PostToolUse * context-limit",
			"PostToolUse Agent reviewer-model",
			"PreToolUse Agent explorer-model",
			"PreToolUse Agent reviewer-model",
			"PreToolUse Bash raw-launch-gate",
			"PreToolUse Grep|Bash lsp-note",
			"PreToolUse Read|Grep|Bash|Write|Edit read-budget",
			"PreToolUse Write write-note",
		},
		"qm-coordinator": {
			"PostToolUse * feedback-deliver",
			"PostToolUse Agent reviewer-model",
			"PreToolUse Agent reviewer-model",
			"PreToolUse AskUserQuestion ask-gate",
			"PreToolUse Bash raw-launch-gate",
			"Stop  ask-gate",
			"UserPromptSubmit  feedback",
			"UserPromptSubmit  feedback-deliver",
		},
	}
	for _, plugin := range pluginNames {
		var got []string
		for _, h := range loadHooks(t, plugin) {
			if h.Type != "command" || h.Timeout != 25 {
				t.Errorf("%s %s %s: type %q timeout %d, want command and 25", plugin, h.Event, h.Matcher, h.Type, h.Timeout)
			}
			got = append(got, h.Event+" "+h.Matcher+" "+hookName(t, h))
		}
		sort.Strings(got)
		if want := wants[plugin]; strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s hooks:\n%s\nwant:\n%s", plugin, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
}

func TestHookPathGuardWithoutQm(t *testing.T) {
	empty := t.TempDir()
	for _, plugin := range pluginNames {
		for _, h := range loadHooks(t, plugin) {
			cmd := exec.Command("/bin/sh", "-c", h.Command)
			cmd.Env = []string{"PATH=" + empty}
			cmd.Stdin = strings.NewReader(`{"session_id":"s"}`)
			out, err := cmd.CombinedOutput()
			if err != nil || len(out) != 0 {
				t.Errorf("%s %s: err %v, output %q", plugin, h.Command, err, out)
			}
		}
	}
}
