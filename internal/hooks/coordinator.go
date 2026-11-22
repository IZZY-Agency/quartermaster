package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/config"
	"github.com/IZZY-Agency/quartermaster/internal/declog"
	"github.com/IZZY-Agency/quartermaster/internal/limits"
	"github.com/IZZY-Agency/quartermaster/internal/paths"
	"github.com/IZZY-Agency/quartermaster/internal/privacy"
	"github.com/IZZY-Agency/quartermaster/internal/reviewin"
	"github.com/IZZY-Agency/quartermaster/internal/route"
	"github.com/IZZY-Agency/quartermaster/internal/typesafe"
)

// jev builds Jev and the privacy gate for a hook. ask is an untyped nil when
// there is no TypeSafe key (Jev off), never a nil *typesafe.Client inside the
// interface. Tests replace it.
var jev = func(cfg config.Config) (ask route.Asker, gate route.Gater) {
	c := typesafe.New(cfg.Key())
	if c == nil {
		return nil, nil
	}
	return c, privacy.New(privacy.Options{
		Helper:     privacy.FindHelper(),
		Gitleaks:   privacy.FindGitleaks(),
		LocalModel: cfg.Privacy.LocalModel,
		Clients:    cfg.Privacy.Clients,
	})
}

// reviewDescription marks an Agent call that starts a reviewer.
var reviewDescription = regexp.MustCompile(`(?i)\breview`)

// agentCodex is the Codex state reviewer-model routes with: a reviewer
// started through the Agent tool is a Claude reviewer.
var agentCodex = limits.CodexState{Reason: "a reviewer started through the Agent tool runs on Claude"}

// reviewerModel routes a Claude reviewer the coordinator or a worker starts
// through the Agent tool: an Agent call whose description says review and
// that sets no model gets the review table's Claude tier (design 5.4).
// Exploration agents stay with explorer-model. It lets the call through
// untouched, and logs nothing, when qm cannot size the work: cwd on the
// default branch or a detached HEAD (a coordinator in the main checkout
// reviewing a worktree), or an empty diff. The review Record that counts the
// round is kept until the Agent call has run (PostToolUse).
func reviewerModel(c *call) (result, error) {
	if c.in.HookEventName == "PostToolUse" {
		return result{}, logPendingReview(c.in)
	}
	desc, _ := c.in.ToolInput["description"].(string)
	if c.in.ToolName != "Agent" || !reviewDescription.MatchString(desc) || isExplorer(c.in.ToolInput) {
		return result{}, nil
	}
	if m, _ := c.in.ToolInput["model"].(string); m != "" {
		return result{}, nil
	}
	prompt, _ := c.in.ToolInput["prompt"].(string)
	in, branch, err := agentReviewInput(c.in.CWD, prompt)
	if errors.Is(err, errNoReviewDiff) {
		return result{}, nil
	}
	if err != nil {
		return result{}, err
	}
	claude, err := limits.Claude()
	if err != nil {
		// No limit state: no downshift, as without the status-line wrapper.
		logRecord(c.in, "error", "reviewer-model", "", "Claude limits unknown: "+err.Error())
		claude = limits.ClaudeState{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookTimeout)
	defer cancel()
	d, err := route.DecideReview(ctx, in, c.ask, c.gate, agentCodex, claude)
	if err != nil {
		return result{}, err
	}
	// Logged like qm spawn review, so Agent reviews count as rounds.
	rec := route.Record{
		Project: in.Repo,
		Branch:  branch,
		Session: c.in.SessionID,
		Name:    os.Getenv("QM_SESSION_NAME"),
		Round:   in.Round,
		Detail:  agentReviewDetail,
	}
	if err := keepPendingReview(c.in.ToolUseID, d, rec); err != nil {
		logRecord(c.in, "error", "reviewer-model", "", err.Error())
	}
	updated := make(map[string]any, len(c.in.ToolInput)+2)
	for k, v := range c.in.ToolInput {
		updated[k] = v
	}
	updated["model"] = d.Tier.Model
	updated["effort"] = d.Tier.Effort
	return result{updated: updated, explain: fmt.Sprintf(
		"Run this reviewer on %s with effort %s (review row %s): set model and effort on this Agent call.",
		d.Tier.Model, d.Tier.Effort, d.Row)}, nil
}

// pendingReviewAge is how long a pending review Record waits for its
// PostToolUse (an Agent call that was refused or failed never gets one).
const pendingReviewAge = 24 * time.Hour

// pendingDir is where review Records wait for their Agent call to run.
func pendingDir() (string, error) {
	state, err := paths.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(state, "pending-reviews"), nil
}

type pendingReview struct {
	Decision route.Decision `json:"decision"`
	Record   route.Record   `json:"record"`
}

// keepPendingReview stores the review Record of an Agent call until it has
// run, under its tool_use_id. Without an id the Record is logged at once.
// Pending Records older than pendingReviewAge are swept.
func keepPendingReview(toolUseID string, d route.Decision, rec route.Record) error {
	if !sessionIDPattern.MatchString(toolUseID) {
		return declog.AppendDecision(d, rec)
	}
	dir, err := pendingDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	sweepOld(dir, pendingReviewAge)
	b, err := json.Marshal(pendingReview{d, rec})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, toolUseID+".json"), b, 0o600)
}

// logPendingReview logs the review Record kept for this Agent call, now that
// it has run.
func logPendingReview(in Input) error {
	if in.ToolName != "Agent" || !sessionIDPattern.MatchString(in.ToolUseID) {
		return nil
	}
	dir, err := pendingDir()
	if err != nil {
		return err
	}
	path := filepath.Join(dir, in.ToolUseID+".json")
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	// Removed first: a second PostToolUse for the same call logs nothing.
	if err := os.Remove(path); err != nil {
		return err
	}
	var p pendingReview
	if err := json.Unmarshal(b, &p); err != nil {
		return fmt.Errorf("pending review %s: %w", in.ToolUseID, err)
	}
	p.Record.Time = time.Now().UTC()
	return declog.AppendDecision(p.Decision, p.Record)
}

// sweepOld removes the files in dir last changed more than age ago.
func sweepOld(dir string, age time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && fi.Mode().IsRegular() && time.Since(fi.ModTime()) > age {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// agentReviewDetail is the Detail of the review Record an Agent review logs:
// its verdict and findings are not known to qm.
const agentReviewDetail = `{"verdict":"agent","p1":0,"p2":0,"p3":0}`

// errNoReviewDiff: qm cannot size the review from cwd. cwd is not in a git
// repository, the repository has no base (reviewin.Base), cwd is on the
// default branch or a detached HEAD, or the diff is empty. All are ordinary
// states, so the hook passes quietly.
var errNoReviewDiff = errors.New("no diff to review")

// agentReviewInput is the review input for a reviewer prompt in the
// repository at cwd, and the repository's branch: the diff of the branch
// against reviewin.Base and the round counted from the decision log.
// reviewin reads the prompt from a file, so it goes through a private
// temporary file, removed at once.
func agentReviewInput(cwd, prompt string) (route.ReviewInput, string, error) {
	if cwd == "" {
		cwd = "."
	}
	top, branch, err := reviewin.Repo(cwd)
	if err != nil {
		if strings.Contains(err.Error(), "not a git repository") {
			return route.ReviewInput{}, "", errNoReviewDiff
		}
		return route.ReviewInput{}, "", err
	}
	if branch == "" || branch == reviewin.DefaultBranch(top) {
		return route.ReviewInput{}, "", errNoReviewDiff
	}
	base := reviewin.Base(top)
	verify := exec.Command("git", "-C", top, "rev-parse", "--verify", "--quiet", base+"^{commit}")
	if err := verify.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return route.ReviewInput{}, "", errNoReviewDiff
		}
		return route.ReviewInput{}, "", fmt.Errorf("git rev-parse %s: %w", base, err)
	}
	f, err := os.CreateTemp("", "qm-review-prompt-*.txt")
	if err != nil {
		return route.ReviewInput{}, "", err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	_, werr := f.WriteString(prompt)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return route.ReviewInput{}, "", werr
	}
	in, err := reviewin.For(top, base, f.Name(), 0)
	if err == nil && in.DiffLines == 0 && in.DiffFiles == 0 {
		return route.ReviewInput{}, "", errNoReviewDiff
	}
	return in, branch, err
}
