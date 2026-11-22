// Package up starts a project's coordinator and restores the coordinator
// and its workers after a restart (qm up design). This file holds the
// project state: one JSON file per project under
// <state>/projects/<id>.json, with a lock file next to it.
package up

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/paths"
	"github.com/IZZY-Agency/quartermaster/internal/route"
)

// stateVersion is the version written in every project file.
const stateVersion = 1

// maxNameLen caps the base-name part of a project id.
const maxNameLen = 40

// ErrBusy: another qm up, qm up done or qm down holds the project's lock.
var ErrBusy = errors.New("qm up is already running for this project")

// Session is a Claude session qm up started: its id, its name (-n) and when
// it was started. Adopted says qm up took it over running (adopt design
// 4.2) rather than starting it.
type Session struct {
	ID      string    `json:"session_id"`
	Name    string    `json:"name"`
	Started time.Time `json:"started"`
	Adopted bool      `json:"adopted,omitempty"`
}

// AdoptedSession is an adopted worker (adopt design 4.2). A worker with a
// decision-log Record has the zero value (its Record supplies the rest); a
// hand-started one has every field: its name, folder, model, the kept
// claude flags (adopt design 4.3) and when it started.
type AdoptedSession struct {
	Name    string    `json:"name,omitempty"`
	Cwd     string    `json:"cwd,omitempty"`
	Model   string    `json:"model,omitempty"`
	Args    []string  `json:"args,omitempty"`
	Started time.Time `json:"started,omitzero"`
}

// Project is the state of one qm up project (design 5.3). Root is the real
// absolute path of the project's folder. Sessions maps a Claude session id
// to the terminal session it last ran in; Done maps a worker's Claude
// session id to when it was marked finished. The state holds no worker
// list: workers come from the decision log.
type Project struct {
	Version          int                          `json:"version"`
	ID               string                       `json:"id"`
	Root             string                       `json:"root"`
	Workspace        string                       `json:"workspace"`
	Launcher         string                       `json:"launcher"`
	ClaudeArgs       []string                     `json:"claude_args"`
	Coordinator      Session                      `json:"coordinator"`
	PastCoordinators []string                     `json:"past_coordinators,omitempty"`
	Sessions         map[string]launch.SessionRef `json:"sessions,omitempty"`
	Done             map[string]time.Time         `json:"done,omitempty"`
	Created          time.Time                    `json:"created"`
	Updated          time.Time                    `json:"updated"`
	DownAt           time.Time                    `json:"down_at,omitzero"`
	// Dropped holds the session ids whose terminal session was closed or
	// is no longer listed: their worker Records' refs are not loaded again.
	Dropped map[string]bool `json:"dropped,omitempty"`
	// Adopted holds the adopted workers, by Claude session id.
	Adopted map[string]AdoptedSession `json:"adopted,omitempty"`
}

// Tied says whether decision Record r belongs to p: its up_project is p, or
// its Claude session is one p adopted.
func (p *Project) Tied(r *route.Record) bool {
	if r.UpProject == p.ID {
		return true
	}
	_, ok := p.Adopted[r.ClaudeSession]
	return ok && r.ClaudeSession != ""
}

// drop removes session id's terminal session from p.Sessions and remembers
// it in p.Dropped.
func (p *Project) drop(id string) {
	delete(p.Sessions, id)
	if p.Dropped == nil {
		p.Dropped = map[string]bool{}
	}
	p.Dropped[id] = true
}

// Root is the project root of dir: git's top level of dir (in a linked
// worktree, that worktree's), else dir itself; absolute, with symlinks
// resolved, so one folder reached by two paths is one project.
func Root(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("project root: %w", err)
	}
	root := abs
	out, err := exec.Command("git", "-C", abs, "rev-parse", "--show-toplevel").Output()
	if err == nil {
		if top := strings.TrimSpace(string(out)); top != "" {
			root = top
		}
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("project root: %w", err)
	}
	return resolved, nil
}

// ProjectID is <name>-<8 hex>: name is root's base name reduced to letters,
// digits, "-" and "_" (at most 40 characters), hex the start of the SHA-256
// of root.
func ProjectID(root string) string {
	return cleanName(root) + "-" + rootHash(root)[:8]
}

// WorkspaceName is the workspace (agterm) or session (tmux) name of the
// project at root: its cleaned base name, or <name>-<4 hex> when taken says
// another recorded project already uses that name.
func WorkspaceName(root string, taken func(string) bool) string {
	name := cleanName(root)
	if taken != nil && taken(name) {
		return name + "-" + rootHash(root)[:4]
	}
	return name
}

func rootHash(root string) string {
	sum := sha256.Sum256([]byte(root))
	return hex.EncodeToString(sum[:])
}

// cleanName is root's base name with every character other than an ASCII
// letter, a digit, "-" and "_" dropped, cut to maxNameLen; "project" when
// nothing is left.
func cleanName(root string) string {
	var b strings.Builder
	for _, r := range filepath.Base(root) {
		if r < 0x80 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			b.WriteRune(r)
			if b.Len() == maxNameLen {
				break
			}
		}
	}
	if b.Len() == 0 {
		return "project"
	}
	return b.String()
}

// projectsDir is <state>/projects, where the project files and locks live.
func projectsDir() (string, error) {
	d, err := paths.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "projects"), nil
}

// ensureDir creates dir with mode 0700, and sets that mode when it exists.
func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// Load reads the state of the project at root; false when none is recorded.
func Load(root string) (Project, bool, error) {
	return LoadID(ProjectID(root))
}

// LoadID reads the state of the project with id; false when none is
// recorded.
func LoadID(id string) (Project, bool, error) {
	if !safeName(id) {
		return Project{}, false, fmt.Errorf("load project: bad id %q", id)
	}
	dir, err := projectsDir()
	if err != nil {
		return Project{}, false, fmt.Errorf("load project: %w", err)
	}
	p, err := readProject(filepath.Join(dir, id+".json"))
	if errors.Is(err, fs.ErrNotExist) {
		return Project{}, false, nil
	}
	if err != nil {
		return Project{}, false, fmt.Errorf("load project: %w", err)
	}
	return p, true, nil
}

func readProject(path string) (Project, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Project{}, err
	}
	var p Project
	if err := json.Unmarshal(b, &p); err != nil {
		return Project{}, fmt.Errorf("%s: %w", path, err)
	}
	if p.Version > stateVersion {
		return Project{}, fmt.Errorf("%s: version %d, newer than this qm reads (%d): upgrade qm", path, p.Version, stateVersion)
	}
	return p, nil
}

// Save writes p to <state>/projects/<id>.json atomically (mode 0600, folder
// 0700), with Updated set to now and Version to the current one.
func Save(p Project) error {
	if p.ID == "" {
		return errors.New("save project: no id")
	}
	dir, err := projectsDir()
	if err != nil {
		return fmt.Errorf("save project: %w", err)
	}
	if err := ensureDir(dir); err != nil {
		return fmt.Errorf("save project: %w", err)
	}
	p.Version = stateVersion
	p.Updated = time.Now().UTC()
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("save project: %w", err)
	}
	if err := writeAtomic(filepath.Join(dir, p.ID+".json"), append(b, '\n')); err != nil {
		return fmt.Errorf("save project: %w", err)
	}
	return nil
}

// writeAtomic writes data to a 0600 temporary file next to path, then
// renames it over path, so a reader sees the old file or the new one.
func writeAtomic(path string, data []byte) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".project-*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Chmod(0o600); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// All returns every recorded project, sorted by file name (the id). A
// missing projects folder holds none.
func All() ([]Project, error) {
	dir, err := projectsDir()
	if err != nil {
		return nil, fmt.Errorf("projects: %w", err)
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, fmt.Errorf("projects: %w", err)
	}
	var out []Project
	for _, f := range files {
		if strings.HasPrefix(filepath.Base(f), ".") {
			continue // a temporary file of Save, or another stray dotfile
		}
		p, err := readProject(f)
		if errors.Is(err, fs.ErrNotExist) {
			continue // removed since the glob
		}
		if err != nil {
			return nil, fmt.Errorf("projects: %w", err)
		}
		out = append(out, p)
	}
	return out, nil
}

// Lock takes the project's lock file, <state>/projects/<id>.lock, with
// flock(LOCK_EX|LOCK_NB): ErrBusy when another run holds it. unlock
// releases it; the lock also ends with the process.
func Lock(id string) (unlock func(), err error) {
	if id == "" || strings.ContainsAny(id, "/\x00") {
		return nil, fmt.Errorf("lock project: bad id %q", id)
	}
	dir, err := projectsDir()
	if err != nil {
		return nil, fmt.Errorf("lock project: %w", err)
	}
	if err := ensureDir(dir); err != nil {
		return nil, fmt.Errorf("lock project: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, id+".lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("lock project: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrBusy
		}
		return nil, fmt.Errorf("lock project: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
