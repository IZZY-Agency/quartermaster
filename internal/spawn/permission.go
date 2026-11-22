package spawn

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
)

// NoCoordinator is ParentPermission's why when no claude process is found:
// the worker then gets no permission flag and prompts.
const NoCoordinator = "no coordinator found: worker will prompt"

// bypass is the flag a bypass coordinator's workers get.
const bypass = "--dangerously-skip-permissions"

// maxParents bounds the walk up the process tree.
const maxParents = 64

// modeName is what a --permission-mode value may hold to be copied.
var modeName = regexp.MustCompile(`^[A-Za-z]+$`)

// ParentPermission returns the permission flags of the coordinator, the
// claude process qm runs under, for its worker to copy (design 5.12). It reads
// the process at CLAUDE_PID when that is set, else walks up from qm's parent
// to the first process whose argv[0] base name is claude. A bypass flag or
// --permission-mode bypassPermissions gives --dangerously-skip-permissions;
// --permission-mode X gives the same mode; no flag, or no coordinator, gives
// nil, so the worker never gets a stronger flag than its coordinator. why
// says where the flags came from. ps returns a process's command line and
// parent pid.
func ParentPermission(env func(string) string, ps func(pid int) (cmd string, ppid int, err error)) (flags []string, why string) {
	if s := env("CLAUDE_PID"); s != "" {
		if pid, err := strconv.Atoi(s); err == nil && pid > 0 {
			if cmd, _, err := ps(pid); err == nil {
				if args, ok := claudecode.IsClaude(cmd); ok {
					return PermissionOf(args, pid)
				}
			}
		}
	}
	pid := os.Getppid()
	for range maxParents {
		if pid <= 0 {
			break
		}
		cmd, ppid, err := ps(pid)
		if err != nil {
			break
		}
		if args, ok := claudecode.IsClaude(cmd); ok {
			return PermissionOf(args, pid)
		}
		if ppid == pid {
			break
		}
		pid = ppid
	}
	return nil, NoCoordinator
}

// PermissionOf reads the permission flags off claude's arguments: as
// ParentPermission gives them, with why naming pid as the coordinator.
func PermissionOf(args []string, pid int) ([]string, string) {
	mode := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == bypass:
			return []string{bypass}, fmt.Sprintf("coordinator (pid %d) runs with %s", pid, bypass)
		case a == "--permission-mode" && i+1 < len(args):
			i++
			mode = args[i]
		case strings.HasPrefix(a, "--permission-mode="):
			mode = strings.TrimPrefix(a, "--permission-mode=")
		}
	}
	switch {
	case mode == "bypassPermissions":
		return []string{bypass}, fmt.Sprintf("coordinator (pid %d) runs with --permission-mode bypassPermissions", pid)
	case modeName.MatchString(mode):
		return []string{"--permission-mode", mode}, fmt.Sprintf("coordinator (pid %d) runs with --permission-mode %s", pid, mode)
	}
	return nil, fmt.Sprintf("coordinator (pid %d) has no permission flag: worker will prompt", pid)
}

// CapPermission is a resumed worker's permission flags (design 5.5): its
// logged flags, lowered to the coordinator's when they are stronger (bypass,
// then --permission-mode X, then none); a mode other than the coordinator's
// becomes the coordinator's. coordinator is the coordinator's full claude
// arguments, read with PermissionOf.
func CapPermission(logged, coordinator []string) []string {
	have, _ := PermissionOf(logged, 0)
	limit, _ := PermissionOf(coordinator, 0)
	rank := func(flags []string) int {
		switch {
		case len(flags) == 0:
			return 0
		case flags[0] == bypass:
			return 2
		}
		return 1
	}
	if rank(have) > rank(limit) || (rank(have) == 1 && rank(limit) == 1 && !slices.Equal(have, limit)) {
		return limit
	}
	return have
}

// LowerPermission is a session's own permission flags or the coordinator's,
// whichever is weaker, in the order plan < default (no flag, or
// --permission-mode default) < acceptEdits < any other mode < bypass. It is
// for a session the user configured (an adopted hand-started worker): its
// own weaker mode is never replaced by the coordinator's stronger one. Two
// different modes of the same rank (both unknown) give no flag. own and
// coordinator are full claude arguments, read with PermissionOf.
func LowerPermission(own, coordinator []string) []string {
	have, _ := PermissionOf(own, 0)
	limit, _ := PermissionOf(coordinator, 0)
	rank := func(flags []string) int {
		switch {
		case len(flags) == 0:
			return 1
		case flags[0] == bypass:
			return 4
		}
		switch flags[1] {
		case "plan":
			return 0
		case "default":
			return 1
		case "acceptEdits":
			return 2
		}
		return 3
	}
	switch rh, rl := rank(have), rank(limit); {
	case rh < rl:
		return have
	case rh > rl:
		return limit
	case slices.Equal(have, limit):
		return have
	case rh == 1:
		return have
	}
	return nil
}

// PS returns a process's command line (its arguments joined by spaces) and
// parent pid, read with ps.
func PS(pid int) (cmd string, ppid int, err error) {
	out, err := exec.Command("ps", "-o", "ppid=", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", 0, fmt.Errorf("ps %d: %w", pid, err)
	}
	line := strings.TrimSpace(string(out))
	first, rest, _ := strings.Cut(line, " ")
	if ppid, err = strconv.Atoi(first); err != nil {
		return "", 0, fmt.Errorf("ps %d: unexpected output %q", pid, line)
	}
	return strings.TrimSpace(rest), ppid, nil
}
