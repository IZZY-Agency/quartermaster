package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/declog"
	"github.com/IZZY-Agency/quartermaster/internal/reviewin"
	"github.com/IZZY-Agency/quartermaster/internal/route"
)

// routeHermetic points every folder qm reads at a temporary one and turns off
// every provider that could reach the network or the owner's own data: no
// TypeSafe key, no fake Jev, no privacy helper, no gitleaks.
func routeHermetic(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	t.Setenv("TYPESAFE_API_KEY", "")
	t.Setenv("QM_FAKE_JEV", "")
	t.Setenv("QM_PRIVCHECK", "")
	t.Setenv("QM_GITLEAKS", "")
	noCodex(t)
}

// noCodex makes codex missing for limits.Codex, whatever PATH holds.
func noCodex(t *testing.T) {
	t.Helper()
	old := codexLook
	codexLook = func(string) (string, error) { return "", exec.ErrNotFound }
	t.Cleanup(func() { codexLook = old })
}

func routeFile(t *testing.T, dir, name, text string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func routeGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	base := []string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}
	out, err := exec.Command("git", append(base, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// routeRepo makes a repository with one commit on main and one on branch
// feature that adds a 3-line file; it returns the repository's folder.
func routeRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	routeGit(t, dir, "init", "-q", "-b", "main")
	routeFile(t, dir, "README.md", "readme\n")
	routeGit(t, dir, "add", ".")
	routeGit(t, dir, "commit", "-q", "-m", "first")
	routeGit(t, dir, "checkout", "-q", "-b", "feature")
	routeFile(t, dir, "notes.txt", "one\ntwo\nthree\n")
	routeGit(t, dir, "add", ".")
	routeGit(t, dir, "commit", "-q", "-m", "second")
	return dir
}

func routeJSON(t *testing.T, args ...string) route.Decision {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := run(append([]string{"route"}, args...), &out, &errOut); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	var d route.Decision
	if err := json.Unmarshal(out.Bytes(), &d); err != nil {
		t.Fatalf("output is not a decision: %v\n%s", err, out.String())
	}
	return d
}

func routeLogged(t *testing.T) []route.Record {
	t.Helper()
	recs, err := declog.Read(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func TestRouteWorkerJSON(t *testing.T) {
	routeHermetic(t)
	fixture, err := filepath.Abs("testdata/jev-worker-plain.json")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("QM_FAKE_JEV", fixture)
	repo := routeRepo(t)
	brief := routeFile(t, t.TempDir(), "brief.md", "Rename the helper function in one file and update its callers.\n")

	d := routeJSON(t, "worker", "--brief", brief, "--type", "feature", "--size", "S", "--repo", repo, "--json")
	if d.Row != "s" || d.Tier != (route.Tier{Model: "sonnet", Effort: "medium"}) {
		t.Errorf("row %q tier %+v, want row s sonnet/medium; reasons %v", d.Row, d.Tier, d.Reasons)
	}
	if d.JevOff || d.Judgments == nil {
		t.Errorf("Jev answers missing: jev_off %v, judgments %v", d.JevOff, d.Judgments)
	}
	if recs := routeLogged(t); len(recs) != 0 {
		t.Errorf("qm route logged %d records; it is a preview and logs none", len(recs))
	}
}

func TestFakeJevMarked(t *testing.T) {
	routeHermetic(t)
	fixture, err := filepath.Abs("testdata/jev-worker-plain.json")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("QM_FAKE_JEV", fixture)
	repo := routeRepo(t)
	brief := routeFile(t, t.TempDir(), "brief.md", "Rename the helper function in one file.\n")
	prompt := routeFile(t, t.TempDir(), "prompt.md", "Check that the notes file reads well.\n")
	for name, args := range map[string][]string{
		"worker":        {"worker", "--brief", brief, "--type", "feature", "--size", "S", "--repo", repo, "--json"},
		"worker forced": {"worker", "--brief", brief, "--type", "feature", "--size", "S", "--repo", repo, "--force", "opus/high", "--why", "test", "--json"},
		"review":        {"review", "--prompt", prompt, "--base", "main", "--repo", repo, "--json"},
		"review round":  {"review", "--prompt", prompt, "--base", "main", "--repo", repo, "--round", "2", "--json"},
	} {
		d := routeJSON(t, args...)
		if !slices.Contains(d.Reasons, reasonFakeJev) {
			t.Errorf("%s: reasons %v lack %q", name, d.Reasons, reasonFakeJev)
		}
	}

	t.Setenv("QM_FAKE_JEV", "")
	d := routeJSON(t, "worker", "--brief", brief, "--type", "feature", "--size", "S", "--repo", repo, "--json")
	if slices.Contains(d.Reasons, reasonFakeJev) {
		t.Errorf("without QM_FAKE_JEV: reasons %v name fake answers", d.Reasons)
	}
}

func TestRouteWorkerNoKey(t *testing.T) {
	routeHermetic(t)
	brief := routeFile(t, t.TempDir(), "brief.md", "Fix a typo in the README.\n")
	var out, errOut bytes.Buffer
	code := run([]string{"route", "worker", "--brief", brief, "--type", "docs", "--size", "S", "--repo", t.TempDir(), "--json"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(out.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if raw["jev_off"] != true {
		t.Errorf("jev_off = %v, want true; output %s", raw["jev_off"], out.String())
	}
	if raw["row"] != "s-light" {
		t.Errorf("row = %v, want s-light", raw["row"])
	}
}

func TestRouteWorkerText(t *testing.T) {
	routeHermetic(t)
	brief := routeFile(t, t.TempDir(), "brief.md", "Fix a typo in the README.\n")
	var out, errOut bytes.Buffer
	if code := run([]string{"route", "worker", "--brief", brief, "--type", "docs", "--size", "S", "--repo", t.TempDir()}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	want := "sonnet/low (row s-light): " + route.ReasonJevOff + "; row s-light: type docs, size S\n"
	if out.String() != want {
		t.Errorf("got %q, want %q", out.String(), want)
	}
}

func TestRoundFromLog(t *testing.T) {
	routeHermetic(t)
	repo := routeRepo(t)
	top, branch, err := reviewin.Repo(repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []route.Record{
		{Kind: "review", Project: top, Branch: branch, Round: 1},
		{Kind: "review", Project: top, Branch: branch, Round: 2},
		{Kind: "review", Project: top, Branch: "other"},
		{Kind: "review", Project: "/elsewhere", Branch: branch},
		{Kind: "worker", Project: top, Branch: branch},
	} {
		r.Time = time.Now().UTC()
		if err := declog.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	prompt := routeFile(t, t.TempDir(), "prompt.md", "Check that the notes file reads well.\n")

	d := routeJSON(t, "review", "--prompt", prompt, "--base", "main", "--repo", repo, "--json")
	if d.Row != "fix-check" || !slices.Contains(d.Reasons, "row fix-check: round 3 re-checks named findings") {
		t.Errorf("row %q, reasons %v; want fix-check at round 3", d.Row, d.Reasons)
	}
	if recs := routeLogged(t); len(recs) != 5 {
		t.Errorf("%d records, want the 5 seeded: qm route logs none", len(recs))
	}
}

func TestRouteReviewFirstRound(t *testing.T) {
	routeHermetic(t)
	repo := routeRepo(t)
	prompt := routeFile(t, t.TempDir(), "prompt.md", "Check that the notes file reads well.\n")
	d := routeJSON(t, "review", "--prompt", prompt, "--base", "main", "--repo", repo, "--json")
	if d.Row != "small" || !d.JevOff {
		t.Errorf("row %q jev_off %v, want small and true; reasons %v", d.Row, d.JevOff, d.Reasons)
	}
	if !strings.Contains(strings.Join(d.Reasons, "|"), "diff 3 lines in 1 files") {
		t.Errorf("reasons %v do not give the diff size", d.Reasons)
	}
	if d.Codex != nil || !slices.Contains(d.Reasons, "Codex off (codex not installed): the review goes to Claude") {
		t.Errorf("codex %+v, reasons %v; want Codex off", d.Codex, d.Reasons)
	}
	if recs := routeLogged(t); len(recs) != 0 {
		t.Errorf("%d records logged, want none", len(recs))
	}
}

// TestRouteReviewDiff: qm route review, the preview of qm spawn review, sizes
// the --diff range and refuses what qm spawn review refuses.
func TestRouteReviewDiff(t *testing.T) {
	routeHermetic(t)
	repo := routeRepo(t)
	routeFile(t, repo, "notes.txt", "one\ntwo\nthree\nfour\n")
	routeGit(t, repo, "commit", "-q", "-am", "third")
	prompt := routeFile(t, t.TempDir(), "prompt.md", "Check that the notes file reads well.\n")

	d := routeJSON(t, "review", "--prompt", prompt, "--base", "main", "--diff", "HEAD~1..HEAD", "--repo", repo, "--json")
	if !strings.Contains(strings.Join(d.Reasons, "|"), "diff 1 lines in 1 files") {
		t.Errorf("reasons %v: want the size of the --diff range, not the branch's", d.Reasons)
	}
	for _, c := range []struct{ name, diff, want string }{
		{"empty", "HEAD..HEAD", "nothing to review"},
		{"not a range", "main", "want A..B or A...B"},
		{"unknown", "nosuch..HEAD", "not a commit"},
	} {
		var out, errOut bytes.Buffer
		if code := run([]string{"route", "review", "--prompt", prompt, "--base", "main", "--diff", c.diff, "--repo", repo}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), c.want) {
			t.Errorf("%s: exit %d, stderr %q; want 1 and %q", c.name, code, errOut.String(), c.want)
		}
	}
	if recs := routeLogged(t); len(recs) != 0 {
		t.Errorf("%d records logged, want none", len(recs))
	}
}

// TestRouteReviewTooLarge: the preview refuses a change over the 20 MB that
// qm spawn review refuses, rather than routing it.
func TestRouteReviewTooLarge(t *testing.T) {
	routeHermetic(t)
	repo := routeRepo(t)
	line := strings.Repeat("x", 99) + "\n"
	routeFile(t, repo, "big.txt", strings.Repeat(line, 21<<20/len(line)+1))
	routeGit(t, repo, "add", ".")
	routeGit(t, repo, "commit", "-q", "-m", "big")
	prompt := routeFile(t, t.TempDir(), "prompt.md", "Check the big file.\n")
	var out, errOut bytes.Buffer
	if code := run([]string{"route", "review", "--prompt", prompt, "--base", "main", "--repo", repo}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "larger than 20 MB") || !strings.Contains(errOut.String(), "--diff") {
		t.Errorf("exit %d, stderr %q; want 1 and the size refusal", code, errOut.String())
	}
	if out.Len() != 0 {
		t.Errorf("a decision was printed: %q", out.String())
	}
}

func TestForceNeedsWhy(t *testing.T) {
	routeHermetic(t)
	brief := routeFile(t, t.TempDir(), "brief.md", "Fix a typo in the README.\n")
	for _, args := range [][]string{
		{"--force", "opus/high"},
		{"--force", "opus/high", "--why", "  "},
		{"--why", "no force given"},
	} {
		var out, errOut bytes.Buffer
		full := append([]string{"route", "worker", "--brief", brief, "--type", "docs", "--size", "S", "--repo", t.TempDir()}, args...)
		if code := run(full, &out, &errOut); code != 2 {
			t.Errorf("%v: exit %d, want 2; stderr %q", args, code, errOut.String())
		}
	}
	if recs := routeLogged(t); len(recs) != 0 {
		t.Errorf("%d records logged, want none", len(recs))
	}

	var out, errOut bytes.Buffer
	full := []string{"route", "worker", "--brief", brief, "--type", "docs", "--size", "S", "--repo", t.TempDir(), "--force", "fable/high", "--why", "owner asked"}
	if code := run(full, &out, &errOut); code != 0 {
		t.Fatalf("forced: exit %d, stderr %q", code, errOut.String())
	}
	if !strings.HasPrefix(out.String(), "fable/high (row force): forced by the coordinator: owner asked") {
		t.Errorf("forced: got %q", out.String())
	}
	if recs := routeLogged(t); len(recs) != 0 {
		t.Errorf("%d records logged, want none", len(recs))
	}
}

func TestRouteBadInput(t *testing.T) {
	routeHermetic(t)
	brief := routeFile(t, t.TempDir(), "brief.md", "b\n")
	for _, args := range [][]string{
		{},
		{"nonsense"},
		{"worker", "--type", "docs", "--size", "S"},
		{"worker", "--brief", brief, "--type", "doc", "--size", "S"},
		{"worker", "--brief", brief, "--type", "docs", "--size", "XL"},
		{"worker", "--brief", brief, "--type", "docs", "--size", "S", "--hint", "opus"},
		{"worker", "--brief", brief, "--type", "docs", "--size", "S", "--hint", "gpt/high"},
		{"review"},
		{"review", "--prompt", brief, "--round", "-1"},
		{"worker", "--brief", brief, "--type", "docs", "--size", "S", "--repo", filepath.Join(brief, "missing")},
		{"worker", "--brief", brief, "--type", "docs", "--size", "S", "--repo", brief},
	} {
		var out, errOut bytes.Buffer
		if code := run(append([]string{"route"}, args...), &out, &errOut); code != 2 {
			t.Errorf("%v: exit %d, want 2; stderr %q", args, code, errOut.String())
		}
	}
	if recs := routeLogged(t); len(recs) != 0 {
		t.Errorf("%d records logged, want none", len(recs))
	}
}

func TestRouteReviewCodexOn(t *testing.T) {
	routeHermetic(t)
	stubCodex(t, "exit 1\n") // a failure to ask it changes nothing
	repo := routeRepo(t)
	prompt := routeFile(t, t.TempDir(), "prompt.md", "Check that the notes file reads well.\n")
	var out, errOut bytes.Buffer
	if code := run([]string{"route", "review", "--prompt", prompt, "--base", "main", "--repo", repo}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	want := "sonnet/high, Codex gpt-6.2-sol/medium (row small): " + route.ReasonJevOff + "; row small: diff 3 lines in 1 files\n"
	if out.String() != want {
		t.Errorf("got %q, want %q", out.String(), want)
	}
}

func TestDecisionLine(t *testing.T) {
	d := route.Decision{Tier: route.Tier{Model: "opus", Effort: "high"}, Row: "risk", Reasons: []string{"a", "b"}}
	if got := decisionLine(d); got != "opus/high (row risk): a; b" {
		t.Errorf("without Codex: %q", got)
	}
	d.Codex = &route.CodexTier{Model: "gpt-6.2-sol", Effort: "xhigh"}
	if got := decisionLine(d); got != "opus/high, Codex gpt-6.2-sol/xhigh (row risk): a; b" {
		t.Errorf("with Codex: %q", got)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }

func TestRouteStdoutFailure(t *testing.T) {
	routeHermetic(t)
	brief := routeFile(t, t.TempDir(), "brief.md", "Fix a typo in the README.\n")
	for _, extra := range [][]string{nil, {"--json"}} {
		var errOut bytes.Buffer
		args := append([]string{"route", "worker", "--brief", brief, "--type", "docs", "--size", "S", "--repo", t.TempDir()}, extra...)
		if code := run(args, failWriter{}, &errOut); code != 1 {
			t.Errorf("%v: exit %d, want 1; stderr %q", extra, code, errOut.String())
		}
	}
}
