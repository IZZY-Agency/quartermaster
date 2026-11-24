package spawn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteScriptNeedsAFileName(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	for _, name := range []string{"", ".", "..", "../x", "a/b", `a\b`, "a\x00b", "a b", strings.Repeat("a", 65)} {
		if path, err := WriteScript(name, "#!/bin/sh\n"); err == nil {
			t.Errorf("WriteScript(%q) = %q, want an error", name, path)
		}
	}
	var files []string
	_ = filepath.Walk(state, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	if len(files) != 0 {
		t.Errorf("files written: %q", files)
	}
	for _, name := range []string{"w1", "_tools-coord", "feature_x-2"} {
		path, err := WriteScript(name, "#!/bin/sh\n")
		if err != nil {
			t.Errorf("WriteScript(%q): %v", name, err)
			continue
		}
		if dir := filepath.Join(state, "quartermaster", "launch"); filepath.Dir(path) != dir {
			t.Errorf("WriteScript(%q) = %q, want a file in %s", name, path, dir)
		}
	}
}
