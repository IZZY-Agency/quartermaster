package limits

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/policy"
)

// installed is a LookPath stand-in that finds codex.
func installed(name string) (string, error) { return "/usr/local/bin/" + name, nil }

// missing is a LookPath stand-in that finds nothing.
func missing(name string) (string, error) { return "", errors.New("not found: " + name) }

// setup points CODEX_HOME and XDG_STATE_HOME at fresh temporary folders.
func setup(t *testing.T) (codexHome, stateDir string) {
	t.Helper()
	codexHome, state := t.TempDir(), t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("XDG_STATE_HOME", state)
	return codexHome, filepath.Join(state, "quartermaster")
}

// addRollout copies a testdata rollout into CODEX_HOME's session tree, with
// RESET replaced by reset (Unix seconds), and sets its mtime.
func addRollout(t *testing.T, codexHome, fixture, name string, reset time.Time, mtime time.Time) {
	t.Helper()
	addRolloutOn(t, codexHome, "2026/11/19", fixture, name, reset, mtime)
}

// addRolloutOn is addRollout into the sessions folder of day (YYYY/MM/DD).
func addRolloutOn(t *testing.T, codexHome, day, fixture, name string, reset time.Time, mtime time.Time) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.ReplaceAll(string(data), "RESET", strconv.FormatInt(reset.Unix(), 10)))
	dir := filepath.Join(codexHome, "sessions", filepath.FromSlash(day))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestNoCodexOnPath(t *testing.T) {
	setup(t)
	st, err := Codex(missing)
	if err != nil {
		t.Fatal(err)
	}
	if st.Available || st.Reason != "codex not installed" {
		t.Errorf("state = %+v", st)
	}
}

func TestCodexUnderLimit(t *testing.T) {
	home, _ := setup(t)
	reset := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	addRollout(t, home, "rollout-under.jsonl", "rollout-b.jsonl", reset, time.Now())
	st, err := Codex(installed)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Available || st.UsedPercent != 42.5 || !st.ResetsAt.Equal(reset) {
		t.Errorf("state = %+v", st)
	}
}

// The last token_count event with a primary limit counts; a later event with
// primary null does not hide it.
func TestCreditsDoNotCount(t *testing.T) {
	home, _ := setup(t)
	reset := time.Now().Add(48 * time.Hour)
	addRollout(t, home, "rollout-limited.jsonl", "rollout-a.jsonl", reset, time.Now())
	st, err := Codex(installed)
	if err != nil {
		t.Fatal(err)
	}
	if st.Available || st.UsedPercent != 100 {
		t.Errorf("state = %+v, want unavailable at 100%%", st)
	}
	if !strings.Contains(st.Reason, "100%") {
		t.Errorf("reason = %q", st.Reason)
	}
}

func TestNewestRolloutWins(t *testing.T) {
	home, _ := setup(t)
	reset := time.Now().Add(48 * time.Hour)
	now := time.Now()
	addRollout(t, home, "rollout-limited.jsonl", "rollout-a.jsonl", reset, now.Add(-time.Hour))
	addRollout(t, home, "rollout-under.jsonl", "rollout-b.jsonl", reset, now)
	st, err := Codex(installed)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Available || st.UsedPercent != 42.5 {
		t.Errorf("state = %+v, want the newer rollout's 42.5%%", st)
	}
}

// Day folders are read newest name first, whatever the mtimes, and a newer
// rollout without rate-limit data gives way to an older one with it.
func TestRolloutWalkOrder(t *testing.T) {
	home, _ := setup(t)
	reset := time.Now().Add(48 * time.Hour)
	now := time.Now()
	addRolloutOn(t, home, "2026/09/30", "rollout-limited.jsonl", "rollout-a.jsonl", reset, now)
	addRolloutOn(t, home, "2026/10/01", "rollout-under.jsonl", "rollout-b.jsonl", reset, now.Add(-time.Hour))
	addRolloutOn(t, home, "2026/10/02", "rollout-no-usage.jsonl", "rollout-c.jsonl", reset, now.Add(-time.Hour))
	st, err := Codex(installed)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Available || st.UsedPercent != 42.5 || !st.UsageKnown {
		t.Errorf("state = %+v, want 2026/10/01's 42.5%%", st)
	}
}

// A limit whose window has reset since the last event no longer holds:
// without this, Codex would stay off forever once reviews stop going to it.
func TestCodexWindowResetSinceLastEvent(t *testing.T) {
	home, _ := setup(t)
	addRollout(t, home, "rollout-limited.jsonl", "rollout-a.jsonl", time.Now().Add(-time.Minute), time.Now())
	st, err := Codex(installed)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Available {
		t.Errorf("state = %+v, want available after the window reset", st)
	}
}

func TestCodexNoUsageData(t *testing.T) {
	home, _ := setup(t)
	st, err := Codex(installed)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Available || st.Reason != "no usage data yet (read from your own Codex sessions; qm reviews save none)" {
		t.Errorf("no rollouts: state = %+v", st)
	}
	addRollout(t, home, "rollout-no-usage.jsonl", "rollout-c.jsonl", time.Now(), time.Now())
	st, err = Codex(installed)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Available || st.Reason != "no usage data yet (read from your own Codex sessions; qm reviews save none)" {
		t.Errorf("no token_count: state = %+v", st)
	}
}

func TestCodexLimitedFile(t *testing.T) {
	home, state := setup(t)
	addRollout(t, home, "rollout-under.jsonl", "rollout-b.jsonl", time.Now().Add(48*time.Hour), time.Now())

	future := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	if err := MarkCodexLimited(future); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(state, "limits", "codex-limited.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", info.Mode().Perm())
	}
	st, err := Codex(installed)
	if err != nil {
		t.Fatal(err)
	}
	if st.Available || !st.ResetsAt.Equal(future) || !strings.Contains(st.Reason, "usage limit") {
		t.Errorf("future reset: state = %+v", st)
	}
	if st.UsedPercent != 42.5 || !st.UsageKnown {
		t.Errorf("future reset: state = %+v, want the last known 42.5%%", st)
	}

	if err := MarkCodexLimited(time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	st, err = Codex(installed)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Available {
		t.Errorf("past reset: state = %+v", st)
	}
}

func TestCodexLimitedUsageUnknown(t *testing.T) {
	setup(t)
	if err := MarkCodexLimited(time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	st, err := Codex(installed)
	if err != nil {
		t.Fatal(err)
	}
	if st.Available || st.UsageKnown || !strings.Contains(st.Reason, "usage unknown") {
		t.Errorf("state = %+v, want limited with usage unknown", st)
	}
}

// A corrupt codex-limited.json keeps Codex off for the fallback time from
// its mtime, then lets it back on; it never fails the call.
func TestCodexLimitedFileCorrupt(t *testing.T) {
	_, state := setup(t)
	path := filepath.Join(state, "limits", "codex-limited.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{torn"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Codex(installed)
	if err != nil {
		t.Fatal(err)
	}
	if st.Available || !strings.Contains(st.Reason, "unreadable") {
		t.Errorf("fresh corrupt file: state = %+v", st)
	}
	old := time.Now().Add(-policy.CodexLimitedFallback - time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	st, err = Codex(installed)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Available {
		t.Errorf("old corrupt file: state = %+v, want available", st)
	}
}

func TestParseCodexLimitError(t *testing.T) {
	msg := "ERROR: You\u2019ve hit your usage limit. Visit https://chatgpt.com/settings/usage to purchase more credits or try again at Nov 23rd, 2026 9:02 AM."
	got, ok := ParseCodexLimitError(msg)
	want := time.Date(2026, 11, 23, 9, 2, 0, 0, time.Local)
	if !ok || !got.Equal(want) {
		t.Errorf("got %v %v, want %v", got, ok, want)
	}

	got, ok = ParseCodexLimitError("You've hit your usage limit. Try again at Dec 1st, 2026 11:30 PM.")
	if want := time.Date(2026, 12, 1, 23, 30, 0, 0, time.Local); !ok || !got.Equal(want) {
		t.Errorf("straight apostrophe: got %v %v", got, ok)
	}

	// A limit message without a reset time is still a limit error.
	got, ok = ParseCodexLimitError("You've hit your usage limit. Upgrade to Plus to continue using Codex.")
	if !ok || !got.IsZero() {
		t.Errorf("no reset time: got %v %v, want zero time and true", got, ok)
	}

	for _, s := range []string{
		"",
		"error: connection reset",
		"try again at Nov 23rd, 2026 9:02 AM", // no limit message
	} {
		if _, ok := ParseCodexLimitError(s); ok {
			t.Errorf("%q parsed as a limit error", s)
		}
	}
}

// A limit error without a reset time marks Codex limited until the rollout
// window's reset, or for policy.CodexLimitedFallback when that is unknown.
func TestCodexLimitedWithoutResetTime(t *testing.T) {
	home, _ := setup(t)
	before := time.Now()
	if err := MarkCodexLimited(time.Time{}); err != nil {
		t.Fatal(err)
	}
	st, err := Codex(installed)
	if err != nil {
		t.Fatal(err)
	}
	if lo, hi := before.Add(policy.CodexLimitedFallback), time.Now().Add(policy.CodexLimitedFallback); st.Available ||
		st.ResetsAt.Before(lo.Truncate(time.Second)) || st.ResetsAt.After(hi) {
		t.Errorf("no rollout: state = %+v, want limited for %v", st, policy.CodexLimitedFallback)
	}

	reset := time.Now().Add(30 * time.Hour).Truncate(time.Second)
	addRollout(t, home, "rollout-under.jsonl", "rollout-b.jsonl", reset, time.Now())
	if err := MarkCodexLimited(time.Time{}); err != nil {
		t.Fatal(err)
	}
	st, err = Codex(installed)
	if err != nil {
		t.Fatal(err)
	}
	if st.Available || !st.ResetsAt.Equal(reset) {
		t.Errorf("with rollout: state = %+v, want limited until %v", st, reset)
	}

	// A window reset already past is unknown too.
	addRollout(t, home, "rollout-under.jsonl", "rollout-b.jsonl", time.Now().Add(-time.Hour), time.Now())
	if err := MarkCodexLimited(time.Time{}); err != nil {
		t.Fatal(err)
	}
	if st, err = Codex(installed); err != nil || st.Available || st.ResetsAt.Before(time.Now().Add(policy.CodexLimitedFallback-time.Minute)) {
		t.Errorf("past window reset: state = %+v, err %v", st, err)
	}
}

func writeClaude(t *testing.T, f ClaudeFile) {
	t.Helper()
	state := os.Getenv("XDG_STATE_HOME")
	if err := WriteClaudeFile(filepath.Join(state, "quartermaster", "limits", "claude.json"), f); err != nil {
		t.Fatal(err)
	}
}

func ptr[T any](v T) *T { return &v }

func TestClaudeMissingFile(t *testing.T) {
	setup(t)
	st, err := Claude()
	if err != nil {
		t.Fatal(err)
	}
	if st.Known {
		t.Errorf("state = %+v", st)
	}
}

func TestClaudeFresh(t *testing.T) {
	setup(t)
	now := time.Now().Truncate(time.Second)
	writeClaude(t, ClaudeFile{
		FiveHourUsed: ptr(91.5), SevenDayUsed: ptr(30.0),
		FiveHourResetsAt: ptr(now.Add(2 * time.Hour)), UpdatedAt: now.Add(-10 * time.Minute),
	})
	st, err := Claude()
	if err != nil {
		t.Fatal(err)
	}
	if !st.Known || st.FiveHourUsed != 91.5 || st.SevenDayUsed != 30 || !st.UpdatedAt.Equal(now.Add(-10*time.Minute)) {
		t.Errorf("state = %+v", st)
	}
}

func TestClaudeStale(t *testing.T) {
	setup(t)
	now := time.Now()
	cases := map[string]ClaudeFile{
		"over an hour old":      {FiveHourUsed: ptr(95.0), UpdatedAt: now.Add(-61 * time.Minute)},
		"updated in the future": {FiveHourUsed: ptr(95.0), UpdatedAt: now.Add(10 * time.Minute)},
	}
	for name, f := range cases {
		writeClaude(t, f)
		st, err := Claude()
		if err != nil {
			t.Fatal(err)
		}
		if st.Known {
			t.Errorf("%s: state = %+v, want Known false", name, st)
		}
	}
}

func TestClaudeIdleWindow(t *testing.T) {
	setup(t)
	now := time.Now()
	cases := map[string]ClaudeFile{
		"no 5-hour usage":     {SevenDayUsed: ptr(12.0), UpdatedAt: now},
		"5-hour window reset": {FiveHourUsed: ptr(95.0), SevenDayUsed: ptr(12.0), FiveHourResetsAt: ptr(now.Add(-time.Minute)), UpdatedAt: now},
	}
	for name, f := range cases {
		writeClaude(t, f)
		st, err := Claude()
		if err != nil {
			t.Fatal(err)
		}
		if !st.Known || st.FiveHourUsed != 0 || st.SevenDayUsed != 12 {
			t.Errorf("%s: state = %+v, want Known, 5-hour 0, 7-day 12", name, st)
		}
	}
}

func TestClaudeCorruptFile(t *testing.T) {
	_, state := setup(t)
	path := filepath.Join(state, "limits", "claude.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st, err := Claude(); err == nil || st.Known {
		t.Errorf("state = %+v, err = %v", st, err)
	}
}

// login writes a Codex auth.json naming account into CODEX_HOME.
func login(t *testing.T, codexHome, account string) {
	t.Helper()
	data := `{"auth_mode":"chatgpt","tokens":{"account_id":"` + account + `"}}`
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// addAccountRollout is addRollout with a session_meta line naming account.
func addAccountRollout(t *testing.T, codexHome, fixture, name, account string, reset, mtime time.Time) {
	t.Helper()
	addRollout(t, codexHome, fixture, name, reset, mtime)
	path := filepath.Join(codexHome, "sessions", "2026", "11", "19", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	meta := `{"timestamp":"2026-11-19T09:59:59.000Z","type":"session_meta","payload":{"id":"s0","creator_account_id":"` + account + `"}}` + "\n"
	if err := os.WriteFile(path, append([]byte(meta), data...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// A limit reading of another account says nothing about the account Codex is
// logged in to now.
func TestCodexOtherAccountReadingIgnored(t *testing.T) {
	home, _ := setup(t)
	reset := time.Now().Add(48 * time.Hour)
	addAccountRollout(t, home, "rollout-limited.jsonl", "rollout-a.jsonl", "acct-old", reset, time.Now())
	login(t, home, "acct-new")
	st, err := Codex(installed)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Available || st.UsageKnown || st.Reason != "no usage data yet (read from your own Codex sessions; qm reviews save none)" {
		t.Errorf("state = %+v, want available with no usage data", st)
	}

	// The same account's reading still counts.
	login(t, home, "acct-old")
	st, err = Codex(installed)
	if err != nil {
		t.Fatal(err)
	}
	if st.Available || !st.UsageKnown {
		t.Errorf("same account: state = %+v, want limited", st)
	}
}

// With two accounts in the history, the current one's newest reading is used,
// also when the other account's is newer.
func TestCodexCurrentAccountReadingWins(t *testing.T) {
	home, _ := setup(t)
	reset := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	now := time.Now()
	addAccountRollout(t, home, "rollout-under.jsonl", "rollout-mine.jsonl", "acct-new", reset, now.Add(-time.Hour))
	addAccountRollout(t, home, "rollout-limited.jsonl", "rollout-other.jsonl", "acct-old", reset, now)
	login(t, home, "acct-new")
	st, err := Codex(installed)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Available || st.UsedPercent != 42.5 {
		t.Errorf("state = %+v, want the current account's 42.5%%", st)
	}
}

// A rollout or a login that names no account counts as before.
func TestCodexUnknownAccountCounts(t *testing.T) {
	home, _ := setup(t)
	reset := time.Now().Add(48 * time.Hour)
	addRollout(t, home, "rollout-limited.jsonl", "rollout-a.jsonl", reset, time.Now())
	login(t, home, "acct-new")
	if st, err := Codex(installed); err != nil || st.Available {
		t.Errorf("rollout without an account: state = %+v, %v; want limited", st, err)
	}
}

// A recorded usage-limit error holds for the account that hit it only.
func TestCodexLimitedFileOtherAccount(t *testing.T) {
	home, state := setup(t)
	login(t, home, "acct-old")
	if err := MarkCodexLimited(time.Now().Add(3 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(state, "limits", "codex-limited.json"))
	if err != nil || !strings.Contains(string(data), `"account":"acct-old"`) {
		t.Fatalf("marker = %s, %v", data, err)
	}
	if st, err := Codex(installed); err != nil || st.Available {
		t.Errorf("same account: state = %+v, %v; want limited", st, err)
	}
	login(t, home, "acct-new")
	if st, err := Codex(installed); err != nil || !st.Available {
		t.Errorf("other account: state = %+v, %v; want available", st, err)
	}
}
