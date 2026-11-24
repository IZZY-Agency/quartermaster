package spawn

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
// claude process qm runs under, for its worker to copy (design 5.12). It
// walks up from qm's parent (ps gives each process's command line and
// parent pid) to the nearest process whose command line is claude's, and
// trusts it only when exe (the executable the system reports for a pid, not
// its argv[0]) is a claude executable (claudeExecutable) and, when
// CLAUDE_PID is set, CLAUDE_PID names that same process. Otherwise, or when
// any of that cannot be read, the answer is nil and the worker prompts. The
// flags are read with WeakestPermission: a bypass flag or --permission-mode
// bypassPermissions gives --dangerously-skip-permissions, --permission-mode
// X the same mode, no flag nil, so the worker never gets a stronger flag
// than its coordinator. A parent that runs a claude executable under a
// command line that is not claude's stops the walk too, with nil. why says
// where the flags came from. err is set only when the coordinator is found
// but its words give permission readings with no mode in common: then no
// flag is safe, prompting included, and the worker must not start.
func ParentPermission(env func(string) string, ps func(pid int) (cmd string, ppid int, err error), exe func(pid int) (string, error)) (flags []string, why string, err error) {
	named := 0
	if s := env("CLAUDE_PID"); s != "" {
		pid, err := strconv.Atoi(s)
		if err != nil || pid <= 0 {
			return nil, fmt.Sprintf("CLAUDE_PID %q is not a process id: worker will prompt", s), nil
		}
		named = pid
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
		args, isClaude := claudecode.IsClaude(cmd)
		path, exeErr := exe(pid)
		isExe := exeErr == nil && claudeExecutable(path)
		switch {
		case isClaude && !isExe:
			return nil, fmt.Sprintf("process %d does not run a claude executable: worker will prompt", pid), nil
		case !isClaude && isExe:
			return nil, fmt.Sprintf("process %d runs claude under a command line qm cannot read: worker will prompt", pid), nil
		case isClaude:
			if named != 0 && named != pid {
				return nil, fmt.Sprintf("CLAUDE_PID %d is not the nearest claude among qm's parent processes (pid %d): worker will prompt", named, pid), nil
			}
			flags, err := WeakestPermission(args)
			if err != nil {
				return nil, fmt.Sprintf("coordinator (pid %d): %v", pid, err), fmt.Errorf("coordinator (pid %d): %w: start the worker by hand with the mode you want", pid, err)
			}
			_, why := PermissionOf(flags, pid)
			return flags, why, nil
		}
		if ppid == pid {
			break
		}
		pid = ppid
	}
	if named != 0 {
		return nil, fmt.Sprintf("CLAUDE_PID %d is not a claude among qm's parent processes: worker will prompt", named), nil
	}
	return nil, NoCoordinator, nil
}

// WeakestPermission reads the permission flags off claude's words as ps
// gives them: the words up to --, and of every permission reading among
// them the one that allows least (LowerPermission), so a prompt that names
// a stronger flag than the session's own cannot raise it. ps keeps no
// argument boundaries, so a session with no permission flag whose prompt
// (before any --) names one is still read as having it. An error when two
// readings have no mode in common.
func WeakestPermission(words []string) ([]string, error) {
	if i := slices.Index(words, "--"); i >= 0 {
		words = words[:i]
	}
	var out []string
	seen := false
	for i := 0; i < len(words); i++ {
		var reading []string
		switch a := words[i]; {
		case a == bypass || strings.HasPrefix(a, "--permission-mode="):
			reading = []string{a}
		case a == "--permission-mode" && i+1 < len(words):
			reading = []string{a, words[i+1]}
			i++
		default:
			continue
		}
		flags, _ := PermissionOf(reading, 0)
		if !seen {
			out, seen = flags, true
			continue
		}
		var err error
		if out, err = LowerPermission(out, flags); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// claudeExecutable says whether exe, the path of the executable a process
// runs as the system reports it, is claude's: an absolute path to a file
// named claude, or to a version file of Claude Code's own installer
// (.../claude/versions/<version>). The check is by name and place, not
// signature.
func claudeExecutable(exe string) bool {
	if !filepath.IsAbs(exe) {
		return false
	}
	dir := filepath.Dir(exe)
	return filepath.Base(exe) == "claude" ||
		filepath.Base(dir) == "versions" && filepath.Base(filepath.Dir(dir)) == "claude"
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

// psPaths are where ps is looked for: by absolute path, never through PATH,
// whose ps could report any command line.
var psPaths = []string{"/bin/ps", "/usr/bin/ps"}

// PS returns a process's command line (its arguments joined by spaces) and
// parent pid, read with the system's ps (psPaths).
func PS(pid int) (cmd string, ppid int, err error) {
	bin := ""
	for _, p := range psPaths {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			bin = p
			break
		}
	}
	if bin == "" {
		return "", 0, fmt.Errorf("ps %d: no ps in %s", pid, strings.Join(psPaths, " or "))
	}
	out, err := exec.Command(bin, "-o", "ppid=", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
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
