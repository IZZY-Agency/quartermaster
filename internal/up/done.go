package up

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// DirRunner runs bin with args in folder dir and returns its standard
// output; tests replace it. gh has no -C flag, so its folder must be set.
type DirRunner func(ctx context.Context, dir, bin string, args ...string) ([]byte, error)

// ExecDir is the DirRunner that runs the real command. A failure carries the
// command's standard error.
func ExecDir(ctx context.Context, dir, bin string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%s: %s: %w", bin, msg, err)
		}
		return nil, fmt.Errorf("%s: %w", bin, err)
	}
	return stdout.Bytes(), nil
}

// checkTimeout bounds each git or gh call of Finished and currentBranch.
const checkTimeout = 30 * time.Second

// ghPRLimit is how many pull requests of a branch Finished reads.
const ghPRLimit = "20"

// Finished returns the check that says why a worker of p is finished (spec
// 6, row 2), or "" when it is not: "marked done" (qm up done), "worktree
// gone" (its folder is missing), "merged" or "closed" (the state of its
// branch's newest pull request created after the worker's line started
// (its first session, before any fresh restart), asked of gh in the
// Record's project folder: an older pull request of a reused branch name is
// not this worker's), or,
// when gh is missing or fails, "merged (git)" (the branch is an ancestor of
// the default branch, at a tip committed after the worker started). A
// worker on no branch, a detached HEAD or the default branch is never read as merged or closed: a worker spawned on the
// default branch would always look merged.
func Finished(p Project, run DirRunner) func(Candidate) string {
	return func(c Candidate) string {
		if _, ok := p.Done[c.SessionID]; ok {
			return "marked done"
		}
		if c.Cwd != "" {
			if _, err := os.Stat(c.Cwd); errors.Is(err, fs.ErrNotExist) {
				return "worktree gone"
			}
		}
		if run == nil || c.Cwd == "" || c.Branch == "" || c.Branch == "HEAD" {
			return ""
		}
		ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
		defer cancel()
		def := defaultBranch(ctx, run, c.Cwd)
		if c.Branch == def || def == "" && (c.Branch == "main" || c.Branch == "master") {
			return ""
		}
		dir := c.Cwd
		if c.Record != nil && c.Record.Project != "" {
			dir = c.Record.Project
		}
		out, err := run(ctx, dir, "gh", "pr", "list", "--head", c.Branch, "--state", "all", "--json", "state,createdAt", "--limit", ghPRLimit)
		if err == nil {
			var prs []struct {
				State     string    `json:"state"`
				CreatedAt time.Time `json:"createdAt"`
			}
			if json.Unmarshal(out, &prs) == nil {
				newest := -1
				for i, pr := range prs {
					if pr.CreatedAt.Before(c.lineStart()) {
						continue // opened before this worker's line started
					}
					if newest < 0 || pr.CreatedAt.After(prs[newest].CreatedAt) {
						newest = i
					}
				}
				if newest < 0 {
					return ""
				}
				switch prs[newest].State {
				case "MERGED":
					return "merged"
				case "CLOSED":
					return "closed"
				}
				return ""
			}
		}
		if def == "" {
			return ""
		}
		if _, err := run(ctx, c.Cwd, "git", "merge-base", "--is-ancestor", c.Branch, def); err != nil {
			return ""
		}
		// A branch with no commits of its own yet is an ancestor too: merged
		// only when its tip is newer than the worker's start.
		out, err = run(ctx, c.Cwd, "git", "log", "-1", "--format=%ct", c.Branch)
		if err != nil {
			return ""
		}
		secs, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
		if err != nil || !time.Unix(secs, 0).After(c.lineStart()) {
			return ""
		}
		return "merged (git)"
	}
}

// defaultBranch is the default branch of the repository at dir: the branch
// origin/HEAD points to, else main or master when one exists; "" when none
// is found.
func defaultBranch(ctx context.Context, run DirRunner, dir string) string {
	if out, err := run(ctx, dir, "git", "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if b := strings.TrimPrefix(strings.TrimSpace(string(out)), "origin/"); b != "" {
			return b
		}
	}
	for _, b := range []string{"main", "master"} {
		if _, err := run(ctx, dir, "git", "rev-parse", "--verify", "--quiet", "refs/heads/"+b); err == nil {
			return b
		}
	}
	return ""
}

// currentBranch is the branch checked out in dir; "" when run is nil, dir is
// empty or missing, or git names none (a detached HEAD, not a repository).
func currentBranch(run DirRunner, dir string) string {
	if run == nil || dir == "" {
		return ""
	}
	if _, err := os.Stat(dir); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	defer cancel()
	out, err := run(ctx, dir, "git", "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
