// Package reviewin builds the input for routing one review: the prompt, the
// size and paths of the diff under review, and the round, counted from the
// decision log so that the reviewer's caller need not supply it.
package reviewin

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/declog"
	"github.com/IZZY-Agency/quartermaster/internal/route"
)

// DefaultBase is the ref the diff under review is taken against when the
// repository does not name its remote's default branch (origin/HEAD).
const DefaultBase = "origin/main"

// Base is the ref a review of the repository at dir is taken against:
// origin/HEAD's target (origin/master, origin/develop...) when the remote
// names one, else DefaultBase.
func Base(dir string) string {
	out, err := git(dir, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD")
	if b := strings.TrimSpace(out); err == nil && b != "" && !strings.HasPrefix(b, "-") {
		return b
	}
	return DefaultBase
}

// DefaultBranch is the local name of the branch Base points at
// ("origin/main" gives "main").
func DefaultBranch(dir string) string {
	b := Base(dir)
	if _, name, ok := strings.Cut(b, "/"); ok {
		return name
	}
	return b
}

// For builds the review input for the repository at repo: the prompt read
// from promptFile, and the diff from `git diff BASE...HEAD` (base "" is
// Base(repo)). Round 0 means "count it": 1 plus the review records logged
// for this repository and branch since the branch forked from base.
// Input.Repo is the top-level path.
func For(repo, base, promptFile string, round int) (route.ReviewInput, error) {
	if round < 0 {
		return route.ReviewInput{}, fmt.Errorf("review input: round %d is negative", round)
	}
	prompt, err := os.ReadFile(promptFile)
	if err != nil {
		return route.ReviewInput{}, fmt.Errorf("review input: %w", err)
	}
	top, branch, err := Repo(repo)
	if err != nil {
		return route.ReviewInput{}, err
	}
	if base == "" {
		base = Base(top)
	}
	if strings.HasPrefix(base, "-") {
		return route.ReviewInput{}, fmt.Errorf("review input: base %q starts with \"-\"", base)
	}
	lines, paths, err := diffStat(top, base)
	if err != nil {
		return route.ReviewInput{}, err
	}
	if round == 0 {
		if round, err = Round(top, branch, base); err != nil {
			return route.ReviewInput{}, err
		}
	}
	return route.ReviewInput{
		Prompt:    string(prompt),
		Round:     round,
		DiffLines: lines,
		DiffFiles: len(paths),
		Paths:     paths,
		Repo:      top,
	}, nil
}

// Repo returns the top-level path of the repository that holds dir ("" is
// the working directory) and its current branch; branch is "" on a detached
// HEAD.
func Repo(dir string) (top, branch string, err error) {
	if dir == "" {
		dir = "."
	}
	out, err := git(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", "", err
	}
	top = strings.TrimSpace(out)
	// With --quiet, symbolic-ref exits 1 without a message only when HEAD is
	// not a symbolic ref (detached); any other failure is an error.
	out, err = git(dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	var exit *exec.ExitError
	switch {
	case err == nil:
		branch = strings.TrimSpace(out)
	case errors.As(err, &exit) && exit.ExitCode() == 1:
	default:
		return "", "", err
	}
	return top, branch, nil
}

// workerLead is how long before the branch's oldest commit its worker may
// have been spawned; worker records older than that belong to an earlier
// branch of the same name.
const workerLead = 24 * time.Hour

// workerSlack is how far before a worker record a review record still
// belongs to it: a reviewer session's own review record is written 1 µs
// before its worker record.
const workerSlack = time.Millisecond

// Round is the next review round for a repository's branch: 1 plus the
// review records logged for that project and branch since the branch began.
// It began at the earlier of the fork time (the commit time of git
// merge-base base HEAD) and the branch's first worker record, so a rebase or
// a merge of main, which moves the merge base forward, does not reset the
// count. Worker records from before the branch's oldest own commit (author
// time, which a rebase keeps) less workerLead are an earlier branch of the
// same name and are ignored, so a reused name still starts at round 1. When
// there is no merge base, every record of the branch counts. A detached HEAD
// (branch "") has no history to count, so its round is 1.
func Round(project, branch, base string) (int, error) {
	if branch == "" {
		return 1, nil
	}
	all, err := declog.Read(time.Time{})
	if err != nil {
		return 0, err
	}
	since := ForkTime(project, base)
	if !since.IsZero() {
		oldest := oldestCommitTime(project, base)
		fromWorker := false
		for _, r := range all {
			if r.Kind == "worker" && r.Project == project && r.Branch == branch && r.Time.Before(since) &&
				(oldest.IsZero() || !r.Time.Before(oldest.Add(-workerLead))) {
				since, fromWorker = r.Time, true
			}
		}
		if fromWorker {
			// A --type review session writes its review record just before
			// its worker record (spawn.logWorker); that round counts.
			since = since.Add(-workerSlack)
		}
	}
	var recs []route.Record
	for _, r := range all {
		if !r.Time.Before(since) {
			recs = append(recs, r)
		}
	}
	n := 1
	for _, r := range recs {
		if r.Kind == "review" && r.Project == project && r.Branch == branch {
			n++
		}
	}
	return n, nil
}

// ForkTime is the commit time of the merge base of base and HEAD in the
// repository at dir: when the branch forked. Zero when git cannot tell.
func ForkTime(dir, base string) time.Time {
	if base == "" || strings.HasPrefix(base, "-") {
		return time.Time{}
	}
	mb, err := git(dir, "merge-base", "--end-of-options", base, "HEAD")
	if err != nil {
		return time.Time{}
	}
	out, err := git(dir, "show", "-s", "--format=%ct", strings.TrimSpace(mb))
	if err != nil {
		return time.Time{}
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(secs, 0).UTC()
}

// oldestCommitTime is the author time of the oldest commit on HEAD that is
// not on base (author times survive a rebase). Zero when there is none.
func oldestCommitTime(dir, base string) time.Time {
	if base == "" || strings.HasPrefix(base, "-") {
		return time.Time{}
	}
	out, err := git(dir, "log", "--no-merges", "--format=%at", "--end-of-options", base+"..HEAD")
	if err != nil {
		return time.Time{}
	}
	var oldest time.Time
	for _, f := range strings.Fields(out) {
		secs, err := strconv.ParseInt(f, 10, 64)
		if err != nil {
			continue
		}
		if t := time.Unix(secs, 0).UTC(); oldest.IsZero() || t.Before(oldest) {
			oldest = t
		}
	}
	return oldest
}

// diffStat returns the lines added plus removed, and the changed paths, of
// base...HEAD. A binary file counts as a path with no lines; a rename counts
// as a removed and an added path.
func diffStat(top, base string) (lines int, paths []string, err error) {
	out, err := git(top, "diff", "--numstat", "-z", "--no-renames", "--end-of-options", base+"...HEAD", "--")
	if err != nil {
		return 0, nil, err
	}
	for _, entry := range strings.Split(out, "\x00") {
		if entry == "" {
			continue
		}
		f := strings.SplitN(entry, "\t", 3)
		if len(f) != 3 {
			return 0, nil, fmt.Errorf("review input: unexpected git diff --numstat line %q", entry)
		}
		for _, n := range f[:2] {
			if n == "-" {
				continue
			}
			v, err := strconv.Atoi(n)
			if err != nil {
				return 0, nil, fmt.Errorf("review input: unexpected git diff --numstat line %q", entry)
			}
			lines += v
		}
		paths = append(paths, f[2])
	}
	return lines, paths, nil
}

// git runs git in dir and returns its stdout. A failure carries git's stderr
// and wraps the *exec.ExitError, so callers can read the exit code.
func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), msg, err)
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return stdout.String(), nil
}
