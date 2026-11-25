package hooks

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadBudgetSparesTheBrief: the worker's read of its own brief is neither
// counted nor denied; any other file is, also one reached through a link to
// the brief, and the brief's path once a symlink stands there.
func TestReadBudgetSparesTheBrief(t *testing.T) {
	cfg := setup(t)
	cfg.ReadBudget = 1
	dir := t.TempDir()
	brief, other, link := filepath.Join(dir, "brief.md"), filepath.Join(dir, "other.md"), filepath.Join(dir, "link.md")
	for _, p := range []string{brief, other} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(brief, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QM_BRIEF", brief)
	read := func(path string) []byte {
		return fire(t, cfg, "read-budget", "Read", map[string]any{"file_path": path})
	}
	for range 5 {
		want(t, read(brief), "pass")
	}
	want(t, read(other), "pass") // the one the budget allows: the brief took none
	want(t, read(brief), "pass")
	want(t, read(other), "deny")
	want(t, read(link), "deny")
	want(t, read(brief), "pass")
	if err := os.Remove(brief); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, brief); err != nil {
		t.Fatal(err)
	}
	want(t, read(brief), "deny")
}

func TestReadBudgetBriefEnv(t *testing.T) {
	cfg := setup(t)
	cfg.ReadBudget = 1
	t.Setenv("QM_BRIEF", "")
	dir := t.TempDir()
	for _, input := range []map[string]any{{"file_path": ""}, {}, {"file_path": dir + "/x"}} {
		fire(t, cfg, "read-budget", "Read", input)
	}
	want(t, fire(t, cfg, "read-budget", "Read", map[string]any{}), "deny")
	// Only Read: a Bash read of the brief counts as any other.
	brief := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(brief, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QM_BRIEF", brief)
	want(t, fire(t, cfg, "read-budget", "Bash", bash("cat "+brief)), "note")
}
