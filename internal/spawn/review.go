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
	"io/fs"
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
	"github.com/IZZY-Agency/quartermaster/internal/reviewin"
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
	codexLast    = "last.txt"   // Codex's last message (-o)
	codexLast2   = "last-2.txt" // that of the second try
	claudeReply  = "reply.txt"  // the Claude reviewer's raw reply
)

// ReviewJob is one review: what route decided and the decision-log record to
// write when it ends. ID is set by NewReviewJob.
type ReviewJob struct {
	ID string `json:"id"`
	// Change is what the job's change files cover (reviewin.BuildChange); a
	// job without it was made by an older qm, which handed the reviewer no
	// files, and does not run.
	Change   *reviewin.Summary `json:"change,omitempty"`
	Repo     string            `json:"repo"`
	Decision route.Decision    `json:"decision"`
	Record   route.Record      `json:"record"`
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
// job, the prompt, the findings schema, the change files (change, from
// reviewin.BuildChange; nil for a job that has none) and status "running"; it
// returns the folder. The job's ID is set here. The status is written last,
// and the folder is removed when anything fails, so no job is left running
// without what it needs.
func NewReviewJob(job ReviewJob, prompt []byte, change *reviewin.Change) (string, error) {
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
	folder := filepath.Join(parent, job.ID)
	if err := os.Mkdir(folder, 0o700); err != nil {
		return "", err
	}
	// One cleanup for every failure below: a return sets dir to "" before
	// this runs, so the folder is named by its own variable.
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(folder)
		}
	}()
	var extra []reviewin.File
	if change != nil {
		job.Change, extra = &change.Summary, change.Files
	}
	b, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return "", err
	}
	files := []reviewin.File{{Name: jobFile, Data: append(b, '\n')}, {Name: promptFile, Data: prompt}, {Name: schemaFile, Data: FindingsSchema}}
	for _, f := range append(files, extra...) {
		if err := os.WriteFile(filepath.Join(folder, f.Name), f.Data, 0o600); err != nil {
			return "", err
		}
	}
	if err := writeStatus(folder, StatusRunning); err != nil {
		return "", err
	}
	ok = true
	return folder, nil
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
// message to the file last, shaped by the findings schema, run in repo. The
// prompt argument is "-": codex exec reads the prompt from stdin, so a prompt
// of any size, or one that starts with "-", passes intact.
func CodexArgs(c route.CodexTier, repo, last, schema string) []string {
	return []string{"exec", "-m", c.Model, "-c", fmt.Sprintf("model_reasoning_effort=%q", c.Effort),
		"-s", "read-only", "--ephemeral", "--json", "-o", last,
		"--output-schema", schema, "-C", repo, "-"}
}

// DefaultReviewTimeout is how long a review job may run before its reviewer
// is killed and the job fails.
const DefaultReviewTimeout = 45 * time.Minute

// reviewerSettings turns qm-coordinator off in the Claude reviewer, so its
// hooks do not fire on the review (preflight F16).
const reviewerSettings = `{"enabledPlugins":{"qm-coordinator@quartermaster":false}}`

// ClaudeArgs is the claude command line for a reviewer at tier t, whose job
// folder is dir: print mode, JSON output, reading tools only. --tools leaves
// only Read, Grep and Glob, with no Bash: the change reaches the reviewer as
// files (reviewin.BuildChange makes them, NewReviewJob writes them), so it
// needs no git or gh, which a user's hook or rule can make unusable.
// --strict-mcp-config with no --mcp-config loads no MCP server, edits, writes
// and MCP tools are denied, and dontAsk turns any other call that would
// prompt into a denial. The reviewer starts in the repository under review,
// so it loads only the user's settings (--setting-sources user): that
// repository's project and local settings and its hooks do not load, since
// they are part of the code under review, not something the reviewer should
// run. Its rule files (CLAUDE.md and the like, reviewin.RuleFiles), which
// that also leaves out, are handed over as files named in the prompt
// section. The user's own allow rules and hooks still apply. --add-dir makes the job folder,
// which holds the change files and lies outside the repository, readable even
// where Claude Code blocks file reads outside the working directory. The
// prompt goes on stdin.
func ClaudeArgs(t route.Tier, dir string) ([]string, error) {
	id, ok := policy.ModelIDs[t.Model]
	if !ok {
		return nil, fmt.Errorf("no Claude model id for %q", t.Model)
	}
	return []string{"-p", "--model", id, "--effort", t.Effort, "--output-format", "json",
		"--settings", reviewerSettings, "--permission-mode", "dontAsk",
		"--setting-sources", "user", "--add-dir", dir, "--tools", "Read,Grep,Glob", "--strict-mcp-config",
		"--allowedTools", "Read", "Grep", "Glob",
		"--disallowedTools", "Edit", "Write", "NotebookEdit", "mcp__*"}, nil
}

// ReviewRunner runs review jobs. Codex and Claude are the commands to run;
// Codex "" means it is not installed. CodexRead says whether Codex may be
// asked for its login (a keyring's) and, before a second try, its usage.
type ReviewRunner struct {
	Codex     string
	Claude    string
	CodexRead limits.CodexRead
}

// Run runs the job in dir to its end: on Codex when the decision has a Codex
// tier, else, or when the Codex review fails in any way, on Claude (a Codex
// usage-limit error also marks Codex limited until the reset it states, for the
// login the run started under; a run whose login was switched is first tried
// once more on Codex, see codexReview). When
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
	// The change files qm wrote at spawn time go to every reviewer, Codex and
	// Claude alike. A job without them was made by an older qm.
	if job.Change == nil {
		return r.fail(dir, job, errors.New("the job was created by an older qm, which wrote no change files; start the review again"))
	}
	if err := reviewin.Check(dir); err != nil {
		return r.fail(dir, job, err)
	}
	prompt = append(bytes.TrimRight(prompt, "\n"), reviewin.Section(dir, job.Repo, *job.Change)...)

	d := job.Decision
	var f Findings
	switch {
	case d.Codex != nil && r.Codex != "":
		var notes []string
		f, notes, err = r.codexReview(ctx, dir, job.Repo, *d.Codex, string(prompt), log)
		d.Reasons = append(slices.Clip(d.Reasons), notes...)
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

// codexReview runs the Codex review. A run that failed while Codex was logged
// in anew is tried once more under the new login, when that one is available:
// that the login is another than the one the run started under decides, not
// what Codex said. A run that ended with ctx, or on a signal, is not tried
// again. notes says what was done, for the decision's reasons, and names no
// account.
func (r ReviewRunner) codexReview(ctx context.Context, dir, repo string, c route.CodexTier, prompt string, log io.Writer) (f Findings, notes []string, err error) {
	note := func(format string, args ...any) {
		n := fmt.Sprintf(format, args...)
		_, _ = fmt.Fprintln(log, "qm: "+n)
		notes = append(notes, n)
	}
	login := limits.CodexLogin(r.Codex, r.CodexRead)
	f, err = r.runCodex(ctx, login, filepath.Join(dir, codexLast), repo, c, prompt, log)
	if err == nil || ctx.Err() != nil || signaled(err) {
		return f, nil, err
	}
	now := limits.CodexLogin(r.Codex, r.CodexRead)
	if !now.Differs(login) {
		return f, nil, err
	}
	const switched = "Codex failed (%v) and its login was switched while it ran: "
	st, serr := limits.CodexFor(now, r.Codex, r.CodexRead)
	switch {
	case ctx.Err() != nil:
		return f, nil, err
	case serr != nil:
		note(switched+"no second try, as the usage of the new login cannot be read (%v)", err, serr)
		return f, notes, err
	case !st.Available:
		note(switched+"no second try, as the new login is not available (%s)", err, st.Reason)
		return f, notes, err
	case !now.Current():
		note(switched+"no second try, as it was changed again since", err)
		return f, notes, err
	}
	note(switched+"the review was tried once more on Codex", err)
	f, err = r.runCodex(ctx, now, filepath.Join(dir, codexLast2), repo, c, prompt, log)
	return f, notes, err
}

// signaled reports whether a command ended on a signal.
func signaled(err error) bool {
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return false
	}
	ws, ok := exit.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled()
}

// runCodex runs Codex once, the prompt on stdin and its last message to the
// file last, which is removed first. A usage-limit error marks login, the one
// Codex starts under, limited and returns errCodexLimited.
func (r ReviewRunner) runCodex(ctx context.Context, login limits.Login, last, repo string, c route.CodexTier, prompt string, log io.Writer) (Findings, error) {
	if err := reviewin.Check(filepath.Dir(last)); err != nil {
		return Findings{}, err
	}
	if err := os.Remove(last); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Findings{}, err
	}
	// Codex writes its last message to last (-o), with its own mode when it
	// makes the file; made here first, and set again after the run, the file
	// is private like the rest of the folder.
	if f, err := os.OpenFile(last, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err != nil {
		return Findings{}, err
	} else if err := f.Close(); err != nil {
		return Findings{}, err
	}
	var out bytes.Buffer
	cmd := reviewerCmd(ctx, r.Codex, CodexArgs(c, repo, last, filepath.Join(filepath.Dir(last), schemaFile))...)
	cmd.Dir = repo
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Stdout = io.MultiWriter(log, &out)
	cmd.Stderr = cmd.Stdout
	runErr := cmd.Run()
	_ = os.Chmod(last, 0o600) // a missing file is the read error below
	reply, readErr := os.ReadFile(last)
	if readErr == nil && len(bytes.TrimSpace(reply)) == 0 {
		readErr = errors.New("its last message is empty")
	}
	if runErr == nil && readErr == nil {
		return ParseFindings(reply)
	}
	if reset, ok := limits.ParseCodexLimitError(codexErrors(out.Bytes())); ok {
		if err := limits.MarkCodexLimited(login, reset); err != nil {
			_, _ = fmt.Fprintf(log, "qm: marking Codex limited: %v\n", err)
		}
		if runErr != nil {
			return Findings{}, fmt.Errorf("%w (%w)", errCodexLimited, runErr)
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
	if err := reviewin.Check(dir); err != nil {
		return Findings{}, err
	}
	args, err := ClaudeArgs(t, dir)
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
