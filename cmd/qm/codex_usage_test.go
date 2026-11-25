package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/limits"
)

// TestMain makes codex missing for every test that does not say otherwise, so
// none starts the real one, whatever PATH it runs with.
func TestMain(m *testing.M) {
	codexLook = func(string) (string, error) { return "", exec.ErrNotFound }
	os.Exit(m.Run())
}

// stubCodex puts a codex command with this shell body in a folder, makes
// codexLook find it, and returns the folder. It logs its starts there.
func stubCodex(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "codex")
	script := "#!/bin/sh\necho \"$*\" >>'" + dir + "/starts'\n" + body
	if err := os.WriteFile(p, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	codexLook = func(string) (string, error) { return p, nil }
	return dir
}

func codexStarts(dir string) string {
	b, _ := os.ReadFile(filepath.Join(dir, "starts"))
	return string(b)
}

// answerUsage is a stub body that answers the usage request: the weekly
// window in primary, used percent used.
func answerUsage(used int, reset time.Time) string {
	return fmt.Sprintf(`read -r a; echo '{"id":0,"result":{}}'; read -r a; read -r a
echo '{"id":1,"result":{"rateLimits":{"limitId":"codex","primary":{"usedPercent":%d,"windowDurationMins":10080,"resetsAt":%d},"secondary":null}}}'
cat >/dev/null
`, used, reset.Unix())
}

func usageReadOff(t *testing.T) {
	t.Helper()
	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "quartermaster")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	routeFile(t, dir, "config.json", `{"codex":{"usage_read":false}}`)
}

// A review is routed with what Codex says now: route review and spawn review.
func TestReviewAsksCodex(t *testing.T) {
	reset := time.Now().Add(72 * time.Hour)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"route", []string{"route", "review"}},
		{"spawn", []string{"spawn", "review"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			routeHermetic(t)
			oldStart := startReview
			startReview = func(string, string) (*os.Process, error) { return nil, nil }
			t.Cleanup(func() { startReview = oldStart })
			dir := stubCodex(t, answerUsage(97, reset))
			repo := routeRepo(t)
			prompt := routeFile(t, t.TempDir(), "prompt.md", "Check that the notes file reads well.\n")
			var out, errOut bytes.Buffer
			if code := run(append(tc.args, "--prompt", prompt, "--base", "main", "--repo", repo), &out, &errOut); code != 0 {
				t.Fatalf("exit %d, stderr %q", code, errOut.String())
			}
			wantIn(t, out.String(), "Codex off (Codex limit at 97% (weekly)): the review goes to Claude")
			if got := codexStarts(dir); got != "app-server\n" {
				t.Errorf("starts %q", got)
			}

			// The config key turns it off: nothing is started, and the stored
			// answer is not used.
			usageReadOff(t)
			out.Reset()
			if code := run(append(tc.args, "--prompt", prompt, "--base", "main", "--repo", repo), &out, &errOut); code != 0 {
				t.Fatalf("exit %d, stderr %q", code, errOut.String())
			}
			wantIn(t, out.String(), "Codex gpt-6.2-sol/medium")
			if got := codexStarts(dir); got != "app-server\n" {
				t.Errorf("with usage_read off: starts %q", got)
			}
		})
	}
}

// A review whose login is switched while it runs is tried again under the new
// one; its runner asks Codex for that one's usage, unless it must not.
func TestReviewRunnerAsksAfterSwitch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		off    bool
		starts string
	}{{"asked", false, "app-server exec app-server exec"}, {"off", true, "exec exec"}} {
		t.Run(tc.name, func(t *testing.T) {
			args := spawnHermetic(t, `{"verdict":"mergeable","findings":[]}`)
			home := os.Getenv("CODEX_HOME")
			if err := os.MkdirAll(home, 0o700); err != nil {
				t.Fatal(err)
			}
			routeFile(t, home, "auth.json", `{"tokens":{"account_id":"acct-1"}}`)
			if tc.off {
				usageReadOff(t)
			}
			dir := stubCodex(t, `if [ "$1" = app-server ]; then
`+answerUsage(10, time.Now().Add(time.Hour))+`exit 0; fi
cat >/dev/null
if [ ! -f '`+home+`/done' ]; then
echo '{"tokens":{"account_id":"acct-2"}}' >'`+home+`/auth.json'; : >'`+home+`/done'; exit 1
fi
while [ "$1" != "-o" ]; do shift; done; printf '%s' '`+oneP1+`' >"$2"
`)
			prompt := routeFile(t, t.TempDir(), "prompt.md", "Check that the notes file reads well.\n")
			code, out, stderr := spawnRun(t, "--prompt", prompt, "--base", "main", "--repo", routeRepo(t), "--wait")
			if code != 0 {
				t.Fatalf("exit %d, stderr %q", code, stderr)
			}
			wantIn(t, out, "not-mergeable")
			if _, err := os.Stat(args); err == nil {
				t.Error("the review went to Claude")
			}
			var got []string
			for line := range strings.Lines(codexStarts(dir)) {
				got = append(got, strings.Fields(line)[0])
			}
			if strings.Join(got, " ") != tc.starts {
				t.Errorf("starts %q, want %q", got, tc.starts)
			}
		})
	}
}

// No worker waits for Codex, a review session included.
func TestWorkerDoesNotAskCodex(t *testing.T) {
	routeHermetic(t)
	t.Setenv("QM_WORKER_PLUGIN", t.TempDir())
	dir := stubCodex(t, answerUsage(10, time.Now().Add(time.Hour)))
	repo := routeRepo(t)
	routeGit(t, repo, "update-ref", "refs/remotes/origin/main", "main")
	brief := routeFile(t, t.TempDir(), "brief.md", "Check that the notes file reads well.\n")
	for _, typ := range []string{"docs", "review"} {
		for _, args := range [][]string{
			{"route", "worker", "--brief", brief, "--type", typ, "--size", "S", "--repo", repo},
			{"spawn", "worker", "--brief", brief, "--type", typ, "--size", "S", "--hint", "sonnet/low", "--name", "w", "--cwd", repo, "--dry-run"},
		} {
			var out, errOut bytes.Buffer
			if code := run(args, &out, &errOut); code != 0 {
				t.Fatalf("%v: exit %d, stderr %q", args, code, errOut.String())
			}
		}
	}
	if got := codexStarts(dir); got != "" {
		t.Errorf("starts %q, want none", got)
	}
}

// loginUntold is the note of a login qm cannot tell.
const loginUntold = "the login cannot be told, so a figure or a limit may be another account's"

func TestStatusAsksCodex(t *testing.T) {
	home := statusHermetic(t)
	t.Setenv("PATH", filepath.Join(home, "bin")+":/bin:/usr/bin")
	reset := time.Now().Add(72 * time.Hour).Truncate(time.Second)
	put := func(body string) {
		t.Helper()
		p := filepath.Join(home, "bin", "codex")
		if err := os.WriteFile(p, []byte("#!/bin/sh\necho x >>'"+home+"/starts'\n"+body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	put(answerUsage(86, reset))
	line := "Codex: available, 86% of the weekly limit used, resets " + reset.Local().Format(time.DateTime) + " (codex app-server, under a minute ago); " + loginUntold + "\n"
	wantIn(t, statusOut(t), line)
	wantIn(t, statusOut(t), line)
	if b, _ := os.ReadFile(filepath.Join(home, "starts")); string(b) != "x\n" {
		t.Errorf("starts %q, want one", b)
	}
	wantIn(t, statusOut(t, "--json"), `"window": "weekly"`, `"source": "codex app-server"`)

	usageReadOff(t)
	out := statusOut(t)
	wantIn(t, out, "Codex: available, usage unknown (no usage data yet")
	if strings.Contains(out, "86%") {
		t.Errorf("the stored answer is shown with usage_read off:\n%s", out)
	}
}

func TestStatusCodexFailure(t *testing.T) {
	home := statusHermetic(t)
	p := filepath.Join(home, "bin", "codex")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	wantIn(t, statusOut(t), "Codex: available, usage unknown (no usage data yet (read from your own Codex sessions; qm reviews save none)); no newer figure: codex app-server ended without an answer; "+loginUntold+"\n")

	// A login of which nothing was read, after another's figure.
	home = statusHermetic(t)
	if err := os.WriteFile(filepath.Join(home, "bin", "codex"), []byte("#!/bin/sh\n"+answerUsage(86, time.Now().Add(time.Hour))), 0o700); err != nil {
		t.Fatal(err)
	}
	codexLogin := func(account string) {
		t.Helper()
		dir := os.Getenv("CODEX_HOME")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		routeFile(t, dir, "auth.json", `{"tokens":{"account_id":"`+account+`"}}`)
	}
	codexLogin("acct-1")
	wantIn(t, statusOut(t), "86% of the weekly limit", "under a minute ago)\n")
	if err := os.WriteFile(filepath.Join(home, "bin", "codex"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	codexLogin("acct-2")
	out := statusOut(t)
	wantIn(t, out, "ended without an answer; the last figure read is another account's and is not used\n")
	if strings.Contains(out, "acct-") || strings.Contains(out, "86%") {
		t.Errorf("status names an account, or its figure:\n%s", out)
	}
}

func TestCodexLine(t *testing.T) {
	now := time.Now()
	reset := now.Add(time.Hour)
	at := reset.Local().Format(time.DateTime)
	for _, tc := range []struct {
		st   limits.CodexState
		want string
	}{
		{limits.CodexState{Reason: "codex not installed"}, "off: codex not installed"},
		{limits.CodexState{Available: true, UsageKnown: true, UsedPercent: 42.5, Window: "5-hour", ResetsAt: reset, Source: limits.SourceSessions, ReadAt: now.Add(-185 * time.Minute), Stale: true, ReadFailure: "F"},
			"available, 42.5% of the 5-hour limit used, resets " + at + " (session files, 3h5m ago, stale); no newer figure: F"},
		{limits.CodexState{Available: true, UsageKnown: true, UsedPercent: 4, Source: limits.SourceSessions},
			"available, 4% used (session files, at an unknown time)"},
		{limits.CodexState{Available: true, UsedPercent: 100, Window: "weekly", ResetsAt: now.Add(-time.Hour), Source: limits.SourceSessions, ReadAt: now.Add(-2 * time.Hour), Stale: true, Reason: "reset since"},
			"available, usage unknown (reset since: 100% of the weekly limit, session files, 2h ago, stale)"},
		{limits.CodexState{Available: true, Source: limits.SourceAppServer, ReadAt: now, Reason: "Codex states no usage limit"},
			"available, usage unknown (Codex states no usage limit, codex app-server, under a minute ago)"},
		{limits.CodexState{UsageKnown: true, UsedPercent: 96, Window: "weekly", ResetsAt: reset, Source: limits.SourceAppServer, ReadAt: now, Reason: "Codex limit at 96% (weekly)"},
			"off: Codex limit at 96% (weekly); 96% of the weekly limit used, resets " + at + " (codex app-server, under a minute ago)"},
		{limits.CodexState{UsageKnown: true, UsedPercent: 3, Window: "5-hour", ResetsAt: reset, Limited: true, Source: limits.SourceAppServer, ReadAt: now, Reason: "Codex usage limit reached; resets X"},
			"off: Codex usage limit reached; resets X; 3% of the 5-hour limit used (codex app-server, under a minute ago)"},
		{limits.CodexState{Limited: true, Reason: "Codex usage limit reached; resets X (usage unknown)", ReadFailure: "F"},
			"off: Codex usage limit reached; resets X (usage unknown); no newer figure: F"},
		{limits.CodexState{Limited: true, Reason: "Codex usage limit reached; resets X (usage unknown)", AccountNote: "N"},
			"off: Codex usage limit reached; resets X (usage unknown); N"},
	} {
		if got := codexLine(tc.st, now); got != tc.want {
			t.Errorf("got  %q\nwant %q", got, tc.want)
		}
	}
}
