// Package setup is qm init and qm init --undo (design 5.11): it writes the
// config, wraps the Claude Code status line, and records what it changed so
// the undo can put it back.
package setup

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/config"
	"github.com/IZZY-Agency/quartermaster/internal/paths"
	"github.com/IZZY-Agency/quartermaster/internal/providers"
)

// Streams are the user's terminal: answers come from In, everything init
// says goes to Out.
type Streams struct {
	In  io.Reader
	Out io.Writer
}

// Options: Yes answers yes to every question (the TypeSafe key prompt is
// skipped), except, in the undo, whether to drop an edit made since init;
// SettingsPath is Claude Code's user settings file ("" is settings.json in
// CLAUDE_CONFIG_DIR, else ~/.claude); Marketplace is what the printed
// "/plugin marketplace add" names: a local checkout's folder or owner/repo
// ("" finds a local checkout, else the public repository).
type Options struct {
	Yes          bool
	SettingsPath string
	Marketplace  string
}

// Record is what init changed, kept in <config>/init.json for the undo.
type Record struct {
	// Created lists the files init created (the config, the settings file).
	Created []string `json:"created"`
	// SettingsPath is the Claude Code settings file init worked on.
	SettingsPath string `json:"settings_path"`
	// ConfigSHA256 is the hash of the config init last wrote, when init
	// created it, so the undo can tell an edit made since.
	ConfigSHA256 string `json:"config_sha256,omitempty"`
	// SettingsBackup is the copy of settings.json made before the status line
	// was changed; "" when the file did not exist.
	SettingsBackup string `json:"settings_backup"`
	// StatusLineBefore is the status-line command before init (nil: none);
	// StatusLineAfter the one init wrote ("" when init did not change it).
	StatusLineBefore *string   `json:"statusline_before"`
	StatusLineAfter  string    `json:"statusline_after"`
	Time             time.Time `json:"time"`
	// HookLine is the line init added to agterm's hooks file HooksPath (""
	// when it added none); HookBefore: the hash of the file before, when its
	// last line had no line break and init put one; HookCreated: init
	// created the file.
	HooksPath   string `json:"hooks_path,omitempty"`
	HookLine    string `json:"hook_line,omitempty"`
	HookBefore  string `json:"hook_before,omitempty"`
	HookCreated bool   `json:"hook_created,omitempty"`
}

// superpowers is the plugin init offers to keep on in workers.
const superpowers = "superpowers@claude-plugins-official"

// Plugin commands printed for the user to run inside Claude Code.
const (
	publicMarketplace = "IZZY-Agency/quartermaster"
	pluginInstall     = "/plugin install qm-coordinator@quartermaster"
	pluginUninstall   = "/plugin uninstall qm-coordinator@quartermaster"
)

// Tests replace these.
var (
	executable = invokedExecutable
)

// invokedExecutable is qm's path as the user invoked it (os.Args[0], looked
// up on PATH when it has no slash) when that names the running binary, else
// os.Executable. On Linux os.Executable reads /proc/self/exe, which resolves
// symlinks: a Linuxbrew install would get the versioned Cellar path, gone
// after an upgrade, where the invoked path is the stable link.
func invokedExecutable() (string, error) {
	return invokedPath(os.Args[0], exec.LookPath, os.Executable)
}

func invokedPath(arg0 string, look func(string) (string, error), exe func() (string, error)) (string, error) {
	self, err := exe()
	if err != nil {
		return "", err
	}
	p := arg0
	if !strings.Contains(p, "/") {
		if p, err = look(p); err != nil {
			return self, nil
		}
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return self, nil
	}
	a, err1 := filepath.EvalSymlinks(abs)
	b, err2 := filepath.EvalSymlinks(self)
	if err1 != nil || err2 != nil || a != b {
		return self, nil
	}
	return abs, nil
}

// Init is qm init: it writes or completes the config, reports the providers,
// asks for the TypeSafe key, offers to wrap the status line, records what it
// changed and prints the /plugin commands.
func Init(ctx context.Context, s Streams, o Options) error {
	settingsPath, err := settingsFile(o)
	if err != nil {
		return err
	}
	cfgPath, err := config.Path()
	if err != nil {
		return err
	}
	in := bufio.NewReader(s.In)
	p := prompter{in: in, out: s.Out, yes: o.Yes, term: terminalOf(s.In)}
	rec, err := readRecord()
	if err != nil {
		return err
	}
	rec.SettingsPath = settingsPath

	cfgMap, existed, err := readConfigMap(cfgPath)
	if err != nil {
		return err
	}
	changed := mergeMissing(cfgMap, defaultsMap())
	cfg, err := asConfig(cfgMap)
	if err != nil {
		return err
	}

	statuses := providers.Detect(ctx, cfg, os.Getenv, exec.LookPath)
	say(s.Out, "Providers:\n")
	WriteStatuses(s.Out, statuses)

	if askKey(p, cfg, cfgMap) {
		changed = true
	}
	if on(statuses, "local_model") && cfg.Privacy.LocalModel == "auto" {
		sub(cfgMap, "privacy")["local_model"] = "required"
		changed = true
		say(s.Out, "privacy.local_model: required (the local model is available; if it goes missing, text that needs it stays on this machine)\n")
	}

	settings, settingsExisted, err := readSettings(settingsPath)
	if err != nil {
		return err
	}
	if enabledPlugin(settings, superpowers) && !slices.Contains(cfg.Worker.Plugins, superpowers) &&
		p.ask("You have "+superpowers+" enabled. Keep it on in workers?") {
		w := sub(cfgMap, "worker")
		list, _ := w["plugins"].([]any)
		w["plugins"] = append(list, superpowers)
		changed = true
	}

	if !existed || changed {
		if !existed {
			// Recorded first, so a failure later still leaves it to the undo.
			rec.Created = appendNew(rec.Created, cfgPath)
			if err := writeRecord(rec); err != nil {
				return err
			}
		}
		data, err := writeConfig(cfgPath, cfgMap)
		if err != nil {
			return err
		}
		if slices.Contains(rec.Created, cfgPath) {
			rec.ConfigSHA256 = sha(data)
			if err := writeRecord(rec); err != nil {
				return err
			}
		}
		say(s.Out, "Config written: %s\n", cfgPath)
	} else {
		say(s.Out, "Config kept: %s (nothing missing)\n", cfgPath)
	}

	if err := offerStatusLine(p, settingsPath, settings, settingsExisted, &rec); err != nil {
		return err
	}

	if err := offerHook(ctx, p, cfg.Launcher, &rec); err != nil {
		return err
	}

	rec.Time = time.Now().UTC()
	if err := writeRecord(rec); err != nil {
		return err
	}

	// The status line was just offered: no hint for it.
	statuses = slices.DeleteFunc(statuses, func(s providers.Status) bool { return s.Name == "statusline" })
	_, qmErr := exec.LookPath("qm")
	hints := Hints(statuses, os.Getenv("SHELL"), qmErr == nil)
	if len(hints) > 0 {
		say(s.Out, "\nTo turn more on:\n")
		for _, h := range hints {
			say(s.Out, "  %s\n", h)
		}
	}
	say(s.Out, "\nInside Claude Code, run:\n  /plugin marketplace add %s\n  %s\nqm-worker needs no install: qm spawn worker loads it.\n", marketplace(o.Marketplace), pluginInstall)
	return nil
}

// askKey asks for the TypeSafe key unless one is set; a typed key goes into
// cfgMap. It reports whether cfgMap changed.
func askKey(p prompter, cfg config.Config, cfgMap map[string]any) bool {
	switch {
	case os.Getenv(config.KeyEnv) != "":
		say(p.out, "TypeSafe key: from %s, not stored in the config\n", config.KeyEnv)
		return false
	case cfg.TypeSafeKey != "":
		say(p.out, "TypeSafe key: already in the config\n")
		return false
	case p.yes:
		say(p.out, "TypeSafe key: skipped (--yes); without one Jev is off\n")
		return false
	}
	key := strings.TrimSpace(p.secret("TypeSafe API key (Enter skips): "))
	if key == "" {
		say(p.out, "TypeSafe key: skipped; without one Jev is off\n")
		return false
	}
	cfgMap["typesafe_api_key"] = key
	say(p.out, "TypeSafe key: stored in the config (mode 0600)\n")
	return true
}

// offerStatusLine shows the status-line change, asks, and writes it after a
// backup; rec gets what changed.
func offerStatusLine(p prompter, path string, settings []byte, existed bool, rec *Record) error {
	if !json.Valid(settings) && len(strings.TrimSpace(string(settings))) > 0 {
		say(p.out, "Status line: %s is not valid JSON; left alone\n", path)
		return nil
	}
	updated, diff, changed := WrapStatusLine(settings)
	repoint := false
	if !changed {
		cmd, ok := statusLineCommand(settings)
		if !ok || !providers.Wrapped(cmd) {
			say(p.out, "Status line: has no command to wrap; left alone\n")
			return nil
		}
		if updated, diff, repoint = RepointStatusLine(settings); !repoint {
			say(p.out, "Status line: already runs qm statusline\n")
			return nil
		}
		say(p.out, "Status line: it runs qm statusline through another qm than this one. Change to %s:\n%s", path, diff)
	} else {
		say(p.out, "Status line: qm statusline records your Claude usage limits. Change to %s\n(qm runs by its absolute path, so the status line does not depend on PATH):\n%s", path, diff)
	}
	if !p.ask("Write this change?") {
		say(p.out, "Status line: left as it is\n")
		return nil
	}
	var before *string
	// A repoint of the wrap an earlier init wrote keeps that init's "before"
	// and its backup, so the undo still puts back the user's own file.
	inherited := false
	if cmd, ok := statusLineCommand(settings); ok {
		before = &cmd
		if repoint && rec.StatusLineAfter == cmd {
			before, inherited = rec.StatusLineBefore, true
		}
	}
	mode := fs.FileMode(0o600)
	backup := ""
	if existed {
		fi, err := os.Stat(path)
		if err != nil {
			return err
		}
		mode = fi.Mode().Perm()
		backup = freeBackupName(path)
		if err := writeFileAtomic(backup, settings, mode); err != nil {
			return fmt.Errorf("back up %s: %w", path, err)
		}
		say(p.out, "Backup: %s\n", backup)
		if inherited && rec.SettingsBackup != "" {
			// The undo restores from the first init's backup, the one that
			// holds the file as the user had it; this one is a safety copy.
			backup = rec.SettingsBackup
		}
	} else if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// Recorded before the write, so the undo knows of the change even if a
	// later step fails.
	after, _ := statusLineCommand(updated)
	rec.SettingsBackup, rec.StatusLineBefore, rec.StatusLineAfter = backup, before, after
	if !existed {
		rec.Created = appendNew(rec.Created, path)
	}
	if err := writeRecord(*rec); err != nil {
		return err
	}
	if err := writeFileAtomic(path, updated, mode); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	say(p.out, "Status line: written\n")
	return nil
}

// Hints are the commands that turn off providers on, for the user's shell
// (fish or POSIX sh). qmOnPath says whether qm itself is on PATH.
func Hints(statuses []providers.Status, shell string, qmOnPath bool) []string {
	fish := filepath.Base(shell) == "fish"
	var out []string
	if !qmOnPath {
		path := `export PATH="$HOME/.local/bin:$PATH"`
		if fish {
			path = "fish_add_path ~/.local/bin"
		}
		out = append(out, "qm is not on PATH: run make install in the quartermaster folder (puts qm in ~/.local/bin), then: "+path)
	}
	for _, s := range statuses {
		if s.On {
			continue
		}
		switch s.Name {
		case "typesafe":
			out = append(out, "Jev: "+SetEnv(shell, config.KeyEnv, "your-key")+" (in your shell's startup file), or run qm init to store it")
		case "launcher":
			out = append(out, `launcher: run qm inside agterm or tmux, or set "launcher" in the quartermaster config`)
		case "statusline":
			out = append(out, "Claude limits: run qm init to wrap the status line")
		case "lsp":
			if s.Fix != "" {
				out = append(out, "LSP: inside Claude Code, run: "+s.Fix)
			}
		case "worker_plugin":
			out = append(out, "qm-worker: run make install in the quartermaster folder, or: "+SetEnv(shell, "QM_WORKER_PLUGIN", "/path/to/plugins/qm-worker"))
		}
	}
	return out
}

// SetEnv is the command that sets an environment variable in shell: set -x
// for fish, export for any other.
func SetEnv(shell, name, value string) string {
	if filepath.Base(shell) == "fish" {
		return "set -x " + name + " " + value
	}
	return "export " + name + "=" + value
}

// WriteStatuses prints one line per provider: on or off, its name and why.
func WriteStatuses(w io.Writer, list []providers.Status) {
	for _, s := range list {
		state := "off"
		if s.On {
			state = "on "
		}
		say(w, "  %s  %-13s %s\n", state, s.Name, s.Why)
	}
}

// marketplace is what "/plugin marketplace add" names: given, as an absolute
// folder when it is one, else the checkout qm's own binary was built in (the
// folder above its bin/), else the public GitHub form. The working folder is
// not looked at: an installed qm run inside a checkout names the public
// repository.
func marketplace(given string) string {
	if given != "" {
		if fi, err := os.Stat(given); err == nil && fi.IsDir() {
			if abs, err := filepath.Abs(given); err == nil {
				return abs
			}
		}
		return given
	}
	exe, err := executable()
	if err != nil {
		return publicMarketplace
	}
	real, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return publicMarketplace
	}
	d := filepath.Dir(filepath.Dir(real))
	b, err := os.ReadFile(filepath.Join(d, ".claude-plugin", "marketplace.json"))
	if err != nil {
		return publicMarketplace
	}
	var m struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(b, &m) == nil && m.Name == "quartermaster" {
		return d
	}
	return publicMarketplace
}

func on(list []providers.Status, name string) bool {
	for _, s := range list {
		if s.Name == name {
			return s.On
		}
	}
	return false
}

func say(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

// prompter asks the user questions.
type prompter struct {
	in   *bufio.Reader
	out  io.Writer
	yes  bool
	term *os.File // the terminal behind in, for echo off; nil when not one
}

// ask asks a yes/no question; anything but y or yes, and end of input, is no.
func (p prompter) ask(q string) bool {
	if p.yes {
		say(p.out, "%s [y/N] yes (--yes)\n", q)
		return true
	}
	return p.read(q)
}

// askOverEdit asks before undoing an edit the user made since init; --yes
// keeps the edit.
func (p prompter) askOverEdit(q string) bool {
	if p.yes {
		say(p.out, "%s [y/N] no (--yes keeps edits made since init)\n", q)
		return false
	}
	return p.read(q)
}

// read asks q and reads a yes or no.
func (p prompter) read(q string) bool {
	say(p.out, "%s [y/N] ", q)
	a := strings.ToLower(strings.TrimSpace(p.line()))
	if p.term == nil { // a terminal echoes the user's line end
		say(p.out, "\n")
	}
	return a == "y" || a == "yes"
}

// secret reads a line with the terminal's echo off when In is a terminal.
func (p prompter) secret(q string) string {
	say(p.out, "%s", q)
	if p.term != nil && stty(p.term, "-echo") == nil {
		defer echoBack(p)()
	}
	line := p.line()
	say(p.out, "\n")
	return line
}

// echoBack turns the terminal's echo back on when qm is interrupted at the
// prompt (then exits 130, as a shell does on Ctrl-C); the returned func
// turns it back on when the prompt ends.
func echoBack(p prompter) func() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		select {
		case <-sig:
			_ = stty(p.term, "echo")
			say(p.out, "\n")
			os.Exit(130)
		case <-done:
		}
	}()
	return func() {
		signal.Stop(sig)
		close(done)
		_ = stty(p.term, "echo")
	}
}

// terminalOf is r when it is a terminal, else nil.
func terminalOf(r io.Reader) *os.File {
	f, ok := r.(*os.File)
	if !ok {
		return nil
	}
	fi, err := f.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return nil
	}
	return f
}

// stty runs stty on the terminal f (stty reads its standard input).
func stty(f *os.File, arg string) error {
	cmd := exec.Command("stty", arg)
	cmd.Stdin = f
	return cmd.Run()
}

// line reads one line, without its line end; end of input gives "".
func (p prompter) line() string {
	l, err := p.in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return ""
	}
	return strings.TrimRight(l, "\r\n")
}

// settingsFile is o.SettingsPath, else Claude Code's user settings.json.
func settingsFile(o Options) (string, error) {
	if o.SettingsPath != "" {
		return o.SettingsPath, nil
	}
	dir, err := paths.ClaudeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "settings.json"), nil
}

// readSettings reads the settings file; a missing one is empty.
func readSettings(path string) ([]byte, bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return b, true, nil
}

// readConfigMap reads the config file as a map, so keys qm does not know
// survive; a missing file is an empty map. A file that is not a JSON object
// is an error: init never overwrites it.
func readConfigMap(path string) (map[string]any, bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]any{}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil || m == nil {
		return nil, true, fmt.Errorf("config %s is not a JSON object; fix or move it, then run qm init again", path)
	}
	return m, true, nil
}

func defaultsMap() map[string]any {
	b, err := json.Marshal(config.Defaults())
	if err != nil {
		panic(err) // the defaults always encode
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		panic(err)
	}
	return m
}

// mergeMissing adds to dst the keys of src it lacks, inside nested objects
// too; it reports whether it added any.
func mergeMissing(dst, src map[string]any) bool {
	added := false
	for k, v := range src {
		cur, ok := dst[k]
		if !ok {
			dst[k] = v
			added = true
			continue
		}
		cm, ok1 := cur.(map[string]any)
		sm, ok2 := v.(map[string]any)
		if ok1 && ok2 && mergeMissing(cm, sm) {
			added = true
		}
	}
	return added
}

// sub is the object at key in m, made when missing or not an object.
func sub(m map[string]any, key string) map[string]any {
	s, ok := m[key].(map[string]any)
	if !ok {
		s = map[string]any{}
		m[key] = s
	}
	return s
}

// asConfig reads the config map the way config.Load reads the file.
func asConfig(m map[string]any) (config.Config, error) {
	c := config.Defaults()
	b, err := json.Marshal(m)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("config: %w", err)
	}
	return c, nil
}

// writeConfig writes the config map, a 0600 file in a 0700 folder, and
// returns what it wrote.
func writeConfig(path string, m map[string]any) ([]byte, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	b = append(b, '\n')
	return b, writeFileAtomic(path, b, 0o600)
}

// sha is the hex SHA-256 of b.
func sha(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// writeFileAtomic writes data to a temporary file next to path with mode
// perm, then renames it over path.
func writeFileAtomic(path string, data []byte, perm fs.FileMode) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
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
	if err = tmp.Chmod(perm); err != nil {
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

func recordPath() (string, error) {
	d, err := paths.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "init.json"), nil
}

// readRecord reads init.json; a missing one is an empty Record, so a second
// init keeps what the first one did.
func readRecord() (Record, error) {
	path, err := recordPath()
	if err != nil {
		return Record{}, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Record{}, nil
	}
	if err != nil {
		return Record{}, err
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return Record{}, fmt.Errorf("%s: %w", path, err)
	}
	return r, nil
}

func writeRecord(r Record) error {
	path, err := recordPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(b, '\n'), 0o600)
}

func appendNew(list []string, s string) []string {
	if slices.Contains(list, s) {
		return list
	}
	return append(list, s)
}

// backupStamp is the number a backup's name ends in: Unix seconds.
var backupStamp = func() int64 { return time.Now().Unix() }

// freeBackupName is <path>.qm-backup-<seconds>, with -2, -3, ... added while
// that name is taken, so a backup never overwrites an earlier one.
func freeBackupName(path string) string {
	base := path + ".qm-backup-" + strconv.FormatInt(backupStamp(), 10)
	name := base
	for n := 2; ; n++ {
		if _, err := os.Lstat(name); errors.Is(err, fs.ErrNotExist) {
			return name
		}
		name = base + "-" + strconv.Itoa(n)
	}
}
