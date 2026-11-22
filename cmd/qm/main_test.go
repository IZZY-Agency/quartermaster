package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionLine(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"--version"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	if got := out.String(); got != "qm dev (unknown)\n" {
		t.Errorf("got %q", got)
	}
}

func TestCommandsInUsage(t *testing.T) {
	if len(commands) == 0 {
		t.Fatal("no commands")
	}
	u := usage()
	for name, c := range commands {
		if c.run == nil || c.summary == "" {
			t.Errorf("%s: missing run or summary", name)
		}
		if !strings.Contains(u, "  "+name+" ") {
			t.Errorf("%s not in usage", name)
		}
	}
	for alias, target := range aliases {
		if _, ok := commands[target]; !ok {
			t.Errorf("alias %s points at unknown command %s", alias, target)
		}
	}
}

func TestUnknownCommand(t *testing.T) {
	for _, args := range [][]string{{"no-such-command"}, {}} {
		var out, errOut bytes.Buffer
		if code := run(args, &out, &errOut); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
		if !strings.Contains(errOut.String(), "usage: qm") {
			t.Errorf("%v: no usage on stderr: %q", args, errOut.String())
		}
	}
}
