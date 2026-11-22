package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// failingReader fails the test when read.
type failingReader struct{ t *testing.T }

func (r failingReader) Read([]byte) (int, error) {
	r.t.Error("stdin was read")
	return 0, io.EOF
}

// TestHelpFirst checks that -h and --help print usage on stdout, exit 0, read
// no stdin and write no decision log.
func TestHelpFirst(t *testing.T) {
	routeHermetic(t)
	oldHook := hookStdin
	hookStdin = failingReader{t}
	t.Cleanup(func() { hookStdin = oldHook })

	// An open pipe nobody writes to: a read of os.Stdin would block.
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStdin := os.Stdin
	os.Stdin = pr
	t.Cleanup(func() { os.Stdin = oldStdin; _ = pr.Close(); _ = pw.Close() })

	for _, cmd := range []string{"statusline", "hook", "route", "spawn", "version", "up", "watch"} {
		for _, flag := range []string{"-h", "--help"} {
			var out, errOut bytes.Buffer
			done := make(chan int, 1)
			go func() { done <- run([]string{cmd, flag}, &out, &errOut) }()
			select {
			case code := <-done:
				if code != 0 {
					t.Errorf("qm %s %s: exit %d, stderr %q", cmd, flag, code, errOut.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("qm %s %s: blocked (reads stdin?)", cmd, flag)
			}
			if !strings.HasPrefix(out.String(), "usage:\n  qm "+cmd) || errOut.Len() != 0 {
				t.Errorf("qm %s %s: stdout %q, stderr %q; want usage on stdout only", cmd, flag, out.String(), errOut.String())
			}
		}
	}
	state := os.Getenv("XDG_STATE_HOME")
	if m, _ := filepath.Glob(filepath.Join(state, "*")); len(m) != 0 {
		t.Errorf("state folder holds %v after help; want nothing written", m)
	}
}

func TestUpHelpListsDoneAndSeparator(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"up", "--help"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"qm up done NAME", "-- CLAUDE ARGS", "done NAME ", "--  "} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("qm up --help lacks %q:\n%s", want, out.String())
		}
	}
}

// TestRouteReviewWorkerMatchesSpawn checks that qm route worker --type review
// previews the decision qm spawn worker --type review --dry-run takes, with
// Codex available (spawn drops its tier for a session) and logs nothing.
func TestRouteReviewWorkerMatchesSpawn(t *testing.T) {
	routeHermetic(t)
	codexLook = func(string) (string, error) { return "/bin/codex", nil }
	t.Setenv("QM_WORKER_PLUGIN", t.TempDir())
	repo := routeRepo(t)
	routeGit(t, repo, "update-ref", "refs/remotes/origin/main", "main") // the default base
	prompt := routeFile(t, t.TempDir(), "prompt.md", "Check that the notes file reads well.\n")

	var rOut, rErr bytes.Buffer
	if code := run([]string{"route", "worker", "--brief", prompt, "--type", "review", "--repo", repo}, &rOut, &rErr); code != 0 {
		t.Fatalf("route: exit %d, stderr %q", code, rErr.String())
	}
	var sOut, sErr bytes.Buffer
	args := []string{"spawn", "worker", "--brief", prompt, "--type", "review", "--size", "S", "--hint", "sonnet/low", "--name", "rv", "--cwd", repo, "--dry-run"}
	if code := run(args, &sOut, &sErr); code != 0 {
		t.Fatalf("spawn: exit %d, stderr %q", code, sErr.String())
	}
	spawnLine, _, _ := strings.Cut(sOut.String(), "\n")
	// Spawn appends launch-only reasons (LSP plugins, permission mode).
	if got := strings.TrimSuffix(rOut.String(), "\n"); !strings.HasPrefix(spawnLine, got) {
		t.Errorf("route %q\nspawn %q", got, spawnLine)
	}
	if strings.Contains(rOut.String(), "Codex gpt") || !strings.Contains(rOut.String(), "reviewer session runs Claude") {
		t.Errorf("route kept the Codex tier: %q", rOut.String())
	}
	if recs := routeLogged(t); len(recs) != 0 {
		t.Errorf("%d records logged, want none", len(recs))
	}
}
