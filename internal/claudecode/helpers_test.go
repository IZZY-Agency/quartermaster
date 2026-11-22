package claudecode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture copies testdata/<name> to dst, with @ROOT@ replaced by root.
func fixture(t *testing.T, name, root, dst string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte(strings.ReplaceAll(string(b), "@ROOT@", root)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// claudeDir is a Claude Code folder in a temporary directory, holding the
// sessions files from testdata; it returns the folder and the root that
// replaced @ROOT@.
func claudeDir(t *testing.T) (dir, root string) {
	t.Helper()
	root = t.TempDir()
	dir = filepath.Join(root, "claude")
	for _, f := range []string{"101.json", "202.json"} {
		fixture(t, filepath.Join("sessions", f), root, filepath.Join(dir, "sessions", f))
	}
	return dir, root
}
