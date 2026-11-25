package spawn

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// proc is one fake process: its command line and parent pid.
type proc struct {
	cmd  string
	ppid int
}

func psOf(procs map[int]proc) func(int) (string, int, error) {
	return func(pid int) (string, int, error) {
		p, ok := procs[pid]
		if !ok {
			return "", 0, errors.New("no such process")
		}
		return p.cmd, p.ppid, nil
	}
}

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestPermissionMirrorsParent(t *testing.T) {
	bypass := []string{"--dangerously-skip-permissions"}
	cases := []struct {
		name string
		cmd  string
		want []string
	}{
		{"bypass flag", "claude --dangerously-skip-permissions", bypass},
		{"bypass mode", "/opt/bin/claude --permission-mode bypassPermissions", bypass},
		{"bypass mode with =", "claude --permission-mode=bypassPermissions --model opus", bypass},
		{"accept edits", "/usr/local/bin/claude --permission-mode acceptEdits", []string{"--permission-mode", "acceptEdits"}},
		{"plan with =", "claude -c --permission-mode=plan", []string{"--permission-mode", "plan"}},
		{"none", "claude --model opus", nil},
		{"mode with no value", "claude --permission-mode", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ps := psOf(map[int]proc{os.Getppid(): {"/bin/zsh -c qm", 4242}, 4242: {c.cmd, 1}})
			exe := exeOf(map[int]string{4242: realClaude})
			got, why, _ := ParentPermission(envMap(map[string]string{"CLAUDE_PID": "4242"}), ps, exe)
			if !slices.Equal(got, c.want) {
				t.Errorf("flags = %q, want %q", got, c.want)
			}
			if why == "" {
				t.Error("why is empty")
			}
		})
	}
}

func TestPermissionWalksParents(t *testing.T) {
	parent := os.Getppid()
	ps := psOf(map[int]proc{
		parent: {"/bin/zsh -c qm spawn worker", 900},
		900:    {"/opt/tools/bin/claude --permission-mode acceptEdits", 800},
		800:    {"claude --dangerously-skip-permissions", 1},
	})
	exe := exeOf(map[int]string{900: realClaude, 800: realClaude})
	got, _, _ := ParentPermission(envMap(nil), ps, exe)
	if want := []string{"--permission-mode", "acceptEdits"}; !slices.Equal(got, want) {
		t.Errorf("flags = %q, want the first claude's %q", got, want)
	}
}

func TestNoParentNoBypass(t *testing.T) {
	parent := os.Getppid()
	cases := map[string]func(int) (string, int, error){
		"no claude among parents": psOf(map[int]proc{
			parent: {"/bin/zsh", 700},
			700:    {"/usr/bin/login -fp user", 1},
			1:      {"/sbin/launchd --dangerously-skip-permissions", 0},
		}),
		"ps fails":         psOf(nil),
		"claude-like name": psOf(map[int]proc{parent: {"/bin/claudette --dangerously-skip-permissions", 1}}),
	}
	for name, ps := range cases {
		t.Run(name, func(t *testing.T) {
			got, why, _ := ParentPermission(envMap(nil), ps, exeOf(nil))
			if got != nil {
				t.Errorf("flags = %q, want nil", got)
			}
			if why != NoCoordinator {
				t.Errorf("why = %q, want %q", why, NoCoordinator)
			}
		})
	}
}

func TestClaudePathWithSpaces(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "My Tools", "claude")
	if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	parent := os.Getppid()
	exe := exeOf(map[int]string{4242: bin})
	ps := psOf(map[int]proc{parent: {"/bin/zsh", 4242}, 4242: {bin + " --permission-mode acceptEdits", 1}})
	got, _, _ := ParentPermission(envMap(map[string]string{"CLAUDE_PID": "4242"}), ps, exe)
	if want := []string{"--permission-mode", "acceptEdits"}; !slices.Equal(got, want) {
		t.Errorf("flags = %q, want %q", got, want)
	}
	// A shell running a claude path is not claude itself.
	ps = psOf(map[int]proc{parent: {"/bin/zsh", 4242}, 4242: {"/bin/sh -c " + bin + " --dangerously-skip-permissions", 1}})
	exe = exeOf(map[int]string{4242: "/bin/sh"})
	if got, why, _ := ParentPermission(envMap(map[string]string{"CLAUDE_PID": "4242"}), ps, exe); got != nil {
		t.Errorf("flags = %q (%s), want nil", got, why)
	}
}

// LowerPermission orders the modes partially (plan and dontAsk below
// default, then acceptEdits, auto, bypass); a pair with no mode that allows
// no more than either is refused, in both orders.
func TestLowerPermission(t *testing.T) {
	mode := func(m string) []string { return []string{"--permission-mode", m} }
	bypass := []string{"--dangerously-skip-permissions"}
	cases := []struct {
		name    string
		a, b    []string
		want    []string
		refused bool
	}{
		{"dontAsk under acceptEdits", mode("dontAsk"), mode("acceptEdits"), mode("dontAsk"), false},
		{"dontAsk under default", mode("dontAsk"), nil, mode("dontAsk"), false},
		{"dontAsk under auto", mode("dontAsk"), mode("auto"), mode("dontAsk"), false},
		{"dontAsk under bypass", mode("dontAsk"), bypass, mode("dontAsk"), false},
		{"plan under acceptEdits", mode("plan"), mode("acceptEdits"), mode("plan"), false},
		{"acceptEdits under auto", mode("acceptEdits"), mode("auto"), mode("acceptEdits"), false},
		{"auto under bypass", mode("auto"), []string{"--permission-mode=bypassPermissions"}, mode("auto"), false},
		{"manual is default", mode("manual"), mode("acceptEdits"), mode("manual"), false},
		{"default flag and none", mode("default"), nil, mode("default"), false},
		{"same unknown", mode("future"), mode("future"), mode("future"), false},
		{"unknown under bypass", mode("future"), bypass, mode("future"), false},
		{"plan and dontAsk", mode("plan"), mode("dontAsk"), nil, true},
		{"unknown and acceptEdits", mode("future"), mode("acceptEdits"), nil, true},
		{"unknown and none", mode("future"), nil, nil, true},
		{"two unknowns", mode("future"), mode("other"), nil, true},
	}
	for _, c := range cases {
		for _, order := range [][2][]string{{c.a, c.b}, {c.b, c.a}} {
			got, err := LowerPermission(order[0], order[1])
			if c.refused {
				if err == nil || !strings.Contains(err.Error(), "no permission mode") {
					t.Errorf("%s: LowerPermission(%q, %q) = %q, %v, want a refusal", c.name, order[0], order[1], got, err)
				}
				continue
			}
			want := c.want
			if c.name == "default flag and none" {
				// Both stand for default: the first argument's is kept.
				if !slices.Equal(order[0], c.a) {
					want = order[0]
				}
			}
			if err != nil || !slices.Equal(got, want) {
				t.Errorf("%s: LowerPermission(%q, %q) = %q, %v, want %q", c.name, order[0], order[1], got, err, want)
			}
		}
	}
}

// A coordinator that is a version file run directly gives its workers its
// permission flag, as it did under the name claude: the executable decides,
// and argv[0] is whatever the process was started with. The same executable
// rule holds as before; node and the npm package's files pass none of it, by
// whatever command line.
func TestPermissionOfOtherClaudeInstalls(t *testing.T) {
	root := t.TempDir()
	file := func(parts ...string) string {
		path := filepath.Join(append([]string{root}, parts...)...)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	version := file("claude", "versions", "2.1.300")
	cli := file("node_modules", "@anthropic-ai", "claude-code", "cli.js")
	native := file("node_modules", "@anthropic-ai", "claude-code", "bin", "claude.exe")
	for _, tc := range []struct {
		name, cmd, exe string
		want           bool
	}{
		{"version file", version + " --dangerously-skip-permissions", version, true},
		{"version file as claude", "claude --dangerously-skip-permissions", version, true},
		{"version command line, another executable", version + " --dangerously-skip-permissions", "/bin/sh", false},
		{"version command line, node executable", version + " --dangerously-skip-permissions", "/usr/local/bin/node", false},
		{"claude command line, node executable", "claude --dangerously-skip-permissions", "/usr/local/bin/node", false},
		{"node command line, node executable", "node " + cli + " --dangerously-skip-permissions", "/usr/local/bin/node", false},
		{"node command line, claude executable", "node " + cli + " --dangerously-skip-permissions", version, false},
		{"npm native binary", "claude --dangerously-skip-permissions", native, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ps := psOf(map[int]proc{os.Getppid(): {cmd: tc.cmd, ppid: 1}})
			got, why, err := ParentPermission(envMap(nil), ps, func(int) (string, error) { return tc.exe, nil })
			if err != nil {
				t.Fatal(err)
			}
			if has := slices.Equal(got, []string{"--dangerously-skip-permissions"}); has != tc.want {
				t.Errorf("flags = %q (%s), want the coordinator's flag: %v", got, why, tc.want)
			}
		})
	}
}
