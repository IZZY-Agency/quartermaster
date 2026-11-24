package spawn

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ProcExe returns the absolute path of the executable process pid runs, as
// the system reports it (/proc/<pid>/exe, which argv[0] does not change).
func ProcExe(pid int) (string, error) {
	p, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
	if err != nil {
		return "", fmt.Errorf("executable of %d: %w", pid, err)
	}
	// A file replaced since it started (an update) still names its path.
	return strings.TrimSuffix(p, " (deleted)"), nil
}
