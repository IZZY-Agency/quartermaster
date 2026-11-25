package hooks

import (
	"path/filepath"
	"regexp"
	"slices"
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
// session without qm: codex exec or review or codex with a prompt, an
// agtermctl session new whose --command starts claude or codex, a tmux
// window, session or send-keys that starts claude or codex, or claude with a
// prompt, -p, --model or -n. qm's own commands in the chain are not
// launches; every other command in it is checked, and so are command
// substitutions, the commands that wrappers (env, sudo, nice, xargs, find
// -exec, eval and the like) and shells (sh -c) run, and what follows shell
// reserved words.
//
// It reads the command, it does not run a shell, so a launch it cannot see
// in the text is out of its reach: a command name that comes from a
// variable or a substitution ($cmd -p x, $(which claude) -p x), and a launch
// inside another interpreter's code (python3 -c, perl -e, node -e,
// osascript -e). Those are the known limit of this gate.
//
// Shells end a substitution that holds a here-document at different places, so
// a launch that any of the readings finds is one.
func rawLaunch(cmd string) bool {
	return slices.ContainsFunc([]reading{readParens, readDocs, readDocsInParens}, func(r reading) bool { return r.rawLaunch(cmd) })
}

// rawLaunch is rawLaunch as one reading finds it.
func (r reading) rawLaunch(cmd string) bool {
	segs, subs := r.parse(cmd)
	for _, seg := range segs {
		if r.launches(launchWords(seg)) {
			return true
		}
	}
	return slices.ContainsFunc(subs, r.rawLaunch)
}

// commandName is the program a command word names: its base name, with the
// substitutions and parameter expansions in it ($(...), backquotes, $NAME,
// ${...}) dropped, so a word that only builds the name around them
// (claude$(true), $HOME/bin/claude) is still read as that name.
func commandName(word string) string {
	var b strings.Builder
	for i := 0; i < len(word); i++ {
		c := word[i]
		if end, _, ok := readParens.substitution(word, i); ok && (c == '`' || c == '$') {
			i = end
			continue
		}
		if c == '$' && i+1 < len(word) {
			switch next := word[i+1]; {
			case next == '{':
				if j := strings.IndexByte(word[i:], '}'); j >= 0 {
					i += j
					continue
				}
			case next == '_' || next >= 'A' && next <= 'Z' || next >= 'a' && next <= 'z':
				j := i + 1
				for j < len(word) && (word[j] == '_' || word[j] >= 'A' && word[j] <= 'Z' || word[j] >= 'a' && word[j] <= 'z' || word[j] >= '0' && word[j] <= '9') {
					j++
				}
				i = j - 1
				continue
			}
		}
		b.WriteByte(c)
	}
	return filepath.Base(b.String())
}

func (r reading) launches(w []string) bool {
	if len(w) == 0 {
		return false
	}
	args := w[1:]
	switch name := commandName(w[0]); name {
	case "codex":
		return r.codexLaunch(args)
	case "eval":
		if len(args) > 0 && args[0] == "--" {
			args = args[1:]
		}
		return r.rawLaunch(strings.Join(args, " "))
	case "find":
		return slices.ContainsFunc(findActions(args), func(a []string) bool { return r.launches(launchWords(a)) })
	case "agtermctl":
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "session" && args[i+1] == "new" {
				return slices.ContainsFunc(agtermCommands(args[i+2:]), r.startsClaude)
			}
		}
	case "tmux":
		return slices.ContainsFunc(tmuxCommands(args), r.startsClaude)
	case "claude":
		return claudeLaunch(args)
	case "fish":
		return slices.ContainsFunc(fishCommands(args), r.rawLaunch)
	case "sh", "bash", "zsh", "dash", "ksh":
		c, ok := shellCommand(args)
		return ok && r.rawLaunch(c)
	}
	return false
}

// dropRedirects drops the redirections parse emits, and their files, from
// a command's words, so neither stands for the command or its arguments.
// A here-document's "<<" has no file word: parse reads its delimiter.
func dropRedirects(w []string) []string {
	out := w[:0:0]
	for i := 0; i < len(w); i++ {
		a := w[i]
		switch {
		case !operatorWord.MatchString(a):
			out = append(out, a)
		case a == "<<":
		case strings.Contains(a, "&") && !strings.HasSuffix(a, "&"):
			// A duplication (2>&1, >&2) names no file.
		default:
			i++ // the file
		}
	}
	return out
}

// shellCommand is the command string a POSIX shell's arguments (sh, bash,
// zsh, dash, ksh) run: the first operand after a -c flag (alone or in a
// cluster such as -lc), wherever the other options sit. A first operand with
// no -c before it is a script, which runs nothing the guards can read.
func shellCommand(args []string) (cmd string, ok bool) {
	cmdNext := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			if cmdNext && i+1 < len(args) {
				return args[i+1], true
			}
			return "", false
		case a == "--rcfile" || a == "--init-file":
			i++
		case strings.HasPrefix(a, "--"):
		case len(a) > 1 && (a[0] == '-' || a[0] == '+'):
			flags := a[1:]
			if a[0] == '-' && strings.IndexByte(flags, 'c') >= 0 {
				cmdNext = true
			}
			// -o and -O take an option name.
			if last := flags[len(flags)-1]; (last == 'o' || last == 'O') && i+1 < len(args) {
				i++
			}
		default:
			return a, cmdNext
		}
	}
	return "", false
}

// fishCommands are the command strings fish's arguments run: every -c/--command and -C/--init-command value, given as
// the next word or merged with the flag (-c"cmd", --command=cmd); its
// operands are a script and its arguments, never code.
func fishCommands(args []string) (cmds []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return cmds
		}
		if name, value, ok := strings.Cut(a, "="); ok && (name == "--command" || name == "--init-command") {
			cmds = append(cmds, value)
			continue
		}
		switch {
		case a == "--command" || a == "--init-command":
			if i+1 < len(args) {
				cmds = append(cmds, args[i+1])
			}
			i++
		case a == "--debug" || a == "--debug-output" || a == "--features" || a == "--profile" || a == "--profile-startup":
			i++
		case strings.HasPrefix(a, "--"):
		case len(a) > 1 && a[0] == '-':
			for k := 1; k < len(a); k++ {
				c := a[k]
				if strings.IndexByte("cCdofp", c) < 0 {
					continue
				}
				value := a[k+1:]
				if value == "" && i+1 < len(args) {
					i++
					value = args[i]
				}
				if c == 'c' || c == 'C' {
					cmds = append(cmds, value)
				}
				break
			}
		default:
			return cmds
		}
	}
	return cmds
}

// findActions are the commands of find's -exec, -execdir, -ok and -okdir
// actions. An action's command runs to a ";" word, or to a "+" right after
// "{}"; scanning goes on after it.
func findActions(args []string) (actions [][]string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if n, ok := findOperands[a]; ok || strings.HasPrefix(a, "-newer") {
			// A primary's operand is no action, whatever it spells.
			i += max(n, 1)
			continue
		}
		if a != "-exec" && a != "-execdir" && a != "-ok" && a != "-okdir" {
			continue
		}
		j := i + 1
		for j < len(args) && args[j] != ";" && (args[j] != "+" || args[j-1] != "{}") {
			j++
		}
		actions = append(actions, args[i+1:j])
		i = j
	}
	return actions
}

// findOperands are find's primaries and options that take operands, with
// their count (the -newer family takes one too).
var findOperands = map[string]int{
	"-name": 1, "-iname": 1, "-path": 1, "-ipath": 1, "-wholename": 1, "-iwholename": 1,
	"-regex": 1, "-iregex": 1, "-lname": 1, "-ilname": 1, "-type": 1, "-xtype": 1,
	"-user": 1, "-group": 1, "-uid": 1, "-gid": 1, "-perm": 1, "-size": 1, "-inum": 1,
	"-links": 1, "-samefile": 1, "-atime": 1, "-ctime": 1, "-mtime": 1, "-amin": 1,
	"-cmin": 1, "-mmin": 1, "-used": 1, "-maxdepth": 1, "-mindepth": 1, "-fstype": 1,
	"-printf": 1, "-fprint": 1, "-fprint0": 1, "-fls": 1, "-context": 1, "-regextype": 1,
	"-files0-from": 1, "-D": 1, "-f": 1, "-fprintf": 2,
}

// codexValueFlags take a value as the next argument in codex and its
// subcommands.
var codexValueFlags = map[string]bool{
	"-c": true, "--config": true, "--enable": true, "--disable": true, "--remote": true,
	"--remote-auth-token-env": true, "-i": true, "--image": true, "-m": true, "--model": true,
	"--local-provider": true, "-p": true, "--profile": true, "-s": true, "--sandbox": true,
	"-C": true, "--cd": true, "--add-dir": true, "-a": true, "--ask-for-approval": true,
	"--sandbox-state-json": true, "--sandbox-state-readable-root": true,
	"-P": true, "--permission-profile": true, "--allow-unix-socket": true,
}

// codexTaskCommands hand codex a task: exec (and its alias e) and review run
// one, queue sends one to a session.
var codexTaskCommands = map[string]bool{"exec": true, "e": true, "review": true, "queue": true}

// codexCommands are codex's other subcommands. Any other first operand is a
// prompt: codex [PROMPT] starts an interactive session with that task.
var codexCommands = map[string]bool{
	"agents": true, "login": true, "logout": true, "mcp": true, "plugin": true,
	"app-server": true, "remote-control": true, "app": true, "completion": true,
	"update": true, "doctor": true, "sandbox": true, "debug": true, "apply": true, "a": true,
	"resume": true, "archive": true, "delete": true, "migrate-rollouts": true,
	"migrate-nfs-db": true, "unarchive": true, "fork": true, "cloud": true,
	"exec-server": true, "execpolicy": true, "features": true, "git-credential": true,
	"responses-api-proxy": true, "tcp-tunnel": true, "help": true,
}

// codexOperands returns the indexes in args of codex's operands, and the
// index of a "--" that ends its options (len(args) when there is none):
// options and their values are dropped. An option that takes several values
// (--image) is read with one, so a further word counts as a prompt: images
// go with a prompt.
func codexOperands(args []string) (ops []int, end int) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			for j := i + 1; j < len(args); j++ {
				ops = append(ops, j)
			}
			return ops, i
		case codexValueFlags[a]:
			i++
		case len(a) > 1 && a[0] == '-':
		default:
			ops = append(ops, i)
		}
	}
	return ops, len(args)
}

// codexLaunch tells whether codex's arguments run a task: a task subcommand,
// a first operand that is a prompt rather than a subcommand (any operand
// after "--" is one), resume or fork with a prompt after the session (or
// after --last), cloud exec, or a sandbox whose command is a raw launch.
func (r reading) codexLaunch(args []string) bool {
	ops, end := codexOperands(args)
	if len(ops) == 0 {
		return false
	}
	if ops[0] > end {
		return true
	}
	switch first := args[ops[0]]; {
	case codexTaskCommands[first]:
		return true
	case first == "resume" || first == "fork":
		n := len(ops) - 1
		return n >= 2 || n == 1 && slices.Contains(args[:end], "--last")
	case first == "cloud":
		return len(ops) > 1 && args[ops[1]] == "exec"
	case first == "sandbox":
		return r.sandboxLaunch(args[ops[0]+1:])
	default:
		return !codexCommands[first]
	}
}

// sandboxLaunch tells whether codex sandbox's arguments run a raw launch:
// the command after its options, or, in the older form, after a platform
// name.
func (r reading) sandboxLaunch(args []string) bool {
	rest := sandboxCommand(args)
	if len(rest) == 0 || rest[0] == "help" || rest[0] == "uninstall" {
		return false
	}
	if r.launches(launchWords(rest)) {
		return true
	}
	switch rest[0] {
	case "macos", "linux", "windows", "seatbelt", "landlock":
		return r.launches(launchWords(sandboxCommand(rest[1:])))
	}
	return false
}

// sandboxCommand drops codex sandbox's options from the front of args.
func sandboxCommand(args []string) []string {
	for len(args) > 0 && len(args[0]) > 1 && args[0][0] == '-' {
		if args[0] == "--" {
			return args[1:]
		}
		if codexValueFlags[args[0]] && len(args) > 1 {
			args = args[1:]
		}
		args = args[1:]
	}
	return args
}

// agtermCommands are the commands agtermctl session new's arguments may run.
// Every --command (or --command=) anywhere counts, so neither a value flag
// this list does not know nor a second --command can hide one; only without
// any is an operand after the options read as a command (agtermctl takes none
// today). A session without a command runs the login shell.
func agtermCommands(args []string) (cmds []string) {
	found := false
	for i, a := range args {
		if v, ok := strings.CutPrefix(a, "--command="); ok {
			found = true
			cmds = append(cmds, v)
		}
		if a == "--command" {
			found = true
			if i+1 < len(args) {
				cmds = append(cmds, args[i+1])
			}
		}
	}
	if found {
		return cmds
	}
	for i := 0; i < len(args); i++ {
		name, _, hasValue := strings.Cut(args[i], "=")
		switch {
		case strings.HasPrefix(args[i], "-"):
			if agtermValueFlags[name] && !hasValue {
				i++
			}
		default:
			return []string{strings.Join(args[i:], " ")}
		}
	}
	return nil
}

// agtermValueFlags take a value in agtermctl session new.
var agtermValueFlags = map[string]bool{
	"--cwd": true, "--workspace": true, "--workspace-name": true, "--name": true,
	"--after": true, "--before": true, "--parent": true, "--remote": true, "--origin-window": true,
	"--tint": true, "--tint-on": true, "--socket": true, "--window": true, "--op-id": true,
	"--epoch": true, "--expect-panes": true,
}

// tmuxCommands are the commands tmux new-window, new-session or send-keys
// may run. Option values (the window name, the target) are skipped.
func tmuxCommands(args []string) []string {
	for i, a := range args {
		switch a {
		case "new-window", "neww", "new-session", "new":
			if rest := tmuxOperands(args[i+1:]); len(rest) > 0 {
				return []string{strings.Join(rest, " ")}
			}
			return nil
		case "send-keys", "send":
			return tmuxOperands(args[i+1:])
		}
	}
	return nil
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
func (r reading) startsClaude(cmd string) bool {
	segs, _ := r.parse(cmd)
	for _, seg := range segs {
		w := launchWords(seg)
		if len(w) > 0 && (commandName(w[0]) == "claude" || commandName(w[0]) == "codex") {
			return true
		}
	}
	return r.rawLaunch(cmd)
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

var assignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// reservedWords are the shell words that may stand before a command word
// without being the command.
var reservedWords = map[string]bool{
	"{": true, "}": true, "!": true, "if": true, "then": true, "else": true, "elif": true,
	"fi": true, "while": true, "until": true, "do": true, "done": true, "esac": true,
}

// wrapper describes a command that runs the command after its own options.
type wrapper struct {
	values     string          // short options that take a value
	long       map[string]bool // long options that take a value
	operands   int             // operands before the command (timeout's duration)
	lookup     string          // short options with which nothing runs (command -v)
	longLookup map[string]bool // long options with which nothing runs
	builtin    bool            // a shell builtin: matched by its name, not a path
	appends    bool            // adds its input to the command's arguments (xargs)
	replace    string          // short options that place the input instead (xargs -I)
	optional   string          // short options whose value, if any, is attached (xargs -i)
	longPlace  map[string]bool // long options that place the input instead (--replace)
}

var wrappers = map[string]wrapper{
	"env": {values: "uCSP", long: set("--unset", "--chdir", "--split-string")},
	"sudo": {
		values:     "ugCDprtUTRh",
		long:       set("--user", "--group", "--close-from", "--chdir", "--prompt", "--role", "--type", "--other-user", "--command-timeout", "--chroot", "--host"),
		lookup:     "lvVKe",
		longLookup: set("--list", "--validate", "--version", "--remove-timestamp", "--edit", "--help"),
	},
	"doas":    {values: "uC"},
	"nice":    {values: "n", long: set("--adjustment")},
	"setsid":  {},
	"ionice":  {values: "cnpPu", long: set("--class", "--classdata", "--pid", "--pgid", "--uid")},
	"time":    {values: "fo", long: set("--format", "--output")},
	"nohup":   {},
	"timeout": {values: "sk", long: set("--signal", "--kill-after"), operands: 1},
	"stdbuf":  {values: "ioe", long: set("--input", "--output", "--error")},
	"xargs": {
		values:  "adEILnPsJRS",
		long:    set("--arg-file", "--delimiter", "--max-args", "--max-procs", "--max-chars", "--process-slot-var"),
		appends: true, replace: "IiJ", optional: "iel",
		longPlace: set("--replace"),
	},
	"exec":    {values: "a", builtin: true},
	"command": {lookup: "vV", builtin: true},
	"builtin": {builtin: true},
}

func set(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// xargsInput stands for the words xargs adds to its command's arguments.
const xargsInput = "{xargs input}"

// commandWords drops what runs before the real command: variable
// assignments, shell reserved words (if, then, do, {, ! and the like, and
// the names of coproc and function), and the wrappers (env, sudo, doas,
// nice, setsid, ionice, time, nohup, timeout, stdbuf, xargs, exec, command,
// builtin) with their options. A wrapper outside the shell is matched by
// its base name (/usr/bin/env too). env -S's string is split into words and
// read as env's arguments, as env does. It returns nil when no command runs
// (command -v, sudo -l).
func commandWords(w []string) []string {
	w, _ = unwrapAll(w)
	return w
}

// launchWords is commandWords for the launch gate: when xargs adds its
// input to the command's arguments, a word stands for that input at the
// end, since it may be a prompt.
func launchWords(w []string) []string {
	w, appended := unwrapAll(dropRedirects(w))
	if appended && len(w) > 0 {
		w = append(slices.Clip(w), xargsInput)
	}
	return w
}

func unwrapAll(w []string) (rest []string, appended bool) {
	for len(w) > 0 {
		switch {
		case assignment.MatchString(w[0]) || reservedWords[w[0]]:
			w = w[1:]
		case w[0] == "coproc":
			w = w[1:]
			if len(w) > 1 && w[1] == "{" {
				w = w[2:]
			}
		case w[0] == "function":
			w = w[min(2, len(w)):]
		default:
			spec, ok := wrappers[filepath.Base(w[0])]
			if !ok || spec.builtin && w[0] != filepath.Base(w[0]) {
				return w, appended
			}
			var run, adds bool
			if w, run, adds = unwrap(w, spec); !run {
				return nil, false
			}
			appended = appended || adds
		}
	}
	return w, appended
}

// unwrap returns the words a wrapper runs: what follows its options, the
// option values and spec.operands operands. run is false when an option
// makes it run nothing; appends tells whether it adds its input to them.
// The words of env's -S value are put in its place and read as env's
// arguments.
func unwrap(w []string, spec wrapper) (rest []string, run, appends bool) {
	env := filepath.Base(w[0]) == "env"
	appends = spec.appends
	i := 1
	for i < len(w) {
		a := w[i]
		if a == "--" {
			i++
			break
		}
		if env && a == "-" {
			i++
			continue
		}
		if len(a) < 2 || a[0] != '-' {
			break
		}
		next := i + 1
		var split []string
		splits := false
		if strings.HasPrefix(a, "--") {
			name, value, hasValue := strings.Cut(a, "=")
			if spec.longLookup[name] {
				return nil, false, false
			}
			if spec.longPlace[name] {
				appends = false
			}
			if spec.long[name] && !hasValue {
				if next < len(w) {
					value = w[next]
				}
				next++
			}
			if env && name == "--split-string" {
				split, splits = envSplit(value), true
			}
		} else {
			for k := 1; k < len(a); k++ {
				c := a[k]
				if strings.IndexByte(spec.lookup, c) >= 0 {
					return nil, false, false
				}
				if strings.IndexByte(spec.replace, c) >= 0 {
					appends = false
				}
				if strings.IndexByte(spec.optional, c) >= 0 {
					break // the rest of the word is its value
				}
				if strings.IndexByte(spec.values, c) < 0 {
					continue
				}
				value := a[k+1:]
				if value == "" {
					if next < len(w) {
						value = w[next]
					}
					next++
				}
				if env && c == 'S' {
					split, splits = envSplit(value), true
				}
				break
			}
		}
		if splits {
			w = append(append([]string{w[0]}, split...), w[min(next, len(w)):]...)
			i = 1
			continue
		}
		i = next
	}
	i = min(i+spec.operands, len(w))
	return w[i:], true, appends
}

// envSplit splits an env -S string into words as env does: blanks and an
// unquoted \_ separate words; single and double quotes group; a backslash
// escapes the next character (\t, \n and the like are blanks' kin, \_ in
// double quotes is a space); \c ends the string; and # at the start of a
// word starts a comment.
func envSplit(s string) []string {
	var (
		out    []string
		word   strings.Builder
		inWord bool
	)
	end := func() {
		if inWord {
			out = append(out, word.String())
			word.Reset()
			inWord = false
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			end()
		case c == '#' && !inWord:
			return out
		case c == '\'':
			inWord = true
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				word.WriteString(s[i+1:])
				i = len(s)
				break
			}
			word.WriteString(s[i+1 : i+1+j])
			i += j + 1
		case c == '"':
			inWord = true
			for i++; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) {
					i++
					if s[i] == '_' {
						word.WriteByte(' ')
						continue
					}
					if s[i] == 'c' {
						end()
						return out
					}
				}
				word.WriteByte(s[i])
			}
		case c == '\\' && i+1 < len(s):
			i++
			switch s[i] {
			case '_':
				end()
			case 'c':
				end()
				return out
			case 't':
				inWord = true
				word.WriteByte('\t')
			case 'n':
				inWord = true
				word.WriteByte('\n')
			default:
				inWord = true
				word.WriteByte(s[i])
			}
		default:
			inWord = true
			word.WriteByte(c)
		}
	}
	end()
	return out
}

// segments splits a shell command into simple commands, each a list of
// words. See parse.
func segments(cmd string) [][]string {
	segs, _ := readParens.parse(cmd)
	return segs
}

// reading is how a here-document's body in a substitution is read, where
// shells differ.
type reading int

const (
	// readParens counts the parentheses and quotes in the body as everywhere
	// else, as bash 3.2 does.
	readParens reading = iota
	// readDocs skips the body to the line that is its delimiter, as zsh and
	// dash do.
	readDocs
	// readDocsInParens also ends the body, inside parentheses, at a line that
	// starts with its delimiter and has a ")" after it, as bash 5.2 does.
	readDocsInParens
)

// parse splits a shell command into simple commands, each a list of words.
// It splits on ; & && || | newlines and parentheses; removes quotes, keeping
// a quoted string as one word and decoding $'...' strings; emits
// redirections as words of their own, with their file descriptor and any
// duplication (">", ">>", "2>", "2>&1", ">&2", "<", "<<", "<<<"); and skips
// here-document bodies. A command substitution ($(...) or backquotes) stays
// in its word as written and is also returned in subs, since it runs as a
// command too; so are the substitutions in the body of a here-document whose
// delimiter is not quoted, which the shell expands. It is a reader for the
// guards, not a shell: it errs towards splitting.
func (r reading) parse(cmd string) (segs [][]string, subs []string) {
	segs, subs, _ = r.parseJoined(cmd)
	return segs, subs
}

// join is how a simple command stands to the ones before it.
type join struct {
	// from is the first command whose output is this one's input, up to this
	// one: the one before, or the start of the group before (( ), { }, a loop);
	// -1 when its input is no pipe.
	from int
	// array: these words are the values of the array assignment that ends the
	// last command that is none (NAME=(...)), not a command.
	array bool
}

// parseJoined is parse with how each simple command stands to the ones before
// it.
func (r reading) parseJoined(cmd string) (segs [][]string, subs []string, joins []join) {
	var (
		seg     []string
		word    strings.Builder
		inWord  bool
		quoted  bool // the word has quoting, so it is no operator
		pending []hereDoc
		shifts  = -1 // the end of the arithmetic or expansion read now, where << is a shift
		next    = join{from: -1}
		opens   []int // the first command in each open parenthesis
		arrays  []bool
		closed  = [2]int{-1, -1} // the commands of the last closed parenthesis
	)
	endWord := func() {
		if inWord {
			w := word.String()
			if quoted && operatorWord.MatchString(w) {
				// A quoted ">" is an argument: keep it apart from the
				// redirections this reader emits.
				w = "'" + w + "'"
			}
			seg = append(seg, w)
			word.Reset()
			inWord, quoted = false, false
		}
	}
	endSeg := func() {
		endWord()
		if len(seg) > 0 {
			segs, joins = append(segs, seg), append(joins, next)
			seg, next = nil, join{from: -1, array: next.array}
		}
	}
	// pipedFrom is the first command whose output a "|" here takes.
	pipedFrom := func() int {
		last := len(segs) - 1
		if closed[0] >= 0 && (closed[1] == last || closed[1] == last-1 && len(dropRedirects(segs[last])) == 0) {
			return closed[0]
		}
		return groupStart(segs)
	}
	n := len(cmd)
	for i := 0; i < n; i++ {
		ch := cmd[i]
		shifts = max(shifts, r.expansionEnd(cmd, i))
		switch {
		case ch == ' ' || ch == '\t':
			endWord()
		case ch == '\n':
			endSeg()
			// Skip the bodies of here-documents opened on this line.
			for _, h := range pending {
				if i+1 >= n {
					break
				}
				inParens := r == readDocsInParens && len(opens) > 0
				body, end, _ := h.body(cmd, i+1, inParens)
				i = end
				if !h.quoted {
					subs = append(subs, r.bodySubs(body)...)
				}
			}
			pending = nil
		case ch == '#' && !inWord:
			// A comment runs to the end of the line.
			if j := strings.IndexByte(cmd[i:], '\n'); j >= 0 {
				i += j - 1
			} else {
				i = n
			}
		case ch == '\'':
			inWord, quoted = true, true
			j := strings.IndexByte(cmd[i+1:], '\'')
			if j < 0 {
				word.WriteString(cmd[i+1:])
				i = n
			} else {
				word.WriteString(cmd[i+1 : i+1+j])
				i += j + 1
			}
		case ch == '$' && i+1 < n && cmd[i+1] == '\'':
			inWord, quoted = true, true
			s, end := ansiC(cmd, i+2)
			word.WriteString(s)
			i = end
		case ch == '$' && i+1 < n && cmd[i+1] == '"':
			// $"..." is a double-quoted string.
		case ch == '"':
			inWord, quoted = true, true
			for i++; i < n && cmd[i] != '"'; i++ {
				if cmd[i] == '\\' && i+1 < n {
					// In double quotes a backslash escapes only $ ` " \ and
					// a newline (which it removes); before anything else it
					// stays.
					switch next := cmd[i+1]; next {
					case '\n':
						i++
						continue
					case '$', '`', '"', '\\':
						i++
						word.WriteByte(next)
						continue
					}
					word.WriteByte('\\')
					continue
				}
				if end, inner, ok := r.substitution(cmd, i); ok {
					subs = append(subs, inner)
					word.WriteString(cmd[i : end+1])
					i = end
					continue
				}
				word.WriteByte(cmd[i])
			}
		case ch == '$' && i+1 < n && cmd[i+1] == '(' || ch == '`':
			end, inner, ok := r.substitution(cmd, i)
			if !ok {
				// Unclosed: split here and read on.
				endSeg()
				if ch == '$' {
					i++
				}
				break
			}
			subs = append(subs, inner)
			inWord = true
			word.WriteString(cmd[i : end+1])
			i = end
		case ch == '\\':
			// A backslash-newline joins lines; any other backslash quotes
			// the next character.
			if i+1 < n {
				i++
				if cmd[i] != '\n' {
					inWord, quoted = true, true
					word.WriteByte(cmd[i])
				}
			}
		case ch == ';' || ch == '&' || ch == '|' || ch == '(' || ch == ')':
			array := ch == '(' && inWord && !quoted && arrayAssign.MatchString(word.String())
			endSeg()
			after := i > 0 && cmd[i-1] == '|' // || and |&
			switch {
			case ch == '(':
				shifts = max(shifts, r.arithmeticEnd(cmd, i))
				opens, arrays = append(opens, len(segs)), append(arrays, next.array)
				next.array = array
			case ch == ')' && len(opens) > 0:
				k := len(opens) - 1
				closed = [2]int{opens[k], len(segs) - 1}
				next = join{from: -1, array: arrays[k]}
				opens, arrays = opens[:k], arrays[:k]
			case ch == '|' && !after && len(segs) > 0:
				next.from = pipedFrom()
			case ch == '|' || ch == ';' || ch == '&' && !after:
				next.from = -1
			}
		case ch == '>':
			fd := ""
			if inWord && !quoted && isDigits(word.String()) {
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
				if i < shifts {
					i++ // a shift
					break
				}
				h, end, ok := readHereDoc(cmd, i)
				if i = end - 1; ok {
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
	return segs, subs, joins
}

// arrayAssign is a word that opens an array assignment before its "(".
var arrayAssign = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*\+?=$`)

// groupWords open (1) and close (-1) a group of commands.
var groupWords = map[string]int{
	"{": 1, "if": 1, "while": 1, "until": 1, "for": 1, "case": 1, "select": 1,
	"}": -1, "fi": -1, "done": -1, "esac": -1,
}

// groupStart is the first command of the group the last command of segs
// closes, or that last command itself.
func groupStart(segs [][]string) int {
	last := len(segs) - 1
	if groupWords[segs[last][0]] != -1 {
		return last
	}
	depth := 0
	for i := last; i >= 0; i-- {
		for _, w := range segs[i] {
			depth += groupWords[w]
			if !reservedWords[w] {
				break
			}
		}
		if depth >= 0 {
			return i
		}
	}
	return 0
}

// operatorWord is a word shaped like a redirection operator parse emits.
var operatorWord = regexp.MustCompile(`^([0-9]*>>?(&[0-9-]*)?|<|<<|<<<)$`)

// oddBackslashes tells whether line ends with an odd number of
// backslashes, so its newline is escaped.
func oddBackslashes(line string) bool {
	k := 0
	for k < len(line) && line[len(line)-1-k] == '\\' {
		k++
	}
	return k%2 == 1
}

// substitution reads a command substitution starting at s[i], "$(" or a
// backquote: end is the index of its closing ")" or backquote and inner the
// command in it. ok is false when there is none or it is not closed.
func (r reading) substitution(s string, i int) (end int, inner string, ok bool) {
	switch {
	case strings.HasPrefix(s[i:], "$("):
		if end = r.closingParen(s, i+2); end > 0 {
			if r == readDocsInParens || r.arithmeticEnd(s, i+1) == end {
				// With its parentheses: they tell arithmetic, and where a
				// here-document's body may end.
				return end, s[i+1 : end+1], true
			}
			return end, s[i+2 : end], true
		}
	case s[i] == '`':
		for j := i + 1; j < len(s); j++ {
			switch s[j] {
			case '\\':
				j++
			case '`':
				return j, s[i+1 : j], true
			}
		}
	}
	return 0, "", false
}

// hereDoc is a here-document: its delimiter, whether tabs at the start of its
// lines are dropped (<<-), and whether the delimiter has quoting, which leaves
// the body unexpanded.
type hereDoc struct {
	delim  string
	tabs   bool
	quoted bool
}

// readHereDoc reads the operator and the delimiter of the here-document that
// opens at s[i:] ("<<"); end is the index after them. ok is false when there is
// no delimiter.
func readHereDoc(s string, i int) (h hereDoc, end int, ok bool) {
	n := len(s)
	i += 2
	if i < n && s[i] == '-' {
		h.tabs = true
		i++
	}
	for i < n && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	// The delimiter is one shell word; any quoting in it (which may hold
	// blanks) leaves the body unexpanded.
	var d strings.Builder
delim:
	for ; i < n && !strings.ContainsRune(" \t\n;&|<>()", rune(s[i])); i++ {
		switch {
		case s[i] == '$' && i+1 < n && s[i+1] == '\'':
			h.quoted = true
			text, e := ansiC(s, i+2)
			d.WriteString(text)
			i = e
		case s[i] == '$' && i+1 < n && s[i+1] == '"':
			// $"..." reads as "...".
		case s[i] == '\'':
			h.quoted = true
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				d.WriteString(s[i+1:])
				i = n
				break delim
			}
			d.WriteString(s[i+1 : i+1+j])
			i += j + 1
		case s[i] == '"':
			h.quoted = true
			for i++; i < n && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < n && strings.IndexByte("\"\\$`\n", s[i+1]) >= 0 {
					i++
					if s[i] == '\n' {
						continue
					}
				}
				d.WriteByte(s[i])
			}
			if i >= n {
				break delim
			}
		case s[i] == '\\':
			if i+1 < n && s[i+1] == '\n' {
				i++ // a continued line: no quoting
				continue
			}
			h.quoted = true
			if i+1 < n {
				i++
				d.WriteByte(s[i])
			}
		default:
			d.WriteByte(s[i])
		}
	}
	h.delim = d.String()
	return h, i, d.Len() > 0 || h.quoted
}

// body reads the here-document's body, which starts at s[from:]. end is the
// index of the last byte read: the newline after the line of the delimiter,
// the last byte of s when the input ends first (closed is false when it does so
// without the delimiter), or, inParens, the byte before what follows the
// delimiter on a line with a ")" after it (see readDocsInParens).
func (h hereDoc) body(s string, from int, inParens bool) (body string, end int, closed bool) {
	n := len(s)
	lineAt := func(p int) (string, int) {
		if j := strings.IndexByte(s[p:], '\n'); j >= 0 {
			return s[p : p+j], p + j
		}
		return s[p:], n
	}
	var b strings.Builder
	end = from - 1
	for end+1 < n {
		line, e := lineAt(end + 1)
		pieces := []int{len(line)} // of a joined line, and where they start
		starts := []int{end + 1}
		// In an unquoted body a backslash-newline joins lines, for the
		// delimiter and for what the body expands.
		for !h.quoted && e < n && oddBackslashes(line) {
			var more string
			starts = append(starts, e+1)
			more, e = lineAt(e + 1)
			line = line[:len(line)-1] + more
			pieces[len(pieces)-1]--
			pieces = append(pieces, len(more))
		}
		end = min(e, n-1)
		if h.tabs {
			line = strings.TrimLeft(line, "\t")
		}
		if line == h.delim {
			return b.String(), end, true
		}
		if rest, ok := strings.CutPrefix(line, h.delim); ok && inParens && strings.Contains(rest, ")") {
			// Back over the pieces rest is in.
			k, left := len(pieces)-1, len(rest)
			for ; left > pieces[k]; k-- {
				left -= pieces[k]
			}
			return b.String(), starts[k] + pieces[k] - left - 1, true
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String(), end, false
}

// arithmeticEnd is the index of the last ")" of the arithmetic ("((...))")
// that opens at s[i], or -1 when none does: parentheses that do not close
// together are a group in a group, and after "<" or ">" the first one opens a
// process substitution. readParens reads no arithmetic.
func (r reading) arithmeticEnd(s string, i int) int {
	if r == readParens || !strings.HasPrefix(s[i:], "((") || i > 0 && (s[i-1] == '<' || s[i-1] == '>') {
		return -1
	}
	if e := readParens.closingParen(s, i+2); e > 0 && e+1 < len(s) && s[e+1] == ')' {
		return e + 1
	}
	return -1
}

// expansionEnd is the index of the bracket that closes the expansion ("${" or
// "$[") that opens at s[i], or -1 when none does: << is a shift in it.
// readParens reads none.
func (r reading) expansionEnd(s string, i int) int {
	if r == readParens || s[i] != '$' || i+1 >= len(s) || s[i+1] != '{' && s[i+1] != '[' {
		return -1
	}
	open, closing := s[i+1], byte('}')
	if open == '[' {
		closing = ']'
	}
	depth := 0
	for i++; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return -1
			}
			i += j + 1
		case '(':
			if s[i-1] == '$' {
				e := readParens.closingParen(s, i+1)
				if e < 0 {
					return -1
				}
				i = e
			}
		case open:
			depth++
		case closing:
			if depth--; depth == 0 {
				return i
			}
		}
	}
	return -1
}

// closingParen is the index of the ")" that closes a "(" just before from,
// or -1. Parentheses inside quotes, after a backslash or in a nested
// substitution's quotes do not count.
func (r reading) closingParen(s string, from int) int {
	if e := r.arithmeticEnd(s, from-1); e > 0 {
		return e
	}
	depth := 1
	var docs []hereDoc // the here-documents opened on this line
	shifts := -1       // the end of the expansion read now, where << is a shift
	for i := from; i < len(s); i++ {
		shifts = max(shifts, r.expansionEnd(s, i))
		switch s[i] {
		case '<':
			switch {
			case strings.HasPrefix(s[i:], "<<<"):
				i += 2
			case i < shifts:
			case r != readParens && strings.HasPrefix(s[i:], "<<"):
				h, end, ok := readHereDoc(s, i)
				if i = end - 1; ok {
					docs = append(docs, h)
				}
			}
		case '\n':
			for _, h := range docs {
				// One that is not closed is none: what follows is read on.
				if _, end, closed := h.body(s, i+1, r == readDocsInParens); closed {
					i = end
				}
			}
			docs = nil
		case '#':
			// A comment at a word's start runs to the end of its line.
			if i == from || strings.IndexByte(" \t\n;&|()", s[i-1]) >= 0 {
				j := strings.IndexByte(s[i:], '\n')
				if j < 0 {
					return -1
				}
				i += j - 1 // the newline is read next
			}
		case '\\':
			i++
		case '$':
			if i+1 < len(s) && s[i+1] == '\'' {
				// $'...' ends at its first unescaped quote.
				_, i = ansiC(s, i+2)
				if i >= len(s) || s[i] != '\'' {
					return -1
				}
			}
		case '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return -1
			}
			i += j + 1
		case '"':
			for i++; i < len(s) && s[i] != '"'; i++ {
				switch {
				case s[i] == '\\':
					i++
				case strings.HasPrefix(s[i:], "$("):
					e := r.closingParen(s, i+2)
					if e < 0 {
						return -1
					}
					i = e
				}
			}
			if i >= len(s) {
				return -1
			}
		case '(':
			if e := r.arithmeticEnd(s, i); e > 0 {
				i = e
				continue
			}
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

// bodySubs returns the command substitutions in an unquoted here-document's
// body, where quotes are plain characters.
func (r reading) bodySubs(body string) []string {
	var out []string
	for i := 0; i < len(body); i++ {
		switch {
		case body[i] == '\\':
			i++
		case body[i] == '$' && i+1 < len(body) && body[i+1] == '(' || body[i] == '`':
			if end, inner, ok := r.substitution(body, i); ok {
				out = append(out, inner)
				i = end
			}
		}
	}
	return out
}

// ansiC decodes a $'...' string whose text starts at s[from], as bash does
// (up to a NUL, which ends it): it returns the text and the index of the
// closing quote (or the last index when there is none).
func ansiC(s string, from int) (string, int) {
	var b strings.Builder
	i := from
	for ; i < len(s) && s[i] != '\''; i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch c := s[i]; c {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case 'a':
			b.WriteByte('\a')
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'v':
			b.WriteByte('\v')
		case 'e', 'E':
			b.WriteByte(0x1b)
		case '\\', '\'', '"', '?':
			b.WriteByte(c)
		case 'c':
			// \cX is the control character X.
			if i+1 < len(s) && s[i+1] != '\'' {
				i++
				b.WriteByte(s[i] & 0x1f)
			} else {
				b.WriteString("\\c")
			}
		case 'x', 'u', 'U', '0', '1', '2', '3', '4', '5', '6', '7':
			base, width, start := 16, map[byte]int{'x': 2, 'u': 4, 'U': 8}[c], i+1
			if c >= '0' && c <= '7' {
				base, width, start = 8, 3, i
			}
			j := start
			for j < len(s) && j-start < width && isDigitIn(s[j], base) {
				j++
			}
			if j == start {
				b.WriteByte('\\')
				b.WriteByte(c)
				continue
			}
			v, _ := strconv.ParseUint(s[start:j], base, 32)
			if c == 'x' || base == 8 {
				b.WriteByte(byte(v))
			} else {
				b.WriteRune(rune(v))
			}
			i = j - 1
		default:
			b.WriteByte('\\')
			b.WriteByte(c)
		}
	}
	out := b.String()
	if k := strings.IndexByte(out, 0); k >= 0 {
		// The shell ends the string at a NUL.
		out = out[:k]
	}
	return out, min(i, len(s)-1)
}

func isDigitIn(c byte, base int) bool {
	if base == 8 {
		return c >= '0' && c <= '7'
	}
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
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
