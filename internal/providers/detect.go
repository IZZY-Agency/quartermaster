package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/IZZY-Agency/quartermaster/internal/config"
	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/limits"
	"github.com/IZZY-Agency/quartermaster/internal/paths"
	"github.com/IZZY-Agency/quartermaster/internal/privacy"
	"github.com/IZZY-Agency/quartermaster/internal/spawn"
)

// Tests replace these.
var (
	// ps reads a process's command line and parent, for the permission source.
	ps = spawn.PS
	// executable is qm's own path, for the qm-worker plugin lookup.
	executable = os.Executable
)

// helperTimeout bounds "qm-privcheck status".
const helperTimeout = 10 * time.Second

// Detect reports each optional provider (design 5.10), in this order:
// claude, typesafe, codex, gitleaks, helper, local_model, launcher,
// permission, statusline, notify, lsp, worker_plugin. env is read for the
// TypeSafe key, the launcher (AGTERM_SESSION_ID, TMUX) and the permission source
// (CLAUDE_PID); look finds a command on PATH. Every other row reads the
// process environment, as the code it reports on does: the privacy helper
// and gitleaks are found the way the privacy gate finds them (QM_PRIVCHECK,
// QM_GITLEAKS), the status line, LSP and Codex rows read Claude Code's and
// Codex's folders through CLAUDE_CONFIG_DIR, CODEX_HOME and HOME.
func Detect(ctx context.Context, cfg config.Config, env func(string) string, look func(string) (string, error)) []Status {
	codex, err := limits.Codex(look)
	return DetectWithCodex(ctx, cfg, env, look, codex, err)
}

// DetectWithCodex is Detect with the Codex state already read by
// limits.Codex, for a caller that shows that state too. env and look are
// read as Detect reads them.
func DetectWithCodex(ctx context.Context, cfg config.Config, env func(string) string, look func(string) (string, error), codex limits.CodexState, codexErr error) []Status {
	helper := privacy.FindHelper()
	return []Status{
		detectClaude(look),
		detectKey(cfg, env),
		detectCodex(codex, codexErr),
		detectGitleaks(),
		detectHelper(helper),
		detectLocalModel(ctx, cfg, helper),
		detectLauncher(cfg, env, look),
		detectPermission(env),
		detectStatusLine(),
		detectNotify(cfg, look),
		detectLSP(),
		detectWorkerPlugin(),
	}
}

func detectClaude(look func(string) (string, error)) Status {
	if p, err := look("claude"); err == nil {
		return Status{Name: "claude", On: true, Why: p}
	}
	return Status{Name: "claude", Why: "claude not found on PATH: workers cannot start"}
}

func detectKey(cfg config.Config, env func(string) string) Status {
	switch {
	case env(config.KeyEnv) != "":
		return Status{Name: "typesafe", On: true, Why: "Jev on: key from " + config.KeyEnv}
	case cfg.TypeSafeKey != "":
		return Status{Name: "typesafe", On: true, Why: "Jev on: key from the config file"}
	}
	return Status{Name: "typesafe", Why: "Jev off: no TypeSafe key; ask gate and feedback off"}
}

func detectCodex(st limits.CodexState, err error) Status {
	const off = "; reviews go to Claude"
	switch {
	case err != nil:
		return Status{Name: "codex", Why: "Codex off: its usage cannot be read (" + err.Error() + ")" + off}
	case st.Available && st.Reason != "":
		return Status{Name: "codex", On: true, Why: "Codex on: " + st.Reason}
	case st.Available:
		return Status{Name: "codex", On: true, Why: "Codex on"}
	case st.Reason == "codex not installed":
		return Status{Name: "codex", Why: "Codex off: not installed" + off}
	}
	return Status{Name: "codex", Why: "Codex off: " + st.Reason + off}
}

func detectGitleaks() Status {
	if p := privacy.FindGitleaks(); p != "" {
		return Status{Name: "gitleaks", On: true, Why: p}
	}
	return Status{Name: "gitleaks", Why: "gitleaks off"}
}

func detectHelper(helper string) Status {
	switch {
	case helper != "":
		return Status{Name: "helper", On: true, Why: helper}
	case runtime.GOOS != "darwin":
		return Status{Name: "helper", Why: "privacy helper off: macOS only; names are not detected, list known ones in privacy.clients"}
	}
	return Status{Name: "helper", Why: "privacy helper off: qm-privcheck not found; names are not detected"}
}

// detectLocalModel asks the helper whether its Foundation Model is
// available, unless the config turns the model off.
func detectLocalModel(ctx context.Context, cfg config.Config, helper string) Status {
	const off = "local model off: redaction and gitleaks only"
	mode := cfg.Privacy.LocalModel
	if mode == "off" {
		return Status{Name: "local_model", Why: off + " (privacy.local_model is off)"}
	}
	why := "qm-privcheck not found"
	if helper != "" {
		ok, reason := helperStatus(ctx, helper)
		if ok {
			return Status{Name: "local_model", On: true, Why: "local model on (privacy.local_model " + mode + ")"}
		}
		why = reason
	}
	if mode == "required" {
		return Status{Name: "local_model", Why: "local model required but unavailable (" + why + "): text that needs it stays on this machine"}
	}
	return Status{Name: "local_model", Why: off + " (" + why + ")"}
}

// helperStatus runs "qm-privcheck status": exit 0 is available; otherwise
// the reason is what it printed, or how it failed.
func helperStatus(ctx context.Context, helper string) (bool, string) {
	ctx, cancel := context.WithTimeout(ctx, helperTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, helper, "status")
	var b bytes.Buffer
	cmd.Stdout, cmd.Stderr = &b, &b
	err := cmd.Run()
	if err == nil {
		return true, ""
	}
	msg := strings.TrimSpace(b.String())
	msg = strings.TrimSpace(strings.TrimPrefix(msg, "unavailable:"))
	if msg == "" {
		msg = "qm-privcheck status: " + err.Error()
	}
	return false, msg
}

// detectLauncher names the launcher qm spawn worker would use and checks its
// command is installed.
func detectLauncher(cfg config.Config, env func(string) string, look func(string) (string, error)) Status {
	l, err := launch.Pick(cfg.Launcher, env)
	if errors.Is(err, launch.ErrNoLauncher) {
		return Status{Name: "launcher", Why: "launcher: none found"}
	}
	if err != nil {
		return Status{Name: "launcher", Why: "launcher: " + err.Error()}
	}
	var name, bin string
	switch v := l.(type) {
	case launch.Agterm:
		name, bin = "agterm", v.Bin
	case launch.Tmux:
		name, bin = "tmux", v.Bin
		if !v.Inside {
			name = "tmux (new detached session)"
		}
	default:
		name, bin = fmt.Sprintf("%T", l), ""
	}
	if bin != "" {
		if _, err := look(bin); err != nil {
			return Status{Name: "launcher", Why: "launcher: " + name + " chosen but " + bin + " not found"}
		}
	}
	return Status{Name: "launcher", On: true, Why: "launcher: " + name}
}

func detectPermission(env func(string) string) Status {
	flags, why := spawn.ParentPermission(env, ps)
	return Status{Name: "permission", On: flags != nil, Why: why}
}

// ParseWrapper splits a status-line command that runs qm statusline into the
// path qm runs by, as the shell reads it (quotes taken off; "qm" for plain
// qm), and the rest from the whitespace before "statusline" on, byte for
// byte: it reads every word qm init writes (a path in single quotes, an
// apostrophe in it written '\”, no quotes for a safe path) and a rest of any
// shape, a newline included. ok is false for a command that does not run qm
// statusline; a program named something else is not qm, and neither is
// "echo qm statusline".
func ParseWrapper(cmd string) (path, rest string, ok bool) {
	s := strings.TrimLeftFunc(cmd, unicode.IsSpace)
	var b strings.Builder
	i := 0
scan:
	for i < len(s) {
		switch c := s[i]; {
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return "", "", false
			}
			b.WriteString(s[i+1 : i+1+j])
			i += j + 2
		case c == '\\' && i+1 < len(s) && s[i+1] == '\'':
			b.WriteByte('\'')
			i += 2
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			break scan
		case c == '\\' || c == '"':
			return "", "", false
		default:
			b.WriteByte(c)
			i++
		}
	}
	path, rest = b.String(), s[i:]
	if path != "qm" && !strings.HasSuffix(path, "/qm") {
		return "", "", false
	}
	word := strings.TrimLeft(rest, " \t")
	after, found := strings.CutPrefix(word, "statusline")
	if !found || len(word) == len(rest) || (after != "" && !unicode.IsSpace(rune(after[0]))) {
		return "", "", false
	}
	return path, rest, true
}

// wrappedPath is the path a wrapped status line runs qm by, or "" when it
// names qm without a path or is not wrapped.
func wrappedPath(cmd string) string {
	path, _, ok := ParseWrapper(cmd)
	if !ok || !strings.Contains(path, "/") {
		return ""
	}
	return path
}

// Wrapped reports whether a status-line command already runs qm statusline.
func Wrapped(cmd string) bool {
	_, _, ok := ParseWrapper(cmd)
	return ok
}

func detectStatusLine() Status {
	const off = "Claude limits unknown: the status line does not run qm statusline"
	dir, err := paths.ClaudeDir()
	if err != nil {
		return Status{Name: "statusline", Why: off}
	}
	b, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		return Status{Name: "statusline", Why: off}
	}
	var s struct {
		StatusLine struct {
			Command string `json:"command"`
		} `json:"statusLine"`
	}
	if json.Unmarshal(b, &s) != nil || !Wrapped(s.StatusLine.Command) {
		return Status{Name: "statusline", Why: off}
	}
	if qm := wrappedPath(s.StatusLine.Command); qm != "" {
		if _, err := os.Stat(qm); err != nil {
			return Status{Name: "statusline", Why: "Claude limits unknown: the status line runs " + qm + ", which is gone; qm init repoints it"}
		}
	}
	return Status{Name: "statusline", On: true, Why: "status-line wrapper on"}
}

func detectNotify(cfg config.Config, look func(string) (string, error)) Status {
	if len(cfg.Notify) == 0 {
		return Status{Name: "notify", Why: "notify off"}
	}
	if _, err := look(cfg.Notify[0]); err != nil {
		return Status{Name: "notify", Why: "notify off: " + cfg.Notify[0] + " not found"}
	}
	return Status{Name: "notify", On: true, Why: "notify: " + strings.Join(cfg.Notify, " ")}
}

// getwd is the folder whose repository detectLSP checks; tests replace it.
var getwd = os.Getwd

// detectLSP reports the official LSP plugin a worker started in the working
// folder's repository would get: on when it is installed, off with the
// /plugin install command when it is not. Outside a repository with code it
// lists the official LSP plugins installed.
func detectLSP() Status {
	if wd, err := getwd(); err == nil {
		if l, _ := spawn.RepoLSP(wd); l.Plugin != "" {
			if l.Installed {
				return Status{Name: "lsp", On: true, Why: "LSP: " + l.Plugin + " installed; workers in this repository turn it on"}
			}
			return Status{Name: "lsp", Why: "LSP off for this repository: " + l.Plugin + " not installed", Fix: l.InstallCommand()}
		}
	}
	installed, err := spawn.InstalledPlugins("")
	if err != nil {
		return Status{Name: "lsp", Why: "LSP off: " + err.Error()}
	}
	var found []string
	for key := range installed {
		name, market, _ := strings.Cut(key, "@")
		if market == spawn.OfficialMarketplace && strings.HasSuffix(name, "-lsp") {
			found = append(found, name)
		}
	}
	if len(found) == 0 {
		return Status{Name: "lsp", Why: "LSP off: no official LSP plugin installed"}
	}
	sort.Strings(found)
	return Status{Name: "lsp", On: true, Why: "LSP plugins installed: " + strings.Join(found, ", ")}
}

func detectWorkerPlugin() Status {
	exe, err := executable()
	if err != nil {
		return Status{Name: "worker_plugin", Why: "qm-worker plugin off: " + err.Error()}
	}
	dir, err := spawn.WorkerPluginDir(exe)
	if err != nil {
		return Status{Name: "worker_plugin", Why: err.Error()}
	}
	return Status{Name: "worker_plugin", On: true, Why: dir}
}
