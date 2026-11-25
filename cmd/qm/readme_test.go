package main

import (
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// codeMention matches "qm <word> [<arg>]" inside code: at the start of the
// text or after a space, backtick, "(", ";", "&", "|", a quote, "=" or "$"
// (so "bin/qm" and "qm-worker" do not match, while "bash -c 'qm x'" and
// "\"command\": \"qm x\"" do), with the word in lowercase letters and dashes.
// The optional arg is the next token, cut at a quote, backtick, shell
// operator, backslash or "#" (a comment).
var codeMention = regexp.MustCompile("(?:^|[\\s`(;&|'\"=$])qm[ \\t]+([a-z][a-z-]*)(?:[ \\t]+([^\\s#\\\\'\"`;&|()]+))?")

// subcommands holds, for each command that dispatches on its first argument,
// the arguments it accepts there. They mirror the switches in runRoute
// (route.go), runSpawn (spawn.go) and runUp (up.go); each also takes -h and
// --help there. A command not listed here takes any argument.
var subcommands = map[string]struct {
	kinds []string
	// open: the command also runs with no argument or with a flag ("-x",
	// "--x", "--") first.
	open bool
}{
	"route": {kinds: []string{"worker", "review"}},
	"spawn": {kinds: []string{"worker", "review"}},
	"up":    {kinds: []string{"done", "handoff", "note"}, open: true},
}

// readmeCommands returns every "qm <command>" that md shows as code, in a
// fenced block (``` or ~~~) or in an inline code span, as "command" or
// "command arg" with the arg that follows it. Prose is left out, where "qm"
// is the subject of a sentence ("qm writes ...").
func readmeCommands(md string) []string {
	var code, para []string
	flush := func() {
		code = append(code, inlineSpans(strings.Join(para, "\n"))...)
		para = para[:0]
	}
	fence := "" // the opening fence while inside a fenced block
	for line := range strings.SplitSeq(md, "\n") {
		t := strings.TrimSpace(line)
		if fence != "" {
			if strings.HasPrefix(t, fence) && strings.Trim(t, fence[:1]) == "" {
				fence = ""
				continue
			}
			code = append(code, line)
			continue
		}
		if f := openingFence(t); f != "" {
			flush()
			fence = f
			continue
		}
		if t == "" {
			flush()
			continue
		}
		para = append(para, line)
	}
	flush()
	var found []string
	for _, c := range code {
		for _, m := range codeMention.FindAllStringSubmatch(c, -1) {
			s := strings.TrimSpace(m[1] + " " + m[2])
			if !slices.Contains(found, s) {
				found = append(found, s)
			}
		}
	}
	return found
}

// openingFence returns the fence (its run of "`" or "~") that the trimmed
// line t opens, or "" when t opens none.
func openingFence(t string) string {
	for _, c := range []string{"`", "~"} {
		n := len(t) - len(strings.TrimLeft(t, c))
		// A backtick fence's info string cannot hold a backtick.
		if n >= 3 && (c == "~" || !strings.Contains(t[n:], "`")) {
			return t[:n]
		}
	}
	return ""
}

// inlineSpans returns the contents of the inline code spans in a paragraph,
// with line breaks read as spaces. A span opens with a run of backticks and
// closes at the next run of the same length; a run left unmatched is text.
func inlineSpans(p string) []string {
	var spans []string
	for i := 0; i < len(p); {
		if p[i] != '`' {
			i++
			continue
		}
		n := runLen(p, i)
		end := -1
		for j := i + n; j < len(p); {
			if p[j] != '`' {
				j++
				continue
			}
			m := runLen(p, j)
			if m == n {
				end = j
				break
			}
			j += m
		}
		if end < 0 {
			i += n
			continue
		}
		spans = append(spans, strings.ReplaceAll(p[i+n:end], "\n", " "))
		i = end + n
	}
	return spans
}

// runLen returns the length of the run of backticks starting at p[i].
func runLen(p string, i int) int {
	j := i
	for j < len(p) && p[j] == '`' {
		j++
	}
	return j - i
}

// knownMention reports whether a mention from readmeCommands names a command
// qm dispatches, with an argument that command accepts.
func knownMention(mention string) bool {
	name, arg, _ := strings.Cut(mention, " ")
	if _, ok := commands[name]; !ok {
		return false
	}
	sub, ok := subcommands[name]
	if !ok {
		return true
	}
	if sub.open && (arg == "" || strings.HasPrefix(arg, "-")) {
		return true
	}
	// Every command prints its usage for -h or --help (isHelp in main.go).
	return arg == "-h" || arg == "--help" || slices.Contains(sub.kinds, arg)
}

func TestReadmeCommandsFindsCodeMentionsOnly(t *testing.T) {
	md := "qm writes a file.\n\n" +
		"Run `qm init --yes` then `qm frobnicate`.\n\n" +
		"```sh\n" +
		"qm up --dry-run\n" +
		"make build && qm status\n" +
		"~/bin/qm version\n" +
		"```\n\n" +
		"The qm-worker plugin and `qm-worker` are not commands.\n\n" +
		// (a) the word after route, spawn and up is kept and checked.
		"`qm spawn worker --brief b.md`, `qm spawn frobnicate`, `qm route foo`,\n" +
		"`qm route review`, `qm up done NAME`, `qm up bogus NAME`, `qm up -- -c`,\n" +
		"`qm spawn --help`, `qm route --json`.\n\n" +
		// (b) quotes before qm.
		"```json\n" +
		"{\"command\": \"qm hooky\"}\n" +
		"```\n\n" +
		"Or `bash -c 'qm shell'`.\n\n" +
		// (c) a ~~~ fence, holding a ``` line that does not close it.
		"~~~\n" +
		"```\n" +
		"qm tilde\n" +
		"~~~\n\n" +
		// (d) a span that wraps onto the next line, then one that does not:
		// split per line, the parity flips and the second is read as prose
		// while "and qm prose" is read as code.
		"A span that `qm\nwrapped` wraps, and qm prose, then `qm after`.\n" +
		"A ``qm double `x` `` span.\n"
	got := readmeCommands(md)
	want := []string{
		"init --yes", "frobnicate", "up --dry-run", "status",
		"spawn worker", "spawn frobnicate", "route foo", "route review", "up done", "up bogus", "up --",
		"spawn --help", "route --json",
		"hooky", "shell", "tilde", "wrapped", "after", "double",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("readmeCommands = %q,\nwant %q", got, want)
	}
	var unknown []string
	for _, m := range got {
		if !knownMention(m) {
			unknown = append(unknown, m)
		}
	}
	wantUnknown := []string{
		"frobnicate", "spawn frobnicate", "route foo", "up bogus", "route --json",
		"hooky", "shell", "tilde", "wrapped", "after", "double",
	}
	if !reflect.DeepEqual(unknown, wantUnknown) {
		t.Fatalf("unknown mentions = %q,\nwant %q", unknown, wantUnknown)
	}
}

// TestReadmeCommandsExist: every "qm <command>" the README shows as code is a
// command qm dispatches, with a kind that command accepts, so the README
// cannot name a command that does not exist.
func TestReadmeCommandsExist(t *testing.T) {
	b, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	mentions := readmeCommands(string(b))
	if len(mentions) == 0 {
		t.Fatal("README.md shows no qm command")
	}
	for _, m := range mentions {
		if !knownMention(m) {
			t.Errorf("README.md shows `qm %s`, which qm does not dispatch", m)
		}
	}
}
