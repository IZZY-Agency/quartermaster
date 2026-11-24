package spawn

import (
	"bufio"
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// lsofPath is the system's lsof, by absolute path.
const lsofPath = "/usr/sbin/lsof"

// ProcExe returns the absolute path of the executable process pid runs, as
// the system reports it (its program text, which argv[0] does not change),
// read with lsof.
func ProcExe(pid int) (string, error) {
	out, err := exec.Command(lsofPath, "-a", "-p", strconv.Itoa(pid), "-d", "txt", "-F", "n").Output()
	if err != nil {
		return "", fmt.Errorf("executable of %d: %w", pid, err)
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if name, ok := strings.CutPrefix(sc.Text(), "n"); ok && filepath.IsAbs(name) {
			return name, nil
		}
	}
	return "", fmt.Errorf("executable of %d: not reported", pid)
}
