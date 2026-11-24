package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/config"
	"github.com/IZZY-Agency/quartermaster/internal/hooks"
	"github.com/IZZY-Agency/quartermaster/internal/limits"
	"github.com/IZZY-Agency/quartermaster/internal/paths"
	"github.com/IZZY-Agency/quartermaster/internal/providers"
	"github.com/IZZY-Agency/quartermaster/internal/setup"
	"github.com/IZZY-Agency/quartermaster/internal/watch"
)

func init() {
	commands["status"] = command{"show what is on and off, and why: qm status [--json]", runStatus}
}

// statusReport is what qm status shows; --json prints it as is.
type statusReport struct {
	Providers    []providers.Status `json:"providers"`
	Rules        map[string]string  `json:"rules"`
	WorkerWindow map[string]int     `json:"worker_window"`
	// Handoff says at which share of its window a worker is told to hand
	// off, and why when the config's value is not usable.
	Handoff string `json:"handoff"`
	// AutoCompactWindow is autoCompactWindow in the user's Claude Code
	// settings (nil: not set); AutoCompactEnv is
	// CLAUDE_CODE_AUTO_COMPACT_WINDOW, which overrides it, when set.
	AutoCompactWindow *int               `json:"auto_compact_window"`
	AutoCompactEnv    string             `json:"auto_compact_env,omitempty"`
	ClaudeLimits      claudeLimitsReport `json:"claude_limits"`
	Codex             limits.CodexState  `json:"codex"`
	CodexError        string             `json:"codex_error,omitempty"`
	Hints             []string           `json:"hints"`
	// Workers are the unfinished workers of the working folder's project;
	// nil outside a project or without workers.
	Workers *workersReport `json:"workers,omitempty"`
}

// workersReport is the Workers section: the project's workers and their
// states, or why they could not be read.
type workersReport struct {
	Project string      `json:"project"`
	Rows    []watch.Row `json:"rows,omitempty"`
	Note    string      `json:"note,omitempty"`
	Error   string      `json:"error,omitempty"`
}

// claudeLimitsReport is the Claude limit file written by qm statusline.
type claudeLimitsReport struct {
	File         string     `json:"file"`
	Exists       bool       `json:"exists"`
	UpdatedAt    *time.Time `json:"updated_at,omitempty"`
	AgeSeconds   *int64     `json:"age_seconds,omitempty"`
	Known        bool       `json:"known"`
	FiveHourUsed float64    `json:"five_hour_used"`
	SevenDayUsed float64    `json:"seven_day_used"`
	Error        string     `json:"error,omitempty"`
}

func runStatus(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("status", flag.ContinueOnError)
	fl.SetOutput(stderr)
	asJSON := fl.Bool("json", false, "print the report as JSON")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "qm status: %v\n", err)
		return 1
	}
	r := buildStatus(context.Background(), cfg)
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(r); err != nil {
			_, _ = fmt.Fprintf(stderr, "qm status: %v\n", err)
			return 1
		}
		return 0
	}
	writeStatus(stdout, r)
	return 0
}

func buildStatus(ctx context.Context, cfg config.Config) statusReport {
	codex, codexErr := limits.Codex(exec.LookPath)
	r := statusReport{
		Providers:    providers.DetectWithCodex(ctx, cfg, os.Getenv, exec.LookPath, codex, codexErr),
		Rules:        map[string]string{},
		WorkerWindow: cfg.WorkerWindow,
		Handoff:      handoffLine(cfg),
		Codex:        codex,
	}
	if codexErr != nil {
		r.CodexError = codexErr.Error()
	}
	for _, n := range hooks.Names() {
		r.Rules[n] = cfg.Rule(n)
	}
	for n := range cfg.Rules {
		r.Rules[n] = cfg.Rule(n)
	}
	r.Workers = statusWorkers(cfg)
	if dir, err := paths.ClaudeDir(); err == nil {
		r.AutoCompactWindow = autoCompactWindow(filepath.Join(dir, "settings.json"))
	}
	r.AutoCompactEnv = os.Getenv("CLAUDE_CODE_AUTO_COMPACT_WINDOW")
	r.ClaudeLimits = claudeLimits()
	_, qmErr := exec.LookPath("qm")
	r.Hints = setup.Hints(r.Providers, os.Getenv("SHELL"), qmErr == nil)
	return r
}

// autoCompactWindow reads autoCompactWindow from a Claude Code settings file.
func autoCompactWindow(path string) *int {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var s struct {
		AutoCompactWindow *int `json:"autoCompactWindow"`
	}
	if json.Unmarshal(b, &s) != nil {
		return nil
	}
	return s.AutoCompactWindow
}

func claudeLimits() claudeLimitsReport {
	var r claudeLimitsReport
	if dir, err := paths.StateDir(); err == nil {
		r.File = filepath.Join(dir, "limits", "claude.json")
	}
	if _, err := os.Stat(r.File); errors.Is(err, fs.ErrNotExist) || r.File == "" {
		return r
	}
	r.Exists = true
	st, err := limits.Claude()
	if err != nil {
		r.Error = err.Error()
		return r
	}
	r.Known, r.FiveHourUsed, r.SevenDayUsed = st.Known, st.FiveHourUsed, st.SevenDayUsed
	if !st.UpdatedAt.IsZero() {
		u := st.UpdatedAt
		age := int64(time.Since(u).Seconds())
		r.UpdatedAt, r.AgeSeconds = &u, &age
	}
	return r
}

func writeStatus(w io.Writer, r statusReport) {
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }
	p("Providers:\n")
	setup.WriteStatuses(w, r.Providers)

	p("Rules:\n")
	for _, n := range sortedKeys(r.Rules) {
		p("  %-17s %s\n", n, r.Rules[n])
	}

	p("Compact windows:\n")
	var ws []string
	for _, m := range sortedKeys(r.WorkerWindow) {
		ws = append(ws, fmt.Sprintf("%s %d", m, r.WorkerWindow[m]))
	}
	p("  workers: %s\n", strings.Join(ws, ", "))
	p("  workers hand off at: %s\n", r.Handoff)
	if r.AutoCompactWindow != nil {
		p("  your sessions: autoCompactWindow %d\n", *r.AutoCompactWindow)
	} else {
		p("  your sessions: autoCompactWindow not set (Claude Code's default)\n")
	}
	if r.AutoCompactEnv != "" {
		p("  CLAUDE_CODE_AUTO_COMPACT_WINDOW=%s overrides both\n", r.AutoCompactEnv)
	}

	c := r.ClaudeLimits
	switch {
	case !c.Exists:
		p("Claude limits: no limit file yet (%s); unknown\n", c.File)
	case c.Error != "":
		p("Claude limits: %s\n", c.Error)
	default:
		age := "at an unknown time"
		if c.AgeSeconds != nil {
			age = ago(time.Duration(*c.AgeSeconds) * time.Second)
		}
		state := "known"
		if !c.Known {
			state = "unknown (stale or reset since)"
		}
		p("Claude limits: %s updated %s; 5-hour %g%%, 7-day %g%%; %s\n", c.File, age, c.FiveHourUsed, c.SevenDayUsed, state)
	}

	switch {
	case r.CodexError != "":
		p("Codex: %s\n", r.CodexError)
	case r.Codex.Available:
		usage := "usage unknown"
		if r.Codex.UsageKnown {
			usage = fmt.Sprintf("%g%% used", r.Codex.UsedPercent)
			if !r.Codex.ResetsAt.IsZero() {
				usage += ", resets " + r.Codex.ResetsAt.Local().Format(time.DateTime)
			}
		}
		if r.Codex.Reason != "" {
			usage += " (" + r.Codex.Reason + ")"
		}
		p("Codex: available, %s\n", usage)
	default:
		p("Codex: off: %s\n", r.Codex.Reason)
	}

	if len(r.Hints) > 0 {
		p("Hints:\n")
		for _, h := range r.Hints {
			p("  %s\n", h)
		}
	}

	switch wr := r.Workers; {
	case wr == nil:
	case wr.Error != "":
		p("Workers: %s\n", wr.Error)
	default:
		p("Workers (%s):\n", wr.Project)
		if wr.Note != "" {
			p("  note: worker states unknown: %s\n", wr.Note)
		}
		home, _ := os.UserHomeDir()
		p("%s", watch.Table(wr.Rows, nil, home))
	}
}

// statusWorkersBound bounds the Workers section: the process list and the
// transcripts it reads. Tests shorten it.
var statusWorkersBound = 5 * time.Second

// statusWorkers builds the Workers section; nil outside a project or
// without workers. A failure, the bound included, is the section's one
// line, never qm status's.
func statusWorkers(cfg config.Config) *workersReport {
	p, ok, err := projectHere()
	if err != nil {
		return &workersReport{Error: err.Error()}
	}
	if !ok {
		return nil
	}
	silent, err := cfg.SilentThreshold()
	if err != nil {
		return &workersReport{Project: p.ID, Error: err.Error()}
	}
	ctx, cancel := context.WithTimeout(context.Background(), statusWorkersBound)
	defer cancel()
	s, err := workersSnapshot(ctx, p.ID, silent)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return &workersReport{Project: p.ID, Error: fmt.Sprintf("not read within %v", statusWorkersBound)}
	case err != nil:
		return &workersReport{Project: p.ID, Error: err.Error()}
	case len(s.Rows) == 0:
		return nil
	}
	return &workersReport{Project: p.ID, Rows: s.Rows, Note: s.Note}
}

// ago is an age at minute resolution: "under a minute ago", "5m ago",
// "2h ago", "2h5m ago"; a time in the future says so.
func ago(d time.Duration) string {
	if d < 0 {
		return "in the future"
	}
	m := int(d / time.Minute)
	switch h := m / 60; {
	case m == 0:
		return "under a minute ago"
	case h == 0:
		return fmt.Sprintf("%dm ago", m)
	case m%60 == 0:
		return fmt.Sprintf("%dh ago", h)
	default:
		return fmt.Sprintf("%dh%dm ago", h, m%60)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// handoffLine is the status line of worker.handoff_percent: the share in
// effect, and the config's error when its value is out of range.
func handoffLine(cfg config.Config) string {
	share, err := cfg.HandoffShare()
	line := fmt.Sprintf("%.0f%% of the window", share*100)
	if err != nil {
		line += " (" + err.Error() + ")"
	}
	return line
}
