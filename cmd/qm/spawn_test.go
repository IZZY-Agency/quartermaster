package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/IZZY-Agency/quartermaster/internal/config"
	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/spawn"
)

func TestSpawnWorkerNeedsHint(t *testing.T) {
	routeHermetic(t)
	dir := t.TempDir()
	brief := routeFile(t, dir, "brief.md", "Fix the typo.\n")
	var out, errb bytes.Buffer
	code := run([]string{"spawn", "worker", "--brief", brief, "--type", "fix", "--size", "S", "--name", "w1", "--cwd", dir}, &out, &errb)
	if code != 2 || !strings.Contains(errb.String(), "hint (model/effort) is required") {
		t.Errorf("exit %d, stderr %q; want 2 and a hint is required", code, errb.String())
	}
}

func TestSpawnWorkerBadInputExits2(t *testing.T) {
	routeHermetic(t)
	dir := t.TempDir()
	brief := routeFile(t, dir, "brief.md", "Fix the typo.\n")
	base := []string{"spawn", "worker", "--brief", brief, "--type", "fix", "--hint", "sonnet/low", "--cwd", dir}
	for name, extra := range map[string][]string{
		"bad size":   {"--size", "XL", "--name", "w1"},
		"bad name":   {"--size", "S", "--name", "a/b"},
		"why alone":  {"--size", "S", "--name", "w1", "--why", "because"},
		"no name":    {"--size", "S"},
		"bad --type": {"--size", "S", "--name", "w1", "--type", "chore"},
	} {
		var out, errb bytes.Buffer
		if code := run(append(slices.Clone(base), extra...), &out, &errb); code != 2 {
			t.Errorf("%s: exit %d, stderr %q; want 2", name, code, errb.String())
		}
	}
}

func TestSpawnWorkerDryRun(t *testing.T) {
	routeHermetic(t)
	t.Setenv("AGTERM_SESSION_ID", "")
	t.Setenv("TMUX", "")
	t.Setenv("CLAUDE_PID", "")
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "claude"))
	t.Setenv("QM_WORKER_PLUGIN", t.TempDir())
	repo := routeRepo(t)
	brief := routeFile(t, t.TempDir(), "brief.md", "Fix the typo in the readme.\n")
	var out, errb bytes.Buffer
	code := run([]string{"spawn", "worker", "--brief", brief, "--type", "docs", "--size", "S", "--hint", "sonnet/low", "--name", "w1", "--cwd", repo, "--dry-run"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	for _, want := range []string{"sonnet/low (row ", "launcher: none found: no launcher", "#!/bin/sh\n", "--model claude-sonnet-5-5 --effort low"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("XDG_STATE_HOME"), "quartermaster")); !os.IsNotExist(err) {
		t.Errorf("a dry run wrote state: %v", err)
	}
}

func TestSpawnWorkerDryRunTiedAndUntrusted(t *testing.T) {
	routeHermetic(t)
	t.Setenv("AGTERM_SESSION_ID", "")
	t.Setenv("TMUX", "")
	t.Setenv("CLAUDE_PID", "")
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "claude"))
	t.Setenv("QM_WORKER_PLUGIN", t.TempDir())
	t.Setenv("QM_PROJECT_ID", "try-00000000")
	t.Setenv("QM_WORKSPACE", "try")
	repo := routeRepo(t)
	brief := routeFile(t, t.TempDir(), "brief.md", "Fix the typo in the readme.\n")
	var out, errb bytes.Buffer
	args := []string{"spawn", "worker", "--brief", brief, "--type", "docs", "--size", "S", "--hint", "sonnet/low", "--name", "w1", "--cwd", repo, "--dry-run"}
	code := run(args, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	for _, want := range []string{" QM_PROJECT_ID=try-00000000 QM_WORKSPACE=try ", "\ntrust: not trusted by Claude Code: " + repo + "\n"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}

	// An explicit --workspace keeps the project's workspace in the launch.
	out.Reset()
	errb.Reset()
	if code := run(append(args, "--workspace", "ws-7"), &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if !strings.Contains(out.String(), " QM_PROJECT_ID=try-00000000 QM_WORKSPACE=try ") {
		t.Errorf("with --workspace, output lacks the project tie:\n%s", out.String())
	}
}

func TestReportStartedPrintsPinWarning(t *testing.T) {
	var out, errb bytes.Buffer
	res := spawn.Result{Ref: launch.SessionRef{Launcher: "agterm", ID: "s-1", Name: "w1", Warning: "agterm restore pin failed: no session"}}
	if err := reportStarted(res, &out, &errb); err != nil {
		t.Fatal(err)
	}
	if errb.String() != "qm spawn: warning: agterm restore pin failed: no session\n" {
		t.Errorf("stderr = %q, want the pin warning", errb.String())
	}
	if !strings.HasPrefix(out.String(), "started agterm s-1 (w1)\n") {
		t.Errorf("stdout = %q", out.String())
	}

	out.Reset()
	errb.Reset()
	res.Ref.Warning = ""
	if err := reportStarted(res, &out, &errb); err != nil || errb.Len() != 0 {
		t.Errorf("no warning: err %v, stderr %q", err, errb.String())
	}
}

func TestSpawnWorkerDryRunShowsTheTint(t *testing.T) {
	routeHermetic(t)
	t.Setenv("AGTERM_SESSION_ID", "")
	t.Setenv("TMUX", "")
	t.Setenv("CLAUDE_PID", "")
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "claude"))
	t.Setenv("QM_WORKER_PLUGIN", t.TempDir())
	repo := routeRepo(t)
	brief := routeFile(t, t.TempDir(), "brief.md", "Fix the typo in the readme.\n")
	args := []string{"spawn", "worker", "--brief", brief, "--type", "docs", "--size", "S", "--hint", "sonnet/low", "--name", "w1", "--cwd", repo, "--dry-run"}
	var out, errb bytes.Buffer
	if code := run(args, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if !strings.Contains(out.String(), "\ntint: teal on icon\n") {
		t.Errorf("output lacks the default tint:\n%s", out.String())
	}

	cfgPath, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ cfg, want, note string }{
		{`{"tint":{"worker":""}}`, "\ntint: none\n", ""},
		{`{"tint":{"worker":"#00FF00"}}`, "\ntint: #00ff00 on icon\n", ""},
		{`{"tint":{"worker":"chartreuse"}}`, "\ntint: teal on icon\n", "qm spawn: note: config: tint.worker"},
	} {
		if err := os.WriteFile(cfgPath, []byte(tc.cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		out.Reset()
		errb.Reset()
		if code := run(args, &out, &errb); code != 0 {
			t.Fatalf("%s: exit %d, stderr %q", tc.cfg, code, errb.String())
		}
		if !strings.Contains(out.String(), tc.want) {
			t.Errorf("%s: output lacks %q:\n%s", tc.cfg, tc.want, out.String())
		}
		if (tc.note == "") != (errb.Len() == 0) || !strings.Contains(errb.String(), tc.note) {
			t.Errorf("%s: stderr %q, want a line with %q", tc.cfg, errb.String(), tc.note)
		}
	}
}

func TestReportStartedPrintsTintNotes(t *testing.T) {
	var out, errb bytes.Buffer
	res := spawn.Result{
		Ref:      launch.SessionRef{Launcher: "agterm", ID: "s-1", Name: "w1", TintNote: "tab not tinted: this agterm does not know --tint; update agterm"},
		TintNote: "config: tint.worker: \"x\" is not a colour",
	}
	if err := reportStarted(res, &out, &errb); err != nil {
		t.Fatal(err)
	}
	want := "qm spawn: note: config: tint.worker: \"x\" is not a colour\nqm spawn: note: tab not tinted: this agterm does not know --tint; update agterm\n"
	if errb.String() != want {
		t.Errorf("stderr = %q, want %q", errb.String(), want)
	}
	if !strings.HasPrefix(out.String(), "started agterm s-1 (w1)\n") {
		t.Errorf("stdout = %q", out.String())
	}
}
