package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// errNoFolder: the folder git would run in does not exist, so it is in no
// repository.
var errNoFolder = errors.New("not a git repository: no such folder")

// gitTimeout bounds the git call of Trusted (a slow or locked file system).
const gitTimeout = 5 * time.Second

// runGit runs git with args in dir and returns its standard output; tests
// replace it. GIT_DIR, GIT_WORK_TREE and GIT_COMMON_DIR are removed from its
// environment, so an inherited one (qm run from a git hook) does not make
// git answer for another repository.
var runGit = func(dir string, args ...string) ([]byte, error) {
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %w", errNoFolder, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return k == "GIT_DIR" || k == "GIT_WORK_TREE" || k == "GIT_COMMON_DIR"
	})
	out, err := cmd.Output()
	if err != nil && ctx.Err() != nil {
		return nil, fmt.Errorf("git: %w", ctx.Err())
	}
	return out, err
}

// Trusted reports whether Claude Code has accepted its trust dialog for dir
// (design 5.7, trust), from claudeJSON's
// projects[<folder>].hasTrustDialogAccepted, in this order:
//
//  1. dir's own entry is true;
//  2. dir is in a git repository: the entry of the repository's main
//     worktree root (the folder holding git's common dir, <root>/.git), so a
//     linked worktree outside the main folder inherits its trust. When the
//     common dir is not a .git folder (a submodule, a bare repository, a
//     separate git dir) there is no such root, and only dir's own entry
//     counts. No folder above the repository counts: Claude Code shows the
//     dialog to a repository nested in a trusted one;
//  3. dir is outside git (git says it is not a repository, or git is
//     missing): the entry
//     of dir or of any folder above it, up to "/".
//
// When git fails otherwise (a timeout, or a git older than 2.31 that does
// not know --path-format), dir may be in a repository
// nested in a trusted folder, so only dir's own entry counts.
//
// dir should be absolute. A missing claudeJSON trusts nothing.
//
// Paths are compared cleaned, as written and with symlinks resolved (on
// macOS /tmp is /private/tmp), so a folder recorded under either spelling
// matches. A mismatch only skips a session as untrusted, never starts one in
// an untrusted folder.
func Trusted(claudeJSON, dir string) (bool, error) {
	t, err := ReadTrust(claudeJSON)
	if err != nil {
		return false, err
	}
	return t.Trusted(dir), nil
}

// Trust is Claude Code's folder trust read from .claude.json once, for a run
// that checks many folders. The zero Trust trusts nothing.
type Trust struct {
	trusted map[string]bool
}

// trustRetry is how long ReadTrust waits before reading a .claude.json that
// did not parse a second time.
const trustRetry = 200 * time.Millisecond

// trustWait waits trustRetry; tests replace it.
var trustWait = func() { time.Sleep(trustRetry) }

// ReadTrust reads claudeJSON's folder trust. A missing file trusts nothing.
// Claude Code rewrites the file while it runs, so one that does not parse
// is read again once after trustRetry before that is an error.
func ReadTrust(claudeJSON string) (Trust, error) {
	t, err := readTrust(claudeJSON)
	var syn *json.SyntaxError
	var typ *json.UnmarshalTypeError
	if errors.As(err, &syn) || errors.As(err, &typ) || errors.Is(err, errEmpty) {
		trustWait()
		t, err = readTrust(claudeJSON)
	}
	if err != nil {
		return Trust{}, fmt.Errorf("read %s: %w", claudeJSON, err)
	}
	return t, nil
}

// errEmpty: an empty .claude.json, as a truncating writer leaves it.
var errEmpty = errors.New("empty file")

func readTrust(claudeJSON string) (Trust, error) {
	b, err := os.ReadFile(claudeJSON)
	if errors.Is(err, fs.ErrNotExist) {
		return Trust{}, nil
	}
	if err != nil {
		return Trust{}, err
	}
	if len(b) == 0 {
		return Trust{}, errEmpty
	}
	var f struct {
		Projects map[string]struct {
			HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return Trust{}, err
	}
	t := Trust{trusted: map[string]bool{}}
	for k, p := range f.Projects {
		if p.HasTrustDialogAccepted {
			for _, s := range spellings(k) {
				t.trusted[s] = true
			}
		}
	}
	return t, nil
}

// Trusted reports whether t trusts dir, by the rules of the package-level
// Trusted.
func (t Trust) Trusted(dir string) bool {
	is := func(d string) bool {
		for _, s := range spellings(d) {
			if t.trusted[s] {
				return true
			}
		}
		return false
	}
	dir = filepath.Clean(dir)
	if is(dir) {
		return true
	}
	if main, ok := MainWorktree(dir); ok {
		return main != "" && is(main)
	}
	for d := dir; ; {
		if is(d) {
			return true
		}
		parent := filepath.Dir(d)
		if parent == d {
			return false
		}
		d = parent
	}
}

// notARepository says whether err is git saying the folder is not in a
// repository (its standard error, or the error's text).
func notARepository(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errNoFolder) {
		return true
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && strings.Contains(string(ee.Stderr), "not a git repository") {
		return true
	}
	return strings.Contains(err.Error(), "not a git repository")
}

// spellings is p cleaned, and with symlinks resolved when that differs.
func spellings(p string) []string {
	p = filepath.Clean(p)
	if real, err := filepath.EvalSymlinks(p); err == nil && real != p {
		return []string{p, real}
	}
	return []string{p}
}

// MainWorktree is the main worktree root of the git repository dir is in:
// the folder holding git's common dir when that is <root>/.git, else ""
// (a submodule, a bare repository, a separate git dir, or git failed in a
// way that does not say dir is outside git: a timeout, or a git too old for
// --path-format); false when dir is outside git or git is missing.
func MainWorktree(dir string) (string, bool) {
	out, err := runGit(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	switch {
	case errors.Is(err, exec.ErrNotFound) || notARepository(err):
		return "", false
	case err != nil:
		return "", true
	}
	common := strings.TrimSpace(string(out))
	if strings.HasPrefix(common, "--path-format") {
		// git before 2.31 echoes an option it does not know: dir may be in a
		// repository nested in a trusted folder, so only its own entry counts.
		return "", true
	}
	if common == "" {
		return "", true
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(dir, common)
	}
	if real, err := filepath.EvalSymlinks(common); err == nil {
		common = real
	}
	common = filepath.Clean(common)
	if filepath.Base(common) != ".git" {
		return "", true
	}
	return filepath.Dir(common), true
}
