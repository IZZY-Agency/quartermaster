package hooks

import (
	"strings"
	"testing"
)

// patternKills end processes found by a pattern.
var patternKills = []string{
	"pkill -f 'make check'",
	"pkill node",
	"pkill -P 123",
	"/usr/bin/pkill -f x",
	"sudo pkill -9 x",
	"sudo -u me pkill x",
	"env A=1 pkill x",
	"A=1 pkill x",
	"timeout 5 pkill x",
	"nohup pkill x &",
	"command pkill x",
	"exec pkill x",
	"killall node",
	"sudo killall -9 node",
	"make check; pkill -f x",
	"make check && killall x || true",
	"true | pkill x",
	"if true; then pkill x; fi",
	"for i in 1 2; do pkill x; done",
	"( pkill x )",
	"{ pkill x; }",
	"f() { pkill x; }; f",
	"! pkill x",
	"echo $(pkill x)",
	"echo `pkill x`",
	"echo \"$(pkill x)\"",
	"cat <<EOF\n$(pkill x)\nEOF",
	"bash -c 'pkill x'",
	"sh -lc \"cd /tmp && pkill x\"",
	"fish -c 'pkill x'",
	"fish --command='pkill x'",
	"eval 'pkill x'",
	"eval pkill x",
	"find . -name x -exec pkill x ';'",
	"find . -name '*.pid' -exec sh -c 'pkill x' ';'",
	"tmux new-window 'pkill x'",
	"tmux send-keys -t a 'pkill x' Enter",
	"agtermctl session new --command 'pkill x'",
	"agtermctl session new --command='pkill x'",
	"echo x | xargs pkill",
	"echo x | xargs -I{} pkill {}",

	"kill $(pgrep -f 'make check')",
	"kill -9 $(pgrep x)",
	"kill -9 \"$(pgrep x)\"",
	"kill `pgrep x`",
	"kill -s TERM `pgrep -f x | head -1`",
	"kill $(pgrep x | head -1)",
	"kill $(ps aux | grep 'make check' | awk '{print $2}')",
	"kill $(ps -ef | awk '/x/{print $2}')",
	"kill $(echo $(pgrep x))",
	"awk '{print $2}' <<< \"$(ps aux | grep server)\" | xargs kill",
	"tr ' ' '\\n' <<< \"$(pgrep -f server)\" | xargs kill",
	"awk '{print $2}' < <(ps aux | grep server) | xargs kill",
	"kill $(bash -c 'pgrep x')",
	"kill $(sudo pgrep x)",
	"kill $(pidof x)",
	"sudo kill $(pgrep x)",
	"/bin/kill $(pgrep x)",
	"kill -- $(pgrep x)",
	"kill 12 $(pgrep x)",
	"pgrep x | xargs kill",
	"pgrep x | xargs -r kill -9",
	"pgrep x | xargs -n1 sudo kill",
	"pgrep x | xargs -I{} kill {}",
	"pgrep x | xargs -I{} sh -c 'kill {}'",
	"pgrep x | sudo xargs kill",
	"pgrep x |& xargs kill",
	"pgrep x |\n  xargs kill",
	"pgrep x \\\n  | xargs kill",
	"ps aux | grep x | grep -v grep | awk '{print $2}' | xargs kill -9",
	"( pgrep a; pgrep b ) | xargs kill",
	"{ pgrep a; pgrep b; } | xargs kill",
	"cd /x && pgrep x | xargs kill",
	"xargs kill <<< \"$(pgrep x)\"",
	"xargs kill < <(pgrep x)",
	"pgrep x | while read p; do kill $p; done",
	"pgrep x | while read -r p; do kill \"$p\"; done",
	"while read p; do kill $p; done < <(pgrep x)",
	"while read p; do kill $p; done <<< $(pgrep x)",
	"for p in $(pgrep x); do kill $p; done",
	"for p in `pgrep x`; do kill -9 ${p}; done",
	"p=$(pgrep x); kill $p",
	"p=$(pgrep x) && kill \"$p\"",
	"export p=$(pgrep x); kill $p",
	"local p=$(pgrep x); kill $p",
	"p=`pgrep x`\nkill -9 $p",
	"p=$(ps aux | grep x | awk '{print $2}'); [ -n \"$p\" ] && kill $p",
	"mapfile -t ps < <(pgrep x); kill \"${ps[@]}\"",
	"p=$(pgrep x); q=$p; kill $q",
	"nohup sh -c 'sleep 1; kill $(pgrep x)' &",
	"bash -c 'p=$(pgrep x); kill $p'",
	"p=$(pgrep x); bash -c \"kill $p\"",
	"p=$(pgrep x); echo $p | xargs kill",
	"p=$(pgrep x); sh -c 'kill $0' $p",
	"p=$(pgrep x); xargs kill <<< \"$p\"",
	"pids=($(pgrep -f server)); kill \"${pids[@]}\"",
	"pids=(1 $(pgrep x)) && kill ${pids[0]}",
	"pids=(\n$(pgrep x)\n); kill ${pids[@]}",
	"pids=(); pids+=($(pgrep x)); kill ${pids[@]}",
	"p+=\" $(pgrep x)\"; kill $p",
	"p=$(pgrep x); p+=\" 12\"; kill $p",
	"local pids=($(pgrep x)); kill ${pids[@]}",
	"(pgrep a; pgrep b) 2>/dev/null | xargs kill",
	"for n in a b; do pgrep $n; done | xargs kill",
	"if true; then pgrep a; fi | xargs kill",
	"cat <(pgrep x) | xargs kill",
	"while read p; do if true; then kill $p; fi; done < <(pgrep x)",
	"git commit -m \"$(cat <<'EOF'\nit can't\nEOF\n)\"; pkill x",
	"echo \"$(cat <<'EOF'\nit can't\nEOF\npkill x)\"",
	"kill $(cat <<'EOF'\nit can't\nEOF\npgrep x)",
}

// otherCommands are not pattern kills.
var otherCommands = []string{
	"",
	"kill 1234",
	"kill -9 1234 1235",
	"kill -TERM -1234",
	"kill %1",
	"kill $!",
	"kill $$",
	"kill $pid",
	"kill \"$pid\"",
	"kill -0 $pid 2>/dev/null && echo alive",
	"sleep 100 & pid=$!; kill $pid",
	"server & p=$!\nwait 5\nkill $p",
	"kill $(cat server.pid)",
	"kill `cat server.pid`",
	"kill \"$(< server.pid)\"",
	"cat server.pid | xargs kill",
	"xargs kill < server.pid",
	"while read p; do kill $p; done < pids",
	"for p in 1 2 3; do kill $p; done",
	"kill -l",
	"kill $(jobs -p)",
	"pgrep -f 'make check'",
	"pgrep x | wc -l",
	"ps aux | grep x",
	"ps -p $pid",
	"ps -p $pid && kill $pid",
	"ps aux | grep x; kill 123",
	"pgrep x; kill 123",
	"pgrep x || kill 123",
	"pgrep x && kill $pid",
	"pgrep x & kill 123",
	"pgrep x | cat; cat server.pid | xargs kill",
	"p=$(pgrep x); echo $p; kill 123",
	"p=$(pgrep x); kill $pid",
	"p=$(pgrep x); p=$!; kill 12",
	"echo pkill x",
	"grep -rn pkill .",
	"rg 'kill \\$\\(pgrep'",
	"git commit -m 'no pkill'",
	"git commit -m \"never use pkill, killall or kill fed by pgrep\"",
	"man pkill",
	"which pkill",
	"command -v pkill",
	"sudo -l pkill",
	"find . -name pkill",
	"cat <<'EOF'\n$(pkill x)\nEOF",
	"cat <<EOF > notes\npkill x\nEOF",
	"# pkill x",
	"ls # pkill x",
	"tmux kill-session -t x",
	"tmux kill-window",
	"git log --grep=pkill",
	"go test -run 'PKill|Killall' ./...",
	"ssh host pkill x",
	"docker exec c pkill x",
	"make kill",
	"echo 'kill $(pgrep x)'",
	"ps aux | awk '{print $2}' | xargs echo",
	"pgrep x | xargs ps -p",
	"pgrep x | xargs -I{} sh -c 'echo {}'",
	"agtermctl session close abc",
	"pids=(1 2); kill \"${pids[@]}\"",
	"pids=($(cat run/*.pid)); kill ${pids[@]}",
	"names=(pkill killall); echo ${names[@]}",
	"eval \"echo $(ps -p $PID -o comm=); kill $PID\"",
	"bash -c \"echo $(ps -p $PID); kill $PID\"",
	"p=$(pgrep x); bash -c \"echo $p; kill 123\"",
	"ps aux | grep server; for f in run/*.pid; do cat $f; done | xargs kill",
	"pgrep -l x; ( cat a.pid; cat b.pid ) | xargs kill",
	"pgrep -l x; { cat a.pid; cat b.pid; } | xargs kill",
	"pgrep -l x; cat $(ls run/server.pid) | xargs kill",
	"awk '{print $2}' <<< \"$(ps aux | grep server)\"; xargs kill < server.pid",
	"awk '{print $2}' <<< \"$(ps aux | grep server)\" | xargs echo",
	"while read p; do echo $p; done < <(ps aux); xargs kill < server.pid",
	"git commit -m \"$(cat <<'EOF'\nThe worker can't run `pkill` now\nEOF\n)\"",
	"git commit -m \"$(cat <<-\"EOF\"\n\tcan't (run `pkill x`\n\tEOF\n)\" && git push",
}

func TestPatternKillRefused(t *testing.T) {
	for _, cmd := range patternKills {
		if !patternKill(cmd) {
			t.Errorf("not refused: %s", cmd)
		}
	}
}

func TestPatternKillAllowsOtherCommands(t *testing.T) {
	for _, cmd := range append(append([]string{}, otherCommands...), ordinaryCommands...) {
		if patternKill(cmd) {
			t.Errorf("refused: %s", cmd)
		}
	}
}

// TestPatternKillCutInput: every prefix of every table command is read
// without a panic.
func TestPatternKillCutInput(t *testing.T) {
	extra := []string{"|", "| |", "kill", "xargs", "for", "for p in", "read", "p=", "$(", "kill $(", "<(", "}", ") |", "export", "local -a"}
	for _, list := range [][]string{patternKills, otherCommands, wrappedLaunches, extra} {
		for _, cmd := range list {
			for i := 0; i <= len(cmd); i++ {
				patternKill(cmd[:i])
			}
		}
	}
}

// TestPatternKillRule: the rule refuses with what to use instead, every time
// (it is never softened to a note), in subagents too; note mode notes and off
// passes.
func TestPatternKillRule(t *testing.T) {
	cfg := setup(t)
	for range 2 * softenAfter {
		out := fire(t, cfg, "pattern-kill", "Bash", bash("pkill -f x"))
		want(t, out, "deny")
		for _, s := range []string{"TaskStop", "pid"} {
			if !strings.Contains(string(out), s) {
				t.Errorf("reason lacks %q: %s", s, out)
			}
		}
	}
	want(t, fire(t, cfg, "pattern-kill", "Bash", bash("kill 123")), "pass")
	want(t, fire(t, cfg, "pattern-kill", "Read", map[string]any{"file_path": "/x"}), "pass")
	cfg.Rules = map[string]string{"pattern-kill": "note"}
	want(t, fire(t, cfg, "pattern-kill", "Bash", bash("pkill x")), "note")
	cfg.Rules = map[string]string{"pattern-kill": "off"}
	want(t, fire(t, cfg, "pattern-kill", "Bash", bash("pkill x")), "pass")
}
