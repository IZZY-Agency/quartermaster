package paths

import (
	"path/filepath"
	"testing"
)

func mustDir(t *testing.T, f func() (string, error)) string {
	t.Helper()
	d, err := f()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestPathsXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg/config")
	t.Setenv("XDG_STATE_HOME", "/xdg/state")
	if got := mustDir(t, ConfigDir); got != filepath.Join("/xdg/config", "quartermaster") {
		t.Errorf("ConfigDir() = %q", got)
	}
	if got := mustDir(t, StateDir); got != filepath.Join("/xdg/state", "quartermaster") {
		t.Errorf("StateDir() = %q", got)
	}
}

func TestPathsDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	if got := mustDir(t, ConfigDir); got != filepath.Join(home, ".config", "quartermaster") {
		t.Errorf("ConfigDir() = %q", got)
	}
	if got := mustDir(t, StateDir); got != filepath.Join(home, ".local", "state", "quartermaster") {
		t.Errorf("StateDir() = %q", got)
	}
}

func TestNoHome(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	if d, err := ConfigDir(); err == nil {
		t.Errorf("ConfigDir() = %q, want an error without a home folder", d)
	}
	if d, err := StateDir(); err == nil {
		t.Errorf("StateDir() = %q, want an error without a home folder", d)
	}
}

func TestClaudeDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if got := mustDir(t, ClaudeDir); got != filepath.Join(home, ".claude") {
		t.Errorf("ClaudeDir() = %q", got)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "/cc")
	if got := mustDir(t, ClaudeDir); got != "/cc" {
		t.Errorf("ClaudeDir() = %q", got)
	}
	if got := mustDir(t, InstalledPluginsFile); got != filepath.Join("/cc", "plugins", "installed_plugins.json") {
		t.Errorf("InstalledPluginsFile() = %q", got)
	}
}

func TestClaudeJSONPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if got := mustDir(t, ClaudeJSON); got != filepath.Join(home, ".claude.json") {
		t.Errorf("ClaudeJSON() = %q, want ~/.claude.json", got)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "/cc")
	if got := mustDir(t, ClaudeJSON); got != filepath.Join("/cc", ".claude.json") {
		t.Errorf("ClaudeJSON() = %q, want $CLAUDE_CONFIG_DIR/.claude.json", got)
	}
}
