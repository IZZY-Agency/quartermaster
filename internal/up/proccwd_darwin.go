package up

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

// ProcCwds lists the running processes' current folders that lsof can read
// (this user's processes): lsof -d cwd, in its field output.
func ProcCwds(ctx context.Context) ([]ProcCwd, error) {
	cmd := exec.CommandContext(ctx, "lsof", "-n", "-P", "-w", "-d", "cwd", "-F", "pcn")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("lsof: %w %s", err, bytes.TrimSpace(stderr.Bytes()))
	}
	return parseLsof(out), nil
}
