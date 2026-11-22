package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// statusHermetic points every folder qm status reads at a temporary one, with
// an empty PATH (no codex, claude, agtermctl or tmux), no key, no helper, no
// gitleaks and no launcher. It returns the temporary HOME.
func statusHermetic(t *testing.T) string {
	t.Helper()
	routeHermetic(t)
	home := os.Getenv("HOME")
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "claude"))
	t.Setenv("PATH", filepath.Join(home, "bin"))
	t.Setenv("AGTERM_SESSION_ID", "")
	t.Setenv("TMUX", "")
	t.Setenv("CLAUDE_PID", "")
	t.Setenv("CLAUDE_CODE_AUTO_COMPACT_WINDOW", "")
	t.Setenv("QM_WORKER_PLUGIN", "")
	t.Setenv("SHELL", "/bin/sh")
	for _, d := range []string{"bin", "claude"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func statusOut(t *testing.T, args ...string) string {
	t.Helper()
	var out, errb bytes.Buffer
	if code := run(append([]string{"status"}, args...), &out, &errb); code != 0 {
		t.Fatalf("qm status: exit %d\n%s", code, errb.String())
	}
	return out.String()
}

func wantIn(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
}

func TestStatusNoKey(t *testing.T) {
	statusHermetic(t)
	wantIn(t, statusOut(t), "Jev off: no TypeSafe key; ask gate and feedback off")
}

func TestStatusNoCodex(t *testing.T) {
	statusHermetic(t)
	wantIn(t, statusOut(t), "Codex off: not installed; reviews go to Claude")
}

func TestStatusCodexUsageUnknown(t *testing.T) {
	home := statusHermetic(t)
	routeFile(t, filepath.Join(home, "bin"), "codex", "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(home, "bin", "codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	wantIn(t, statusOut(t), "Codex: available, usage unknown")
}

func TestStatusNoLocalModel(t *testing.T) {
	statusHermetic(t)
	wantIn(t, statusOut(t), "local model off: redaction and gitleaks only")
}

func TestStatusNoLauncher(t *testing.T) {
	statusHermetic(t)
	wantIn(t, statusOut(t), "launcher: none found")
}

func TestStatusShellHint(t *testing.T) {
	statusHermetic(t)
	t.Setenv("SHELL", "/opt/homebrew/bin/fish")
	out := statusOut(t)
	wantIn(t, out, "set -x TYPESAFE_API_KEY ")
	if strings.Contains(out, "export ") {
		t.Errorf("fish hints use export:\n%s", out)
	}
	t.Setenv("SHELL", "/bin/zsh")
	wantIn(t, statusOut(t), "export TYPESAFE_API_KEY=")
}

func TestStatusRulesWindowsAndLimits(t *testing.T) {
	home := statusHermetic(t)
	cfgDir := filepath.Join(home, "config", "quartermaster")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	routeFile(t, cfgDir, "config.json", `{"rules": {"read-budget": "off"}}`)
	routeFile(t, filepath.Join(home, "claude"), "settings.json", `{"autoCompactWindow": 500000}`)
	limitsDir := filepath.Join(home, "state", "quartermaster", "limits")
	if err := os.MkdirAll(limitsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// 5m20s old: "5m ago" for any run under 40 s.
	updated := time.Now().Add(-5*time.Minute - 20*time.Second).UTC().Format(time.RFC3339)
	routeFile(t, limitsDir, "claude.json", `{"five_hour_used": 42, "updated_at": "`+updated+`"}`)

	out := statusOut(t)
	wantIn(t, out, "read-budget", "off", "write-note", "sonnet 250000", "opus 400000",
		"autoCompactWindow 500000", "updated 5m ago", "5-hour 42%")
}

func TestStatusClaudeLimitsMissing(t *testing.T) {
	statusHermetic(t)
	wantIn(t, statusOut(t), "Claude limits: no limit file yet")
}

func TestStatusJSON(t *testing.T) {
	statusHermetic(t)
	var r struct {
		Providers []struct {
			Name string `json:"name"`
			On   bool   `json:"on"`
		} `json:"providers"`
		Rules        map[string]string `json:"rules"`
		WorkerWindow map[string]int    `json:"worker_window"`
		Codex        struct {
			Available bool   `json:"available"`
			Reason    string `json:"reason"`
		} `json:"codex"`
		Hints []string `json:"hints"`
	}
	if err := json.Unmarshal([]byte(statusOut(t, "--json")), &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Providers) != 12 || r.Rules["read-budget"] != "on" || r.WorkerWindow["sonnet"] != 250000 ||
		r.Codex.Available || r.Codex.Reason != "codex not installed" || len(r.Hints) == 0 {
		t.Errorf("status json = %+v", r)
	}
}

func TestAgo(t *testing.T) {
	for d, want := range map[time.Duration]string{
		30 * time.Second:                            "under a minute ago",
		5*time.Minute + 59*time.Second:              "5m ago",
		2 * time.Hour:                               "2h ago",
		2*time.Hour + 5*time.Minute + 9*time.Second: "2h5m ago",
		-time.Second:                                "in the future",
	} {
		if got := ago(d); got != want {
			t.Errorf("ago(%v) = %q, want %q", d, got, want)
		}
	}
}
