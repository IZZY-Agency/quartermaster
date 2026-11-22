package spawn

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/IZZY-Agency/quartermaster/internal/config"
	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/paths"
	"github.com/IZZY-Agency/quartermaster/internal/policy"
	"github.com/IZZY-Agency/quartermaster/internal/route"
)

// CoordinatorPlugin is the coordinator plugin's enabledPlugins key; it is off
// in every worker.
const CoordinatorPlugin = "qm-coordinator@quartermaster"

// Worker output caps (design 5.5).
const (
	bashMaxOutput     = "20000"
	fileReadMaxTokens = "12000"
)

// lspPlugins maps file extensions to a language and its official LSP plugin.
// The order breaks ties in the file count.
var lspPlugins = []struct {
	lang, plugin string
	exts         []string
}{
	{"go", "gopls-lsp", []string{".go"}},
	{"python", "pyright-lsp", []string{".py"}},
	{"typescript", "typescript-lsp", []string{".ts", ".tsx"}},
	{"php", "php-lsp", []string{".php"}},
}

// WorkerPluginDir finds the qm-worker plugin: QM_WORKER_PLUGIN when set, else
// next to qm's executable exe (symlinks resolved):
// <exe dir>/../share/quartermaster/plugins/qm-worker (Homebrew, make install),
// then <exe dir>/../plugins/qm-worker (a source build), then
// <exe dir>/share/quartermaster/plugins/qm-worker (an unpacked release
// archive). The error lists the paths tried.
func WorkerPluginDir(exe string) (string, error) {
	if p := os.Getenv("QM_WORKER_PLUGIN"); p != "" {
		if !isDir(p) {
			return "", fmt.Errorf("qm-worker plugin: QM_WORKER_PLUGIN %s is not a folder", p)
		}
		return p, nil
	}
	real, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", fmt.Errorf("qm-worker plugin: %w", err)
	}
	up := filepath.Dir(filepath.Dir(real))
	tried := []string{
		filepath.Join(up, "share", "quartermaster", "plugins", "qm-worker"),
		filepath.Join(up, "plugins", "qm-worker"),
		// an unpacked release archive: qm beside share/
		filepath.Join(filepath.Dir(real), "share", "quartermaster", "plugins", "qm-worker"),
	}
	for _, p := range tried {
		if isDir(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("qm-worker plugin not found; tried %s (or set QM_WORKER_PLUGIN)", strings.Join(tried, ", "))
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// EnabledPlugins reads enabledPlugins from the Claude Code settings a
// session in repo starts with: the user's settings.json, then the
// repository's .claude/settings.json and .claude/settings.local.json, a later
// file overriding an earlier one. It maps a plugin key to whether it is on
// (true, or a list of versions). A missing file adds nothing.
func EnabledPlugins(repo string) (map[string]bool, error) {
	dir, err := paths.ClaudeDir()
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, path := range []string{
		filepath.Join(dir, "settings.json"),
		filepath.Join(repo, ".claude", "settings.json"),
		filepath.Join(repo, ".claude", "settings.local.json"),
	} {
		if err := readEnabled(path, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// readEnabled adds the enabledPlugins of the settings file at path to out.
func readEnabled(path string, out map[string]bool) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("claude settings: %w", err)
	}
	var s struct {
		EnabledPlugins map[string]json.RawMessage `json:"enabledPlugins"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("claude settings %s: %w", path, err)
	}
	for k, v := range s.EnabledPlugins {
		v = bytes.TrimSpace(v)
		out[k] = string(v) == "true" || bytes.HasPrefix(v, []byte("["))
	}
	return nil
}

// syncedPlugins lists the plugins claude.ai account sync brings in, as
// name@marketplace keys, from Claude Code's sync manifests
// (<claude dir>/plugins/synced/*/manifest.json). No manifest gives none.
func syncedPlugins() ([]string, error) {
	dir, err := paths.ClaudeDir()
	if err != nil {
		return nil, err
	}
	files, err := filepath.Glob(filepath.Join(dir, "plugins", "synced", "*", "manifest.json"))
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("claude.ai plugin sync: %w", err)
		}
		var m struct {
			Plugins []struct {
				Name            string `json:"name"`
				MarketplaceName string `json:"marketplaceName"`
			} `json:"plugins"`
		}
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("claude.ai plugin sync %s: %w", f, err)
		}
		for _, pl := range m.Plugins {
			if pl.Name != "" && pl.MarketplaceName != "" {
				seen[pl.Name+"@"+pl.MarketplaceName] = true
			}
		}
	}
	return sortedKeys(seen), nil
}

// OfficialMarketplace is the marketplace of Claude Code's official plugins,
// the LSP plugins among them.
const OfficialMarketplace = "claude-plugins-official"

// LSP is the official LSP plugin for a repository's main language.
type LSP struct {
	Lang   string // go, python, typescript, php; "" when none applies
	Plugin string // the plugin's name, such as gopls-lsp
	// Installed: Claude Code has the plugin installed for the repository,
	// so a worker can turn it on through enabledPlugins.
	Installed bool
}

// Key is the plugin's enabledPlugins key.
func (l LSP) Key() string { return l.Plugin + "@" + OfficialMarketplace }

// InstallCommand is what the user runs inside Claude Code to install it.
func (l LSP) InstallCommand() string { return "/plugin install " + l.Key() }

// RepoLSP picks the LSP plugin by the most common language among the
// repository's tracked files and says whether it is installed. note says why
// no plugin is loaded ("" when one is).
func RepoLSP(repo string) (l LSP, note string) {
	out, err := exec.Command("git", "-C", repo, "ls-files", "-z").Output()
	if err != nil {
		return LSP{}, "no LSP plugin: not a git repository"
	}
	counts := make([]int, len(lspPlugins))
	for _, f := range strings.Split(string(out), "\x00") {
		ext := strings.ToLower(filepath.Ext(f))
		for i, p := range lspPlugins {
			if slices.Contains(p.exts, ext) {
				counts[i]++
			}
		}
	}
	best := -1
	for i, n := range counts {
		if n > 0 && (best < 0 || n > counts[best]) {
			best = i
		}
	}
	if best < 0 {
		return LSP{}, "no LSP plugin: no Go, Python, TypeScript or PHP files"
	}
	l = LSP{Lang: lspPlugins[best].lang, Plugin: lspPlugins[best].plugin}
	installed, err := InstalledPlugins(repo)
	if err != nil {
		return l, fmt.Sprintf("LSP plugin %s not loaded: %v", l.Plugin, err)
	}
	if !installed[l.Key()] {
		return l, fmt.Sprintf("LSP plugin %s not loaded: not installed (inside Claude Code, run %s)", l.Plugin, l.InstallCommand())
	}
	l.Installed = true
	return l, ""
}

// InstalledPlugins reads Claude Code's installed_plugins.json: the plugin
// keys (name@marketplace) installed at user scope, or at a project or local
// scope for repo. Only the keys and scopes are read. A missing file gives
// none.
func InstalledPlugins(repo string) (map[string]bool, error) {
	path, err := paths.InstalledPluginsFile()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("installed plugins: %w", err)
	}
	var f struct {
		Plugins map[string][]struct {
			Scope       string `json:"scope"`
			ProjectPath string `json:"projectPath"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("installed plugins %s: %w", path, err)
	}
	out := map[string]bool{}
	for key, entries := range f.Plugins {
		for _, e := range entries {
			if e.Scope == "user" || (e.ProjectPath != "" && repo != "" && filepath.Clean(e.ProjectPath) == filepath.Clean(repo)) {
				out[key] = true
			}
		}
	}
	return out, nil
}

// autocompact is the compact window for a worker on model: the config's
// worker_window, else the default, else the Sonnet default.
func autocompact(cfg config.Config, model string) int {
	if n := cfg.WorkerWindow[model]; n > 0 {
		return n
	}
	def := config.Defaults().WorkerWindow
	if n := def[model]; n > 0 {
		return n
	}
	return def["sonnet"]
}

// BuildProfile builds the launch profile of a worker for decision d in repo
// (design 5.5): its compact window; the qm-worker plugin (workerDir); the
// repository's official LSP plugin, turned on through enabledPlugins when it
// is installed (QM_LSP is set only then); the plugins in the Claude Code
// settings (enabled, see EnabledPlugins) and the plugins claude.ai sync
// brings in, switched off except the config's worker.plugins and
// project_plugins[repo]; claude.ai sync itself switched off unless a kept
// plugin comes only from it (not enabled locally); the coordinator plugin
// off; the project's MCP config; and perm, the coordinator's permission
// flags. notes say what was left out and why.
func BuildProfile(d route.Decision, repo string, cfg config.Config, enabled map[string]bool, perm []string, workerDir string) (route.Profile, []string) {
	var notes []string
	n := autocompact(cfg, d.Tier.Model)
	p := route.Profile{
		Autocompact: n,
		PluginsOn:   []string{"qm-worker"},
		PluginDirs:  []string{workerDir},
		MCPConfig:   cfg.ProjectMCP[repo],
		Env: map[string]string{
			"QM_ROLE":                "worker",
			"QM_AUTOCOMPACT":         strconv.Itoa(n),
			"BASH_MAX_OUTPUT_LENGTH": bashMaxOutput,
			"CLAUDE_CODE_FILE_READ_MAX_OUTPUT_TOKENS": fileReadMaxTokens,
		},
		Permission: perm,
	}

	keep := append(slices.Clone(cfg.Worker.Plugins), cfg.ProjectPlugins[repo]...)
	l, note := RepoLSP(repo)
	if l.Installed {
		p.Env["QM_LSP"] = l.Lang
		keep = append(keep, l.Key())
	} else {
		notes = append(notes, note)
	}
	kept := func(key string) bool {
		name, _, _ := strings.Cut(key, "@")
		return slices.Contains(keep, key) || slices.Contains(keep, name)
	}
	synced, err := syncedPlugins()
	if err != nil {
		notes = append(notes, "claude.ai plugins not read: "+err.Error())
	}
	all := maps.Clone(enabled)
	if all == nil {
		all = map[string]bool{}
	}
	for _, key := range synced {
		if _, ok := all[key]; !ok {
			all[key] = true
		}
		// The sync stays on only for a kept plugin that comes from it alone:
		// one enabled in the local settings, or the installed LSP plugin,
		// loads without it.
		if kept(key) && !enabled[key] && key != l.Key() && !p.KeepClaudeAiPlugins {
			p.KeepClaudeAiPlugins = true
			notes = append(notes, "claude.ai plugin sync left on: "+key+" comes from it")
		}
	}

	on, off := map[string]bool{}, map[string]bool{CoordinatorPlugin: true}
	for key := range all {
		if key == CoordinatorPlugin {
			continue
		}
		if !kept(key) {
			off[key] = true
			continue
		}
		on[key] = true
	}
	for _, key := range keep {
		switch {
		case key == CoordinatorPlugin:
		case strings.Contains(key, "@"):
			on[key] = true
		case !hasName(all, key):
			notes = append(notes, fmt.Sprintf("worker plugin %s not loaded: not in the Claude Code settings (write it as name@marketplace)", key))
		}
	}
	p.PluginsOn = append(p.PluginsOn, sortedKeys(on)...)
	p.PluginsOff = sortedKeys(off)
	return p, notes
}

// hasName says whether a plugin key in enabled has the plugin name name.
func hasName(enabled map[string]bool, name string) bool {
	for k := range enabled {
		if n, _, _ := strings.Cut(k, "@"); n == name {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// scriptEnv is the order of the profile's variables in the launch script.
var scriptEnv = []string{"QM_AUTOCOMPACT", "QM_LSP", "QM_PROJECT_ID", "QM_WORKSPACE", "BASH_MAX_OUTPUT_LENGTH", "CLAUDE_CODE_FILE_READ_MAX_OUTPUT_TOKENS"}

// spawnerPath is the PATH of the process that runs qm spawn: the
// coordinator's, with the user's login folders. Terminals such as agterm run
// a session's command with a bare PATH, so hooks in the user's settings
// (formatters, proxies) would not find their programs without it.
var spawnerPath = func() string { return os.Getenv("PATH") }

// LaunchScript is the /bin/sh script that starts the worker: PATH is qmDir,
// qm's own folder ("" for none), then the spawner's PATH, then the session's
// own; then cd to cwd, then exec
// claude on tier t with the profile's flags, named name, with the Claude
// session id sessionID ("" lets Claude pick one), the brief file's text as
// the first prompt. The prompt starts with a newline, so a brief that starts
// with "-" is not read as a flag.
func LaunchScript(p route.Profile, t route.Tier, name, sessionID, cwd, briefPath, qmDir string) string {
	w := claudeLine(p, t, name)
	if sessionID != "" {
		w = append(w, "--session-id", sessionID)
	}
	return script(cwd, qmDir, w, briefPath)
}

// ResumeScript is the /bin/sh script that resumes a worker's session
// sessionID (design 5.5): LaunchScript's script with --resume in place of
// --session-id, and no first prompt.
func ResumeScript(p route.Profile, t route.Tier, name, sessionID, cwd, qmDir string) string {
	return script(cwd, qmDir, append(claudeLine(p, t, name), "--resume", sessionID), "")
}

// PlainResumeScript is the /bin/sh script that resumes a hand-started
// worker's session sessionID (adopt design 4.3): the PATH and cd of
// LaunchScript, then exec env with env's variables of scriptEnv (in that
// order), claude with args as given, -n name and --resume sessionID. It sets
// no QM_ROLE: the worker keeps its own settings.
func PlainResumeScript(args []string, env map[string]string, name, sessionID, cwd, qmDir string) string {
	w := []string{"exec", "env"}
	for _, k := range scriptEnv {
		if v, ok := env[k]; ok && v != "" {
			w = append(w, k+"="+v)
		}
	}
	w = append(w, "claude")
	w = append(w, args...)
	w = append(w, "-n", name, "--resume", sessionID)
	return script(cwd, qmDir, w, "")
}

// claudeLine is a worker's command, unquoted: exec env with the worker's
// variables, then claude on tier t with the profile's flags, named name.
func claudeLine(p route.Profile, t route.Tier, name string) []string {
	w := []string{"exec", "env", "QM_ROLE=worker", "QM_SESSION_NAME=" + name}
	for _, k := range scriptEnv {
		if v, ok := p.Env[k]; ok && v != "" {
			w = append(w, k+"="+v)
		}
	}
	w = append(w, "claude")
	w = append(w, p.Permission...)
	model := policy.ModelIDs[t.Model]
	if model == "" {
		model = t.Model
	}
	w = append(w, "--model", model, "--effort", t.Effort, "--autocompact", strconv.Itoa(p.Autocompact), "--strict-mcp-config")
	if p.MCPConfig != "" {
		w = append(w, "--mcp-config", p.MCPConfig)
	}
	w = append(w, "--settings", settingsJSON(p))
	for _, d := range p.PluginDirs {
		w = append(w, "--plugin-dir", d)
	}
	return append(w, "-n", name)
}

// CoordinatorLaunch is what a qm up coordinator is started with.
type CoordinatorLaunch struct {
	Name, SessionID, Cwd, QMDir string
	ProjectID, Workspace        string // the qm up project and its workspace
	// HandoffPath is the file the coordinator writes its handoff note to
	// (QM_HANDOFF), <state>/handoff/<project id>/NAME.md; "" leaves
	// QM_HANDOFF out.
	HandoffPath string
	// PromptPath is the first prompt's file (a fresh coordinator's summary);
	// "" gives none.
	PromptPath string
	Args       []string // the coordinator's own claude arguments
	// Resume resumes SessionID; otherwise a new session takes that id.
	Resume bool
}

// CoordinatorScript is the /bin/sh script that starts or resumes a qm up
// coordinator (design 5.5): the PATH and cd of LaunchScript, then exec claude
// with the project, workspace, session name and handoff note file in its
// environment, the coordinator's own arguments, -n NAME, and --resume ID or
// --session-id ID; then the prompt file's text when there is one. An empty
// session id, or a name that is not a plain file name, is an error.
func CoordinatorScript(c CoordinatorLaunch) (string, error) {
	switch {
	case c.SessionID == "":
		return "", errors.New("coordinator script: no session id")
	case c.Name == "" || c.Name == "." || c.Name == ".." || filepath.Base(c.Name) != c.Name:
		return "", fmt.Errorf("coordinator script: name %q is not a plain file name", c.Name)
	}
	w := []string{"exec", "env", "QM_PROJECT_ID=" + c.ProjectID, "QM_WORKSPACE=" + c.Workspace, "QM_SESSION_NAME=" + c.Name}
	if c.HandoffPath != "" {
		w = append(w, "QM_HANDOFF="+c.HandoffPath)
	}
	w = append(w, "claude")
	w = append(w, c.Args...)
	w = append(w, "-n", c.Name)
	if c.Resume {
		w = append(w, "--resume", c.SessionID)
	} else {
		w = append(w, "--session-id", c.SessionID)
	}
	return script(c.Cwd, c.QMDir, w, c.PromptPath), nil
}

// script is a /bin/sh launch script: PATH is qmDir ("" for none), then the
// spawner's PATH, then the session's own; then cd to cwd, then the command
// words w, each quoted, followed by the text of promptPath ("" for none) as
// one argument that starts with a newline.
func script(cwd, qmDir string, w []string, promptPath string) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	var dirs []string
	if qmDir != "" {
		dirs = append(dirs, qmDir)
	}
	if sp := spawnerPath(); sp != "" {
		dirs = append(dirs, sp)
	}
	if len(dirs) > 0 {
		fmt.Fprintf(&b, "PATH=%s:\"$PATH\"\nexport PATH\n", launch.Quote(strings.Join(dirs, ":")))
	}
	fmt.Fprintf(&b, "cd %s || exit 1\n", launch.Quote(cwd))
	q := make([]string, len(w))
	for i, s := range w {
		q[i] = launch.Quote(s)
	}
	b.WriteString(strings.Join(q, " "))
	if promptPath != "" {
		fmt.Fprintf(&b, " \"$(printf '\\n'; cat %s)\"", launch.Quote(promptPath))
	}
	b.WriteString("\n")
	return b.String()
}

// settingsJSON is the --settings value: enabledPlugins with the profile's
// marketplace plugins on and the rest of the user's plugins off, and
// claude.ai plugin sync off unless the profile keeps it.
func settingsJSON(p route.Profile) string {
	enabled := map[string]bool{}
	for _, k := range p.PluginsOn {
		if strings.Contains(k, "@") {
			enabled[k] = true
		}
	}
	for _, k := range p.PluginsOff {
		enabled[k] = false
	}
	s := map[string]any{"enabledPlugins": enabled}
	if !p.KeepClaudeAiPlugins {
		s["syncClaudeAiPlugins"] = false
	}
	b, _ := json.Marshal(s) // maps of bools always encode
	return string(b)
}
