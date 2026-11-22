package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IZZY-Agency/quartermaster/internal/limits"
)

// runStatuslineWith runs the statusline command on input with the state
// folder in a temp dir; it returns stdout and the qm state dir.
func runStatuslineWith(t *testing.T, input string, args ...string) (string, string) {
	t.Helper()
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	var out, errb bytes.Buffer
	if code := statusline(strings.NewReader(input), args, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	return out.String(), filepath.Join(state, "quartermaster")
}

func readClaudeFile(t *testing.T, stateDir string) (limits.ClaudeFile, string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(stateDir, "limits", "claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f limits.ClaudeFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	return f, string(data)
}

func TestStatuslineWritesLimits(t *testing.T) {
	in := `{"rate_limits":{"five_hour":{"used_percentage":42.5,"resets_at":1790000000},"seven_day":{"used_percentage":10,"resets_at":1790500000}}}`
	_, dir := runStatuslineWith(t, in)
	f, _ := readClaudeFile(t, dir)
	if f.FiveHourUsed == nil || *f.FiveHourUsed != 42.5 || f.SevenDayUsed == nil || *f.SevenDayUsed != 10 {
		t.Fatalf("used: %+v", f)
	}
	if f.FiveHourResetsAt == nil || f.FiveHourResetsAt.Unix() != 1790000000 || f.SevenDayResetsAt == nil || f.SevenDayResetsAt.Unix() != 1790500000 {
		t.Fatalf("resets: %+v", f)
	}
	if f.UpdatedAt.IsZero() {
		t.Fatal("updated_at not set")
	}
}

// TestStatuslineNullLimits: input with no usage figure (an API-key session)
// writes nothing, so a reading another session recorded stays.
func TestStatuslineNullLimits(t *testing.T) {
	good := `{"rate_limits":{"five_hour":{"used_percentage":42.5}}}`
	for _, in := range []string{`{}`, `{"rate_limits":null}`, `{"rate_limits":{"five_hour":null,"seven_day":{"used_percentage":null}}}`} {
		_, dir := runStatuslineWith(t, in)
		if _, err := os.Stat(filepath.Join(dir, "limits", "claude.json")); !os.IsNotExist(err) {
			t.Fatalf("%s: a limit file was written (%v)", in, err)
		}
		var out, errb bytes.Buffer
		if code := statusline(strings.NewReader(good), nil, &out, &errb); code != 0 {
			t.Fatal(errb.String())
		}
		if code := statusline(strings.NewReader(in), nil, &out, &errb); code != 0 {
			t.Fatal(errb.String())
		}
		if f, _ := readClaudeFile(t, dir); f.FiveHourUsed == nil || *f.FiveHourUsed != 42.5 {
			t.Fatalf("%s wiped the reading: %+v", in, f)
		}
	}
}

func TestStatuslinePassthrough(t *testing.T) {
	in := `{"rate_limits":{"five_hour":{"used_percentage":1}}}`
	out, dir := runStatuslineWith(t, in, "--", "cat")
	if out != in {
		t.Fatalf("stdout %q, want %q", out, in)
	}
	if f, _ := readClaudeFile(t, dir); f.FiveHourUsed == nil || *f.FiveHourUsed != 1 {
		t.Fatalf("file: %+v", f)
	}
}

func TestStatuslineNoCommand(t *testing.T) {
	out, _ := runStatuslineWith(t, `{}`)
	if out != "" {
		t.Fatalf("stdout %q, want empty", out)
	}
}

func TestStatuslineWriteFailureKeepsOutput(t *testing.T) {
	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(state, "quartermaster"), nil, 0o600); err != nil { // a file where the folder should be
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", state)
	var out, errb bytes.Buffer
	if code := statusline(strings.NewReader("hello"), []string{"--", "cat"}, &out, &errb); code != 0 || out.String() != "hello" {
		t.Fatalf("code %d out %q err %q", code, out.String(), errb.String())
	}
}

func TestStatuslineMalformedFieldKeepsOthers(t *testing.T) {
	in := `{"rate_limits":{"five_hour":{"used_percentage":"high","resets_at":1790000000},"seven_day":{"used_percentage":7,"resets_at":"soon"}}}`
	_, dir := runStatuslineWith(t, in)
	f, _ := readClaudeFile(t, dir)
	if f.FiveHourUsed != nil || f.SevenDayResetsAt != nil {
		t.Fatalf("malformed fields should be nil: %+v", f)
	}
	if f.SevenDayUsed == nil || *f.SevenDayUsed != 7 || f.FiveHourResetsAt == nil || f.FiveHourResetsAt.Unix() != 1790000000 {
		t.Fatalf("good fields lost: %+v", f)
	}
}

func TestStatuslineArgsKeepSpaces(t *testing.T) {
	out, _ := runStatuslineWith(t, "x", "--", "printf", "%s|", "a b", "c")
	if out != "a b|c|" {
		t.Fatalf("stdout %q", out)
	}
}

func TestStatuslineLargeInput(t *testing.T) {
	in := `{"pad":"` + strings.Repeat("x", 1<<20) + `"}`
	out, _ := runStatuslineWith(t, in, "--", "cat")
	if out != in {
		t.Fatalf("passthrough lost data: got %d bytes, want %d", len(out), len(in))
	}
}

func TestStatuslineFileModeAndNoTempFiles(t *testing.T) {
	_, dir := runStatuslineWith(t, `{"rate_limits":{"five_hour":{"used_percentage":1}}}`)
	limitsDir := filepath.Join(dir, "limits")
	st, err := os.Stat(filepath.Join(limitsDir, "claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v, want 0600", st.Mode().Perm())
	}
	if d, _ := os.Stat(limitsDir); d.Mode().Perm() != 0o700 {
		t.Fatalf("folder mode %v, want 0700", d.Mode().Perm())
	}
	entries, _ := os.ReadDir(limitsDir)
	if len(entries) != 1 {
		t.Fatalf("leftover files: %v", entries)
	}
}
