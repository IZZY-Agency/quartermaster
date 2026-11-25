package spawn

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/IZZY-Agency/quartermaster/internal/route"
)

const testSession = "00000000-0000-4000-8000-000000000001"

// TestWorkerCopiesTheBrief: the worker is pointed to a copy of its own (0600,
// named by its session id, the brief's text as it was), and the text is in no
// script, Record or launch.
func TestWorkerCopiesTheBrief(t *testing.T) {
	hermetic(t)
	l := &fakeLauncher{}
	req := request(t, repoWith(t, "README.md"), "docs")
	const text = "Rename the helper in one file.\n"
	res, err := Worker(context.Background(), req, testDeps(l))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(os.Getenv("XDG_STATE_HOME"), "quartermaster", "launch", res.SessionID+".md"); res.BriefPath != want {
		t.Fatalf("brief copy %s, want %s", res.BriefPath, want)
	}
	if err := os.WriteFile(req.Brief, []byte("changed since"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(res.BriefPath)
	if err != nil || string(b) != text {
		t.Errorf("copy = %q, %v; want the brief", b, err)
	}
	if fi, err := os.Stat(res.BriefPath); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("copy mode %v, %v; want 0600", fi, err)
	}
	recs, err := json.Marshal(records(t))
	if err != nil {
		t.Fatal(err)
	}
	for what, s := range map[string]string{"script": res.Script, "records": string(recs)} {
		if strings.Contains(s, "Rename the helper") {
			t.Errorf("the brief's text is in the %s:\n%s", what, s)
		}
	}
	if strings.Contains(res.Script, req.Brief) || !strings.Contains(res.Script, " QM_BRIEF="+res.BriefPath+" ") {
		t.Errorf("script does not point to the copy alone:\n%s", res.Script)
	}
	if !strings.Contains(res.Script, `"permissions":{"allow":["Read(/`+res.BriefPath+`)"`) {
		t.Errorf("the worker may not read its brief without asking:\n%s", res.Script)
	}
}

func TestWorkerDryRunWritesNoBrief(t *testing.T) {
	hermetic(t)
	req := request(t, repoWith(t, "README.md"), "docs")
	req.DryRun = true
	res, err := Worker(context.Background(), req, testDeps(&fakeLauncher{}))
	if err != nil {
		t.Fatal(err)
	}
	if got := launchDirEntries(t); len(got) != 0 {
		t.Errorf("a dry run wrote %v", got)
	}
	if !strings.Contains(res.Script, "the file "+res.BriefPath+": ") {
		t.Errorf("the dry run does not show the prompt:\n%s", res.Script)
	}
}

func TestWorkerBadBrief(t *testing.T) {
	hermetic(t)
	repo := repoWith(t, "README.md")
	big := writeFile(t, filepath.Join(t.TempDir(), "big.md"), strings.Repeat("x", maxBrief+1))
	for name, path := range map[string]string{"too large": big, "a folder": t.TempDir(), "missing": filepath.Join(repo, "none.md")} {
		req := request(t, repo, "docs")
		req.Brief = path
		l := &fakeLauncher{}
		if _, err := Worker(context.Background(), req, testDeps(l)); !errors.Is(err, ErrInput) || len(l.specs) != 0 {
			t.Errorf("%s: err %v, launched %d; want ErrInput and none", name, err, len(l.specs))
		}
	}
}

// TestLargeBriefReadsAtOnce: a brief over Read's defaults raises the worker's
// output cap and names the limit, in a dry run too; a small one changes
// nothing.
func TestLargeBriefReadsAtOnce(t *testing.T) {
	hermetic(t)
	repo := repoWith(t, "README.md")
	req := request(t, repo, "docs")
	req.DryRun = true
	res, err := Worker(context.Background(), req, testDeps(&fakeLauncher{}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Script, readTokensEnv+"="+fileReadMaxTokens+" ") || strings.Contains(res.Script, "and limit") {
		t.Errorf("small brief:\n%s", res.Script)
	}
	req.Brief = writeFile(t, filepath.Join(t.TempDir(), "long.md"), strings.Repeat("a line of it\n", 3000))
	if res, err = Worker(context.Background(), req, testDeps(&fakeLauncher{})); err != nil {
		t.Fatal(err)
	}
	// Over the raised cap: read in parts that stay under it.
	if want := readTokensEnv + "=" + strconv.Itoa(defaultReadTokens) + " "; !strings.Contains(res.Script, want) || !strings.Contains(res.Script, "with Read, in parts of 2000 lines (offset and limit)") {
		t.Errorf("script lacks %q or the parts:\n%s", want, res.Script)
	}
	// Between the two caps: the worker's is raised, to what the brief needs.
	req.Brief = writeFile(t, filepath.Join(t.TempDir(), "mid.md"), strings.Repeat("a line of it\n", 1200))
	if res, err = Worker(context.Background(), req, testDeps(&fakeLauncher{})); err != nil {
		t.Fatal(err)
	}
	if want := readTokensEnv + "=" + strconv.Itoa(13*1200/2+4*1201) + " "; !strings.Contains(res.Script, want) || strings.Contains(res.Script, "in parts") {
		t.Errorf("script lacks %q, or reads in parts:\n%s", want, res.Script)
	}
	if got := res.Decision.Profile.Env[readTokensEnv]; got != fileReadMaxTokens {
		t.Errorf("the profile's own cap changed to %s", got)
	}
}

// TestReadPartsFitTheCap: no part costs more than a Read returns, whatever the
// lines are like, the parts are the whole file, and they are few.
func TestReadPartsFitTheCap(t *testing.T) {
	short, block := strings.Repeat("ten bytes\n", 1800), strings.Repeat(strings.Repeat("x", 999)+"\n", 200)
	for _, c := range []struct {
		name, text string
		most       int    // parts
		say        string // in the prompt
	}{
		{"one read", "a\nb\n", 0, ""},
		{"as many lines as a Read returns", strings.Repeat("a\n", readLines), 0, ""},
		{"a dense block at the end", short + block, 8, ", in 5 parts (offset and limit): 1 and 1817, 1818 and 49, 1867 and 49, 1916 and 49, 1965 and 36"},
		{"a dense block at the start", block + short, 8, ", in "},
		{"a dense block in the middle", short + block + short, 10, ", in "},
		{"equal lines", strings.Repeat("a line of it\n", 3000), 2, ", in parts of 2000 lines (offset and limit)"},
		{"a long line", short + strings.Repeat("x", maxLine-1) + "\n" + short, 5, ", in "},
		{"no newline at the end", strings.Repeat("x", maxLine-1) + "\n" + strings.Repeat("x", maxLine-1), 2, ", in parts of 1 lines (offset and limit)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := writeFile(t, filepath.Join(t.TempDir(), "b.md"), c.text)
			lines := strings.SplitAfter(c.text, "\n")
			if lines[len(lines)-1] == "" {
				lines = lines[:len(lines)-1]
			}
			parts := partsOf(lines, defaultReadTokens)
			next := 0
			for _, p := range parts {
				if p.from != next || p.lines < 1 || p.lines > readLines {
					t.Fatalf("parts %+v: part at %d, want at %d", parts, p.from, next)
				}
				next += p.lines
				if n := readTokens(len(strings.Join(lines[p.from:next], "")), p.lines); n > defaultReadTokens {
					t.Errorf("part %+v costs %d tokens", p, n)
				}
			}
			if next != len(lines) {
				t.Errorf("parts %+v end at line %d of %d", parts, next, len(lines))
			}
			got := readParts(path, defaultReadTokens)
			if c.say == "" && got != "" || !strings.HasPrefix(got, c.say) {
				t.Errorf("says %q, want %q", got, c.say)
			}
			if c.most > 0 && len(parts) > c.most {
				t.Errorf("%d parts, want at most %d", len(parts), c.most)
			}
		})
	}
}

// TestLongLine: a brief with a line no Read returns is refused at spawn; a
// note or summary qm up starts a session with, which nobody can be asked to
// break, is copied with that line broken, and loses nothing.
func TestLongLine(t *testing.T) {
	hermetic(t)
	long := "start\n" + strings.Repeat("é", maxLine) + "\nend\n"
	path := writeFile(t, filepath.Join(t.TempDir(), "long.md"), long)
	req := request(t, repoWith(t, "README.md"), "docs")
	req.Brief = path
	l := &fakeLauncher{}
	if _, err := Worker(context.Background(), req, testDeps(l)); !errors.Is(err, ErrInput) || len(l.specs) != 0 || !strings.Contains(err.Error(), "line 2 ") {
		t.Errorf("err %v, launched %d; want ErrInput naming line 2, and none", err, len(l.specs))
	}
	copied, err := CopyBrief(testSession, path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(copied)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ReplaceAll(string(b), "\n", "") != strings.ReplaceAll(long, "\n", "") || !utf8.Valid(b) {
		t.Error("the copy lost text")
	}
	for i, line := range strings.SplitAfter(string(b), "\n") {
		if len(line) > maxLine {
			t.Errorf("line %d of the copy has %d bytes", i+1, len(line))
		}
	}
	if from, to := readParts(path, defaultReadTokens), readParts(copied, defaultReadTokens); from != to || from == "" {
		t.Errorf("the prompt says %q, the copy is read %q", from, to)
	}
	if n := partsOf(strings.SplitAfter(string(b), "\n"), defaultReadTokens); len(n) > 5 {
		t.Errorf("%d parts", len(n))
	}
}

func TestBriefPathBadSession(t *testing.T) {
	hermetic(t)
	for _, id := range []string{"", "x", "../../00000000-0000-4000-8000-000000000001", "a/b", testSession + "/x"} {
		if p, err := BriefPath(id); err == nil {
			t.Errorf("BriefPath(%q) = %s, want an error", id, p)
		}
	}
}

// TestSweepKeepsBriefs: a brief copy outlives the launch scripts and is
// removed past BriefAge; no other file is.
func TestSweepKeepsBriefs(t *testing.T) {
	hermetic(t)
	ages := map[string]time.Duration{
		"00000000-0000-4000-8000-00000000000a": ScriptAge + time.Hour,
		"00000000-0000-4000-8000-00000000000b": BriefAge - time.Hour,
		"00000000-0000-4000-8000-00000000000c": BriefAge + time.Hour,
	}
	for id, age := range ages {
		path, err := WriteBrief(id, []byte("x"))
		if err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(-age)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	dir, err := launchDir()
	if err != nil {
		t.Fatal(err)
	}
	other := writeFile(t, filepath.Join(dir, "notes.md"), "x")
	at := time.Now().Add(-2 * BriefAge)
	if err := os.Chtimes(other, at, at); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteScript("task-a", "#!/bin/sh\n"); err != nil {
		t.Fatal(err)
	}
	for id, age := range ages {
		if kept, want := briefOf(id) != "", age < BriefAge; kept != want {
			t.Errorf("copy aged %v kept: %v, want %v", age, kept, want)
		}
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("another file was removed: %v", err)
	}
}

// TestResumeKeepsTheBrief: the resumed session of a worker that has only its
// prompt may still read the copy; with no copy, or a symlink in its place,
// the script names none.
func TestResumeKeepsTheBrief(t *testing.T) {
	hermetic(t)
	p, _ := profileFor(t, "sonnet", "low", nil)
	tier := route.Tier{Model: "sonnet", Effort: "low"}
	resume := func() string { return ResumeScript(p, tier, "task-a", testSession, "/w", "") }
	if s := resume(); strings.Contains(s, BriefEnv) || strings.Contains(s, "permissions") {
		t.Errorf("no copy, yet:\n%s", s)
	}
	path, err := WriteBrief(testSession, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if s := resume(); !strings.Contains(s, " QM_BRIEF="+path+" ") || !strings.Contains(s, "Read(/"+path+")") {
		t.Errorf("script lacks the copy %s:\n%s", path, s)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hosts", path); err != nil {
		t.Fatal(err)
	}
	if s := resume(); strings.Contains(s, BriefEnv) {
		t.Errorf("a symlink was taken for the copy:\n%s", s)
	}
}

// TestCoordinatorOwnSettings: a coordinator's own --settings is not replaced.
func TestCoordinatorOwnSettings(t *testing.T) {
	c, _ := coordinatorLaunch(t)
	c.PromptPath = "/state/up/summary.md"
	for _, args := range [][]string{{"--settings", "/cfg/s.json"}, {"--settings=/cfg/s.json"}} {
		c.Args = args
		if s, err := CoordinatorScript(c); err != nil || strings.Count(s, "--settings") != 1 || strings.Contains(s, "Read(") {
			t.Errorf("args %q: %v\n%s", args, err, s)
		}
	}
}

// TestWorkerReadRuleBehindASymlink: the first launch, whose copy is written
// after the script is built, allows the resolved path as well.
func TestWorkerReadRuleBehindASymlink(t *testing.T) {
	hermetic(t)
	real := t.TempDir()
	real, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", filepath.Join(link, "state"))
	for _, dry := range []bool{true, false} {
		req := request(t, repoWith(t, "README.md"), "docs")
		req.DryRun = dry
		res, err := Worker(context.Background(), req, testDeps(&fakeLauncher{}))
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(real, "state", "quartermaster", "launch", res.SessionID+".md")
		if !strings.Contains(res.Script, `"Read(/`+res.BriefPath+`)","Read(/`+target+`)"]`) {
			t.Errorf("dry %v: the script lacks a rule for %s:\n%s", dry, target, res.Script)
		}
	}
}

// TestCopyBrief: a file qm up wrote for a fresh session is kept as its brief,
// so a resume before the first read still finds it, the coordinator's too.
func TestCopyBrief(t *testing.T) {
	c, _ := coordinatorLaunch(t)
	note := writeFile(t, filepath.Join(t.TempDir(), "note.md"), "Left: the tests.\n")
	path, err := CopyBrief(c.SessionID, note)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "Left: the tests.\n" || briefOf(c.SessionID) != path {
		t.Errorf("copy %s = %q, %v", path, b, err)
	}
	c.Resume = true
	if s, err := CoordinatorScript(c); err != nil || !strings.Contains(s, "Read(/"+path+")") || !strings.HasSuffix(s, " --resume "+c.SessionID+"\n") {
		t.Errorf("resumed coordinator, %v:\n%s", err, s)
	}
	if _, err := CopyBrief(testSession, filepath.Join(t.TempDir(), "none")); err == nil {
		t.Error("a missing file was copied")
	}
}
