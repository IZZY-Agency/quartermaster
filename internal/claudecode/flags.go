package claudecode

import "strings"

// noValue are claude's flags that take no value.
var noValue = map[string]bool{"-c": true, "--continue": true, "--verbose": true, "--ide": true, "-p": true, "--print": true, "-h": true, "--help": true, "-v": true, "--version": true}

// KeptFlags walks claude's arguments, as sessionOfArgs does (it stops at
// "--" and at the first token holding a newline, which ps prints as \012),
// and returns the fixed list of flags qm keeps when it restarts a session it
// did not start, in order: --model V, --effort V, --permission-mode V (the
// --flag=V spellings become two words; a value starting with "-" is not
// taken), --dangerously-skip-permissions and
// --allow-dangerously-skip-permissions. dropped is every other token
// starting with "-", flag name only (=... cut), once, in order. It also
// stops at the first positional token that cannot be the value of the flag
// before it (a token after another positional, after a flag that takes no
// value, or after a flag written --flag=V): it starts a prompt, which ps
// shows split into words, so a flag name inside it is never read. A token
// after a dropped flag counts as that flag's value; losing a later flag is
// safer than reading one out of a prompt.
//
// claude's flags known to take no value (noValue) never take the next token:
// after one, a plain word starts the prompt.
func KeptFlags(args []string) (kept, dropped []string) {
	seen := map[string]bool{}
	drop := func(name string) {
		if !seen[name] {
			seen[name] = true
			dropped = append(dropped, name)
		}
	}
	value := func(i int) (string, bool) {
		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") && !strings.Contains(args[i+1], `\012`) {
			return args[i+1], true
		}
		return "", false
	}
	canValue := false // the token before may take args[i] as its value
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" || strings.Contains(a, `\012`) {
			break
		}
		if !strings.HasPrefix(a, "-") {
			if !canValue {
				break
			}
			canValue = false
			continue
		}
		canValue = false
		name, val, hasEq := strings.Cut(a, "=")
		switch name {
		case "--model", "--effort", "--permission-mode":
			if hasEq {
				if val != "" && !strings.HasPrefix(val, "-") {
					kept = append(kept, name, val)
				}
			} else if v, ok := value(i); ok {
				kept = append(kept, name, v)
				i++
			}
		case "--dangerously-skip-permissions", "--allow-dangerously-skip-permissions":
			if !hasEq {
				kept = append(kept, name)
			}
		default:
			drop(name)
			canValue = !hasEq && !noValue[name]
		}
	}
	return kept, dropped
}
