package up

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/paths"
	"github.com/IZZY-Agency/quartermaster/internal/spawn"
)

// Limits of the owner's messages in the summary: how many, and how long.
const (
	summaryMessages   = 3
	summaryMessageLen = 2000
)

// GitFacts are what the summary says about the project's repository. Each
// field is empty when its command failed or git or gh is missing. OpenPRs
// read "#<number> <title> (<branch>)".
type GitFacts struct {
	Branch string
	// Dirty is the count of uncommitted files; read only when DirtyKnown.
	Dirty      int
	DirtyKnown bool
	Worktrees  []string
	OpenPRs    []string
}

// ReadGit reads the facts of the repository at root through run. A failed
// command leaves its field empty.
func ReadGit(root string, run DirRunner) GitFacts {
	var g GitFacts
	if run == nil {
		return g
	}
	call := func(bin string, args ...string) (string, bool) {
		ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
		defer cancel()
		out, err := run(ctx, root, bin, args...)
		return string(out), err == nil
	}
	if out, ok := call("git", "rev-parse", "--abbrev-ref", "HEAD"); ok {
		g.Branch = strings.TrimSpace(out)
	}
	if out, ok := call("git", "status", "--porcelain"); ok {
		g.DirtyKnown = true
		for l := range strings.SplitSeq(out, "\n") {
			if strings.TrimSpace(l) != "" {
				g.Dirty++
			}
		}
	}
	if out, ok := call("git", "worktree", "list", "--porcelain"); ok {
		for l := range strings.SplitSeq(out, "\n") {
			if p, found := strings.CutPrefix(l, "worktree "); found {
				g.Worktrees = append(g.Worktrees, p)
			}
		}
	}
	if out, ok := call("gh", "pr", "list", "--author", "@me", "--json", "number,title,headRefName"); ok {
		var prs []struct {
			Number      int    `json:"number"`
			Title       string `json:"title"`
			HeadRefName string `json:"headRefName"`
		}
		if json.Unmarshal([]byte(out), &prs) == nil {
			for _, p := range prs {
				g.OpenPRs = append(g.OpenPRs, fmt.Sprintf("#%d %s (%s)", p.Number, oneLine(p.Title), p.HeadRefName))
			}
		}
	}
	return g
}

// SummaryInput is what Summary renders. Old is the previous coordinator
// (its Handoff is the note's path), HandoffNote the text of that note when
// there is one, Threshold the coordinator's context limit in tokens, Steps
// the decisions for the project's sessions, OwnerMessages the owner's
// messages of the old transcript, oldest first.
type SummaryInput struct {
	Project       Project
	Old           Candidate
	Threshold     int
	Steps         []Step
	HandoffNote   string
	OwnerMessages []string
	Git           GitFacts
	Now           time.Time
}

// Summary is the restart summary of spec 7, the first prompt of a fresh
// coordinator. It is deterministic: workers sorted by name, times in UTC,
// contexts in thousands of tokens rounded down.
func Summary(in SummaryInput) string {
	var b strings.Builder
	transcriptPath := in.Old.Transcript
	if transcriptPath == "" {
		transcriptPath = "missing"
	}
	fmt.Fprintf(&b, "# Restart summary: %s (qm up, %s)\n\n", in.Project.Workspace, utc(in.Now))
	oldCtx := "unknown"
	if !in.Old.Last.At.IsZero() {
		oldCtx = kTokens(in.Old.Last.Context) + " tokens"
	}
	b.WriteString("You are the coordinator of this project, started fresh by qm up: the previous\n")
	fmt.Fprintf(&b, "coordinator's context was %s (threshold %s). Its transcript is\n", oldCtx, kTokens(in.Threshold))
	fmt.Fprintf(&b, "%s; read from it only what\n", transcriptPath)
	b.WriteString("this summary lacks. Workers are addressed by their session names, as before.\n\n")

	b.WriteString("## Workers\n")
	var steps []Step
	for _, s := range in.Steps {
		if s.Role == RoleWorker {
			steps = append(steps, s)
		}
	}
	if len(steps) == 0 {
		b.WriteString("none\n")
	} else {
		slices.SortStableFunc(steps, func(a, c Step) int { return strings.Compare(a.Name, c.Name) })
		b.WriteString("qm up wrote this before it started the workers: the states are its plan,\n")
		b.WriteString("and a worker whose launch then failed is still listed as planned. A worker\n")
		b.WriteString("that does not answer may not have started: check the workspace's sessions.\n\n")
		b.WriteString("| name | state | branch | pull request | last activity | context | qm up plan |\n")
		b.WriteString("|---|---|---|---|---|---|---|\n")
		for _, s := range steps {
			state, did := workerState(s)
			last, ctxk := "unknown", "unknown"
			if !s.Last.At.IsZero() {
				last, ctxk = utc(s.Last.At), kTokens(s.Last.Context)
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s |\n",
				cell(s.Name), cell(state), cell(orDash(s.Branch)), cell(orDash(prOf(in.Git.OpenPRs, s.Branch))), last, ctxk, cell(did))
		}
	}

	b.WriteString("\n## Previous coordinator's handoff note\n")
	switch {
	case in.Old.Handoff == "" && in.HandoffNote == "":
		b.WriteString("none\n")
	default:
		if in.Old.Handoff != "" {
			fmt.Fprintf(&b, "%s\n", in.Old.Handoff)
		}
		if note := strings.TrimSpace(in.HandoffNote); note != "" {
			fmt.Fprintf(&b, "\n%s\n", note)
		}
	}

	b.WriteString("\n## The owner's last messages\n")
	msgs := in.OwnerMessages
	if len(msgs) > summaryMessages {
		msgs = msgs[len(msgs)-summaryMessages:]
	}
	if len(msgs) == 0 {
		b.WriteString("none\n")
	}
	for i, m := range msgs {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "Message %d:\n", i+1)
		for l := range strings.SplitSeq(cut(strings.TrimSpace(m), summaryMessageLen), "\n") {
			fmt.Fprintf(&b, "> %s\n", l)
		}
	}

	b.WriteString("\n## Git\n")
	g := in.Git
	if g.Branch == "" && !g.DirtyKnown && len(g.Worktrees) == 0 && len(g.OpenPRs) == 0 {
		b.WriteString("none\n")
		return b.String()
	}
	if g.Branch != "" {
		fmt.Fprintf(&b, "- branch: %s\n", g.Branch)
	}
	switch {
	case g.DirtyKnown:
		fmt.Fprintf(&b, "- uncommitted files: %d\n", g.Dirty)
	case g.Branch != "":
		b.WriteString("- uncommitted files: unknown\n")
	}
	if len(g.Worktrees) > 0 {
		b.WriteString("- worktrees:\n")
		for _, w := range g.Worktrees {
			fmt.Fprintf(&b, "  - %s\n", w)
		}
	}
	if len(g.OpenPRs) > 0 {
		b.WriteString("- open pull requests:\n")
		for _, p := range g.OpenPRs {
			fmt.Fprintf(&b, "  - %s\n", p)
		}
	}
	return b.String()
}

// workerState is a worker's state column and the "qm up plan" column for
// its decision.
func workerState(s Step) (state, did string) {
	switch s.Action {
	case Leave:
		return "running", "left running"
	case Resume:
		return "resumed", "resumed: " + s.Why
	case Fresh:
		return "started fresh", "started fresh: " + s.Why
	}
	switch {
	case strings.HasPrefix(s.Why, WhyFinished):
		return "finished (" + strings.TrimPrefix(s.Why, WhyFinished) + ")", "left alone"
	case strings.HasPrefix(s.Why, WhyNotTrusted):
		return "not trusted", "not started: " + s.Why
	}
	return "lost", "not started: " + s.Why
}

// prOf is the open pull request of branch in prs ("" when none).
func prOf(prs []string, branch string) string {
	if branch == "" {
		return ""
	}
	for _, p := range prs {
		if strings.HasSuffix(p, " ("+branch+")") {
			num, _, _ := strings.Cut(p, " ")
			return num
		}
	}
	return ""
}

func utc(t time.Time) string { return t.UTC().Format("2006-01-02 15:04") + "Z" }

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// cell makes s safe inside a table cell.
func cell(s string) string {
	return strings.ReplaceAll(oneLine(s), "|", "\\|")
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// cut shortens s to at most n characters.
func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// WriteSummary writes text to <state>/up/<projectID>/summary-<unixnano>.md,
// mode 0600 in a 0700 folder, never over an existing file, and returns its
// path. Summaries there older than spawn.ScriptAge are removed first.
func WriteSummary(projectID, text string) (string, error) {
	if projectID == "" || strings.ContainsAny(projectID, "/\x00") || projectID == "." || projectID == ".." {
		return "", fmt.Errorf("summary: bad project id %q", projectID)
	}
	state, err := paths.StateDir()
	if err != nil {
		return "", fmt.Errorf("summary: %w", err)
	}
	dir := filepath.Join(state, "up", projectID)
	if err := ensureDir(dir); err != nil {
		return "", fmt.Errorf("summary: %w", err)
	}
	sweepSummaries(dir, spawn.ScriptAge)
	tmp, err := os.CreateTemp(dir, ".summary-*.tmp") // mode 0600
	if err != nil {
		return "", fmt.Errorf("summary: %w", err)
	}
	done := false
	defer func() {
		if !done {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.WriteString(text); err != nil {
		return "", fmt.Errorf("summary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("summary: %w", err)
	}
	for range 100 {
		path := filepath.Join(dir, fmt.Sprintf("summary-%d.md", time.Now().UnixNano()))
		if _, err := os.Lstat(path); err == nil {
			continue
		}
		if err := os.Rename(tmp.Name(), path); err != nil {
			return "", fmt.Errorf("summary: %w", err)
		}
		done = true
		return path, nil
	}
	return "", errors.New("summary: no free file name")
}

// sweepSummaries removes the summaries in dir older than age.
func sweepSummaries(dir string, age time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		fi, err := e.Info()
		n := e.Name()
		ours := (strings.HasPrefix(n, "summary-") && strings.HasSuffix(n, ".md")) ||
			(strings.HasPrefix(n, ".summary-") && strings.HasSuffix(n, ".tmp")) // left by a crash
		if err == nil && fi.Mode().IsRegular() && ours && time.Since(fi.ModTime()) > age {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
