package providers

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IZZY-Agency/quartermaster/internal/config"
)

// hermetic points every folder Detect reads at a temporary one, with no
// helper, no gitleaks and no coordinator process, and returns the folder.
func hermetic(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "claude"))
	t.Setenv("QM_PRIVCHECK", "")
	t.Setenv("QM_GITLEAKS", "")
	t.Setenv("QM_WORKER_PLUGIN", "")
	oldPS := ps
	ps = func(int) (string, int, error) { return "", 0, errors.New("no process") }
	t.Cleanup(func() { ps = oldPS })
	return home
}

// envOf is an env func over m; a name not in m is unset.
func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// lookIn finds only the named commands, at /fake/<name>.
func lookIn(names ...string) func(string) (string, error) {
	return func(n string) (string, error) {
		for _, x := range names {
			if x == n {
				return "/fake/" + n, nil
			}
		}
		return "", exec.ErrNotFound
	}
}

func byName(t *testing.T, list []Status, name string) Status {
	t.Helper()
	for _, s := range list {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no status %q in %v", name, list)
	return Status{}
}

// fakeHelper writes a qm-privcheck stand-in whose status prints out and exits
// with code, and points QM_PRIVCHECK at it.
func fakeHelper(t *testing.T, out string, code int) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "qm-privcheck")
	script := "#!/bin/sh\n[ \"$1\" = status ] || exit 2\necho '" + out + "'\nexit " + string(rune('0'+code)) + "\n"
	if err := os.WriteFile(p, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QM_PRIVCHECK", p)
}

func TestDetectListsEveryProvider(t *testing.T) {
	hermetic(t)
	got := Detect(context.Background(), config.Defaults(), envOf(nil), lookIn())
	var names []string
	for _, s := range got {
		names = append(names, s.Name)
	}
	want := "claude typesafe codex gitleaks helper local_model launcher permission statusline notify lsp worker_plugin"
	if strings.Join(names, " ") != want {
		t.Fatalf("names = %v, want %s", names, want)
	}
}

func TestDetectNothingInstalled(t *testing.T) {
	hermetic(t)
	got := Detect(context.Background(), config.Defaults(), envOf(nil), lookIn())
	want := map[string]string{
		"typesafe":    "Jev off: no TypeSafe key; ask gate and feedback off",
		"codex":       "Codex off: not installed; reviews go to Claude",
		"gitleaks":    "gitleaks off",
		"local_model": "local model off: redaction and gitleaks only",
		"launcher":    "launcher: none found",
		"statusline":  "Claude limits unknown",
		"notify":      "notify off",
		"lsp":         "LSP off",
	}
	for name, why := range want {
		s := byName(t, got, name)
		if s.On || !strings.HasPrefix(s.Why, why) {
			t.Errorf("%s = %+v, want off with %q", name, s, why)
		}
	}
	for _, name := range []string{"claude", "helper", "permission", "worker_plugin"} {
		if s := byName(t, got, name); s.On || s.Why == "" {
			t.Errorf("%s = %+v, want off with a reason", name, s)
		}
	}
}

func TestDetectKeyFromEnvOrConfig(t *testing.T) {
	hermetic(t)
	cfg := config.Defaults()
	s := byName(t, Detect(context.Background(), cfg, envOf(map[string]string{config.KeyEnv: "k"}), lookIn()), "typesafe")
	if !s.On || !strings.Contains(s.Why, config.KeyEnv) {
		t.Errorf("env key: %+v", s)
	}
	cfg.TypeSafeKey = "k"
	s = byName(t, Detect(context.Background(), cfg, envOf(nil), lookIn()), "typesafe")
	if !s.On || !strings.Contains(s.Why, "config") {
		t.Errorf("config key: %+v", s)
	}
}

func TestDetectLocalModel(t *testing.T) {
	hermetic(t)
	fakeHelper(t, "available", 0)
	cfg := config.Defaults()
	got := Detect(context.Background(), cfg, envOf(nil), lookIn())
	if s := byName(t, got, "helper"); !s.On {
		t.Errorf("helper = %+v, want on", s)
	}
	if s := byName(t, got, "local_model"); !s.On {
		t.Errorf("local_model = %+v, want on", s)
	}

	cfg.Privacy.LocalModel = "off"
	if s := byName(t, Detect(context.Background(), cfg, envOf(nil), lookIn()), "local_model"); s.On || !strings.Contains(s.Why, "privacy.local_model is off") {
		t.Errorf("config off: %+v", s)
	}

	fakeHelper(t, "unavailable: Apple Intelligence is off", 3)
	cfg.Privacy.LocalModel = "required"
	s := byName(t, Detect(context.Background(), cfg, envOf(nil), lookIn()), "local_model")
	if s.On || !strings.Contains(s.Why, "Apple Intelligence is off") || !strings.Contains(s.Why, "stays on this machine") {
		t.Errorf("required, unavailable: %+v", s)
	}
}

func TestDetectLauncher(t *testing.T) {
	hermetic(t)
	cfg := config.Defaults()
	env := envOf(map[string]string{"AGTERM_SESSION_ID": "1"})
	if s := byName(t, Detect(context.Background(), cfg, env, lookIn("agtermctl")), "launcher"); !s.On || !strings.Contains(s.Why, "agterm") {
		t.Errorf("agterm: %+v", s)
	}
	if s := byName(t, Detect(context.Background(), cfg, env, lookIn()), "launcher"); s.On || !strings.Contains(s.Why, "agtermctl not found") {
		t.Errorf("agterm without agtermctl: %+v", s)
	}
	env = envOf(map[string]string{"TMUX": "/tmp/tmux-1/default,1,0"})
	if s := byName(t, Detect(context.Background(), cfg, env, lookIn("tmux")), "launcher"); !s.On || !strings.Contains(s.Why, "tmux") {
		t.Errorf("tmux: %+v", s)
	}
}

func TestDetectPermissionFromParent(t *testing.T) {
	hermetic(t)
	ps = func(pid int) (string, int, error) {
		if pid == 42 {
			return "claude --dangerously-skip-permissions", 1, nil
		}
		return "", 0, errors.New("no process")
	}
	s := byName(t, Detect(context.Background(), config.Defaults(), envOf(map[string]string{"CLAUDE_PID": "42"}), lookIn()), "permission")
	if !s.On || !strings.Contains(s.Why, "--dangerously-skip-permissions") {
		t.Errorf("permission = %+v", s)
	}
}

func TestDetectStatusLineAndLSPAndNotify(t *testing.T) {
	home := hermetic(t)
	claude := filepath.Join(home, "claude")
	installed := `{"version":2,"plugins":{"gopls-lsp@claude-plugins-official":[{"scope":"user"}],"pyright-lsp@claude-plugins-official":[{"scope":"user"}]}}`
	if err := os.MkdirAll(filepath.Join(claude, "plugins"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claude, "plugins", "installed_plugins.json"), []byte(installed), 0o600); err != nil {
		t.Fatal(err)
	}
	getwd = func() (string, error) { return home, nil } // not a repository
	t.Cleanup(func() { getwd = os.Getwd })
	settings := `{"statusLine": {"type": "command", "command": "qm statusline -- 'my-line'"}}`
	if err := os.WriteFile(filepath.Join(claude, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Notify = []string{"tg", "send"}
	got := Detect(context.Background(), cfg, envOf(map[string]string{"CLAUDE_CONFIG_DIR": claude}), lookIn("tg"))
	if s := byName(t, got, "statusline"); !s.On {
		t.Errorf("statusline = %+v", s)
	}
	if s := byName(t, got, "lsp"); !s.On || s.Why != "LSP plugins installed: gopls-lsp, pyright-lsp" {
		t.Errorf("lsp = %+v", s)
	}
	if s := byName(t, got, "notify"); !s.On || !strings.Contains(s.Why, "tg") {
		t.Errorf("notify = %+v", s)
	}
}

func TestWrapped(t *testing.T) {
	for cmd, want := range map[string]bool{
		"qm statusline":                     true,
		"qm statusline -- 'x'":              true,
		"/usr/local/bin/qm statusline -- x": true,
		"'/a b/qm' statusline -- 'x'":       true,
		"'/a b/qmx' statusline":             false,
		"qm statuslinex":                    false,
		"my-line":                           false,
		"echo qm statusline":                false,
	} {
		if got := Wrapped(cmd); got != want {
			t.Errorf("Wrapped(%q) = %v, want %v", cmd, got, want)
		}
	}
}

// TestDetectLSPForRepository: in a Go repository the row is about gopls-lsp:
// off with the install command when it is not installed, on when it is.
func TestDetectLSPForRepository(t *testing.T) {
	home := hermetic(t)
	repo := filepath.Join(home, "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "main.go"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	getwd = func() (string, error) { return repo, nil }
	t.Cleanup(func() { getwd = os.Getwd })
	s := detectLSP()
	if s.On || s.Fix != "/plugin install gopls-lsp@claude-plugins-official" || !strings.Contains(s.Why, "gopls-lsp not installed") {
		t.Errorf("not installed: %+v", s)
	}
	claude := os.Getenv("CLAUDE_CONFIG_DIR")
	if err := os.MkdirAll(filepath.Join(claude, "plugins"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claude, "plugins", "installed_plugins.json"),
		[]byte(`{"version":2,"plugins":{"gopls-lsp@claude-plugins-official":[{"scope":"user"}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if s := detectLSP(); !s.On || s.Fix != "" || !strings.Contains(s.Why, "gopls-lsp installed") {
		t.Errorf("installed: %+v", s)
	}
}
