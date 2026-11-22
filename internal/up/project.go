package up

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
)

// Worktrees lists the project's folders: root and the other worktrees of its
// repository, when root is the main worktree (the first entry of git
// worktree list --porcelain); symlinks resolved. Otherwise (root is a linked
// worktree, outside git, or git fails) just root.
func Worktrees(ctx context.Context, root string, run DirRunner) []string {
	out, err := run(ctx, root, "git", "worktree", "list", "--porcelain")
	if err != nil {
		return []string{root}
	}
	var all []string
	for line := range strings.Lines(string(out)) {
		path, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "worktree ")
		if !ok || path == "" {
			continue
		}
		if real, err := filepath.EvalSymlinks(path); err == nil {
			path = real
		}
		all = append(all, filepath.Clean(path))
	}
	real := filepath.Clean(root)
	if r, err := filepath.EvalSymlinks(real); err == nil {
		real = r
	}
	if len(all) == 0 || all[0] != real {
		return []string{root}
	}
	res := []string{root}
	for _, p := range all[1:] {
		if p != real {
			res = append(res, p)
		}
	}
	return res
}

// ProjectFor finds the project a folder belongs to: the one rooted at
// Root(dir), else, when dir is in a linked worktree, the one rooted at the
// repository's main worktree. false when neither has a state. It changes
// neither Root nor ProjectID.
func ProjectFor(dir string) (Project, bool, error) {
	root, err := Root(dir)
	if err != nil {
		return Project{}, false, err
	}
	p, ok, err := Load(root)
	if err != nil || ok {
		return p, ok, err
	}
	main, isGit := claudecode.MainWorktree(root)
	if !isGit || main == "" || main == root {
		return Project{}, false, nil
	}
	return Load(main)
}
