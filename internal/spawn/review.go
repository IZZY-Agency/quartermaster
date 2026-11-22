package spawn

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed" // findings.schema.json
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/declog"
	"github.com/IZZY-Agency/quartermaster/internal/limits"
	"github.com/IZZY-Agency/quartermaster/internal/paths"
	"github.com/IZZY-Agency/quartermaster/internal/policy"
	"github.com/IZZY-Agency/quartermaster/internal/route"
)

// FindingsSchema is the JSON Schema every review reply must match: a verdict
// and a list of findings (severity, file, line, scenario, fix). It is
// embedded so an installed qm, which has no source tree, can hand it to
// Codex (preflight F18).
//
//go:embed findings.schema.json
var FindingsSchema []byte

// Review job status values, the whole content of a job's status file.
const (
	StatusRunning = "running"
	StatusDone    = "done"
	StatusFailed  = "failed"
)

// Files in a review job folder.
const (
	jobFile      = "job.json"
	promptFile   = "prompt.md"
	schemaFile   = "findings.schema.json"
	statusFile   = "status"
	logFile      = "log"
	findingsFile = "findings.json"
	codexLast    = "last.txt"  // Codex's last message (-o)
	claudeReply  = "reply.txt" // the Claude reviewer's raw reply
)

// ReviewJob is one review: what route decided and the decision-log record to
// write when it ends. ID is set by NewReviewJob.
type ReviewJob struct {
	ID       string         `json:"id"`
	Repo     string         `json:"repo"`
	Decision route.Decision `json:"decision"`
	Record   route.Record   `json:"record"`
}

// Finding is one review finding.
type Finding struct {
	Severity string `json:"severity"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Scenario string `json:"scenario"`
	Fix      string `json:"fix"`
}

// Findings is a review's reply, as FindingsSchema describes it.
type Findings struct {
	Verdict  string    `json:"verdict"`
	Findings []Finding `json:"findings"`
}

// Detail is the review record's Detail: compact JSON with the verdict and the
// count of findings at each severity.
func (f Findings) Detail() string {
	n := map[string]int{}
	for _, x := range f.Findings {
		n[x.Severity]++
	}
	return fmt.Sprintf(`{"verdict":%q,"p1":%d,"p2":%d,"p3":%d}`, f.Verdict, n["P1"], n["P2"], n["P3"])
}

// ReviewsDir is the folder that holds the review jobs, <state>/reviews.
func ReviewsDir() (string, error) {
	d, err := paths.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "reviews"), nil
}

// ReviewJobDir is the folder of the job with this id; an id that is not a
// plain folder name is an error.
func ReviewJobDir(id string) (string, error) {
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, `/\`) {
		return "", fmt.Errorf("review job id %q is not valid", id)
	}
	d, err := ReviewsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, id), nil
}

// NewReviewJob creates the job's folder (0700) under ReviewsDir with the
// job, the prompt, the findings schema and status "running"; it returns the
// folder. The job's ID is set here.
func NewReviewJob(job ReviewJob, prompt []byte) (string, error) {
	parent, err := ReviewsDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", err
	}
	suffix := make([]byte, 3)
	if _, err := rand.Read(suffix); err != nil {
		return "", err
	}
	job.ID = time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(suffix)
	dir := filepath.Join(parent, job.ID)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return "", err
	}
	for name, data := range map[string][]byte{jobFile: append(b, '\n'), promptFile: prompt, schemaFile: FindingsSchema} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			return "", err
		}
	}
	if err := writeStatus(dir, StatusRunning); err != nil {
		return "", err
	}
	return dir, nil
}

// ReviewStatus reads a job's status: running, done or failed.
func ReviewStatus(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, statusFile))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// MarkReviewFailed sets a job's status to failed, for a job whose runner
// could not be started.
func MarkReviewFailed(dir string) error {
	return writeStatus(dir, StatusFailed)
}

// writeStatus replaces the status file in one rename, so a reader never sees
// it half written.
func writeStatus(dir, status string) error {
	tmp, err := os.CreateTemp(dir, ".status-*")
	if err != nil {
		return err
	}
	if _, err := tmp.WriteString(status + "\n"); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, statusFile))
}

// CodexArgs is the codex command line for a review: explicit model and
// effort, a read-only sandbox, no saved session, JSONL events, the last
// message to <out>/last.txt, shaped by the findings schema, run in repo. The
// prompt argument is "-": codex exec reads the prompt from stdin, so a prompt
// of any size, or one that starts with "-", passes intact.
func CodexArgs(c route.CodexTier, repo, out, schema string) []string {
	return []string{"exec", "-m", c.Model, "-c", fmt.Sprintf("model_reasoning_effort=%q", c.Effort),
		"-s", "read-only", "--ephemeral", "--json", "-o", filepath.Join(out, codexLast),
		"--output-schema", schema, "-C", repo, "-"}
}

// DefaultReviewTimeout is how long a review job may run before its reviewer
// is killed and the job fails.
const DefaultReviewTimeout = 45 * time.Minute

// reviewerSettings turns qm-coordinator off in the Claude reviewer, so its
// hooks do not fire on the review (preflight F16).
const reviewerSettings = `{"enabledPlugins":{"qm-coordinator@quartermaster":false}}`

// ClaudeArgs is the claude command line for a reviewer at tier t: print
// mode, JSON output, reading tools only. --tools leaves only Read, Grep, Glob
// and Bash (naming Grep and Glob also brings them back on builds that search
// through Bash), --strict-mcp-config with no --mcp-config loads no MCP
// server, the allowlist names the Bash commands it may run, edits, writes and
// MCP tools are denied, and dontAsk turns any other call that would prompt
// into a denial. The user's own allow rules and hooks still apply. The
// prompt goes on stdin.
func ClaudeArgs(t route.Tier) ([]string, error) {
	id, ok := policy.ModelIDs[t.Model]
	if !ok {
		return nil, fmt.Errorf("no Claude model id for %q", t.Model)
	}
	return []string{"-p", "--model", id, "--effort", t.Effort, "--output-format", "json",
		"--settings", reviewerSettings, "--permission-mode", "dontAsk",
		"--tools", "Read,Grep,Glob,Bash", "--strict-mcp-config",
		"--allowedTools", "Read", "Grep", "Glob", "Bash(git diff *)", "Bash(git log *)", "Bash(gh pr view *)", "Bash(gh pr diff *)",
		"--disallowedTools", "Edit", "Write", "NotebookEdit", "mcp__*"}, nil
}

// ReviewRunner runs review jobs. Codex and Claude are the commands to run;
// Codex "" means it is not installed.
type ReviewRunner struct {
	Codex  string
	Claude string
}

// Run runs the job in dir to its end: on Codex when the decision has a Codex
// tier, else, or when the Codex review fails in any way, on Claude (a Codex
// usage-limit error also marks Codex limited until the reset it states). When
// ctx ends, the running reviewer's process group is killed and the job
// fails, with no fallback. A valid reply is written to
// findings.json, logged as a review record with the counts, and the status
// becomes done. Any failure sets status failed, is logged as an error record
// (so it does not count as a review round) and is returned.
func (r ReviewRunner) Run(ctx context.Context, dir string) error {
	var job ReviewJob
	b, err := os.ReadFile(filepath.Join(dir, jobFile))
	if err == nil {
		err = json.Unmarshal(b, &job)
	}
	if err != nil {
		return r.fail(dir, job, fmt.Errorf("read job: %w", err))
	}
	log, err := os.OpenFile(filepath.Join(dir, logFile), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return r.fail(dir, job, err)
	}
	defer func() { _ = log.Close() }()
	prompt, err := os.ReadFile(filepath.Join(dir, promptFile))
	if err != nil {
		return r.fail(dir, job, err)
	}

	d := job.Decision
	var f Findings
	switch {
	case d.Codex != nil && r.Codex != "":
		f, err = r.runCodex(ctx, dir, job.Repo, *d.Codex, string(prompt), log)
		if err == nil || ctx.Err() != nil {
			break
		}
		_, _ = fmt.Fprintf(log, "qm: Codex review failed: %v\n", err)
		d.Codex = nil
		d.Reasons = append(d.Reasons, "Codex failed ("+err.Error()+"): the review went to Claude")
		f, err = r.runClaude(ctx, dir, job.Repo, d.Tier, string(prompt), log)
	case d.Codex != nil:
		d.Codex = nil
		d.Reasons = append(d.Reasons, "codex not found at run time: the review went to Claude")
		fallthrough
	default:
		f, err = r.runClaude(ctx, dir, job.Repo, d.Tier, string(prompt), log)
	}
	if err != nil && ctx.Err() != nil {
		err = fmt.Errorf("%w: %v", ctx.Err(), err)
	}
	if err != nil {
		_, _ = fmt.Fprintf(log, "qm: review failed: %v\n", err)
		return r.fail(dir, job, err)
	}

	out, err := json.MarshalIndent(f, "", "  ")
	if err == nil {
		err = os.WriteFile(filepath.Join(dir, findingsFile), append(out, '\n'), 0o600)
	}
	if err != nil {
		return r.fail(dir, job, err)
	}
	rec := job.Record
	rec.Name, rec.Detail = job.ID, f.Detail()
	if err := declog.AppendDecision(d, rec); err != nil {
		_, _ = fmt.Fprintf(log, "qm: %v\n", err)
	}
	return writeStatus(dir, StatusDone)
}

// fail marks the job failed, logs an error record and returns err.
func (r ReviewRunner) fail(dir string, job ReviewJob, err error) error {
	rec := job.Record
	rec.Time, rec.Kind, rec.Name = time.Now().UTC(), "error", job.ID
	rec.Round, rec.Detail = 0, "review failed: "+err.Error()
	err = fmt.Errorf("review %s: %w", job.ID, err)
	if lerr := declog.Append(rec); lerr != nil {
		err = errors.Join(err, lerr)
	}
	if serr := writeStatus(dir, StatusFailed); serr != nil {
		err = errors.Join(err, serr)
	}
	return err
}

// errCodexLimited is a Codex run that ended on Codex's usage-limit error.
var errCodexLimited = errors.New("codex usage limit reached")

// runCodex runs the Codex review, the prompt on stdin. A usage-limit error
// marks Codex limited and returns errCodexLimited.
func (r ReviewRunner) runCodex(ctx context.Context, dir, repo string, c route.CodexTier, prompt string, log io.Writer) (Findings, error) {
	var out bytes.Buffer
	cmd := reviewerCmd(ctx, r.Codex, CodexArgs(c, repo, dir, filepath.Join(dir, schemaFile))...)
	cmd.Dir = repo
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Stdout = io.MultiWriter(log, &out)
	cmd.Stderr = cmd.Stdout
	runErr := cmd.Run()
	last, readErr := os.ReadFile(filepath.Join(dir, codexLast))
	if runErr == nil && readErr == nil {
		return ParseFindings(last)
	}
	if reset, ok := limits.ParseCodexLimitError(codexErrors(out.Bytes())); ok {
		if err := limits.MarkCodexLimited(reset); err != nil {
			_, _ = fmt.Fprintf(log, "qm: marking Codex limited: %v\n", err)
		}
		return Findings{}, errCodexLimited
	}
	if runErr != nil {
		return Findings{}, fmt.Errorf("codex: %w", runErr)
	}
	return Findings{}, fmt.Errorf("codex wrote no last message: %w", readErr)
}

// reviewerCmd is a reviewer command in its own process group; when ctx ends
// the whole group is killed, so a reviewer's own children (a shell, git, a
// language server) cannot keep it, or its output pipes, alive.
func reviewerCmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// codexErrors is the part of Codex's output that can carry its own errors:
// lines that are not JSON (its stderr) and JSON events of type error or
// turn.failed. Other events can quote any text, the code under review
// included, so they are left out.
func codexErrors(out []byte) string {
	var b strings.Builder
	for _, line := range bytes.Split(out, []byte("\n")) {
		var ev struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(line, &ev) != nil || ev.Type == "error" || ev.Type == "turn.failed" {
			b.Write(line)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// sessionEnv are the variables a Claude Code session or a qm worker sets for
// its own children; a reviewer started from one must not inherit them (its
// effort comes from --effort, not CLAUDE_EFFORT). sessionEnvPrefixes drop
// whole families. The user's own settings (CLAUDE_CONFIG_DIR, the API and
// provider variables) are kept.
var (
	sessionEnv = map[string]bool{
		"CLAUDECODE": true, "CLAUDE_PID": true, "CLAUDE_EFFORT": true,
		"CLAUDE_CODE_SESSION_ID": true, "CLAUDE_CODE_ENTRYPOINT": true, "CLAUDE_CODE_EXECPATH": true,
		"CLAUDE_CODE_SSE_PORT": true, "CLAUDE_CODE_SESSION_ATTENDED": true,
		"CLAUDE_PLUGIN_ROOT": true, "CLAUDE_PLUGIN_DATA": true,
		"QM_SESSION_NAME": true, "QM_LSP": true, "QM_AUTOCOMPACT": true, "QM_ROLE": true,
	}
	sessionEnvPrefixes = []string{"CLAUDE_CODE_MESSAGING_", "CLAUDE_CODE_CHILD_"}
)

// reviewerEnv is env without the calling session's own variables.
func reviewerEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if sessionEnv[name] || slices.ContainsFunc(sessionEnvPrefixes, func(p string) bool { return strings.HasPrefix(name, p) }) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// reviewerInstructions follow the prompt given to a Claude reviewer.
const reviewerInstructions = "\n\nReply with JSON only, no other text, matching this JSON Schema:\n\n"

// runClaude runs a Claude reviewer at tier t in repo, without qm-worker and
// with qm-coordinator off, QM_ROLE=reviewer (preflight F16). Its raw reply is
// kept in reply.txt.
func (r ReviewRunner) runClaude(ctx context.Context, dir, repo string, t route.Tier, prompt string, log io.Writer) (Findings, error) {
	args, err := ClaudeArgs(t)
	if err != nil {
		return Findings{}, err
	}
	var out bytes.Buffer
	cmd := reviewerCmd(ctx, r.Claude, args...)
	cmd.Dir = repo
	cmd.Env = append(reviewerEnv(os.Environ()), "QM_ROLE=reviewer")
	cmd.Stdin = strings.NewReader(prompt + reviewerInstructions + string(FindingsSchema))
	cmd.Stdout = io.MultiWriter(log, &out)
	cmd.Stderr = log
	runErr := cmd.Run()
	_, _ = fmt.Fprintln(log)
	var res struct {
		IsError bool   `json:"is_error"`
		Subtype string `json:"subtype"`
		Result  string `json:"result"`
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		if runErr != nil {
			return Findings{}, fmt.Errorf("claude: %w", runErr)
		}
		return Findings{}, fmt.Errorf("claude: reply is not claude -p JSON: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, claudeReply), []byte(res.Result), 0o600); err != nil {
		return Findings{}, err
	}
	if runErr != nil || res.IsError {
		return Findings{}, fmt.Errorf("claude: %s: %s (exit: %v)", res.Subtype, res.Result, runErr)
	}
	return ParseFindings([]byte(res.Result))
}

// ParseFindings reads a review reply and checks it against FindingsSchema:
// every field present with its type, no other field, known verdict and
// severities. A reply wrapped in a Markdown code fence is accepted.
func ParseFindings(b []byte) (Findings, error) {
	b = unfence(bytes.TrimSpace(b))
	var top map[string]json.RawMessage
	if err := strictUnmarshal(b, &top); err != nil {
		return Findings{}, fmt.Errorf("findings: %w", err)
	}
	if err := exactKeys(top, "verdict", "findings"); err != nil {
		return Findings{}, fmt.Errorf("findings: %w", err)
	}
	var f Findings
	if err := json.Unmarshal(top["verdict"], &f.Verdict); err != nil {
		return Findings{}, fmt.Errorf("findings: verdict: %w", err)
	}
	switch f.Verdict {
	case "mergeable", "mergeable-after-fixes", "not-mergeable":
	default:
		return Findings{}, fmt.Errorf("findings: verdict %q is not mergeable, mergeable-after-fixes or not-mergeable", f.Verdict)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(top["findings"], &items); err != nil {
		return Findings{}, fmt.Errorf("findings: findings is not a list: %w", err)
	}
	f.Findings = make([]Finding, 0, len(items))
	for i, raw := range items {
		var item map[string]json.RawMessage
		if err := strictUnmarshal(raw, &item); err != nil {
			return Findings{}, fmt.Errorf("findings[%d]: %w", i, err)
		}
		if err := exactKeys(item, "severity", "file", "line", "scenario", "fix"); err != nil {
			return Findings{}, fmt.Errorf("findings[%d]: %w", i, err)
		}
		var x Finding
		if err := strictUnmarshal(raw, &x); err != nil {
			return Findings{}, fmt.Errorf("findings[%d]: %w", i, err)
		}
		switch x.Severity {
		case "P1", "P2", "P3":
		default:
			return Findings{}, fmt.Errorf("findings[%d]: severity %q is not P1, P2 or P3", i, x.Severity)
		}
		f.Findings = append(f.Findings, x)
	}
	return f, nil
}

// unfence strips a Markdown code fence around b, if there is one. A reply
// that puts prose around its findings is accepted too, when it holds exactly
// one fenced block: that block is the reply (a reviewer told to answer with
// JSON only sometimes adds a sentence before or after it).
func unfence(b []byte) []byte {
	fence := []byte("```")
	if bytes.HasPrefix(b, []byte("{")) || bytes.Count(b, fence) != 2 {
		return b
	}
	parts := bytes.SplitN(b, fence, 3)
	_, body, ok := bytes.Cut(parts[1], []byte("\n"))
	if !ok {
		return b
	}
	return bytes.TrimSpace(body)
}

// strictUnmarshal decodes exactly one JSON value into v: unknown fields,
// a null object and trailing data are errors.
func strictUnmarshal(b []byte, v any) error {
	if bytes.Equal(b, []byte("null")) {
		return errors.New("null is not an object")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("data after the JSON value")
	}
	return nil
}

// exactKeys checks that m has exactly the given keys, none of them null.
func exactKeys(m map[string]json.RawMessage, keys ...string) error {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			return fmt.Errorf("%s is missing", k)
		}
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return fmt.Errorf("%s is null", k)
		}
	}
	if len(m) != len(keys) {
		for k := range m {
			if !slices.Contains(keys, k) {
				return fmt.Errorf("unexpected field %q", k)
			}
		}
	}
	return nil
}
