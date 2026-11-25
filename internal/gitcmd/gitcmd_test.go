package gitcmd

import (
	"context"
	"slices"
	"testing"
)

func TestEnvDropsEveryGitVariable(t *testing.T) {
	got := Env([]string{
		"HOME=/h", "GIT_DIR=/a/.git", "GIT_CEILING_DIRECTORIES=/a", "GIT_INDEX_FILE=/a/index",
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.worktree", "GIT_CONFIG_VALUE_0=/a",
		"GIT_SSH_COMMAND=ssh", "GIT_=x", "GITHUB_TOKEN=t", "XGIT_DIR=/b", "PATH=/bin", "NOTE=GIT_DIR=/c",
	})
	want := []string{"HOME=/h", "GITHUB_TOKEN=t", "XGIT_DIR=/b", "PATH=/bin", "NOTE=GIT_DIR=/c"}
	if !slices.Equal(got, want) {
		t.Errorf("Env = %q, want %q", got, want)
	}
}

func TestCommandRunsInDirWithoutGitVariables(t *testing.T) {
	t.Setenv("GIT_DIR", "/somewhere/.git")
	cmd := Command(context.Background(), "/repo", "rev-parse", "--show-toplevel")
	if want := []string{"git", "-C", "/repo", "rev-parse", "--show-toplevel"}; !slices.Equal(cmd.Args, want) {
		t.Errorf("args = %q, want %q", cmd.Args, want)
	}
	if cmd.Env == nil || slices.Contains(cmd.Env, "GIT_DIR=/somewhere/.git") {
		t.Errorf("env = %q, want qm's environment without GIT_DIR", cmd.Env)
	}
}
