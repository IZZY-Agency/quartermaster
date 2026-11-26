// Package config loads and saves the user config,
// $XDG_CONFIG_HOME/quartermaster/config.json.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/paths"
	"github.com/IZZY-Agency/quartermaster/internal/policy"
)

// KeyEnv is the environment variable that holds the TypeSafe API key.
const KeyEnv = "TYPESAFE_API_KEY"

// Pseudonym replaces a client name matching Pattern with Replace.
type Pseudonym struct {
	Pattern string `json:"pattern"`
	Replace string `json:"replace"`
}

// expr is the expression a pseudonym's pattern is matched with: the pattern
// as a regular expression, without case, or as plain text when it does not
// compile, so a typo never leaves the name in place.
func (p Pseudonym) expr() string {
	if _, err := regexp.Compile("(?i)" + p.Pattern); err != nil {
		return "(?i)" + regexp.QuoteMeta(p.Pattern)
	}
	return "(?i)" + p.Pattern
}

// Regexp is the matcher for the pseudonym's pattern (see expr). It prefers
// the longest match at each place, so the longest spelling wins and a
// pattern that can also match empty text still matches the name.
func (p Pseudonym) Regexp() *regexp.Regexp {
	re := regexp.MustCompile(p.expr())
	re.Longest()
	return re
}

// Check reports a pseudonym that cannot work: an empty replacement, which
// would delete the name, or a pattern that can match empty text. Its message
// never repeats the pattern, which holds a client name.
func (p Pseudonym) Check() error {
	if p.Replace == "" {
		return errors.New("replace is empty; set the pseudonym the client name becomes")
	}
	re, err := syntax.Parse(p.expr(), syntax.Perl)
	if err != nil {
		return errors.New("pattern does not parse")
	}
	if matchesEmpty(re) {
		return errors.New("pattern matches empty text; it must match at least one character")
	}
	return nil
}

// matchesEmpty reports whether re can match empty text somewhere: an
// assertion such as ^ or \b counts as able to hold.
func matchesEmpty(re *syntax.Regexp) bool {
	switch re.Op {
	case syntax.OpEmptyMatch, syntax.OpBeginLine, syntax.OpEndLine, syntax.OpBeginText,
		syntax.OpEndText, syntax.OpWordBoundary, syntax.OpNoWordBoundary, syntax.OpStar, syntax.OpQuest:
		return true
	case syntax.OpLiteral:
		return len(re.Rune) == 0
	case syntax.OpCapture, syntax.OpPlus:
		return matchesEmpty(re.Sub[0])
	case syntax.OpRepeat:
		return re.Min == 0 || matchesEmpty(re.Sub[0])
	case syntax.OpConcat:
		return !slices.ContainsFunc(re.Sub, func(s *syntax.Regexp) bool { return !matchesEmpty(s) })
	case syntax.OpAlternate:
		return slices.ContainsFunc(re.Sub, matchesEmpty)
	}
	return false
}

// Config is the user config.
type Config struct {
	Privacy struct {
		Clients    []Pseudonym `json:"clients"`
		LocalModel string      `json:"local_model"`
	} `json:"privacy"`
	// Worker configures workers. HandoffPercent is the share of its compact
	// window at which a worker is told to hand off; nil (left out) means the
	// default, 85.
	Worker struct {
		Plugins        []string `json:"plugins"`
		HandoffPercent *int     `json:"handoff_percent,omitempty"`
		// NamePrefix is what qm spawn worker puts in front of a worker's name:
		// "auto" (nil, left out, means the same) is the project's workspace name,
		// "" is nothing; project_name_prefix overrides it per project. Read it
		// with NamePrefixMode.
		NamePrefix *string `json:"name_prefix,omitempty"`
	} `json:"worker"`
	// Rules maps a rule name to on|note|off; a rule not listed is on.
	Rules          map[string]string   `json:"rules"`
	ProjectPlugins map[string][]string `json:"project_plugins"`
	ProjectMCP     map[string]string   `json:"project_mcp"`
	// ProjectNamePrefix maps a repository's folder to the prefix of its
	// workers' names ("" for none), over worker.name_prefix.
	ProjectNamePrefix map[string]string `json:"project_name_prefix"`
	ReadBudget        int               `json:"read_budget"`
	WorkerWindow      map[string]int    `json:"worker_window"`
	TypeSafeKey       string            `json:"typesafe_api_key,omitempty"`
	Launcher          string            `json:"launcher"`
	// Report configures qm report. WorkerMarkers are extra regexes (case
	// insensitive, multiline) that mark a session whose first prompt matches as
	// a worker, next to the built-in worker-brief markers.
	Report struct {
		WorkerMarkers []string `json:"worker_markers"`
	} `json:"report"`
	Notify []string `json:"notify"`
	// Up configures qm up. ClaudeArgs are a new project's coordinator claude
	// arguments when qm up is given none after "--" (its permission flag,
	// say). CoordinatorTab is where qm up starts a coordinator: "new" (a new
	// tab, the default) or "here" (the tab qm up runs in); see CoordinatorTab.
	// CoordinatorModel and CoordinatorFallback are the model, and the model to
	// fall back to, of a coordinator that starts from scratch; nil (left out)
	// means the default; see the methods of those names.
	Up struct {
		ClaudeArgs          []string `json:"claude_args"`
		CoordinatorTab      string   `json:"coordinator_tab"`
		CoordinatorModel    *string  `json:"coordinator_model,omitempty"`
		CoordinatorFallback *string  `json:"coordinator_fallback,omitempty"`
	} `json:"up"`
	// Watch configures qm watch. SilentMinutes is how long a worker's
	// transcript goes unwritten before it counts as silent; nil (left out)
	// means the default, 10.
	Watch struct {
		SilentMinutes *int64 `json:"silent_minutes,omitempty"`
	} `json:"watch"`
	// Codex configures the use of Codex. UsageRead is whether qm asks it for
	// its usage (codex app-server); nil (left out) means yes.
	Codex struct {
		UsageRead *bool `json:"usage_read,omitempty"`
	} `json:"codex"`
	// Tint holds the sidebar colours of agterm tabs, by role (coordinator,
	// worker) and by what a worker needs (stopped, silent, gone). A key left
	// out (nil) means the default; an empty string means no colour. Read it
	// with Tints.
	Tint struct {
		Coordinator *string `json:"coordinator,omitempty"`
		Worker      *string `json:"worker,omitempty"`
		Stopped     *string `json:"stopped,omitempty"`
		Silent      *string `json:"silent,omitempty"`
		Gone        *string `json:"gone,omitempty"`
	} `json:"tint"`
}

// CodexUsageRead is codex.usage_read: on unless it is false.
func (c Config) CodexUsageRead() bool { return c.Codex.UsageRead == nil || *c.Codex.UsageRead }

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
	c.ProjectNamePrefix = map[string]string{}
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
// defaults. A file that does not parse gives the defaults with the error; one
// that parses but holds an invalid entry is returned as read with the error,
// so a caller that runs on after the error (the hooks) still redacts every
// client name and keeps the local-model setting.
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
	var bad []error
	for i, p := range c.Privacy.Clients {
		if err := p.Check(); err != nil {
			bad = append(bad, fmt.Errorf("privacy.clients[%d]: %w", i, err))
		}
	}
	if len(bad) > 0 {
		return c, fmt.Errorf("config %s: %w", path, errors.Join(bad...))
	}
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
	if c.ProjectNamePrefix == nil {
		c.ProjectNamePrefix = d.ProjectNamePrefix
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

// NamePrefixMode is worker.name_prefix: "auto" when unset, "" for no prefix.
// Any other value is the caller's to refuse, naming the key.
func (c Config) NamePrefixMode() string {
	if c.Worker.NamePrefix == nil {
		return "auto"
	}
	return *c.Worker.NamePrefix
}

// DefaultHandoffPercent is worker.handoff_percent when it is unset.
const DefaultHandoffPercent = 85

// HandoffShare is the share of a worker's compact window at which the
// context-limit rule tells it to hand off: worker.handoff_percent as a
// fraction, 0.85 when unset. A value outside 1 to 99 is an error, returned
// with the default share, so the rule goes on with it.
func (c Config) HandoffShare() (float64, error) {
	p := c.Worker.HandoffPercent
	switch {
	case p == nil:
		return DefaultHandoffPercent / 100.0, nil
	case *p < 1 || *p > 99:
		return DefaultHandoffPercent / 100.0, fmt.Errorf("worker.handoff_percent is %d; it must be a whole number from 1 to 99", *p)
	}
	return float64(*p) / 100, nil
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

// CoordinatorTab is up.coordinator_tab: "here" or "new"; unset or empty is
// "new". Any other value is an error.
func (c Config) CoordinatorTab() (string, error) {
	switch c.Up.CoordinatorTab {
	case "", "new":
		return "new", nil
	case "here":
		return "here", nil
	}
	return "", fmt.Errorf("up.coordinator_tab is %q; it must be %q or %q", c.Up.CoordinatorTab, "new", "here")
}

// CoordinatorModel is the model a coordinator started from scratch gets:
// up.coordinator_model, "fable" when unset, "" for none. A value that is not
// a name routing uses is an error, returned with the default, so qm up goes
// on with it.
func (c Config) CoordinatorModel() (string, error) {
	return modelName("up.coordinator_model", c.Up.CoordinatorModel, "fable")
}

// CoordinatorFallback is the model that coordinator falls back to:
// up.coordinator_fallback, "opus" when unset, read as CoordinatorModel is.
func (c Config) CoordinatorFallback() (string, error) {
	return modelName("up.coordinator_fallback", c.Up.CoordinatorFallback, "opus")
}

func modelName(key string, set *string, def string) (string, error) {
	if set == nil {
		return def, nil
	}
	value := *set
	if _, ok := policy.ModelIDs[value]; ok || value == "" {
		return value, nil
	}
	return def, fmt.Errorf("%s is %q; it must be one of %s, or \"\"; it is read as the default, %q", key, value,
		strings.Join(slices.Sorted(maps.Keys(policy.ModelIDs)), ", "), def)
}

// Tints are the colours qm gives agterm tabs: a colour name agterm accepts, or
// "#rrggbb", in lower case; "" is no colour. Where the colour shows (icon,
// name) is not configurable: see launch.CoordinatorTint, launch.WorkerTint
// and launch.AttentionTint.
type Tints struct {
	Coordinator, Worker, Stopped, Silent, Gone string
}

// DefaultTints is the palette used when tint.* is unset: the coordinator
// purple, workers teal; a worker that stopped orange, one that went silent
// yellow, one that is gone red.
var DefaultTints = Tints{Coordinator: "purple", Worker: "teal", Stopped: "orange", Silent: "yellow", Gone: "red"}

// tintNames are the colour names agtermctl session tint accepts.
var tintNames = []string{"red", "orange", "yellow", "green", "blue", "purple", "gray", "teal", "pink", "brown", "indigo", "mint"}

var tintHex = regexp.MustCompile(`^#[0-9a-f]{6}$`)

// NormTint checks a colour and returns it in lower case without spaces:
// "" (no colour), a name agterm accepts, or #rrggbb. agterm folds case and
// reports a colour back in lower case, so the lower-case form is the one qm
// compares and remembers.
func NormTint(color string) (string, error) {
	c := strings.ToLower(strings.TrimSpace(color))
	if c == "" || tintHex.MatchString(c) || slices.Contains(tintNames, c) {
		return c, nil
	}
	return "", fmt.Errorf("%q is not a colour: use %s, a #rrggbb colour, or %q for none", color, strings.Join(tintNames, ", "), "")
}

// Tints is the configured palette. A colour that is not valid falls back to
// its default and is named in the error, which joins one error per bad key,
// so the launch goes on with the rest; a key left out is its default and ""
// is no colour.
func (c Config) Tints() (Tints, error) {
	out := DefaultTints
	var bad []error
	for _, k := range []struct {
		key string
		set *string
		dst *string
	}{
		{"tint.coordinator", c.Tint.Coordinator, &out.Coordinator},
		{"tint.worker", c.Tint.Worker, &out.Worker},
		{"tint.stopped", c.Tint.Stopped, &out.Stopped},
		{"tint.silent", c.Tint.Silent, &out.Silent},
		{"tint.gone", c.Tint.Gone, &out.Gone},
	} {
		if k.set == nil {
			continue
		}
		v, err := NormTint(*k.set)
		if err != nil {
			bad = append(bad, fmt.Errorf("%s: %w", k.key, err))
			continue
		}
		*k.dst = v
	}
	return out, errors.Join(bad...)
}

// Rule is a rule's switch: on, note or off. A rule not in the file is on.
func (c Config) Rule(name string) string {
	if v, ok := c.Rules[name]; ok && v != "" {
		return v
	}
	return "on"
}
