package up

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/route"
)

// MarkDone marks the newest worker of p named name finished (qm up done):
// it is never restored. cands are p's candidates as Gather shows them, so
// a worker whose fresh restart never ran is marked in the session Gather
// restores in its place (and the never-run one with it). An older session
// of a reused name is left as it is, so marking the finished one never
// marks a running one. It returns the session's id; ErrInput when the
// project has no worker of that name. The caller saves p.
func MarkDone(p *Project, cands []Candidate, name string, now time.Time) (string, error) {
	var marked *Candidate
	// Gather lists the recorded workers, then the hand-started ones, each
	// oldest first: the newest is the latest start across both, a tie going
	// to the later listed.
	for i := range cands {
		c := &cands[i]
		if c.Role == RoleWorker && c.Name == name && (marked == nil || !c.Started.Before(marked.Started)) {
			marked = c
		}
	}
	if marked == nil {
		return "", fmt.Errorf("%w: no worker %q in project %s", ErrInput, name, p.ID)
	}
	if p.Done == nil {
		p.Done = map[string]time.Time{}
	}
	p.Done[marked.SessionID] = now
	if marked.Instead != "" {
		p.Done[marked.Instead] = now
	}
	return marked.SessionID, nil
}

// WorkerOf reports whether root is the folder of a worker tied to another
// recorded project (spec 5.2: qm up refuses to run there): a worker Record
// tied to it (up_project, or a session it adopted), or a hand-started worker
// it adopted. It returns the worker's name and that project's root.
func WorkerOf(root string, projects []Project, recs []route.Record) (name, projectRoot string, ok bool) {
	roots := map[string]string{}
	adoptedBy := map[string]string{} // session id: the root of the project that adopted it
	for _, p := range projects {
		roots[p.ID] = p.Root
		for sid := range p.Adopted {
			adoptedBy[sid] = p.Root
		}
	}
	for i := len(recs) - 1; i >= 0; i-- {
		r := recs[i]
		pr, tied := roots[r.UpProject]
		if !tied && r.ClaudeSession != "" {
			pr, tied = adoptedBy[r.ClaudeSession]
		}
		if r.Kind != "worker" || !tied || r.Cwd == "" || pr == root {
			continue
		}
		if realDir(r.Cwd) == root {
			return r.Name, pr, true
		}
	}
	for _, p := range projects {
		if p.Root == root {
			continue
		}
		for _, sid := range slices.Sorted(maps.Keys(p.Adopted)) {
			if a := p.Adopted[sid]; a.Cwd != "" && realDir(a.Cwd) == root {
				return a.Name, p.Root, true
			}
		}
	}
	return "", "", false
}

// realDir is dir with symlinks resolved, else dir.
func realDir(dir string) string {
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		return real
	}
	return dir
}
