package paths

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func writeAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestInvokedPathKeepsLink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "Cellar", "1.0", "qm")
	writeAll(t, target)
	link := filepath.Join(dir, "bin", "qm")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	resolved := func() (string, error) { return target, nil } // Linux: /proc/self/exe
	look := func(name string) (string, error) {
		if name == "qm" {
			return link, nil
		}
		return "", os.ErrNotExist
	}
	for _, arg0 := range []string{"qm", link} {
		if got, err := InvokedPath(arg0, look, resolved); err != nil || got != link {
			t.Errorf("InvokedPath(%q) = %q, %v; want the link %s", arg0, got, err, link)
		}
	}
	// argv[0] that names another file, or none found: os.Executable's path.
	other := filepath.Join(dir, "other")
	writeAll(t, other)
	for _, arg0 := range []string{other, "missing"} {
		if got, _ := InvokedPath(arg0, look, resolved); got != target {
			t.Errorf("InvokedPath(%q) = %q, want %s", arg0, got, target)
		}
	}
}

// PATH names a relative folder holding a link to qm: LookPath finds it with
// exec.ErrDot, and the link's path, not its target's, is the invoked path.
func TestInvokedPathErrDot(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "keg", "qm")
	writeAll(t, target)
	if err := os.MkdirAll(filepath.Join(dir, "rel"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "rel", "qm")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("PATH", "rel")
	if _, err := exec.LookPath("qm"); !errors.Is(err, exec.ErrDot) {
		t.Skipf("LookPath does not report ErrDot here: %v", err)
	}
	got, err := InvokedPath("qm", exec.LookPath, func() (string, error) { return target, nil })
	if want := filepath.Join(dir, "rel", "qm"); err != nil || got != want {
		t.Errorf("InvokedPath = %q, %v; want the link %s", got, err, want)
	}
}

// A ".." after a symlink is kept: it names a folder only the symlink knows.
func TestInvokedPathKeepsDotDot(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "sub", "bin", "qm")
	writeAll(t, target)
	if err := os.MkdirAll(filepath.Join(root, "sub", "deep"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "front"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "sub", "deep"), filepath.Join(root, "front", "link")); err != nil {
		t.Fatal(err)
	}
	arg0 := root + "/front/link/../bin/qm"
	got, err := InvokedPath(arg0, nil, func() (string, error) { return target, nil })
	if err != nil || got != arg0 {
		t.Errorf("InvokedPath = %q, %v; want %s unchanged", got, err, arg0)
	}
}
