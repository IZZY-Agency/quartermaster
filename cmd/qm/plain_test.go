package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// No command writes a control character to the terminal, whatever it
// prints; hook and statusline pass their output on as it is.
func TestRunKeepsControlCharactersOffTheTerminal(t *testing.T) {
	const evil = "a\x1b]0;owned\x07\rb"
	for _, name := range []string{"probe", "hook", "statusline"} {
		old, had := commands[name]
		commands[name] = command{"probe", func(_ []string, stdout, stderr io.Writer) int {
			_, _ = io.WriteString(stdout, evil)
			_, _ = io.WriteString(stderr, evil)
			return 0
		}}
		var out, errb bytes.Buffer
		run([]string{name}, &out, &errb)
		if had {
			commands[name] = old
		} else {
			delete(commands, name)
		}
		want := "a]0;ownedb"
		if name != "probe" {
			want = evil
		}
		if out.String() != want || errb.String() != want {
			t.Errorf("%s wrote %q and %q, want %q", name, out.String(), errb.String(), want)
		}
	}
	// An argument qm quotes back goes through the same writer.
	var out, errb bytes.Buffer
	run([]string{"no\x1b[2Jsuch"}, &out, &errb)
	if strings.ContainsRune(errb.String(), '\x1b') {
		t.Errorf("unknown command wrote %q", errb.String())
	}
}
