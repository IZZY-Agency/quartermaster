package privacy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IZZY-Agency/quartermaster/internal/config"
)

// testToken is a made-up, high-entropy string shaped like an API token.
const testToken = "Zq8vR2mK9xT4wB7nL3pF6hJ1cY5dG0sE"

func TestEmailBeforeClient(t *testing.T) {
	clients := []config.Pseudonym{{Pattern: "example", Replace: "ClientA"}}
	got, found := Redact("Write to info@example.com about the example site.", clients, nil)
	want := "Write to <EMAIL_1> about the ClientA site."
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if found["<EMAIL_1>"] != "info@example.com" {
		t.Errorf("found = %v", found)
	}
}

func TestSlugNotToken(t *testing.T) {
	in := "Notes are in example-launch-day-2026-11-08 and example_launch_day_notes_for_the_team."
	if got, _ := Redact(in, nil, nil); got != in {
		t.Errorf("got %q, want it unchanged", got)
	}
}

func TestAllowlistedEmail(t *testing.T) {
	in := "Co-Authored-By: Claude <noreply@anthropic.com>, local admin@127.0.0.1, ask ops@example.com"
	want := "Co-Authored-By: Claude <noreply@anthropic.com>, local admin@127.0.0.1, ask <EMAIL_1>"
	if got, _ := Redact(in, nil, nil); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSamePersonSamePlaceholder(t *testing.T) {
	in := "Pat Example asked for it; check with Pat Example and Sam Sample, not Patrick."
	got, found := Redact(in, nil, []string{"Pat Example", "Sam Sample", "Pat Example", "Pat"})
	want := "<PERSON_1> asked for it; check with <PERSON_1> and <PERSON_2>, not Patrick."
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if found["<PERSON_1>"] != "Pat Example" || found["<PERSON_2>"] != "Sam Sample" {
		t.Errorf("found = %v", found)
	}
}

func TestRedactPatterns(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || len(home) < 2 {
		t.Skip("no home folder")
	}
	project := strings.ReplaceAll(filepath.Join(home, "src", "app"), "/", "-")
	cases := []struct{ name, in, want string }{
		{"home path", "open " + filepath.Join(home, "src", "app", "main.go"), "open ~/src/app/main.go"},
		{"project folder", "~/.claude/projects/" + project + "/log.jsonl", "~/.claude/projects/-src-app/log.jsonl"},
		{"ip", "server 203.0.113.7 and 203.0.113.7, local 127.0.0.1 and 0.0.0.0", "server <IP_1> and <IP_1>, local 127.0.0.1 and 0.0.0.0"},
		{"phone", "call +1 555 010 0199 today", "call <PHONE_1> today"},
		{"date is not a phone", "due 2026-11-08 12:30, or 19.11.2026", "due 2026-11-08 12:30, or 19.11.2026"},
		{"token", "key " + testToken + " here", "key <TOKEN_1> here"},
		{"hex is not a token", "commit 3f9a1c0b7e5d2a4f6b8c0e1d3f5a7b9c1e2d4f6a", "commit 3f9a1c0b7e5d2a4f6b8c0e1d3f5a7b9c1e2d4f6a"},
		{"low entropy is not a token", "aaaaaaaaaaaaaaaaaaaabbbbbbbbbbbbbbbb", "aaaaaaaaaaaaaaaaaaaabbbbbbbbbbbbbbbb"},
		{"client pattern ignores case", "Example Corp and EXAMPLE-CORP", "ClientA and ClientA"},
	}
	clients := []config.Pseudonym{{Pattern: "example[\\s-]?corp", Replace: "ClientA"}}
	for _, c := range cases {
		if got, _ := Redact(c.in, clients, nil); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestBadClientPatternIsLiteral(t *testing.T) {
	clients := []config.Pseudonym{{Pattern: "acme(", Replace: "ClientB"}}
	if got, _ := Redact("the acme( site", clients, nil); got != "the ClientB site" {
		t.Errorf("got %q", got)
	}
}

func TestHomeWholeSegment(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || len(home) < 2 {
		t.Skip("no home folder")
	}
	home = strings.TrimRight(home, "/")
	dashed := strings.ReplaceAll(home, "/", "-")
	cases := []struct{ in, want string }{
		{home + "xtra/notes.txt", home + "xtra/notes.txt"},
		{"cd " + home, "cd ~"},
		{"(" + home + ")", "(~)"},
		{"projects/" + dashed + "xtra-app", "projects/" + dashed + "xtra-app"},
		{"projects/" + dashed + "-app/", "projects/-app/"},
		{"projects/" + dashed, "projects/"},
	}
	for _, c := range cases {
		if got, _ := Redact(c.in, nil, nil); got != c.want {
			t.Errorf("Redact(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestClientSkipsPlaceholders(t *testing.T) {
	clients := []config.Pseudonym{{Pattern: "email", Replace: "ClientA"}, {Pattern: "person", Replace: "ClientB"}}
	got, found := Redact("Mail ops@example.com about the email form for Pat Example.", clients, []string{"Pat Example"})
	want := "Mail <EMAIL_1> about the ClientA form for <PERSON_1>."
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if _, ok := found["ClientB"]; ok {
		t.Errorf("a client pattern matched inside a placeholder: %v", found)
	}
}

func TestClientKeepsSpellings(t *testing.T) {
	clients := []config.Pseudonym{{Pattern: "example[\\s-]?corp", Replace: "ClientA"}}
	_, found := Redact("Example Corp, EXAMPLE-CORP and Example Corp again", clients, nil)
	if found["ClientA"] != "Example Corp\nEXAMPLE-CORP" {
		t.Errorf("found[ClientA] = %q", found["ClientA"])
	}
}

func TestTokenWithDigitRun(t *testing.T) {
	in := "key Ab-1234567890123-QwErTyUiOpAsDfGhJk here"
	if got, _ := Redact(in, nil, nil); got != "key <TOKEN_1> here" {
		t.Errorf("got %q", got)
	}
}

func TestBase64Secret(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plus and slash", "secret kQ9+zR4/vW2xT7nL3pF6hJ1cY5dG0sEaB8mN+uIo end", "secret <TOKEN_1> end"},
		{"padding", "aGVsbG8gd29ybGQgaGVsbG8gd29ybGQgaGVsbG8gd28=", "<TOKEN_1>"},
		{"mixed case and digits across a slash", "Zq8vR2mK9xT4wB7nL3pF/6hJ1cY5dG0sEaB8mNuIo", "<TOKEN_1>"},
		{"absolute path", "see /opt/Build2026/Release/Payload/Assets/Images/Large", "see /opt/Build2026/Release/Payload/Assets/Images/Large"},
		{"home-relative path", "see ~/Projects/App2026/Sources/Feature/ViewModels/Main", "see ~/Projects/App2026/Sources/Feature/ViewModels/Main"},
		{"commit hash", "3f9a1c0b7e5d2a4f6b8c0e1d3f5a7b9c1e2d4f6a3f9a1c0b", "3f9a1c0b7e5d2a4f6b8c0e1d3f5a7b9c1e2d4f6a3f9a1c0b"},
		{"secret after a path", "cat /tmp/kQ9+zR4/vW2xT7nL3pF6hJ1cY5dG0sEaB8mN+uIo", "cat /tmp/<TOKEN_1>"},
		{"secret after a URL path", "GET https://example.com/files/Zq8vR2mK9xT4wB7nL3pF/6hJ1cY5dG0sEaB8mNuIo", "GET https://example.com/files/<TOKEN_1>"},
		{"path after =", "--config=/opt/Build2026/Release/Payload/Assets/Images/Large", "--config=/opt/Build2026/Release/Payload/Assets/Images/Large"},
		{"relative path", "edit src/Internal/Handlers2/Middleware/AuthTokens/Refresh9", "edit src/Internal/Handlers2/Middleware/AuthTokens/Refresh9"},
		{"long relative path", "Applications/Visual2Studio/Contents/Resources/app/out/vs/Workbench", "Applications/Visual2Studio/Contents/Resources/app/out/vs/Workbench"},
	}
	for _, c := range cases {
		if got, _ := Redact(c.in, nil, nil); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestClientSkipsPseudonyms(t *testing.T) {
	clients := []config.Pseudonym{{Pattern: "example corp", Replace: "ClientA"}, {Pattern: "client", Replace: "ClientB"}}
	got, _ := Redact("Example Corp asked the client.", clients, nil)
	if want := "ClientA asked the ClientB."; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
