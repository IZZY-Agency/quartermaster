package claudecode

import (
	"slices"
	"strings"
	"testing"
)

func TestKeptFlags(t *testing.T) {
	for _, tc := range []struct {
		name          string
		args          string
		kept, dropped string
	}{
		{
			name:    "mixed",
			args:    "--model opus --effort high --plugin-dir /p --permission-mode=acceptEdits --mcp-config a.json --dangerously-skip-permissions",
			kept:    "--model opus --effort high --permission-mode acceptEdits --dangerously-skip-permissions",
			dropped: "--plugin-dir --mcp-config",
		},
		// A value-less flag never takes the prompt's first word as its value.
		{name: "-c then a prompt", args: "-c add --dangerously-skip-permissions to the script", kept: "", dropped: "-c"},
		{name: "--verbose then a prompt", args: "--model m --verbose fix --permission-mode bypassPermissions", kept: "--model m", dropped: "--verbose"},
		{name: "allow flag", args: "--allow-dangerously-skip-permissions --model=m", kept: "--allow-dangerously-skip-permissions --model m"},
		{name: "stops at --", args: "--model a -- --effort high --x", kept: "--model a"},
		{name: "stops at a newline token", args: `--model a first\012 --effort high`, kept: "--model a"},
		{name: "value starting with a dash", args: "--model -x --effort low", kept: "--effort low"},
		{name: "value is a newline token", args: `--model x\012 --effort low`, kept: ""},
		{name: "stops at a prompt", args: "why is --dangerously-skip-permissions risky", kept: ""},
		{name: "stops at a prompt after flags", args: "--model opus hello --effort high", kept: "--model opus"},
		{name: "stops after a valueless flag", args: "--dangerously-skip-permissions fix --permission-mode plan", kept: "--dangerously-skip-permissions"},
		{name: "unknown flag's value read past", args: "--plugin-dir /p --model opus", kept: "--model opus", dropped: "--plugin-dir"},
		{name: "deduplicated, equals cut", args: "-n w --resume=a --resume b --settings={} --settings x", dropped: "-n --resume --settings"},
	} {
		kept, dropped := KeptFlags(strings.Fields(tc.args))
		if want := strings.Fields(tc.kept); !slices.Equal(kept, want) {
			t.Errorf("%s: kept = %q, want %q", tc.name, kept, want)
		}
		if tc.dropped != "" {
			if want := strings.Fields(tc.dropped); !slices.Equal(dropped, want) {
				t.Errorf("%s: dropped = %q, want %q", tc.name, dropped, want)
			}
		}
	}
}
