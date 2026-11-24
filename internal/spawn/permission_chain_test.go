package spawn

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// exeOf is a fake executable reader: the kernel-reported executable of
// each pid in exes; an error for any other pid.
func exeOf(exes map[int]string) func(int) (string, error) {
	return func(pid int) (string, error) {
		e, ok := exes[pid]
		if !ok {
			return "", errors.New("no such process")
		}
		return e, nil
	}
}

const realClaude = "/opt/claude/bin/claude"

func TestPermissionNamedPidMustBeAnAncestor(t *testing.T) {
	parent := os.Getppid()
	ps := psOf(map[int]proc{
		parent: {"/bin/zsh -c qm spawn worker", 900},
		900:    {"claude --permission-mode acceptEdits", 1},
		4242:   {"claude --dangerously-skip-permissions", 1},
	})
	exe := exeOf(map[int]string{900: realClaude, 4242: realClaude})
	got, why, _ := ParentPermission(envMap(map[string]string{"CLAUDE_PID": "4242"}), ps, exe)
	if got != nil {
		t.Errorf("flags = %q, want nil: pid 4242 is not one of qm's parent processes", got)
	}
	if !strings.Contains(why, "4242") || !strings.Contains(why, "prompt") {
		t.Errorf("why = %q, want it to name pid 4242 and say the worker will prompt", why)
	}
}

func TestPermissionUsesNearestClaudeOnly(t *testing.T) {
	parent := os.Getppid()
	ps := psOf(map[int]proc{
		parent: {"/bin/zsh -c qm spawn worker", 900},
		900:    {"claude --permission-mode acceptEdits", 800},
		800:    {"claude --dangerously-skip-permissions", 1},
	})
	exe := exeOf(map[int]string{900: realClaude, 800: realClaude})
	if got, why, _ := ParentPermission(envMap(map[string]string{"CLAUDE_PID": "800"}), ps, exe); got != nil || !strings.Contains(why, "nearest") {
		t.Errorf("CLAUDE_PID naming an outer claude: flags = %q (%s), want nil", got, why)
	}
	want := []string{"--permission-mode", "acceptEdits"}
	if got, _, _ := ParentPermission(envMap(map[string]string{"CLAUDE_PID": "900"}), ps, exe); !slices.Equal(got, want) {
		t.Errorf("CLAUDE_PID naming the nearest claude: flags = %q, want %q", got, want)
	}
}

func TestPermissionReadsTheWeakestReading(t *testing.T) {
	cases := []struct {
		cmd  string
		want []string
	}{
		{"claude --permission-mode acceptEdits fix the --dangerously-skip-permissions docs", []string{"--permission-mode", "acceptEdits"}},
		{"claude --permission-mode plan -- --dangerously-skip-permissions", []string{"--permission-mode", "plan"}},
		{"claude -- --dangerously-skip-permissions", nil},
	}
	for _, c := range cases {
		ps := psOf(map[int]proc{os.Getppid(): {c.cmd, 1}})
		got, _, err := ParentPermission(envMap(nil), ps, exeOf(map[int]string{os.Getppid(): realClaude}))
		if !slices.Equal(got, c.want) || err != nil {
			t.Errorf("%q: flags = %q, %v, want %q", c.cmd, got, err, c.want)
		}
	}
	// plan and dontAsk have no mode in common: no flag is safe.
	ps := psOf(map[int]proc{os.Getppid(): {"claude --permission-mode plan explain dontAsk --permission-mode dontAsk", 1}})
	if got, _, err := ParentPermission(envMap(nil), ps, exeOf(map[int]string{os.Getppid(): realClaude})); got != nil || err == nil {
		t.Errorf("plan and dontAsk: flags = %q, err %v, want an error", got, err)
	}
}

func TestPermissionStopsAtClaudeExecutableWithOtherCommandLine(t *testing.T) {
	parent := os.Getppid()
	ps := psOf(map[int]proc{
		parent: {"/opt/claude-home/.local/share/claude/versions/2.1.0 --permission-mode plan", 800},
		800:    {"claude --dangerously-skip-permissions", 1},
	})
	exe := exeOf(map[int]string{parent: "/opt/claude-home/.local/share/claude/versions/2.1.0", 800: realClaude})
	if got, why, _ := ParentPermission(envMap(nil), ps, exe); got != nil || !strings.Contains(why, "prompt") {
		t.Errorf("flags = %q (%s), want nil: the nearest claude's command line is not read", got, why)
	}
}

func TestPermissionNeedsAClaudeExecutable(t *testing.T) {
	parent := os.Getppid()
	cases := map[string]struct {
		env map[string]string
		exe map[int]string
	}{
		"named pid runs another program":    {map[string]string{"CLAUDE_PID": "900"}, map[int]string{900: "/bin/sleep"}},
		"found parent runs another program": {nil, map[int]string{900: "/bin/sleep"}},
		"executable unreadable":             {nil, nil},
		"named pid executable unreadable":   {map[string]string{"CLAUDE_PID": "900"}, nil},
		"named pid not a number":            {map[string]string{"CLAUDE_PID": "x900"}, map[int]string{900: realClaude}},
		"named pid not claude":              {map[string]string{"CLAUDE_PID": "500"}, map[int]string{900: realClaude}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			ps := psOf(map[int]proc{
				parent: {"/bin/zsh -c qm spawn worker", 500},
				500:    {"/bin/zsh", 900},
				900:    {"claude --dangerously-skip-permissions", 1},
			})
			got, why, _ := ParentPermission(envMap(c.env), ps, exeOf(c.exe))
			if got != nil {
				t.Errorf("flags = %q, want nil", got)
			}
			if !strings.Contains(why, "prompt") {
				t.Errorf("why = %q, want it to say the worker will prompt", why)
			}
		})
	}
}

func TestPermissionStopsAtFirstClaudeName(t *testing.T) {
	parent := os.Getppid()
	ps := psOf(map[int]proc{
		parent: {"claude --dangerously-skip-permissions", 900},
		900:    {"claude --permission-mode plan", 1},
	})
	exe := exeOf(map[int]string{parent: "/bin/bash", 900: realClaude})
	if got, _, _ := ParentPermission(envMap(nil), ps, exe); got != nil {
		t.Errorf("flags = %q, want nil: the nearest claude-named parent is not claude", got)
	}
}

func TestClaudeExecutable(t *testing.T) {
	cases := map[string]bool{
		"/usr/local/bin/claude":                               true,
		"/opt/claude-home/.local/share/claude/versions/2.1.0": true,
		"/opt/homebrew/Caskroom/claude-code/2.1.0/claude":     true,
		"/bin/sleep":    false,
		"/usr/bin/node": false,
		"/opt/claude-home/.local/share/other/versions/2.1.0": false,
		"/opt/claude-home/versions/2.1.0":                    false,
		"":                                                   false,
		"claude":                                             false,
	}
	for exe, want := range cases {
		if got := claudeExecutable(exe); got != want {
			t.Errorf("claudeExecutable(%q) = %v, want %v", exe, got, want)
		}
	}
}

func TestProcExeReadsOwnExecutable(t *testing.T) {
	got, err := ProcExe(os.Getpid())
	if err != nil {
		t.Fatalf("ProcExe: %v", err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(self)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("ProcExe = %q, want %q", got, want)
	}
}

func TestPSIgnoresPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	cmd, ppid, err := PS(os.Getpid())
	if err != nil {
		t.Fatalf("PS with an empty PATH: %v", err)
	}
	if cmd == "" || ppid != os.Getppid() {
		t.Errorf("PS = %q, %d; want a command and ppid %d", cmd, ppid, os.Getppid())
	}
}
