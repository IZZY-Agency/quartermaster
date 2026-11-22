// Package paths locates quartermaster's config and state folders.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

const app = "quartermaster"

// ConfigDir is $XDG_CONFIG_HOME/quartermaster, else ~/.config/quartermaster.
// It fails only when XDG_CONFIG_HOME is unset and there is no home folder.
func ConfigDir() (string, error) {
	return dir("XDG_CONFIG_HOME", ".config")
}

// StateDir is $XDG_STATE_HOME/quartermaster, else ~/.local/state/quartermaster.
// It fails only when XDG_STATE_HOME is unset and there is no home folder.
func StateDir() (string, error) {
	return dir("XDG_STATE_HOME", filepath.Join(".local", "state"))
}

// ClaudeDir is Claude Code's folder: $CLAUDE_CONFIG_DIR, else ~/.claude.
func ClaudeDir() (string, error) {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("CLAUDE_CONFIG_DIR is unset and there is no home folder: %w", err)
	}
	return filepath.Join(home, ".claude"), nil
}

// ClaudeJSON is Claude Code's settings and state file, where it records
// folder trust: $CLAUDE_CONFIG_DIR/.claude.json, else ~/.claude.json.
func ClaudeJSON() (string, error) {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, ".claude.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("CLAUDE_CONFIG_DIR is unset and there is no home folder: %w", err)
	}
	return filepath.Join(home, ".claude.json"), nil
}

// InstalledPluginsFile is Claude Code's record of the plugins installed,
// with their scopes.
func InstalledPluginsFile() (string, error) {
	d, err := ClaudeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "plugins", "installed_plugins.json"), nil
}

func dir(env, fallback string) (string, error) {
	if base := os.Getenv(env); base != "" {
		return filepath.Join(base, app), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("%s is unset and there is no home folder: %w", env, err)
	}
	return filepath.Join(home, fallback, app), nil
}
