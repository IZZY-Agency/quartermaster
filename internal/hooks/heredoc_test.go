package hooks

import (
	"strings"
	"testing"
)

// hereDocShapes are commands around here-documents in substitutions; COMMAND
// stands for the command a guard looks for. main is what the launch gate
// decided before it read here-documents there (origin/main at be1df0c), with
// "claude -p hi" as the command: it is fixed, and never to be changed to let a
// command through. runs tells whether a shell (bash 3.2 or 5.2, zsh, dash) runs
// the command.
var hereDocShapes = []struct {
	name, cmd  string
	main, runs bool
}{
	{"ended by EOF)", "git commit -m \"$(cat <<'EOF'\nmsg\nEOF)\"\nCOMMAND \"$(cat <<'EOF'\ntask\nEOF\n)\"", true, true},
	{"ended by EOF), unquoted", "git commit -m \"$(cat <<EOF\nmsg\nEOF)\"\nCOMMAND \"$(cat <<EOF\ntask\nEOF\n)\"", true, true},
	{"ended by EOF), apostrophe", "git commit -m \"$(cat <<'EOF'\nit's\nEOF)\"\nCOMMAND \"$(cat <<'EOF'\ntask\nEOF\n)\"", true, true},
	{"ended by EOF), <<-", "git commit -m \"$(cat <<-EOF\n\tmsg\n\tEOF)\"\nCOMMAND\nEOF\n", true, true},
	{"ended by EOF )", "echo \"$(cat <<EOF\nmsg\nEOF )\"\nCOMMAND \"$(cat <<EOF\ntask\nEOF\n)\"", true, true},
	{"ended by EOFX)", "echo \"$(cat <<EOF\nmsg\nEOFX)\"\nCOMMAND \"$(cat <<EOF\ntask\nEOF\n)\"", true, true},
	{"EOF) in a nested group", "echo \"$( (cat <<EOF\nmsg\nEOF) ; COMMAND)\"", false, true},
	{"EOF) in a process substitution", "cat <(cat <<'EOF'\nmsg\nEOF)\nCOMMAND", false, true},
	{"EOF) ends no document of a list", "cat <<EOF\nmsg\nEOF)\nCOMMAND\nEOF", false, false},
	{"EOF) in prose", "git commit -m \"$(cat <<'EOF'\nthe delimiter (EOF) can't run `COMMAND` now\nEOF\n)\"", true, false},

	{"shift in an expansion", "echo \"$(echo $((1<<2))\n)\"\nCOMMAND\n\"\n2\n)\"", true, true},
	{"shift in an expansion, own word", "echo $((1<<2))\nCOMMAND\n2\n", true, true},
	{"shift in an expansion, quoted", "echo \"$((1<<2))\"\nCOMMAND\n2\n", true, true},
	{"shift over lines", "echo $((1<<2\n+3))\nCOMMAND\n2\n", true, true},
	{"shift in nested parentheses", "echo $(( (1<<2) + (1<<3) ))\nCOMMAND\n3\n", true, true},
	{"shift in a command", "((x=1<<2))\nCOMMAND\n2\n", false, true},
	{"shift in a command, spaced", "(( x = 1 << y ))\nCOMMAND\ny\n", false, true},
	{"shift in a command in a substitution", "echo \"$( ((x=1<<2)); echo\n)\"\nCOMMAND\n\"\n2\n)\"", true, true},
	{"substitution in arithmetic", "echo $(( 1 << $(COMMAND) ))", true, true},
	{"substitution in arithmetic, quoted shift", "echo $(( 1 << 'x'\n+ $(COMMAND) ))", false, true},
	{"substitution in an arithmetic command", "(( x = 1 << $(COMMAND) ))", true, true},
	{"on the delimiter's line", "echo \"$(cat <<EOF\nmsg\nEOF COMMAND)\"", false, true},
	{"on the delimiter's line, quoted", "echo \"$(cat <<'EOF'\nit's\nEOF COMMAND)\"", false, true},
	{"on the delimiter's line, joined", "echo \"$(cat <<EOF\nmsg\nEO\\\nF ec\\\nho; COMMAND)\"", false, true},
	{"shift in $[ ]", "echo $[1<<2]\nCOMMAND", false, true},
	{"shift in $[ ], in a substitution", "echo \"$(echo $[1<<2]\n)\"\nCOMMAND\n\"\n2]\n)\"", true, true},
	{"shift in a substring", "echo ${s:1<<2}\nCOMMAND", false, true},
	{"shift in a subscript", "echo ${a[1<<2]}\nCOMMAND", false, true},
	{"document after an expansion", "echo ${s:1} $[1] \"$(cat <<'EOF'\nit's `COMMAND` }\nEOF\n)\"", true, false},
	{"document in a group at once", "((cat <<'EOF'\nit's COMMAND (\nEOF\n); echo)", false, false},

	{"after, quoted", "echo \"$(cat <<'EOF'\nmsg\nEOF\n)\"\nCOMMAND", true, true},
	{"after, double-quoted", "echo \"$(cat <<\"EOF\"\nmsg\nEOF\n)\"\nCOMMAND", true, true},
	{"after, unquoted", "echo \"$(cat <<EOF\nmsg\nEOF\n)\"\nCOMMAND", true, true},
	{"after, backslash", "echo \"$(cat <<\\EOF\nmsg\nEOF\n)\"\nCOMMAND", true, true},
	{"after, partly quoted", "echo \"$(cat <<E'O'F\nit's\nEOF\n)\"\nCOMMAND", true, true},
	{"after, dashed word", "echo \"$(cat <<EOF-MSG\nit's\nEOF-MSG\n)\"\nCOMMAND", true, true},
	{"after, blanks", "echo \"$(cat <<  'END OF IT'\nit's\nEND OF IT\n)\"\nCOMMAND", true, true},
	{"after, <<-", "echo \"$(cat <<-EOF\n\tit's\n\tEOF\n)\"\nCOMMAND", true, true},
	{"after, tabs and no <<-", "echo \"$(cat <<'EOF'\n\tEOF\nit's\nEOF\n)\"\nCOMMAND", true, true},
	{"after, spaces before the delimiter", "echo \"$(cat <<'EOF'\n  EOF\nit's\nEOF\n)\"\nCOMMAND", true, true},
	{"after, apostrophes that pair up", "x=\"$(cat <<'EOF'\nit's\nEOF\n)\"; COMMAND 'hi'; echo 'y)'", false, true},
	{"after, joined line", "echo \"$(cat <<EOF\nmsg\\\nEOF\nEOF\n)\"\nCOMMAND", true, true},
	{"after, parentheses", "echo \"$(cat <<'EOF'\n:-) and (\nEOF\n)\"\nCOMMAND", true, true},
	{"after, not in quotes", "echo $(cat <<'EOF'\nit's\nEOF\n)\nCOMMAND", true, true},
	{"after, backquotes", "echo `cat <<'EOF'\nmsg\nEOF\n`\nCOMMAND", true, true},

	{"in the body, quoted", "echo \"$(cat <<'EOF'\nit's COMMAND, `COMMAND` or $(COMMAND)\nEOF\n)\"", true, false},
	{"in the body, backslash", "git commit -m \"$(cat <<\\EOF\nThe worker can't run `COMMAND` now\nEOF\n)\"", true, false},
	{"in the body, dashed word", "git commit -m \"$(cat <<'EOF-MSG'\nThe worker can't run `COMMAND` now\nEOF-MSG\n)\"", true, false},
	{"in the body, <<-", "git commit -m \"$(cat <<-'EOF'\n\tcan't (run `COMMAND`\n\tEOF\n)\"", true, false},
	{"in the body, unquoted", "echo \"$(cat <<EOF\nCOMMAND\nEOF\n)\"", false, false},
	{"in the body, expanded", "echo \"$(cat <<EOF\n$(COMMAND)\nEOF\n)\"", true, true},
	{"in the body, expanded, apostrophe", "echo \"$(cat <<EOF\nit's `COMMAND`\nEOF\n)\"", true, true},

	{"here-string", "echo \"$(cat <<<EOF\n)\"\nCOMMAND\n\"\nEOF\n)\"", true, true},
	{"here-string, quoted", "echo \"$(cat <<<'EOF'\n)\"\nCOMMAND\n\"\nEOF\n)\"", true, true},
	{"unterminated", "echo \"$(cat <<'EOF'\nmsg\n)\"\nCOMMAND", true, true},
	{"unterminated, command in it", "cat <<'EOF'\nCOMMAND", false, false},
	{"opened in a comment", "echo \"$(echo # <<EOF\n)\"\nCOMMAND\n\"\nEOF\n)\"", true, true},
	{"opened in quotes", "echo \"$(echo '<<EOF' \"<<EOF\" \\<<EOF\n)\"\nCOMMAND", true, true},
	{"closed on its own line", "echo $(cat <<EOF)\nCOMMAND\nEOF", true, true},

	{"two documents", "echo \"$(cat <<'A' <<'B'\nit's\nA\n)\"\nB\n)\"\nCOMMAND", false, true},
	{"two documents, command in the second", "echo \"$(cat <<'A' <<'B'\nit's\nA\n`COMMAND`\nB\n)\"", true, false},
	{"two documents, last ended by B)", "echo \"$(cat <<A <<B\na\nA\nb\nB)\"\nCOMMAND\n\"\nB\n)\"", true, true},
	{"nested substitutions", "echo \"$(echo \"$(cat <<'EOF'\nit's\nEOF\n)\")\"\nCOMMAND", true, true},
	{"nested substitutions, ended by EOF)", "echo \"$(echo \"$(cat <<'EOF'\nmsg\nEOF)\")\"\nCOMMAND \"$(cat <<'EOF'\ntask\nEOF\n)\"", true, true},
	{"nested substitutions, inside", "echo \"$(echo \"$(cat <<'EOF'\nit's\nEOF\nCOMMAND)\")\"", true, true},
	{"document after a nested substitution", "echo \"$(cat $(echo f) <<'EOF'\nit's `COMMAND`\nEOF\n)\"", true, false},
}

// TestHereDocShapesLaunchGate: the launch gate denies every shape it denied
// before it read here-documents in substitutions, and every shape a shell
// runs; it denies nothing else.
func TestHereDocShapesLaunchGate(t *testing.T) {
	for _, s := range hereDocShapes {
		cmd := strings.ReplaceAll(s.cmd, "COMMAND", "claude -p hi")
		got := rawLaunch(cmd)
		if s.main && !got {
			t.Errorf("%s: main denies it, the gate allows it: %q", s.name, cmd)
		}
		if want := s.main || s.runs; got != want {
			t.Errorf("%s: denied %v, want %v: %q", s.name, got, want, cmd)
		}
	}
}

// TestHereDocShapesPatternKill: the rule, which never softens, refuses the
// shapes a shell runs and no other.
func TestHereDocShapesPatternKill(t *testing.T) {
	for _, s := range hereDocShapes {
		cmd := strings.ReplaceAll(s.cmd, "COMMAND", "pkill x")
		if got := patternKill(cmd); got != s.runs {
			t.Errorf("%s: refused %v, want %v: %q", s.name, got, s.runs, cmd)
		}
	}
}

// TestHereDocShapesCutInput: every prefix of every shape is read without a
// panic.
func TestHereDocShapesCutInput(t *testing.T) {
	for _, s := range hereDocShapes {
		for i := 0; i <= len(s.cmd); i++ {
			rawLaunch(s.cmd[:i])
			patternKill(s.cmd[:i])
		}
	}
}
