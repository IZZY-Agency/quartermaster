package hooks

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/config"
	"github.com/IZZY-Agency/quartermaster/internal/declog"
	"github.com/IZZY-Agency/quartermaster/internal/gitcmd"
	"github.com/IZZY-Agency/quartermaster/internal/paths"
	"github.com/IZZY-Agency/quartermaster/internal/route"
	"github.com/IZZY-Agency/quartermaster/internal/spawn"
	"github.com/IZZY-Agency/quartermaster/internal/transcript"
)

// Texts Claude reads.
const (
	readBudgetReason = "Delegate this exploration to the explorer agent (Agent, subagent_type qm-worker:explorer); read a specific line range yourself only to verify it."
	launchGateReason = "Use qm spawn review or qm spawn worker."
	lspNoteText      = "Use the LSP tool (load it with ToolSearch select:LSP first): workspaceSymbol, findReferences, goToDefinition."
	writeNoteText    = "Use Edit for changes to an existing file."
	explorerNoteText = "Run exploration agents on Haiku: set model \"haiku\" on this Agent call."
)

const (
	// softenAfter is the denials in one phase after which a guard only notes.
	softenAfter = 3
	// softenFor is how long a guard stays softened when no new phase comes
	// first; then it is strict again, its denials counted afresh.
	softenFor = 30 * time.Minute
	// writeNoteMinBytes: a Write over an existing file larger than this gets
	// the write-note.
	writeNoteMinBytes = 2000
	// contextNoteStep is the growth of the context between two
	// context-limit notes.
	contextNoteStep = 50000
)

// hookTimeout bounds one hook call, reading its input included; past it the action passes and an error is
// logged. Claude Code's own timeout for these hooks is 25 s.
var hookTimeout = 20 * time.Second

// call is one hook invocation as a rule sees it.
type call struct {
	in   Input
	cfg  config.Config
	mode string // on or note; an off rule is never called
	st   *State // the session's state, for stateful rules only
	// ask and gate are Jev and the privacy gate, for usesJev and needsKey
	// rules only; ask nil means Jev is off (no TypeSafe key).
	ask  route.Asker
	gate route.Gater
	// softened is set when this call softened the rule for the phase.
	softened bool
}

// result is a rule's verdict; the zero value lets the action pass.
type result struct {
	deny    string
	note    string
	updated map[string]any
	// explain is what a rewrite becomes in note mode.
	explain string
}

// rule is one hook. track, when set, runs on the session state even when
// the rule is off (phase tracking must not stop with the rule).
// coordinator rules return at once in any session qm started (QM_ROLE set);
// usesJev rules get Jev and the privacy gate (ask nil when there is no
// key); needsKey rules return at once without a TypeSafe key, whether or not
// they call Jev themselves (the feedback hooks hand the work to the analyzer).
type rule struct {
	stateful    bool
	coordinator bool
	usesJev     bool
	needsKey    bool
	track       func(*State, Input)
	run         func(*call) (result, error)
}

// rules holds every hook by name; hooks.json entries call them as
// "qm hook <name>".
var rules = map[string]rule{
	"read-budget":     {stateful: true, track: trackPhase, run: readBudget},
	"raw-launch-gate": {stateful: true, run: launchGate},
	"pattern-kill":    {run: patternKillGate},
	"explorer-model":  {run: explorerModel},
	"lsp-note":        {run: lspNote},
	"write-note":      {run: writeNote},
	"context-limit":   {stateful: true, run: contextLimit},
	// Turn hooks (turn.go) keep their own files, outside the session state
	// and its lock: qm watch reads them.
	"turn-start": {run: turnStart},
	"turn-stop":  {run: turnStop},
	// Coordinator rules (coordinator.go); reviewer-model is in both plugins.
	"reviewer-model":   {usesJev: true, run: reviewerModel},
	"ask-gate":         {coordinator: true, usesJev: true, needsKey: true, run: askGate},
	"feedback":         {coordinator: true, needsKey: true, run: feedback},
	"feedback-deliver": {coordinator: true, needsKey: true, run: feedbackDeliver},
}

// Names lists the hook names, sorted.
func Names() []string {
	out := make([]string, 0, len(rules))
	for n := range rules {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Handle runs hook name on the input read from stdin and returns what to
// print: nil lets the action through. It never fails: a bad input, an
// unknown hook, a rule error, a panic or a call past hookTimeout passes the
// action and is logged as a kind "error" Record.
func Handle(name string, stdin io.Reader, cfg config.Config) []byte {
	deadline := time.After(hookTimeout)
	var in Input
	read := make(chan error, 1)
	go func() {
		got, err := Read(stdin)
		if err == nil {
			in = got // read below only after the channel says so
		}
		read <- err
	}()
	select {
	case err := <-read:
		if err != nil {
			logRecord(Input{}, "error", name, "", err.Error())
			return nil
		}
	case <-deadline:
		logRecord(Input{}, "error", name, "", fmt.Sprintf("timed out after %v reading the input", hookTimeout))
		return nil
	}
	r, ok := rules[name]
	if !ok {
		logRecord(in, "error", name, "", fmt.Sprintf("unknown hook %q", name))
		return nil
	}
	done := make(chan []byte, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				logRecord(in, "error", name, "", fmt.Sprintf("panic: %v", p))
				done <- nil
			}
		}()
		out, err := handle(name, r, in, cfg)
		if err != nil {
			logRecord(in, "error", name, "", err.Error())
			out = nil
		}
		done <- out
	}()
	select {
	case out := <-done:
		return out
	case <-deadline:
		logRecord(in, "error", name, "", fmt.Sprintf("timed out after %v", hookTimeout))
		return nil
	}
}

func handle(name string, r rule, in Input, cfg config.Config) ([]byte, error) {
	if r.coordinator && os.Getenv("QM_ROLE") != "" {
		return nil, nil
	}
	mode := cfg.Rule(name)
	if mode != "on" && mode != "note" {
		mode = "off"
	}
	if mode == "off" && r.track == nil {
		return nil, nil
	}
	c := &call{in: in, cfg: cfg, mode: mode}
	if r.usesJev || r.needsKey {
		if c.ask, c.gate = jev(cfg); c.ask == nil && r.needsKey {
			return nil, nil
		}
	}
	var res result
	var err error
	if r.stateful {
		err = withState(in.SessionID, func(s *State) error {
			if r.track != nil {
				r.track(s, in)
			}
			if mode == "off" {
				return nil
			}
			c.st = s
			res, err = r.run(c)
			return err
		})
	} else {
		res, err = r.run(c)
	}
	if err != nil {
		return nil, err
	}
	if c.softened {
		// Side effects run after the state lock is released.
		logRecord(in, "guard", name, "softened", "")
		if nerr := Notify(cfg, sessionName(in), fmt.Sprintf("%s softened after %d denials", name, softenAfter)); nerr != nil {
			logRecord(in, "error", name, "", nerr.Error())
		}
	}
	if in.HookEventName == "Stop" && mode == "note" && res.deny != "" {
		// Any Stop output makes Claude continue, so a note would act as a
		// block: note mode lets the stop through and only logs it.
		logRecord(in, "guard", name, "stop-allowed", res.deny)
		return nil, nil
	}
	return render(in, mode, res)
}

// render turns a verdict into hook output; note mode turns a deny or a
// rewrite into a note.
func render(in Input, mode string, res result) ([]byte, error) {
	event := in.HookEventName
	if event == "" {
		event = "PreToolUse"
	}
	if event == "Stop" {
		// Any Stop output makes Claude continue: only a block may give one.
		if res.deny != "" && mode != "note" {
			return BlockStop(res.deny), nil
		}
		return nil, nil
	}
	switch {
	case res.deny != "" && mode == "note":
		return Note(event, res.deny), nil
	case res.deny != "":
		return Deny(event, res.deny), nil
	case res.updated != nil && mode == "note":
		return Note(event, res.explain), nil
	case res.updated != nil:
		return Rewrite(event, res.updated, res.note)
	case res.note != "":
		return Note(event, res.note), nil
	}
	return nil, nil
}

// clock is the time guardDeny reads; tests replace it.
var clock = time.Now

// guardDeny denies with reason, counting the denial for the phase: the
// softenAfter-th denial softens the rule for the rest of the phase, or for
// softenFor, whichever ends first, during which it only notes. In note mode
// nothing is counted.
func (c *call) guardDeny(rule, reason string) result {
	if c.mode == "note" {
		return result{deny: reason}
	}
	now := clock()
	if c.st.Soft[rule] && now.Sub(time.Unix(c.st.SoftAt[rule], 0)) >= softenFor {
		c.st.Soft[rule] = false
		c.st.Denials[rule] = 0
		delete(c.st.SoftAt, rule)
	}
	if c.st.Soft[rule] {
		return result{note: reason}
	}
	c.st.Denials[rule]++
	if c.st.Denials[rule] >= softenAfter {
		c.st.Soft[rule] = true
		c.st.SoftAt[rule] = now.Unix()
		c.softened = true
	}
	return result{deny: reason}
}

// trackPhase starts a new phase when the worker writes or edits a file.
func trackPhase(s *State, in Input) {
	if in.AgentID != "" {
		return
	}
	if in.ToolName == "Write" || in.ToolName == "Edit" {
		s.newPhase()
	}
}

// readBudget denies exploration reads over cfg.ReadBudget in one phase.
// Subagents (the explorer itself) are not counted, nor is the worker's read of
// its own brief.
func readBudget(c *call) (result, error) {
	if c.in.AgentID != "" || !isExploration(c.in.ToolName, c.in.ToolInput) || isBriefRead(c.in) {
		return result{}, nil
	}
	c.st.Reads++
	budget := c.cfg.ReadBudget
	if budget <= 0 {
		budget = config.Defaults().ReadBudget
	}
	if c.st.Reads <= budget {
		return result{}, nil
	}
	return c.guardDeny("read-budget", readBudgetReason), nil
}

// isBriefRead tells whether a call is a Read of the file the worker was given
// its task in (QM_BRIEF, set by its launch script): that path exactly, and a
// file there that is not a symlink to another.
func isBriefRead(in Input) bool {
	brief := os.Getenv(spawn.BriefEnv)
	path, _ := in.ToolInput["file_path"].(string)
	if in.ToolName != "Read" || brief == "" || path != brief {
		return false
	}
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode().IsRegular()
}

// launchGate denies raw launches of Codex reviews and Claude sessions.
func launchGate(c *call) (result, error) {
	cmd, _ := c.in.ToolInput["command"].(string)
	if c.in.ToolName != "Bash" || !rawLaunch(cmd) {
		return result{}, nil
	}
	return c.guardDeny("raw-launch-gate", launchGateReason), nil
}

// explorerModel runs Explore and explorer agents on Haiku.
func explorerModel(c *call) (result, error) {
	if c.in.ToolName != "Agent" || !isExplorer(c.in.ToolInput) {
		return result{}, nil
	}
	if m, _ := c.in.ToolInput["model"].(string); m == "haiku" {
		return result{}, nil
	}
	updated := make(map[string]any, len(c.in.ToolInput)+1)
	for k, v := range c.in.ToolInput {
		updated[k] = v
	}
	updated["model"] = "haiku"
	return result{updated: updated, explain: explorerNoteText}, nil
}

var symbolPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{2,}$`)

// lspNote points symbol lookups at the LSP tool, when the worker has an LSP
// plugin (QM_LSP): a Grep, or a Bash grep or rg over code (some Claude Code
// builds have no Grep tool).
func lspNote(c *call) (result, error) {
	if os.Getenv("QM_LSP") == "" {
		return result{}, nil
	}
	var p string
	switch c.in.ToolName {
	case "Grep":
		p, _ = c.in.ToolInput["pattern"].(string)
	case "Bash":
		cmd, _ := c.in.ToolInput["command"].(string)
		var ok bool
		if p, ok = bashSearch(cmd); !ok {
			return result{}, nil
		}
	default:
		return result{}, nil
	}
	if symbolPattern.MatchString(p) {
		return result{note: lspNoteText}, nil
	}
	for _, prefix := range []string{"func ", "def ", "class ", "type "} {
		if strings.HasPrefix(p, prefix) {
			return result{note: lspNoteText}, nil
		}
	}
	return result{}, nil
}

// writeNote suggests Edit when Write replaces an existing, sizeable file.
func writeNote(c *call) (result, error) {
	p, _ := c.in.ToolInput["file_path"].(string)
	if c.in.ToolName != "Write" || p == "" {
		return result{}, nil
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(c.in.CWD, p)
	}
	fi, err := os.Stat(p)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() <= writeNoteMinBytes {
		return result{}, nil
	}
	return result{note: writeNoteText}, nil
}

// contextLimit tells the worker to hand off once its context passes the
// config's handoff share (worker.handoff_percent, 85% by default; the
// default too when the value is out of range) of its compact window
// (QM_AUTOCOMPACT), and again every contextNoteStep tokens after that.
func contextLimit(c *call) (result, error) {
	window, err := strconv.Atoi(os.Getenv("QM_AUTOCOMPACT"))
	if err != nil || window <= 0 || c.in.AgentID != "" || c.in.TranscriptPath == "" {
		return result{}, nil
	}
	last, err := transcript.LastContext(c.in.TranscriptPath)
	used := last.Context
	if err != nil || used == 0 {
		return result{}, err
	}
	share, _ := c.cfg.HandoffShare()
	// Whole tokens, rounded up: 85% of 250,000 is 212,500, not a float a
	// hair under it.
	if used < int(math.Ceil(share*float64(window)-1e-6)) {
		return result{}, nil
	}
	// A context below the last note means a compaction since: note again.
	if c.st.LastNoteCtx > 0 && used >= c.st.LastNoteCtx && used-c.st.LastNoteCtx < contextNoteStep {
		return result{}, nil
	}
	state, err := paths.StateDir()
	if err != nil {
		return result{}, err
	}
	// A worker of a qm up project writes its note to the project's folder,
	// <state>/handoff/<project id>/, where qm up reads it.
	dir := filepath.Join(state, "handoff")
	if pid := os.Getenv("QM_PROJECT_ID"); pid != "" && pid != "." && pid != ".." && !strings.ContainsAny(pid, "/\\\x00") {
		dir = filepath.Join(dir, pid)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return result{}, err
	}
	name := os.Getenv("QM_SESSION_NAME")
	if name == "" || strings.ContainsAny(name, `/\`) {
		name = c.in.SessionID
	}
	c.st.LastNoteCtx = used
	return result{note: fmt.Sprintf("Context is at %d tokens. Finish the current step, write the handoff note to %s, then send the coordinator a message with SendMessage.",
		used, filepath.Join(dir, name+".md"))}, nil
}

// isExplorer reports whether an Agent call starts an exploration agent:
// Explore, or the qm-worker explorer.
func isExplorer(input map[string]any) bool {
	typ, _ := input["subagent_type"].(string)
	return typ == "Explore" || typ == "explorer" || strings.HasSuffix(typ, ":explorer")
}

// sessionName is the worker's name from qm spawn worker, else the session id.
func sessionName(in Input) string {
	if n := os.Getenv("QM_SESSION_NAME"); n != "" {
		return n
	}
	return in.SessionID
}

// logRecord appends a guard or error Record. A failure to log has nowhere
// left to go: hooks never fail the action.
func logRecord(in Input, kind, rule, action, detail string) {
	_ = declog.Append(route.Record{
		Time:    time.Now().UTC(),
		Kind:    kind,
		Project: project(in.CWD),
		Session: in.SessionID,
		Name:    os.Getenv("QM_SESSION_NAME"),
		Rule:    rule,
		Action:  action,
		Detail:  detail,
	})
}

// projects caches project per cwd for the life of the process.
var projects sync.Map

// project is the repository top-level folder of cwd, else cwd itself. The
// git lookup runs once per cwd per process.
func project(cwd string) string {
	if cwd == "" {
		return ""
	}
	if p, ok := projects.Load(cwd); ok {
		return p.(string)
	}
	p := gitTop(cwd)
	projects.Store(cwd, p)
	return p
}

func gitTop(cwd string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := gitcmd.Command(ctx, cwd, "rev-parse", "--show-toplevel").Output()
	if err != nil || len(bytes.TrimSpace(out)) == 0 {
		return cwd
	}
	return string(bytes.TrimSpace(out))
}
