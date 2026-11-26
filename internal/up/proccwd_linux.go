package up

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ProcCwds lists the running processes' current folders that /proc lets
// this user read (its own processes).
func ProcCwds(ctx context.Context) ([]ProcCwd, error) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var procs []ProcCwd
	for _, e := range ents {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		dir := filepath.Join("/proc", e.Name())
		cwd, err := os.Readlink(filepath.Join(dir, "cwd"))
		if err != nil {
			continue // another user's, or ended meanwhile
		}
		comm, _ := os.ReadFile(filepath.Join(dir, "comm"))
		procs = append(procs, ProcCwd{PID: pid, Cmd: strings.TrimSpace(string(comm)), Cwd: cwd})
	}
	return procs, nil
}
