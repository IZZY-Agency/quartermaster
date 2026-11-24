//go:build !darwin && !linux

package spawn

import "fmt"

// ProcExe cannot read a process's executable on this system: the
// coordinator's permission flag is then never copied.
func ProcExe(pid int) (string, error) {
	return "", fmt.Errorf("executable of %d: not readable on this system", pid)
}
