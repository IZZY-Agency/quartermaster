package hooks

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// exploreCommands are the Bash commands that count as exploration reads.
var exploreCommands = map[string]bool{
	"cat": true, "sed": true, "grep": true, "rg": true, "head": true, "tail": true, "find": true,
}

// smallRead is the most lines a Read limit or a sed -n range may cover and
// still not count as exploration.
const smallRead = 80

var sedRange = regexp.MustCompile(`^(\d+),(\d+)p$`)

// isExploration tells whether a tool call reads code to explore it: a Read
// of a whole file or more than smallRead lines, any Grep, or a Bash command
// whose first command (after any leading cd) is cat, sed, grep, rg, head,
// tail or find, except a small sed -n 'A,Bp' range and a cat that writes a
// file.
func isExploration(tool string, input map[string]any) bool {
	switch tool {
	case "Read":
		limit, ok := input["limit"].(float64)
		if !ok {
			if n, isInt := input["limit"].(int); isInt {
				limit, ok = float64(n), true
			}
		}
		return !ok || limit > smallRead
	case "Grep":
		return true
	case "Bash":
		cmd, _ := input["command"].(string)
		return bashExplores(cmd)
	}
	return false
}

func bashExplores(cmd string) bool {
	segs := segments(cmd)
	for len(segs) > 0 && len(segs[0]) > 0 && segs[0][0] == "cd" {
		segs = segs[1:]
	}
	if len(segs) == 0 {
		return false
	}
	w := commandWords(segs[0])
	if len(w) == 0 || !exploreCommands[filepath.Base(w[0])] {
		return false
	}
	switch filepath.Base(w[0]) {
	case "cat":
		for i, a := range w[1:] {
			if a == "<<" {
				return false
			}
			// A redirection of stdout to a file makes cat a write; stderr
			// redirections (2>/dev/null, 2>&1) do not.
			stdout := a == ">" || a == ">>" || a == "1>" || a == "1>>"
			if stdout && (i+2 >= len(w) || w[i+2] != "/dev/null") {
				return false
			}
		}
	case "sed":
		for i := 1; i+1 < len(w); i++ {
			if w[i] != "-n" {
				continue
			}
			if m := sedRange.FindStringSubmatch(w[i+1]); m != nil {
				a, _ := strconv.Atoi(m[1])
				b, _ := strconv.Atoi(m[2])
				if b >= a && b-a+1 <= smallRead {
					return false
				}
			}
		}
	}
	return true
}

// claudeSafeFirst are first arguments after which claude does not start a
// session.
var claudeSafeFirst = map[string]bool{
	"plugin": true, "mcp": true, "--version": true, "-v": true, "--help": true, "-h": true,
	"doctor": true, "update": true, "config": true,
}

// claudeLaunchFlags start a session with a task.
var claudeLaunchFlags = map[string]bool{"-p": true, "--print": true, "--model": true, "-n": true, "--name": true}

// claudeValueFlags take a value as the next argument, which is not a prompt.
var claudeValueFlags = map[string]bool{
	"--permission-mode": true, "--resume": true, "-r": true, "--session-id": true,
	"--settings": true, "--setting-sources": true, "--plugin-dir": true, "--mcp-config": true,
	"--add-dir": true, "--allowedTools": true, "--allowed-tools": true,
	"--disallowedTools": true, "--disallowed-tools": true, "--tools": true,
	"--append-system-prompt": true, "--system-prompt": true, "--effort": true, "--agent": true,
	"--agents": true, "--output-format": true, "--input-format": true, "--autocompact": true,
	"--fallback-model": true, "--max-turns": true, "--permission-prompt-tool": true,
}

// tmuxValueFlags take a value in tmux new-window, new-session and send-keys.
var tmuxValueFlags = map[string]bool{
	"-c": true, "-e": true, "-F": true, "-n": true, "-s": true, "-t": true,
	"-x": true, "-y": true, "-f": true, "-N": true,
}

// rawLaunch tells whether a Bash command launches a reviewer or a Claude
// session without qm: codex exec or review, an agtermctl session new whose
// --command starts claude or codex, a tmux window, session or send-keys
// that starts claude or codex, or claude with a prompt, -p, --model or -n. qm's own commands in the chain are not
// launches; every other command in it is checked, and so are command
// substitutions inside double quotes.
func rawLaunch(cmd string) bool {
	segs, subs := parse(cmd)
	for _, seg := range segs {
		if launches(commandWords(seg)) {
			return true
		}
	}
	for _, s := range subs {
		if rawLaunch(s) {
			return true
		}
	}
	return false
}

func launches(w []string) bool {
	if len(w) == 0 {
		return false
	}
	args := w[1:]
	switch filepath.Base(w[0]) {
	case "codex":
		return contains(args, "exec") || contains(args, "review")
	case "agtermctl":
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "session" && args[i+1] == "new" {
				return agtermLaunch(args[i+2:])
			}
		}
	case "tmux":
		return tmuxLaunch(args)
	case "claude":
		return claudeLaunch(args)
	case "sh", "bash", "zsh", "fish", "dash", "ksh":
		for i := 0; i+1 < len(args); i++ {
			a := args[i]
			if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "c") {
				return rawLaunch(args[i+1])
			}
		}
	}
	return false
}

// agtermLaunch tells whether agtermctl session new's arguments start claude
// or codex. Every --command (or --command=) anywhere is checked first, so
// neither a value flag this list does not know nor a second --command can
// hide one; only without any is an operand after the options read as a
// command (agtermctl takes none today). A session without a command runs
// the login shell, which is no launch.
func agtermLaunch(args []string) bool {
	found := false
	for i, a := range args {
		if v, ok := strings.CutPrefix(a, "--command="); ok {
			found = true
			if startsClaude(v) {
				return true
			}
		}
		if a == "--command" {
			found = true
			if i+1 < len(args) && startsClaude(args[i+1]) {
				return true
			}
		}
	}
	if found {
		return false
	}
	for i := 0; i < len(args); i++ {
		name, _, hasValue := strings.Cut(args[i], "=")
		switch {
		case strings.HasPrefix(args[i], "-"):
			if agtermValueFlags[name] && !hasValue {
				i++
			}
		default:
			return startsClaude(strings.Join(args[i:], " "))
		}
	}
	return false
}

// agtermValueFlags take a value in agtermctl session new.
var agtermValueFlags = map[string]bool{
	"--cwd": true, "--workspace": true, "--workspace-name": true, "--name": true,
	"--after": true, "--before": true, "--parent": true, "--remote": true, "--origin-window": true,
	"--tint": true, "--tint-on": true, "--socket": true, "--window": true, "--op-id": true,
	"--epoch": true, "--expect-panes": true,
}

// tmuxLaunch tells whether tmux new-window, new-session or send-keys starts
// claude or codex (with any arguments) or another raw launch. Option values (the
// window name, the target) are skipped.
func tmuxLaunch(args []string) bool {
	for i, a := range args {
		switch a {
		case "new-window", "neww", "new-session", "new":
			rest := tmuxOperands(args[i+1:])
			return len(rest) > 0 && startsClaude(strings.Join(rest, " "))
		case "send-keys", "send":
			for _, k := range tmuxOperands(args[i+1:]) {
				if startsClaude(k) {
					return true
				}
			}
			return false
		}
	}
	return false
}

// tmuxOperands drops a tmux subcommand's options and their values.
func tmuxOperands(args []string) []string {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		if tmuxValueFlags[args[0]] && len(args) > 1 {
			args = args[1:]
		}
		args = args[1:]
	}
	return args
}

// startsClaude tells whether a shell command runs claude or codex in any
// form, or another raw launch.
func startsClaude(cmd string) bool {
	for _, seg := range segments(cmd) {
		w := commandWords(seg)
		if len(w) > 0 && (filepath.Base(w[0]) == "claude" || filepath.Base(w[0]) == "codex") {
			return true
		}
	}
	return rawLaunch(cmd)
}

// claudeLaunch tells whether claude's arguments start a session with a
// task: a launch flag anywhere, or any argument that is neither a flag nor a
// flag's value (a prompt).
func claudeLaunch(args []string) bool {
	if len(args) == 0 || claudeSafeFirst[args[0]] {
		return false
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, _, hasValue := strings.Cut(a, "=")
		switch {
		case claudeLaunchFlags[name]:
			return true
		case claudeValueFlags[name]:
			if !hasValue {
				i++
			}
		case !strings.HasPrefix(a, "-"):
			return true
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

var assignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// commandWords drops what runs before the real command: variable
// assignments and the env, exec, command, nohup and time wrappers (with
// env's flags).
func commandWords(w []string) []string {
	for len(w) > 0 {
		switch {
		case assignment.MatchString(w[0]):
			w = w[1:]
		case w[0] == "env":
			w = w[1:]
			for len(w) > 0 && strings.HasPrefix(w[0], "-") {
				w = w[1:]
			}
		case w[0] == "exec" || w[0] == "command" || w[0] == "nohup" || w[0] == "time":
			w = w[1:]
		default:
			return w
		}
	}
	return w
}

// segments splits a shell command into simple commands, each a list of
// words. See parse.
func segments(cmd string) [][]string {
	segs, _ := parse(cmd)
	return segs
}

// parse splits a shell command into simple commands, each a list of words.
// It splits on ; & && || | newlines, parentheses, backquotes and $(;
// removes quotes, keeping a quoted string as one word; emits redirections as
// words of their own, with their file descriptor and any duplication (">",
// ">>", "2>", "2>&1", ">&2", "<", "<<", "<<<"); and skips here-document
// bodies. It also returns the command substitutions ($(...) and backquotes)
// found inside double quotes, which run as commands too. It is a reader for
// the guards, not a shell: it errs towards splitting.
func parse(cmd string) (segs [][]string, subs []string) {
	type heredoc struct {
		delim string
		tabs  bool
	}
	var (
		seg     []string
		word    strings.Builder
		inWord  bool
		pending []heredoc
	)
	endWord := func() {
		if inWord {
			seg = append(seg, word.String())
			word.Reset()
			inWord = false
		}
	}
	endSeg := func() {
		endWord()
		if len(seg) > 0 {
			segs = append(segs, seg)
			seg = nil
		}
	}
	n := len(cmd)
	for i := 0; i < n; i++ {
		ch := cmd[i]
		switch {
		case ch == ' ' || ch == '\t':
			endWord()
		case ch == '\n':
			endSeg()
			// Skip the bodies of here-documents opened on this line.
			for _, h := range pending {
				for i+1 < n {
					j := strings.IndexByte(cmd[i+1:], '\n')
					line := cmd[i+1:]
					if j >= 0 {
						line = cmd[i+1 : i+1+j]
					}
					if j < 0 {
						i = n
					} else {
						i += j + 1
					}
					if h.tabs {
						line = strings.TrimLeft(line, "\t")
					}
					if line == h.delim {
						break
					}
				}
			}
			pending = nil
		case ch == '\'':
			inWord = true
			j := strings.IndexByte(cmd[i+1:], '\'')
			if j < 0 {
				word.WriteString(cmd[i+1:])
				i = n
			} else {
				word.WriteString(cmd[i+1 : i+1+j])
				i += j + 1
			}
		case ch == '"':
			inWord = true
			for i++; i < n && cmd[i] != '"'; i++ {
				switch {
				case cmd[i] == '\\' && i+1 < n:
					i++
				case strings.HasPrefix(cmd[i:], "$("):
					if end := closingParen(cmd, i+2); end > 0 {
						subs = append(subs, cmd[i+2:end])
					}
				case cmd[i] == '`':
					if j := strings.IndexByte(cmd[i+1:], '`'); j >= 0 {
						subs = append(subs, cmd[i+1:i+1+j])
					}
				}
				word.WriteByte(cmd[i])
			}
		case ch == '\\':
			inWord = true
			if i+1 < n {
				i++
				if cmd[i] != '\n' {
					word.WriteByte(cmd[i])
				}
			}
		case ch == ';' || ch == '&' || ch == '|' || ch == '(' || ch == ')' || ch == '`':
			endSeg()
		case ch == '$' && i+1 < n && cmd[i+1] == '(':
			endSeg()
			i++
		case ch == '>':
			fd := ""
			if inWord && isDigits(word.String()) {
				fd = word.String()
				word.Reset()
				inWord = false
			}
			endWord()
			op := fd + ">"
			if i+1 < n && cmd[i+1] == '>' {
				op += ">"
				i++
			}
			switch {
			case i+1 < n && cmd[i+1] == '|':
				i++
			case i+1 < n && cmd[i+1] == '&':
				op += "&"
				for i++; i+1 < n && (isDigits(cmd[i+1:i+2]) || cmd[i+1] == '-'); i++ {
					op += cmd[i+1 : i+2]
				}
			}
			seg = append(seg, op)
		case ch == '<':
			endWord()
			switch {
			case strings.HasPrefix(cmd[i:], "<<<"):
				seg = append(seg, "<<<")
				i += 2
			case strings.HasPrefix(cmd[i:], "<<"):
				seg = append(seg, "<<")
				i += 2
				h := heredoc{}
				if i < n && cmd[i] == '-' {
					h.tabs = true
					i++
				}
				for i < n && (cmd[i] == ' ' || cmd[i] == '\t') {
					i++
				}
				var d strings.Builder
				for ; i < n && !strings.ContainsRune(" \t\n;&|<>()", rune(cmd[i])); i++ {
					if cmd[i] != '\'' && cmd[i] != '"' && cmd[i] != '\\' {
						d.WriteByte(cmd[i])
					}
				}
				i--
				if d.Len() > 0 {
					h.delim = d.String()
					pending = append(pending, h)
				}
			default:
				seg = append(seg, "<")
			}
		default:
			inWord = true
			word.WriteByte(ch)
		}
	}
	endSeg()
	return segs, subs
}

// closingParen is the index of the ")" that closes a "(" just before from,
// or -1.
func closingParen(s string, from int) int {
	depth := 1
	for i := from; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// codeExtensions are the file types an LSP plugin covers.
var codeExtensions = map[string]bool{
	".go": true, ".py": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true, ".mjs": true,
	".php": true, ".rb": true, ".java": true, ".kt": true, ".swift": true, ".c": true, ".h": true,
	".cc": true, ".cpp": true, ".hpp": true, ".rs": true, ".cs": true,
}

// searchValueFlags take a value in grep and rg; for -e and --regexp the
// value is the pattern.
var searchValueFlags = map[string]bool{
	"-e": true, "--regexp": true, "-f": true, "--file": true, "-m": true, "--max-count": true,
	"-A": true, "-B": true, "-C": true, "-g": true, "--glob": true, "-t": true, "--type": true,
	"-T": true, "--type-not": true, "--include": true, "--exclude": true, "--exclude-dir": true,
}

// bashSearch returns the pattern of a Bash grep or rg run as the first
// command (after any cd) over code: no path (rg's working folder, or a
// recursive grep), a folder, a code file, or a code-file type or glob. ok is
// false for anything else, such as grep over a log or standard input.
func bashSearch(cmd string) (pattern string, ok bool) {
	segs := segments(cmd)
	for len(segs) > 0 && len(segs[0]) > 0 && segs[0][0] == "cd" {
		segs = segs[1:]
	}
	if len(segs) == 0 {
		return "", false
	}
	w := commandWords(segs[0])
	if len(w) == 0 {
		return "", false
	}
	tool := filepath.Base(w[0])
	if tool != "grep" && tool != "rg" {
		return "", false
	}
	var paths []string
	recursive, codeFilter, havePattern := tool == "rg", false, false
	for i := 1; i < len(w); i++ {
		a := w[i]
		name, value, hasValue := strings.Cut(a, "=")
		switch {
		case searchValueFlags[name]:
			if !hasValue && i+1 < len(w) {
				i++
				value = w[i]
			}
			switch name {
			case "-e", "--regexp":
				pattern, havePattern = value, true
			case "-t", "--type":
				codeFilter = true
			case "-g", "--glob", "--include":
				codeFilter = codeFilter || codeExtensions[filepath.Ext(strings.Trim(value, "*"))]
			}
		case strings.HasPrefix(a, "-"):
			if (!strings.HasPrefix(a, "--") && strings.ContainsAny(a, "rR")) || a == "--recursive" {
				recursive = true
			}
		case !havePattern:
			pattern, havePattern = a, true
		default:
			paths = append(paths, a)
		}
	}
	if !havePattern {
		return "", false
	}
	if len(paths) == 0 {
		return pattern, recursive || codeFilter
	}
	for _, p := range paths {
		if ext := filepath.Ext(p); ext == "" || p == "." || p == ".." || codeExtensions[ext] || strings.HasSuffix(p, "/") {
			return pattern, true
		}
	}
	return pattern, codeFilter
}
