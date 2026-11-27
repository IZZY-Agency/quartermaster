package paths

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Executable reads the running qm once: running is its path with symlinks
// resolved, and invoked is the path the user called it by (os.Args[0], looked
// up on PATH when it has no slash) when that resolves to the very same file,
// else "". The invoked path is left as written, not cleaned, so a ".."
// after a symlink keeps its meaning. On Linux os.Executable reads
// /proc/self/exe, which resolves symlinks, so only invoked can show the link
// qm was called through.
func Executable() (running, invoked string, err error) {
	self, err := os.Executable()
	if err != nil {
		return "", "", err
	}
	running = self
	if r, err := filepath.EvalSymlinks(self); err == nil {
		running = r
	}
	return running, invokedMatching(os.Args[0], exec.LookPath, running), nil
}

// InvokedExecutable is the path to write into a command that runs qm again:
// the invoked path when there is one, else os.Executable's.
func InvokedExecutable() (string, error) {
	return InvokedPath(os.Args[0], exec.LookPath, os.Executable)
}

// InvokedPath is InvokedExecutable with its inputs given: arg0, a PATH lookup
// and os.Executable.
func InvokedPath(arg0 string, look func(string) (string, error), exe func() (string, error)) (string, error) {
	self, err := exe()
	if err != nil {
		return "", err
	}
	running := self
	if r, err := filepath.EvalSymlinks(self); err == nil {
		running = r
	}
	if p := invokedMatching(arg0, look, running); p != "" {
		// Tidied only when that names the same file: "link/../bin/qm" is
		// not "bin/qm" when link is a symlink.
		if c := filepath.Clean(p); c != p {
			if r, err := filepath.EvalSymlinks(c); err == nil && r == running {
				return c, nil
			}
		}
		return p, nil
	}
	return self, nil
}

// invokedMatching is arg0 made absolute, when it resolves to running; "".
// A lookup that fails with exec.ErrDot found the file in the current folder,
// which PATH named as a relative entry: its path is still the one invoked.
func invokedMatching(arg0 string, look func(string) (string, error), running string) string {
	p := arg0
	if !strings.Contains(p, "/") {
		var err error
		if p, err = look(p); err != nil && !errors.Is(err, exec.ErrDot) {
			return ""
		}
	}
	if p == "" {
		return ""
	}
	if !filepath.IsAbs(p) {
		wd, err := os.Getwd()
		if err != nil {
			return ""
		}
		// No Join: it would clean away a ".." before its symlink is read.
		p = wd + string(filepath.Separator) + p
	}
	if r, err := filepath.EvalSymlinks(p); err != nil || r != running {
		return ""
	}
	return p
}
