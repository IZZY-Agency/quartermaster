package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func hermetic(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
}

func TestReportEmptyLog(t *testing.T) {
	hermetic(t)
	var out, errb bytes.Buffer
	if code := run([]string{"report"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "workers: 0") {
		t.Errorf("output = %q", out.String())
	}
}

func TestReportBeforeAndProjectNeedBaseline(t *testing.T) {
	hermetic(t)
	for _, args := range [][]string{{"report", "--before", "2026-01-01"}, {"report", "--project", "x"}} {
		var out, errb bytes.Buffer
		if code := run(args, &out, &errb); code != 2 || !strings.Contains(errb.String(), "need --baseline") {
			t.Errorf("%v: exit %d, stderr %q", args, code, errb.String())
		}
	}
}

func TestReportBaselineEmptyLogNeedsBefore(t *testing.T) {
	hermetic(t)
	var out, errb bytes.Buffer
	if code := run([]string{"report", "--baseline"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "decision log is empty") {
		t.Errorf("exit %d, stderr %q", code, errb.String())
	}
}

func TestReportBadSince(t *testing.T) {
	hermetic(t)
	var out, errb bytes.Buffer
	if code := run([]string{"report", "--since", "nope"}, &out, &errb); code != 2 {
		t.Errorf("bad --since: exit %d", code)
	}
}

func put(t *testing.T, path, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestReportBaselineCountsWorkersOnly: a worker brief and a reviewer count; an
// ordinary session and a session of another project do not.
func TestReportBaselineCountsWorkersOnly(t *testing.T) {
	hermetic(t)
	d := os.Getenv("CLAUDE_CONFIG_DIR")
	line := func(prompt, branch string) string {
		return `{"type":"user","timestamp":"2026-01-01T00:00:00Z","cwd":"/r","gitBranch":"` + branch + `","message":{"content":"` + prompt + `"}}` + "\n" +
			`{"type":"assistant","message":{"id":"m","model":"claude-opus-5-5","usage":{"input_tokens":1000000}}}` + "\n"
	}
	put(t, filepath.Join(d, "projects", "-alpha", "w.jsonl"), line("You are a worker.\\ntype: feature", "feat"))
	put(t, filepath.Join(d, "projects", "-alpha", "r.jsonl"), line("You are a fresh reviewer", "feat"))
	put(t, filepath.Join(d, "projects", "-alpha", "c.jsonl"), line("let us plan", "main"))
	put(t, filepath.Join(d, "projects", "-beta", "w.jsonl"), line("You are a worker", "x"))
	cfg := os.Getenv("XDG_CONFIG_HOME")
	put(t, filepath.Join(cfg, "quartermaster", "config.json"), `{"report":{"worker_markers":["^MY-BRIEF"]}}`)
	put(t, filepath.Join(d, "projects", "-alpha", "x.jsonl"), line("MY-BRIEF go", "feat2"))

	var out, errb bytes.Buffer
	code := run([]string{"report", "--baseline", "--before", "2027-01-01", "--project", "alpha"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "baseline: 3 worker sessions, API-equivalent cost $12.00") {
		t.Errorf("output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "feature") {
		t.Errorf("the type word in the brief is not used:\n%s", out.String())
	}
}

func TestParsePR(t *testing.T) {
	pr, ok := parsePR([]byte(`{"additions":10,"deletions":5,"changedFiles":3,"state":"MERGED"}`))
	if !ok || pr.Lines() != 15 || pr.Files != 3 || !pr.Merged {
		t.Errorf("pr = %+v %v", pr, ok)
	}
	if pr, _ := parsePR([]byte(`{"state":"OPEN"}`)); pr.Merged {
		t.Error("open PR read as merged")
	}
	if _, ok := parsePR([]byte("nope")); ok {
		t.Error("garbage parsed")
	}
}

// TestReportFindsFinishedWorker: a worker that has exited has no
// sessions/*.json; the report finds its transcript by the session id qm
// launched it with.
func TestReportFindsFinishedWorker(t *testing.T) {
	hermetic(t)
	id := "0b4f1c2e-3d5a-4b6c-8d7e-9f0a1b2c3d4e"
	put(t, filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects", "-r", id+".jsonl"),
		`{"type":"assistant","message":{"id":"m","model":"claude-sonnet-5-5","usage":{"input_tokens":1000000}}}`+"\n")
	put(t, filepath.Join(os.Getenv("XDG_STATE_HOME"), "quartermaster", "decisions.jsonl"),
		`{"time":"2026-11-19T10:00:00Z","kind":"worker","project":"/r","branch":"f","name":"w1","claude_session":"`+id+`","type":"fix","decision":{"kind":"worker","tier":{"model":"sonnet","effort":"medium"}}}`+"\n")
	var out, errb bytes.Buffer
	if code := run([]string{"report"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "workers: 1, API-equivalent cost $2.00") || strings.Contains(out.String(), "without a transcript") {
		t.Errorf("output:\n%s", out.String())
	}
}
