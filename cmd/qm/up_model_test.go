package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/up"
)

const defaultModelText = "model fable (up.coordinator_model), fallback opus (up.coordinator_fallback)"

// upOK runs qm up with args and wants exit 0; it returns its output.
func upOK(t *testing.T, args ...string) string {
	t.Helper()
	var out, errb bytes.Buffer
	if code := run(append([]string{"up"}, args...), &out, &errb); code != 0 {
		t.Fatalf("qm up %q: exit %d, stdout %q, stderr %q", args, code, out.String(), errb.String())
	}
	return out.String()
}

// upScript is the newest coordinator launch script.
func upScript(t *testing.T) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(os.Getenv("XDG_STATE_HOME"), "quartermaster", "launch", "*-coord*"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no coordinator script: %v", err)
	}
	b, err := os.ReadFile(files[len(files)-1])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestUpFirstRunModel(t *testing.T) {
	m := newUpMachine(t)
	if out := upOK(t, "--dry-run"); !strings.Contains(out, "the coordinator would start with "+defaultModelText+"\n") {
		t.Errorf("dry run: %q", out)
	}
	out := upOK(t)
	p := loadProject(t, m.root)
	if !strings.Contains(out, "in workspace "+p.Workspace+", "+defaultModelText+"\n") {
		t.Errorf("start: %q", out)
	}
	if s := upScript(t); !strings.Contains(s, "claude --model fable --fallback-model opus -n ") {
		t.Errorf("script:\n%s", s)
	}
	if len(p.ClaudeArgs) != 0 || p.DefaultModels[p.Coordinator.ID] != "fable" {
		t.Errorf("project %+v", p)
	}
}

func TestUpFirstRunExplicitModel(t *testing.T) {
	m := newUpMachine(t)
	swap(t, &upClaudeVersion, func() (string, error) {
		t.Error("claude's version was read, and nothing needs it")
		return "", nil
	})
	want := "model sonnet (its claude arguments), fallback not set"
	if out := upOK(t, "--dry-run", "--", "--model", "sonnet"); !strings.Contains(out, want) {
		t.Errorf("dry run: %q", out)
	}
	if out := upOK(t, "--new", "--", "--model", "sonnet"); !strings.Contains(out, want) {
		t.Errorf("start: %q", out)
	}
	if s := upScript(t); !strings.Contains(s, "claude --model sonnet -n ") {
		t.Errorf("script:\n%s", s)
	}
	if p := loadProject(t, m.root); len(p.DefaultModels) != 0 {
		t.Errorf("default models %v", p.DefaultModels)
	}
}

func TestUpModelHereAndNoLauncher(t *testing.T) {
	hereMachine(t)
	if out := upOK(t, "--here"); !strings.Contains(out, defaultModelText) {
		t.Errorf("here: %q", out)
	}
	newUpMachine(t)
	swap(t, &upPick, func(string) (launch.Launcher, error) { return nil, launch.ErrNoLauncher })
	var out, errb bytes.Buffer
	if code := run([]string{"up"}, &out, &errb); code != 1 || !strings.Contains(out.String(), "the coordinator starts with "+defaultModelText+"\n") {
		t.Errorf("no launcher: exit %d, stdout %q, stderr %q", code, out.String(), errb.String())
	}
}

func TestUpModelConfig(t *testing.T) {
	for _, tc := range []struct {
		config, text, note string
		script             string
	}{
		{`{"up":{"coordinator_model":""}}`, "model not set, fallback not set", "", "claude -n "},
		{`{"up":{"coordinator_model":"opus","coordinator_fallback":""}}`, "model opus (up.coordinator_model), fallback not set", "", "claude --model opus -n "},
		{`{"up":{"coordinator_model":"nope"}}`, defaultModelText, `note: up.coordinator_model is "nope"`, "claude --model fable --fallback-model opus -n "},
		{`{"up":{"claude_args":["--model","sonnet"]}}`, "model sonnet (its claude arguments), fallback not set", "", "claude --model sonnet -n "},
	} {
		newUpMachine(t)
		writeConfig(t, tc.config)
		out := upOK(t)
		if !strings.Contains(out, tc.text) || !strings.Contains(out, tc.note) || (tc.note == "") == strings.Contains(out, "note:") {
			t.Errorf("%s: %q", tc.config, out)
		}
		if s := upScript(t); !strings.Contains(s, tc.script) {
			t.Errorf("%s: script\n%s", tc.config, s)
		}
	}
}

// A claude that cannot be given a fallback, or whose version is not known,
// gets neither: the coordinator starts as it did before.
func TestUpModelNeedsAClaudeThatKnowsIt(t *testing.T) {
	for _, tc := range []struct {
		version string
		err     error
		used    bool
		note    string
	}{
		{"2.1.332 (Claude Code)\n", nil, true, ""},
		{"2.1.170 (Claude Code)", nil, true, ""},
		{"3.0.0 (Claude Code)", nil, true, ""},
		{"2.2.0", nil, true, ""},
		{"2.1.169 (Claude Code)", nil, false, "Claude Code 2.1.169"},
		{"1.0.200 (Claude Code)", nil, false, "Claude Code 1.0.200"},
		{"hello", nil, false, "version"},
		{"", errors.New("no claude on the PATH"), false, "no claude on the PATH"},
	} {
		newUpMachine(t)
		reads := 0
		swap(t, &upClaudeVersion, func() (string, error) {
			reads++
			return tc.version, tc.err
		})
		out := upOK(t)
		if strings.Contains(out, defaultModelText) != tc.used || strings.Contains(upScript(t), "--fallback-model") != tc.used || strings.Contains(upScript(t), "--model") != tc.used {
			t.Errorf("%q: %q", tc.version, out)
		}
		if (tc.note == "") == strings.Contains(out, "note:") || !strings.Contains(out, tc.note) {
			t.Errorf("%q: note in %q", tc.version, out)
		}
		if reads != 1 {
			t.Errorf("%q: claude's version read %d times", tc.version, reads)
		}
	}
}

// The real reader is bounded: a claude that hangs costs its time limit, not
// the run.
func TestUpReadClaudeVersion(t *testing.T) {
	bin := t.TempDir()
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\n"+body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	write("[ \"$*\" = --version ] && echo '2.1.200 (Claude Code)'\n")
	if v, err := readClaudeVersion(context.Background(), 10*time.Second); err != nil || strings.TrimSpace(v) != "2.1.200 (Claude Code)" {
		t.Errorf("version %q, %v", v, err)
	}
	write("sleep 30 & wait\n")
	start := time.Now()
	if v, err := readClaudeVersion(context.Background(), 200*time.Millisecond); err == nil {
		t.Errorf("a hanging claude answered %q", v)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("it took %s", d)
	}
	write("exit 3\n")
	if _, err := readClaudeVersion(context.Background(), 10*time.Second); err == nil {
		t.Error("a failing claude gave no error")
	}
}

// freshMachine is a project whose coordinator is started fresh: its
// transcript is missing, so there is nothing to resume.
func freshMachine(t *testing.T) (*upMachine, up.Project) {
	t.Helper()
	m := newUpMachine(t)
	p := m.project(t)
	if err := os.RemoveAll(filepath.Join(m.claude, "projects")); err != nil {
		t.Fatal(err)
	}
	return m, p
}

func TestUpRestoreFreshModel(t *testing.T) {
	m, _ := freshMachine(t)
	if out := upOK(t, "--dry-run"); !strings.Contains(out, "the coordinator would start with "+defaultModelText+"\n") {
		t.Errorf("dry run: %q", out)
	}
	if out := upOK(t, "--dry-run", "--", "--model", "sonnet"); !strings.Contains(out, "model sonnet (its claude arguments), fallback not set") {
		t.Errorf("dry run with a model: %q", out)
	}
	out := upOK(t, "--yes")
	if !strings.Contains(out, "the coordinator starts with "+defaultModelText+"\n") {
		t.Errorf("plan: %q", out)
	}
	if !strings.Contains(out, "(agterm s-1), "+defaultModelText+"\n") {
		t.Errorf("start line: %q", out)
	}
	p := loadProject(t, m.root)
	if p.DefaultModels[p.Coordinator.ID] != "fable" || !slices.Equal(p.ClaudeArgs, []string{"--permission-mode", "acceptEdits"}) {
		t.Errorf("project %+v", p)
	}
}

func TestUpRestoreResumedModel(t *testing.T) {
	m := newUpMachine(t)
	p := m.project(t)
	if out := upOK(t, "--dry-run"); strings.Contains(out, "model") || strings.Contains(out, "fallback") {
		t.Errorf("a resumed coordinator qm gave no default: %q", out)
	}
	p.DefaultModels = map[string]string{upCoordID: "fable"}
	if err := up.Save(p); err != nil {
		t.Fatal(err)
	}
	if out := upOK(t, "--dry-run"); !strings.Contains(out, "the coordinator would start with fallback opus (up.coordinator_fallback)\n") {
		t.Errorf("dry run: %q", out)
	}
	if out := upOK(t, "--yes"); !strings.Contains(out, "(agterm s-1), fallback opus (up.coordinator_fallback)\n") {
		t.Errorf("start: %q", out)
	}
	if s := upScript(t); !strings.Contains(s, "claude --fallback-model opus --permission-mode acceptEdits -n ") {
		t.Errorf("script:\n%s", s)
	}
	if got := loadProject(t, m.root).DefaultModels[upCoordID]; got != "fable" {
		t.Errorf("default model %q after the resume", got)
	}
}

// Arguments given with a model are the user's choice: qm's default is
// forgotten with them.
func TestUpRestoreExplicitModelForgetsTheDefault(t *testing.T) {
	m := newUpMachine(t)
	p := m.project(t)
	p.DefaultModels = map[string]string{upCoordID: "fable"}
	if err := up.Save(p); err != nil {
		t.Fatal(err)
	}
	upOK(t, "--yes", "--", "--model", "opus")
	if s := upScript(t); !strings.Contains(s, "claude --model opus -n ") {
		t.Errorf("script:\n%s", s)
	}
	if got := loadProject(t, m.root); got.DefaultModels[upCoordID] != "" || !slices.Equal(got.ClaudeArgs, []string{"--model", "opus"}) {
		t.Errorf("project %+v", got)
	}
}
