package up

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/spawn"
	"github.com/IZZY-Agency/quartermaster/internal/transcript"
)

func summaryInput() SummaryInput {
	mk := func(name, branch string, ctx int, ago time.Duration) Candidate {
		return Candidate{
			Role: RoleWorker, SessionID: "id-" + name, Name: name, Branch: branch,
			Last: transcript.Last{Context: ctx, At: now.Add(-ago)},
		}
	}
	return SummaryInput{
		Project: Project{Workspace: "demo"},
		Old: Candidate{
			Role: RoleCoordinator, Name: "coord", Transcript: "/claude/projects/demo/coord.jsonl",
			Handoff: "/state/handoff/coord.md", Last: transcript.Last{Context: 231999, At: now.Add(-time.Hour)},
		},
		Threshold: 200000,
		Steps: []Step{
			{Candidate: Candidate{Role: RoleCoordinator, Name: "coord"}, Action: Fresh, Why: "context 232k > 200k: summary"},
			{Candidate: mk("zeta", "feature-z", 92400, 12*time.Minute), Action: Resume, Why: "cache warm (last activity 12 min ago)"},
			{Candidate: mk("alpha", "feature-a", 310000, 3*time.Hour), Action: Skip, Why: "finished: merged"},
			{Candidate: mk("mid", "feature-m", 151000, 2*time.Hour), Action: Fresh, Why: "context 151k, handoff note"},
		},
		HandoffNote:   "Finish the importer; tests for the parser are still open.",
		OwnerMessages: []string{"dropped: too old", "first kept", "second kept\nover two lines", "third kept"},
		Git: GitFacts{
			Branch: "main", Dirty: 2, DirtyKnown: true,
			Worktrees: []string{"/work/demo", "/work/demo/.claude/worktrees/feature-z"},
			OpenPRs:   []string{"#12 Add the parser (feature-z)", "#13 Other (feature-q)"},
		},
		Now: now,
	}
}

func TestSummaryGolden(t *testing.T) {
	got := Summary(summaryInput())
	want, err := os.ReadFile("testdata/summary-golden.md")
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("summary differs from testdata/summary-golden.md:\n%s", got)
	}
}

func TestSummaryNoNoteNoGit(t *testing.T) {
	in := summaryInput()
	in.Old.Handoff, in.HandoffNote, in.Git, in.OwnerMessages, in.Steps = "", "", GitFacts{}, nil, nil
	got := Summary(in)
	for _, want := range []string{
		"## Workers\nnone\n",
		"## Previous coordinator's handoff note\nnone\n",
		"## The owner's last messages\nnone\n",
		"## Git\nnone\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "- branch") || strings.Contains(got, "worktrees") {
		t.Errorf("git rows without facts:\n%s", got)
	}
}

func TestSummaryStatesAndEdges(t *testing.T) {
	in := summaryInput()
	in.Steps = []Step{
		{Candidate: Candidate{Role: RoleWorker, Name: "a|b", Branch: "feature-q"}, Action: Skip, Why: WhyNotTrusted + ": run claude in /w once"},
		{Candidate: Candidate{Role: RoleWorker, Name: "gone", Branch: "no-pr"}, Action: Skip, Why: "transcript missing"},
	}
	got := Summary(in)
	for _, want := range []string{
		"| a\\|b | not trusted | feature-q | #13 | unknown | unknown | not started: not trusted: run claude in /w once |",
		"| gone | lost | no-pr | - | unknown | unknown | not started: transcript missing |",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary lacks %q:\n%s", want, got)
		}
	}
}

func TestSummaryUnknownFacts(t *testing.T) {
	in := summaryInput()
	in.Old.Last = transcript.Last{}
	in.Git.DirtyKnown = false
	got := Summary(in)
	if !strings.Contains(got, "context was unknown (threshold 200k)") || !strings.Contains(got, "- uncommitted files: unknown\n") {
		t.Errorf("unknown facts not shown:\n%s", got)
	}
}

func TestSummaryCutsMessages(t *testing.T) {
	in := summaryInput()
	in.OwnerMessages = []string{strings.Repeat("é", 2500)}
	got := Summary(in)
	if !strings.Contains(got, "> "+strings.Repeat("é", 2000)+"\n") || strings.Contains(got, strings.Repeat("é", 2001)) {
		t.Error("message not cut to 2000 characters")
	}
}

func TestReadGitFakeRunner(t *testing.T) {
	f := &fakeDir{out: map[string]string{
		"git rev-parse --abbrev-ref HEAD":                         "feature\n",
		"git status --porcelain":                                  " M a.go\n?? b.go\n",
		"git worktree list --porcelain":                           "worktree /r\nHEAD abc\nbranch refs/heads/feature\n\nworktree /r/wt\nHEAD def\n",
		"gh pr list --author @me --json number,title,headRefName": `[{"number":7,"title":"Fix  it","headRefName":"feature"}]`,
	}}
	g := ReadGit("/r", f.run)
	if g.Branch != "feature" || g.Dirty != 2 || !g.DirtyKnown || len(g.Worktrees) != 2 || g.Worktrees[1] != "/r/wt" ||
		len(g.OpenPRs) != 1 || g.OpenPRs[0] != "#7 Fix it (feature)" {
		t.Errorf("ReadGit = %+v", g)
	}
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "/r: ") {
			t.Errorf("call %q not run in the root", c)
		}
	}
	// Every command failing leaves every field empty.
	failing := func(context.Context, string, string, ...string) ([]byte, error) { return nil, errors.New("no") }
	if g := ReadGit("/r", failing); g.Branch != "" || g.Dirty != 0 || g.Worktrees != nil || g.OpenPRs != nil {
		t.Errorf("failed ReadGit = %+v", g)
	}
	// A failing git status is unknown, not zero.
	noStatus := &fakeDir{out: map[string]string{"git rev-parse --abbrev-ref HEAD": "feature\n"}}
	if g := ReadGit("/r", noStatus.run); g.DirtyKnown || g.Branch != "feature" {
		t.Errorf("without status: %+v", g)
	}
	// Only gh failing keeps the git facts.
	delete(f.out, "gh pr list --author @me --json number,title,headRefName")
	if g := ReadGit("/r", f.run); g.Branch != "feature" || g.OpenPRs != nil {
		t.Errorf("without gh: %+v", g)
	}
}

func TestWriteSummaryModeAndSweep(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	first, err := WriteSummary("demo-1234abcd", "one")
	if err == nil {
		if ents, _ := os.ReadDir(filepath.Dir(first)); len(ents) != 1 {
			t.Errorf("folder holds %d entries after one write, want 1 (no temp file)", len(ents))
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(first)
	old := filepath.Join(dir, "summary-1.md")
	keep := filepath.Join(dir, "notes.txt")
	for _, p := range []string{old, keep} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		past := time.Now().Add(-spawn.ScriptAge - time.Hour)
		if err := os.Chtimes(p, past, past); err != nil {
			t.Fatal(err)
		}
	}
	second, err := WriteSummary("demo-1234abcd", "two")
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Error("second summary overwrote the first")
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("old summary not swept: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("a file that is not a summary was swept: %v", err)
	}
	if b, _ := os.ReadFile(first); string(b) != "one" {
		t.Errorf("first summary = %q", b)
	}
	fi, err := os.Stat(second)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("summary mode = %v, %v", fi, err)
	}
	if di, _ := os.Stat(dir); di.Mode().Perm() != 0o700 {
		t.Errorf("folder mode = %v", di.Mode().Perm())
	}
	if !strings.HasPrefix(filepath.Base(second), "summary-") || !strings.HasSuffix(second, ".md") {
		t.Errorf("name = %s", second)
	}
	if _, err := WriteSummary("../x", "bad"); err == nil {
		t.Error("a project id with a slash was accepted")
	}
}
