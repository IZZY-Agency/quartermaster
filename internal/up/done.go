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
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/gitcmd"
)

// DirRunner runs bin with args in folder dir and returns its standard
// output; tests replace it. gh has no -C flag, so its folder must be set.
type DirRunner func(ctx context.Context, dir, bin string, args ...string) ([]byte, error)

// ExecDir is the DirRunner that runs the real command. A failure carries the
// command's standard error. git, and gh, which asks git about its folder,
// run without the GIT_* variables of qm's environment (gitcmd). gh runs
// without GH_REPO too: it would answer for that repository instead of the
// folder's. GH_HOST stays: it only picks among the folder's own remotes.
func ExecDir(ctx context.Context, dir, bin string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	if bin == "git" || bin == "gh" {
		cmd.Env = slices.DeleteFunc(gitcmd.Env(cmd.Environ()), func(kv string) bool { return strings.HasPrefix(kv, "GH_REPO=") })
	}
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
// Record's project folder, when that is of the same repository as the
// worker's folder: an older pull request of a reused branch name is not this
// worker's), or, when gh is not asked, is missing or fails, "merged (git)"
// (the branch is an ancestor of the default branch, at a tip committed after
// the worker started). A worker on no branch, a detached HEAD or the default
// branch is never read as merged or closed: a worker spawned on the default
// branch would always look merged.
func Finished(p Project, run DirRunner) func(Candidate) string {
	check := finished(p, run)
	return func(c Candidate) string {
		why, _ := check(c)
		return why
	}
}

// finished is Finished that also says whether its answer is sure: it is not
// when the worker's pull request was to be asked of gh and was not read, and
// git alone, which cannot see a squash merge, did not find it merged.
func finished(p Project, run DirRunner) func(Candidate) (why string, sure bool) {
	return func(c Candidate) (string, bool) {
		if _, ok := p.Done[c.SessionID]; ok {
			return "marked done", true
		}
		if filepath.IsAbs(c.Cwd) {
			if _, err := os.Stat(c.Cwd); errors.Is(err, fs.ErrNotExist) {
				return "worktree gone", true
			}
		}
		if run == nil || !filepath.IsAbs(c.Cwd) || c.Branch == "HEAD" || !validBranch(c.Branch) {
			return "", true
		}
		ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
		defer cancel()
		def := defaultBranch(ctx, run, c.Cwd)
		if c.Branch == def || def == "" && (c.Branch == "main" || c.Branch == "master") {
			return "", true
		}
		dir := c.Cwd
		if c.Record != nil && c.Record.Project != "" {
			dir = c.Record.Project
		}
		out, err := []byte(nil), errors.New("no usable project folder")
		if isDir(dir) && (dir == c.Cwd || sameRepo(ctx, run, dir, c.Cwd)) {
			out, err = run(ctx, dir, "gh", "pr", "list", "--head", c.Branch, "--state", "all", "--json", "state,createdAt", "--limit", ghPRLimit)
		}
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
					return "", true
				}
				switch prs[newest].State {
				case "MERGED":
					return "merged", true
				case "CLOSED":
					return "closed", true
				}
				return "", true
			}
		}
		if !validBranch(def) {
			return "", false
		}
		if _, err := run(ctx, c.Cwd, "git", "merge-base", "--is-ancestor", c.Branch, def); err != nil {
			return "", false
		}
		// A branch with no commits of its own yet is an ancestor too: merged
		// only when its tip is newer than the worker's start.
		out, err = run(ctx, c.Cwd, "git", "log", "-1", "--format=%ct", c.Branch)
		if err != nil {
			return "", false
		}
		secs, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
		if err != nil || !time.Unix(secs, 0).After(c.lineStart()) {
			return "", false
		}
		return "merged (git)", true
	}
}

// defaultBranch is the default branch of the repository at dir: the branch
// origin/HEAD points to, else main or master when one exists; "" when none
// is found.
func defaultBranch(ctx context.Context, run DirRunner, dir string) string {
	if out, err := run(ctx, dir, "git", "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if b := strings.TrimPrefix(gitLine(out), "origin/"); b != "" {
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

// sameRepo says whether folders a and b are of one git repository, worktrees
// of it included: git names one common folder for both. False when it names
// none, as for a folder that is in no repository.
func sameRepo(ctx context.Context, run DirRunner, a, b string) bool {
	common := func(dir string) string {
		out, err := run(ctx, dir, "git", "rev-parse", "--path-format=absolute", "--git-common-dir")
		if p := gitLine(out); err == nil && filepath.IsAbs(p) {
			return realPath(p)
		}
		return ""
	}
	p := common(a)
	return p != "" && p == common(b)
}

// currentBranch is the branch checked out in dir; "" when run is nil, dir is
// empty or missing, or git names none (a detached HEAD, not a repository).
func currentBranch(run DirRunner, dir string) string {
	return currentBranchCtx(context.Background(), run, dir)
}

// currentBranchCtx is currentBranch under ctx's deadline too.
func currentBranchCtx(parent context.Context, run DirRunner, dir string) string {
	if run == nil || !filepath.IsAbs(dir) {
		return ""
	}
	if _, err := os.Stat(dir); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(parent, checkTimeout)
	defer cancel()
	out, err := run(ctx, dir, "git", "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return ""
	}
	return gitLine(out)
}

// gitLine is git's one-line answer without its line end: only that, since
// a branch name may end in a character strings.TrimSpace would take too.
func gitLine(out []byte) string {
	return strings.TrimSuffix(strings.TrimSuffix(string(out), "\n"), "\r")
}
