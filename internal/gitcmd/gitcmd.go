// Package gitcmd starts git without the GIT_* variables of qm's environment:
// the one way qm runs git. An inherited variable (qm run from a git hook, or
// set by whoever started the session) must not decide which repository git
// answers for, how far it looks for one, which index it lists or which config
// it reads: qm's trust check, review sizing and LSP pick rest on that answer.
package gitcmd

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"strings"
)

// Env is env without its GIT_* variables.
func Env(env []string) []string {
	return slices.DeleteFunc(slices.Clone(env), func(kv string) bool { return strings.HasPrefix(kv, "GIT_") })
}

// Command is "git -C dir args..." in qm's environment without its GIT_*
// variables.
func Command(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = Env(os.Environ())
	return cmd
}
