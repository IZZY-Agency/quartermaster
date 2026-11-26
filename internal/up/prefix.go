package up

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
	"github.com/IZZY-Agency/quartermaster/internal/config"
	"github.com/IZZY-Agency/quartermaster/internal/spawn"
)

// NamePrefixAuto is the value of worker.name_prefix that means the project's
// workspace name; it is the default.
const NamePrefixAuto = "auto"

// mainRoot is the folder of the main worktree of root's repository, root
// itself when it is not a linked worktree or not in git.
func mainRoot(root string) string {
	if main, isGit := claudecode.MainWorktree(root); isGit && main != "" {
		return main
	}
	return root
}

// configuredPrefix is the project_name_prefix entry of the project that holds
// root: keyed by the main repository's folder, else by root itself, as the
// other per-project maps are keyed by a repository's folder. ok says there is
// an entry; "" in it means no prefix for that project.
func configuredPrefix(cfg config.Config, root string) (prefix string, ok bool) {
	for _, key := range []string{mainRoot(root), root} {
		if p, found := cfg.ProjectNamePrefix[key]; found {
			return p, true
		}
	}
	return "", false
}

// PrefixName is the name a worker spawned from dir gets: name with the
// project's prefix in front, PREFIX-NAME. The prefix is, in order, the
// project's project_name_prefix entry; else, unless worker.name_prefix is "",
// the project's workspace name: workspace when the caller knows it (the
// project the worker is tied to), else the one in the project's state, else
// the base name of the main repository's folder.
//
// projectRoot is the root of the project the worker is tied to, "" when it is
// tied to none: the configured prefix is then read from the same project
// that Prefixes reads it from, not from the repository of dir.
//
// A name that already is the prefix, or starts with PREFIX-, is returned as
// it is, and so is a name that is not valid by itself (spawn reports it). A
// configured prefix that cannot be used is an error naming its key; a derived
// one is skipped, and note says so. worker.name_prefix is checked first, on
// every call, whatever the project's entry.
func PrefixName(cfg config.Config, dir, workspace, projectRoot, name string) (full, note string, err error) {
	mode := cfg.NamePrefixMode()
	if mode != "" && mode != NamePrefixAuto {
		return "", "", fmt.Errorf("worker.name_prefix is %q; it must be %q or %q (no prefix)", mode, NamePrefixAuto, "")
	}
	if !spawn.ValidName(name) {
		return name, "", nil
	}
	root, rerr := Root(dir)
	if rerr != nil {
		return name, "", nil // the request check reports a folder that is not there
	}
	keyRoot := root
	if projectRoot != "" {
		keyRoot = projectRoot
	}
	if p, ok := configuredPrefix(cfg, keyRoot); ok {
		if p == "" {
			return name, "", nil
		}
		key := fmt.Sprintf("project_name_prefix[%q]", mainRoot(keyRoot))
		full, why := joinPrefix(p, name)
		if why != "" {
			return "", "", fmt.Errorf("%s is %q: %s", key, p, why)
		}
		return full, "", nil
	}
	if mode == "" {
		return name, "", nil
	}
	ws := workspace
	if ws == "" {
		if p, ok, err := ProjectFor(dir); err == nil && ok {
			ws = p.Workspace
		}
	}
	if ws == "" {
		ws = cleanName(mainRoot(root))
	}
	full, why := joinPrefix(ws, name)
	if why != "" {
		return name, fmt.Sprintf("worker name not prefixed: the project's name %q %s", ws, why), nil
	}
	return full, "", nil
}

// joinPrefix is PREFIX-NAME, or name when it already is the prefix or starts
// with PREFIX-; why says what is wrong when the prefix or the result is not a
// valid worker name.
func joinPrefix(prefix, name string) (full, why string) {
	if !spawn.ValidName(prefix) {
		return "", "is not usable in a worker name (1 to 64 letters, digits, - or _, starting with a letter or digit)"
	}
	if name == prefix || strings.HasPrefix(name, prefix+"-") {
		return name, ""
	}
	full = prefix + "-" + name
	if !spawn.ValidName(full) {
		return "", fmt.Sprintf("makes the name %q too long (at most 64 characters)", full)
	}
	return full, ""
}

// Prefixes are the prefixes a worker of project p may carry: its configured
// one and its workspace name. They are all tried when a bare name is
// resolved, whatever the config says now, so a worker spawned under another
// setting is still found.
func Prefixes(cfg config.Config, p Project) []string {
	var out []string
	if c, ok := configuredPrefix(cfg, p.Root); ok && c != "" {
		out = append(out, c)
	}
	if p.Workspace != "" && !slices.Contains(out, p.Workspace) {
		out = append(out, p.Workspace)
	}
	return out
}

// NamedWorker is a worker as ResolveName reads it: its name, the name it was
// spawned with before the prefix (Record.BaseName; "" when unknown, a worker
// spawned before the prefix existed or restored from one), and, when set,
// whether it is finished (asked only to settle an ambiguity).
type NamedWorker struct {
	Name     string
	Base     string
	Finished func() bool
}

// MarkedOrGone says whether c is a finished worker the cheap way: marked done,
// or its folder is gone. It reads no pull request.
func MarkedOrGone(p Project, c Candidate) bool {
	if _, ok := p.Done[c.SessionID]; ok {
		return true
	}
	if filepath.IsAbs(c.Cwd) {
		_, err := os.Stat(c.Cwd)
		return errors.Is(err, fs.ErrNotExist)
	}
	return false
}

// ResolveName turns the name a user typed into the name of a worker in ws:
// the full name, or the bare one. A worker with a stored Base is meant by its
// Base; one without is meant by its name less a prefix of the project, one of
// prefixes (a guess, so a worker spawned under another setting is still
// found). matched says some worker was meant; when none was, input is
// returned as typed, for the caller to report.
//
// Two workers it could mean (x and PREFIX-x, for the bare x) are refused, both
// named, unless the name typed already is a full one, or all but one of them
// are finished (Finished).
func ResolveName(ws []NamedWorker, input string, prefixes []string) (full string, matched bool, err error) {
	hasPrefix := func(n string) bool {
		return slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(n, p+"-") })
	}
	var names []string
	byName := map[string][]NamedWorker{}
	for _, w := range ws {
		meant := w.Name == input
		if w.Name == input && (w.Base != "" && w.Base != w.Name || w.Base == "" && hasPrefix(input)) {
			return input, true, nil // a full name, whatever else the bare one means
		}
		switch {
		case w.Base != "":
			meant = meant || w.Base == input
		default:
			meant = meant || slices.ContainsFunc(prefixes, func(p string) bool { return w.Name == p+"-"+input })
		}
		if meant {
			if _, seen := byName[w.Name]; !seen {
				names = append(names, w.Name)
			}
			byName[w.Name] = append(byName[w.Name], w)
		}
	}
	if len(names) > 1 {
		var live []string
		for _, n := range names {
			if slices.ContainsFunc(byName[n], func(w NamedWorker) bool { return w.Finished == nil || !w.Finished() }) {
				live = append(live, n)
			}
		}
		if len(live) > 0 {
			names = live
		}
	}
	switch len(names) {
	case 0:
		return input, false, nil
	case 1:
		return names[0], true, nil
	}
	slices.Sort(names)
	return "", false, fmt.Errorf("%w: worker name %q is ambiguous: %s; use the full name", ErrInput, input, strings.Join(names, " and "))
}
