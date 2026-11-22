// Package config loads and saves the user config,
// $XDG_CONFIG_HOME/quartermaster/config.json.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/paths"
)

// KeyEnv is the environment variable that holds the TypeSafe API key.
const KeyEnv = "TYPESAFE_API_KEY"

// Pseudonym replaces a client name matching Pattern with Replace.
type Pseudonym struct {
	Pattern string `json:"pattern"`
	Replace string `json:"replace"`
}

// Config is the user config.
type Config struct {
	Privacy struct {
		Clients    []Pseudonym `json:"clients"`
		LocalModel string      `json:"local_model"`
	} `json:"privacy"`
	Worker struct {
		Plugins []string `json:"plugins"`
	} `json:"worker"`
	// Rules maps a rule name to on|note|off; a rule not listed is on.
	Rules          map[string]string   `json:"rules"`
	ProjectPlugins map[string][]string `json:"project_plugins"`
	ProjectMCP     map[string]string   `json:"project_mcp"`
	ReadBudget     int                 `json:"read_budget"`
	WorkerWindow   map[string]int      `json:"worker_window"`
	TypeSafeKey    string              `json:"typesafe_api_key,omitempty"`
	Launcher       string              `json:"launcher"`
	// Report configures qm report. WorkerMarkers are extra regexes (case
	// insensitive, multiline) that mark a session whose first prompt matches as
	// a worker, next to the built-in worker-brief markers.
	Report struct {
		WorkerMarkers []string `json:"worker_markers"`
	} `json:"report"`
	Notify []string `json:"notify"`
	// Up configures qm up. ClaudeArgs are a new project's coordinator claude
	// arguments when qm up is given none after "--" (its permission flag,
	// say).
	Up struct {
		ClaudeArgs []string `json:"claude_args"`
	} `json:"up"`
	// Watch configures qm watch. SilentMinutes is how long a worker's
	// transcript goes unwritten before it counts as silent; nil (left out)
	// means the default, 10.
	Watch struct {
		SilentMinutes *int64 `json:"silent_minutes,omitempty"`
	} `json:"watch"`
}

// Defaults is the config used when there is no file; a file overrides only
// the fields it sets.
func Defaults() Config {
	var c Config
	c.Privacy.Clients = []Pseudonym{}
	c.Privacy.LocalModel = "auto"
	c.Worker.Plugins = []string{}
	c.Rules = map[string]string{}
	c.ProjectPlugins = map[string][]string{}
	c.ProjectMCP = map[string]string{}
	c.ReadBudget = 8
	c.WorkerWindow = map[string]int{"sonnet": 250000, "opus": 400000, "fable": 400000}
	c.Launcher = "auto"
	c.Report.WorkerMarkers = []string{}
	c.Notify = []string{}
	c.Up.ClaudeArgs = []string{}
	return c
}

// Path is the config file's path.
func Path() (string, error) {
	d, err := paths.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "config.json"), nil
}

// Load reads the config file over the defaults; a missing file gives the
// defaults.
func Load() (Config, error) {
	c := Defaults()
	path, err := Path()
	if err != nil {
		return c, fmt.Errorf("load config: %w", err)
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return Defaults(), fmt.Errorf("parse config %s: %w", path, err)
	}
	fillNil(&c)
	return c, nil
}

// fillNil puts the defaults back where the file wrote null: a null map or
// list must not wipe a default (worker_window, say) or leave a nil to write.
func fillNil(c *Config) {
	d := Defaults()
	if c.Privacy.Clients == nil {
		c.Privacy.Clients = d.Privacy.Clients
	}
	if c.Worker.Plugins == nil {
		c.Worker.Plugins = d.Worker.Plugins
	}
	if c.Rules == nil {
		c.Rules = d.Rules
	}
	if c.ProjectPlugins == nil {
		c.ProjectPlugins = d.ProjectPlugins
	}
	if c.ProjectMCP == nil {
		c.ProjectMCP = d.ProjectMCP
	}
	if c.WorkerWindow == nil {
		c.WorkerWindow = d.WorkerWindow
	}
	if c.Report.WorkerMarkers == nil {
		c.Report.WorkerMarkers = d.Report.WorkerMarkers
	}
	if c.Notify == nil {
		c.Notify = d.Notify
	}
	if c.Up.ClaudeArgs == nil {
		c.Up.ClaudeArgs = d.Up.ClaudeArgs
	}
}

// Save writes the config atomically: a 0600 file in a 0700 folder.
func Save(c Config) error {
	path, err := Path()
	if err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	if err := writeAtomic(path, append(b, '\n')); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	return nil
}

// writeAtomic writes data to a 0600 temporary file next to path, then renames
// it over path, so a reader sees the old file or the new one, never half.
func writeAtomic(path string, data []byte) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.json")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Chmod(0o600); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Key is the TypeSafe API key: the environment first, then the file.
func (c Config) Key() string {
	if k := os.Getenv(KeyEnv); k != "" {
		return k
	}
	return c.TypeSafeKey
}

// SilentThreshold is qm watch's silence threshold: watch.silent_minutes,
// 10 minutes when unset; an error for a value below 1.
func (c Config) SilentThreshold() (time.Duration, error) {
	m := c.Watch.SilentMinutes
	switch {
	case m == nil:
		return 10 * time.Minute, nil
	case *m < 1 || *m > int64(math.MaxInt64/int64(time.Minute)):
		return 0, fmt.Errorf("watch.silent_minutes is %d; it must be a whole number from 1 to %d", *m, int64(math.MaxInt64/int64(time.Minute)))
	}
	return time.Duration(*m) * time.Minute, nil
}

// Rule is a rule's switch: on, note or off. A rule not in the file is on.
func (c Config) Rule(name string) string {
	if v, ok := c.Rules[name]; ok && v != "" {
		return v
	}
	return "on"
}
