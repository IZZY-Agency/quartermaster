package hooks

import "testing"

// TestLaunchGateSeesThroughShellForms: a launch is seen behind the shell
// forms a command can take: wrappers, reserved words, substitutions, merged
// flags, quoting and here-documents.
func TestLaunchGateSeesThroughShellForms(t *testing.T) {
	for _, cmd := range wrappedLaunches {
		if !rawLaunch(cmd) {
			t.Errorf("%q: allowed, want denied", cmd)
		}
	}
}

// wrappedLaunches are launches behind shell forms.
var wrappedLaunches = []string{
	// env and its flags
	"env -u FOO claude -p x",
	"env -uFOO claude -p x",
	"env --unset FOO claude -p x",
	"env -C /tmp claude -p x",
	"env --chdir=/tmp claude -p x",
	"env -i PATH=/bin claude -p x",
	`env -S "claude -p x"`,
	`env -S"claude -p x"`,
	`env --split-string="claude -p x"`,
	`env -S "FOO=1 claude -p x"`,
	// wrappers
	"sudo claude -p x",
	"sudo -u root claude -p x",
	"sudo -E -u root -- claude -p x",
	"sudo -u root FOO=1 claude -p x",
	"doas -u root claude -p x",
	"nice claude -p x",
	"nice -n 5 claude -p x",
	"nice -n5 claude -p x",
	"nice -5 claude -p x",
	"setsid claude -p x",
	"setsid -f claude -p x",
	"ionice -c 3 claude -p x",
	"ionice -c3 -n 7 claude -p x",
	"time claude -p x",
	"time -p claude -p x",
	"nohup claude -p x",
	"timeout 60 claude -p x",
	"timeout -s KILL 60 claude -p x",
	"stdbuf -oL claude -p x",
	"xargs -I{} claude -p {}",
	"xargs -I {} claude -p {}",
	"echo x | xargs -n 1 claude -p",
	"xargs -0 codex exec",
	`find . -exec claude -p x {} \;`,
	`find . -name '*.go' -execdir codex exec x {} +`,
	`find . -ok claude -p x {} ';'`,
	`eval "claude -p x"`,
	"eval claude -p x",
	`eval 'echo hi; claude -p x'`,
	"exec claude -p x",
	"exec -a fake claude -p x",
	"exec -cl claude -p x",
	"builtin exec claude -p x",
	"command claude -p x",
	"command -p claude -p x",
	`sudo eval "claude -p x"`,
	`nice find . -exec claude -p x \;`,
	"sudo xargs claude -p",
	"/usr/bin/env claude -p x",
	"/usr/bin/time -p claude -p x",
	"/usr/bin/sudo -u root claude -p x",
	"/usr/bin/xargs claude -p",
	"/usr/bin/nice -n 5 claude -p x",
	`env -S '-i claude -p x'`,
	`env -S '-u FOO claude -p x'`,
	`env -S 'FOO=1\_claude\_-p\_x'`,
	"sudo -h host claude -p x",
	"xargs -J % claude -p %",
	"echo x | xargs claude",
	"echo task | xargs codex",
	"echo task | xargs -0 codex",
	`find . -exec claude + {} +`,
	`find . -name a -exec true {} \; -exec claude -p x {} \;`,
	`eval -- "claude -p x"`,
	// redirections anywhere in the command
	">out env claude -p x",
	"env >out claude -p x",
	"sudo 2>/dev/null claude -p x",
	"<in nice claude -p x",
	`claude "2">out`,
	`env -S "claude\_-p\_x"`,
	"echo task | xargs -0 --max-args=1 codex",
	`find . -name -exec -o -exec claude -p x {} \;`,
	`find . -newermt 2026-01-01 -exec claude -p x {} \;`,
	// reserved words before the command word
	"{ claude -p x; }",
	"if true; then claude -p x; fi",
	"if claude -p x; then :; fi",
	"if false; then :; else claude -p x; fi",
	"if false; then :; elif claude -p x; then :; fi",
	"while true; do claude -p x; done",
	"until false; do claude -p x; done",
	"! claude -p x",
	"coproc claude -p x",
	"coproc w { claude -p x; }",
	"function f { claude -p x; }",
	"f() { claude -p x; }",
	// command substitution in the middle of a command
	"claude $(echo) -p x",
	"claude `echo` -p x",
	"claude --model $(cat m) -p x",
	"echo $(claude -p x)",
	"echo `claude -p x`",
	"x=$(claude -p x)",
	// a command word built around a substitution or an expansion
	"claude$(true) -p x",
	"cl`true`aude -p x",
	"$HOME/bin/claude -p x",
	"${HOME}/.local/bin/claude -p x",
	`cl""aude -p x`,
	`c\laude -p x`,
	// a shell flag merged with its argument, or followed by more flags
	`fish -c"claude -p x"`,
	`fish -lc"claude -p x"`,
	`fish --command="claude -p x"`,
	`fish --command "claude -p x"`,
	`fish -c "echo hi" -c "claude -p x"`,
	`fish --init-command "claude -p x" -c "echo hi"`,
	`fish -C"claude -p x"`,
	`fish --init-command="claude -p x"`,
	`bash --rcfile r -c "claude -p x"`,
	`bash -c -e "claude -p x"`,
	`bash -o pipefail -c "claude -p x"`,
	`bash -c -- "claude -p x"`,
	// a parenthesis inside quotes within a substitution
	`echo "$(foo ')'; claude -p x)"`,
	`echo "$(foo ")"; claude -p x)"`,
	`echo $(foo ')'; claude -p x)`,
	`echo "$(printf '%s' "(" ; claude -p x)"`,
	`echo "$(echo $'\')'; claude -p x)"`,
	"echo \"$(echo hi # )\nclaude -p x)\"",
	"echo $(echo hi # )\nclaude -p x)",
	// a here-document with a quoted delimiter is skipped to that delimiter
	"cat <<\"E OF\"\nx\nE OF\nclaude -p x",
	"cat <<'E OF'\nx\nE OF\nclaude -p x",
	"cat <<'EOF'\nx\nEOF\nclaude -p x",
	"cat <<E\\ OF\nx\nE OF\nclaude -p x",
	"cat <<\"E\\\"OF\"\nx\nE\"OF\nclaude -p x",
	"cat <<$'E OF'\nx\nE OF\nclaude -p x",
	"cat <<EOF\nx\nEO\\\nF\nclaude -p x",
	"cat <<A; cat <<B\na\nA\nb\nB\nclaude -p x",
	"cat <<'A'; cat <<B\na\nA\n$(claude -p x)\nB",
	"cat <<EOF\n$\\\n(claude -p x)\nEOF",
	"cat <<EOF\n`clau\\\nde -p x`\nEOF",
	// an unquoted here-document's body runs its substitutions
	"cat <<EOF\n$(claude -p x)\nEOF",
	"cat <<EOF\n`claude -p x`\nEOF",
	// ANSI-C and locale quoting of the command word
	"$'claude' -p x",
	`$'\x63laude' -p x`,
	`$'\143laude' -p x`,
	`$"claude" -p x`,
	"$'codex' exec x",
	`$'clau\0x'de -p x`,
	// quoted words shaped like redirections are arguments
	`claude '>' 'do the task'`,
	`claude ">"`,
	`claude \> out`,
	// a comment ends at the end of its line
	"echo hi # note\nclaude -p x",
	// codex with a prompt starts a session
	`codex "fix the bug"`,
	`codex -m o3 "fix the bug"`,
	`codex --model=o3 "fix the bug"`,
	`codex -c model=o3 "fix the bug"`,
	`codex e "task"`,
	`codex --full-auto "task"`,
	`codex sandbox macos claude -p x`,
	`codex sandbox claude -p x`,
	`codex sandbox -c k=v claude -p x`,
	`codex sandbox --permission-profile team claude -p x`,
	`codex sandbox --permission-profile=team claude -p x`,
	`codex sandbox -P team claude -p x`,
	`codex sandbox --allow-unix-socket /tmp/s claude -p x`,
	`codex -- login`,
	`codex -m o3 -- resume`,
	`codex resume abc "fix it"`,
	`codex resume --last "fix it"`,
	`codex fork abc "fix it"`,
	`codex cloud exec "task"`,
	`codex queue abc "next step"`,
	`codex -- "-p is in this prompt"`,
	`codex -i a.png b.png`,
	`codex -c x=1 exec "task"`,
	`codex "task" > out.txt`,
}

// TestLaunchGateAllowsOrdinaryCommands: commands that only mention claude or
// codex, or use the same wrappers for something else, stay allowed.
func TestLaunchGateAllowsOrdinaryCommands(t *testing.T) {
	for _, cmd := range ordinaryCommands {
		if rawLaunch(cmd) {
			t.Errorf("%q: denied, want allowed", cmd)
		}
	}
}

// ordinaryCommands only mention claude or codex.
var ordinaryCommands = []string{
	`git commit -m "claude helped"`,
	`git commit -m "fix: codex exec wording"`,
	"grep claude file",
	"grep -rn codex .",
	"echo codex",
	"echo claude -p x",
	"echo $'claude' -p x",
	`printf '%s\n' "claude -p x"`,
	"env FOO=1 go test ./...",
	"env -u FOO go test ./...",
	"env -S 'go test ./...'",
	"env",
	"sudo -u x ls",
	"sudo -l",
	"doas -u x ls",
	"find . -name claude",
	"find . -name codex -type d",
	`find . -name '*.go' -exec grep -l claude {} \;`,
	"xargs -I{} echo claude {}",
	"ls | xargs grep claude",
	"nice -n 5 make test",
	"time go test ./...",
	"timeout 5 go test ./...",
	"setsid htop",
	"exec -a name htop",
	"command -v claude",
	"command -V codex",
	"builtin echo claude",
	`eval "$(ssh-agent -s)"`,
	"eval echo claude",
	"if [ -f claude ]; then echo claude; fi",
	"{ echo claude; }",
	"while read l; do echo codex; done < f",
	"for x in claude codex; do echo $x; done",
	"ls $(dirname claude)",
	`echo "$(echo ')')"`,
	"echo $((1 + 2))",
	"cat <<'EOF'\n$(claude -p x)\nEOF",
	"cat <<\"EOF\"\n`claude -p x`\nEOF",
	"cat > notes.md <<EOF\nclaude -p x\nEOF",
	"cat <<'E OF'\nclaude -p x\nE OF",
	`bash -c "echo claude"`,
	`sh -c 'grep -rn claude docs'`,
	`fish -c "echo codex"`,
	"bash script.sh -c claude",
	"which claude",
	"$EDITOR claude.md",
	"echo $HOME/bin/claude",
	"codex",
	"codex --version",
	"codex --help",
	"codex login",
	"codex mcp list",
	"codex features list",
	"codex completion zsh",
	"codex -m o3 login",
	"codex doctor",
	"codex sandbox macos ls",
	"codex sandbox ls -la",
	"codex sandbox --permission-profile team ls",
	"codex resume -- --last",
	"codex --version < /dev/null",
	"codex --version <<< x",
	"echo x | xargs --replace codex",
	"echo x | xargs --replace=% codex",
	"echo x | xargs -i codex",
	"echo x | xargs -ifoo codex",
	"find . -name -exec -o -name codex",
	"echo \"$(echo hi # ) claude -p x\n)\"",
	"codex resume",
	"codex resume abc",
	"codex resume --last",
	"codex fork --last",
	"codex cloud list",
	"codex help exec",
	"codex -p review",
	"codex --version >/dev/null",
	"codex --version 2>&1",
	"codex login > out.txt",
	"codex --version # check",
	"codex execpolicy --help",
	"codex git-credential get",
	`bash -c"claude -p x"`,
	"bash --rcfile claude -i",
	`fish script.fish "claude -p x"`,
	`fish -c 'echo $argv' 'claude -p x'`,
	`find . -exec grep -e -exec codex {} \;`,
	"echo hi # claude -p x",
	"claude \\\n  --version",
	"sudo -l claude -p x",
	"sudo -v",
	"/usr/bin/env go test ./...",
	"echo x | xargs -I{} echo codex {}",
	"eval -- echo claude",
	"cat <<\"E\\\"OF\"\n$(claude -p x)\nE\"OF",
}

// TestLaunchGateCutInput: every prefix of every table command, and a few
// cut forms, is read without a panic.
func TestLaunchGateCutInput(t *testing.T) {
	extra := []string{"cat <<A; cat <<B\nx", "cat <<A <<B\n", "cat <<A\nx\\", "a #", "#", "$'\\c", "$'\\c'", `"$($'\'`, "env -S", "env -S'", "fish -c", "codex sandbox", "codex resume --last", "$", "$'", `$'\`, `$'\x`, "<<", "<<'", "<<\"", "<<\\", "`", "$(", `"$(`, `"\`, "\\", "bash -c", "env -S", "find . -exec", "coproc", "function"}
	for _, list := range [][]string{wrappedLaunches, ordinaryCommands, extra} {
		for _, cmd := range list {
			for i := 0; i <= len(cmd); i++ {
				rawLaunch(cmd[:i])
				bashExplores(cmd[:i])
				_, _ = bashSearch(cmd[:i])
			}
		}
	}
}

// TestWrapperRunsNothing: a wrapper option that runs no command is no read
// and no search either.
func TestWrapperRunsNothing(t *testing.T) {
	if bashExplores("sudo -l rg Widget .") {
		t.Error("sudo -l rg: counted as a read")
	}
	if _, ok := bashSearch("sudo -l rg Widget ."); ok {
		t.Error("sudo -l rg: read as a search")
	}
	if !bashExplores("sudo rg Widget .") {
		t.Error("sudo rg: not counted as a read")
	}
}
