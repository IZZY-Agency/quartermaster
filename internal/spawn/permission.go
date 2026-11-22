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

// below holds, for each permission level qm knows, the levels that allow no
// more than it without asking: plan (read-only exploration) and dontAsk
// (denies every call that would prompt) below default (asks before editing
// or running), then acceptEdits, auto (a classifier approves routine
// actions) and bypass. plan and dontAsk are not ordered: plan stops edits an
// allowed tool may make under dontAsk, and dontAsk denies what plan would
// ask about. A mode qm does not know is below bypass only.
var below = map[string][]string{
	"plan":        nil,
	"dontAsk":     nil,
	"default":     {"plan", "dontAsk"},
	"acceptEdits": {"default", "plan", "dontAsk"},
	"auto":        {"acceptEdits", "default", "plan", "dontAsk"},
}

// level is the permission level of flags read with PermissionOf: "bypass",
// "default" for no flag (or the manual alias), else the mode.
func level(flags []string) string {
	switch {
	case len(flags) == 0:
		return "default"
	case flags[0] == bypass:
		return "bypass"
	case flags[1] == "manual":
		return "default"
	}
	return flags[1]
}

// atMost reports whether level a allows no more than level b.
func atMost(a, b string) bool {
	return a == b || b == "bypass" || slices.Contains(below[b], a)
}

// describe names flags read with PermissionOf for an error.
func describe(flags []string) string {
	if len(flags) == 0 {
		return "no permission flag"
	}
	return strings.Join(flags, " ")
}

// LowerPermission is a session's own permission flags or the coordinator's,
// whichever allows less (the order of below): a worker's flags are never
// raised to the coordinator's, and a session the user configured (an adopted
// hand-started worker) keeps its own weaker mode. When both stand for
// default, own's are kept. Two levels neither of which allows no more than
// the other (plan and dontAsk, or a mode qm does not know against any but
// itself and bypass) have no mode in common that is safe for both: that is
// an error, and the session is not started. own and coordinator are full
// claude arguments, read with PermissionOf.
func LowerPermission(own, coordinator []string) ([]string, error) {
	have, _ := PermissionOf(own, 0)
	limit, _ := PermissionOf(coordinator, 0)
	switch lh, ll := level(have), level(limit); {
	case atMost(lh, ll):
		return have, nil
	case atMost(ll, lh):
		return limit, nil
	}
	return nil, fmt.Errorf("%s and %s have no permission mode in common that allows no more than either", describe(have), describe(limit))
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
