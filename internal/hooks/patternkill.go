package hooks

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// patternKillReason is what a refused worker reads.
const patternKillReason = "Do not end processes found by a pattern or a name (pkill, killall, kill fed by pgrep or ps): it can end other Claude sessions and their work. Stop background work with the tool that started it (TaskStop), or with kill and a pid you recorded when you started it ($!, a pid file)."

// patternKillGate refuses commands that end processes found by a pattern. It
// is never softened to a note: the harm is to other sessions.
func patternKillGate(c *call) (result, error) {
	cmd, _ := c.in.ToolInput["command"].(string)
	if c.in.ToolName != "Bash" || !patternKill(cmd) {
		return result{}, nil
	}
	return result{deny: patternKillReason}, nil
}

// pidFinders print the pids of processes found by a pattern or a name.
var pidFinders = map[string]bool{"pgrep": true, "ps": true, "pidof": true}

// patternKill tells whether a Bash command ends processes found by a pattern:
// pkill, killall, or a kill fed by pgrep, ps or pidof - in a substitution, a
// pipe into xargs or a read, a process substitution or a here-string, or a
// variable the same command sets from one. A kill with a literal pid, $! or
// another variable is not one. It looks into what the launch gate looks into
// (wrappers, sh -c, eval, find -exec, tmux and agterm commands,
// substitutions).
//
// It is a guard against accidents, not a sandbox: a command built by another
// program (awk's system(), a script file, python -c), a name from a variable,
// or a pid list kept in a file between two commands is out of its reach.
//
// Shells end a substitution that holds a here-document at different places;
// what either reading of it finds counts. readParens is none of them: an
// apostrophe in a commit message would end the substitution.
func patternKill(cmd string) bool {
	return slices.ContainsFunc([]reading{readDocs, readDocsInParens}, func(r reading) bool {
		return pids{reading: r, found: map[string]bool{}}.killedBy(cmd)
	})
}

// pids follows the pids found by a pattern through a command, as one reading
// reads it. found are the variables that hold some.
type pids struct {
	reading
	found map[string]bool
}

// killedBy is patternKill with the variables found so far. It adds the ones cmd
// sets.
func (p pids) killedBy(cmd string) bool {
	segs, subs, joins := p.parseJoined(cmd)
	feeds := finderFeeds(segs)
	tainted := make([]bool, len(segs)) // the segment's output holds found pids
	command := 0                       // the last segment that is a command
	for i, seg := range segs {
		if joins[i].array {
			p.setArray(segs[command], seg)
			continue
		}
		command = i
		from := joins[i].from
		fed := from >= 0 && from < i && slices.Contains(tainted[from:i], true)
		fed = fed || slices.ContainsFunc(feeds, func(f feed) bool {
			return f.from <= i && i <= f.to && (f.word != "" && p.holdsFound(f.word) || f.seg != nil && p.findsPids(f.seg))
		})
		tainted[i] = fed || p.findsPids(seg)
		if p.killSegment(seg, fed) {
			return true
		}
		p.setFound(seg, fed)
	}
	return slices.ContainsFunc(subs, p.killedBy)
}

// killSegment tells whether one simple command ends processes found by a
// pattern. fed says its standard input holds pids found by one.
func (p pids) killSegment(seg []string, fed bool) bool {
	w := launchWords(seg)
	if len(w) == 0 {
		return false
	}
	args := w[1:]
	holds := p.holdsFound
	fed = fed && viaXargs(seg)
	switch commandName(w[0]) {
	case "pkill", "killall":
		return true
	case "kill":
		return fed || slices.ContainsFunc(args, holds)
	}
	// A command string is judged by itself; what it is given besides are the
	// words xargs adds and a shell's operands.
	cmds, actions, operands := nested(w)
	given := fed || slices.ContainsFunc(operands, holds)
	for _, c := range cmds {
		if given && p.runsKill(c) || p.killedBy(c) {
			return true
		}
	}
	return slices.ContainsFunc(actions, func(a []string) bool { return p.killSegment(a, false) })
}

// runsKill tells whether a command string runs kill at all.
func (r reading) runsKill(cmd string) bool {
	segs, _ := r.parse(cmd)
	return slices.ContainsFunc(segs, func(seg []string) bool {
		w := launchWords(seg)
		return len(w) > 0 && commandName(w[0]) == "kill"
	})
}

// findsPids tells whether a simple command's output holds pids found by a
// pattern: it runs a pid finder, or is given the output or the variable of
// one.
func (p pids) findsPids(seg []string) bool {
	w := launchWords(seg)
	if len(w) == 0 {
		return false
	}
	if pidFinders[commandName(w[0])] || slices.ContainsFunc(w[1:], p.holdsFound) {
		return true
	}
	cmds, _, _ := nested(w)
	return slices.ContainsFunc(cmds, p.commandFinds)
}

// commandFinds tells whether any simple command of cmd finds pids.
func (p pids) commandFinds(cmd string) bool {
	segs, _ := p.parse(cmd)
	return slices.ContainsFunc(segs, p.findsPids)
}

// holdsFound tells whether a word holds pids found by a pattern: a
// substitution that runs a pid finder, or a variable in found.
func (p pids) holdsFound(word string) bool {
	for i := 0; i < len(word); i++ {
		if word[i] != '$' && word[i] != '`' {
			continue
		}
		if end, inner, ok := p.substitution(word, i); ok {
			if p.commandFinds(inner) {
				return true
			}
			i = end
			continue
		}
		name := strings.TrimLeft(word[i+1:], "{#!")
		if p.found[name[:nameLen(name)]] {
			return true
		}
	}
	return false
}

// nameLen is the length of the variable name s starts with.
func nameLen(s string) int {
	n := 0
	for n < len(s) && (s[n] == '_' || s[n] >= 'A' && s[n] <= 'Z' || s[n] >= 'a' && s[n] <= 'z' || n > 0 && s[n] >= '0' && s[n] <= '9') {
		n++
	}
	return n
}

// feed is what the commands from to to read from a process substitution
// (its command, seg) or a here-string (word).
type feed struct {
	from, to int
	seg      []string
	word     string
}

// finderFeeds are the feeds in segs: each belongs to the command it stands
// after, or to the group (a loop, { }) that command closes.
func finderFeeds(segs [][]string) (feeds []feed) {
	for i, seg := range segs {
		f := feed{from: groupStart(segs[:i+1]), to: i}
		for j, w := range seg {
			if w == "<<<" && j+1 < len(seg) {
				f.word = seg[j+1]
				feeds = append(feeds, f)
			}
		}
		n := len(seg)
		if n > 1 && seg[n-1] == "<" && seg[n-2] == "<" && i+1 < len(segs) {
			f.word, f.seg = "", segs[i+1]
			feeds = append(feeds, f)
		}
	}
	return feeds
}

// setArray notes in found the variable of the array assignment that ends seg,
// with the words values.
func (p pids) setArray(seg, values []string) {
	name, _ := assigned(seg[len(seg)-1])
	if name == "" {
		return
	}
	// seg itself emptied the variable (setFound), unless it adds to it; an
	// array's lines are a segment each.
	p.found[name] = p.found[name] || slices.ContainsFunc(values, p.holdsFound)
}

// assigned is the variable an assignment word sets ("" for no assignment),
// and whether it adds to it (+=).
func assigned(w string) (name string, adds bool) {
	m := assignedWord.FindStringSubmatch(w)
	if m == nil {
		return "", false
	}
	return m[1], m[2] == "+"
}

var assignedWord = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)(?:\[[^]]*\])?(\+?)=`)

// setFound notes in found the variables a simple command sets: an assignment
// (alone, or after export, local and the like), a for loop's variable, and
// what read, mapfile and readarray read when fed says their input holds found
// pids. A variable set to something else is found no more.
func (p pids) setFound(seg []string, fed bool) {
	assign := func(w string) bool {
		name, adds := assigned(w)
		if name == "" {
			return false
		}
		_, value, _ := strings.Cut(w, "=")
		p.found[name] = adds && p.found[name] || p.holdsFound(value)
		return true
	}
	w := dropRedirects(seg)
	for len(w) > 0 && (reservedWords[w[0]] || assign(w[0])) {
		w = w[1:]
	}
	w = launchWords(w)
	if len(w) == 0 {
		return
	}
	switch args := w[1:]; w[0] {
	case "export", "local", "declare", "typeset", "readonly":
		for _, a := range args {
			assign(a)
		}
	case "for", "select":
		if len(args) > 1 && args[1] == "in" {
			p.found[args[0]] = slices.ContainsFunc(args[2:], p.holdsFound)
		}
	case "read", "mapfile", "readarray":
		for _, a := range args {
			if n := nameLen(a); n > 0 && n == len(a) {
				p.found[a] = fed
			}
		}
	}
}

// viaXargs tells whether a simple command is run by xargs.
func viaXargs(seg []string) bool {
	w := dropRedirects(seg)
	rest, _ := unwrapAll(w)
	return len(rest) <= len(w) && slices.ContainsFunc(w[:len(w)-len(rest)], func(a string) bool { return filepath.Base(a) == "xargs" })
}

// nested is what a command runs besides itself: command strings (eval, the
// shells' -c, tmux, agterm) and simple commands (find's actions); operands
// are the arguments a shell gives its command string.
func nested(w []string) (cmds []string, actions [][]string, operands []string) {
	args := w[1:]
	switch commandName(w[0]) {
	case "eval":
		if len(args) > 0 && args[0] == "--" {
			args = args[1:]
		}
		return []string{strings.Join(args, " ")}, nil, nil
	case "find":
		return nil, findActions(args), nil
	case "agtermctl":
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "session" && args[i+1] == "new" {
				return agtermCommands(args[i+2:]), nil, nil
			}
		}
	case "tmux":
		return tmuxCommands(args), nil, nil
	case "fish":
		return fishCommands(args), nil, nil
	case "sh", "bash", "zsh", "dash", "ksh":
		if c, ok := shellCommand(args); ok {
			return []string{c}, nil, args[slices.Index(args, c)+1:]
		}
	}
	return nil, nil, nil
}
