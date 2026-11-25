package limits

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeRollout writes a rollout of the given lines into CODEX_HOME's session
// tree. As Codex does, it opens with a session_meta line: one that names no
// account, unless the lines hold one.
func writeRollout(t *testing.T, codexHome, name string, mtime time.Time, lines ...string) {
	t.Helper()
	if !strings.Contains(strings.Join(lines, "\n"), `"session_meta"`) {
		lines = append([]string{`{"type":"session_meta","payload":{}}`}, lines...)
	}
	dir := filepath.Join(codexHome, "sessions", "2026", "11", "19")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// tokenCount is a token_count line written at at; rateLimits is the body of
// its rate_limits object.
func tokenCount(at time.Time, rateLimits string) string {
	return fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":null,"rate_limits":{%s}}}`,
		at.UTC().Format(time.RFC3339Nano), rateLimits)
}

// window is one rate-limit window of a rollout line; a zero reset is null.
func window(used float64, minutes int, reset time.Time) string {
	r := "null"
	if !reset.IsZero() {
		r = fmt.Sprint(reset.Unix())
	}
	return fmt.Sprintf(`{"used_percent":%g,"window_minutes":%d,"resets_at":%s}`, used, minutes, r)
}

func codexState(t *testing.T) CodexState {
	t.Helper()
	st, err := Codex(installed, CodexSessions)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// The highest used window decides, whichever position it has, and is named
// by its length.
func TestRolloutBothWindowsCount(t *testing.T) {
	now := time.Now()
	short, long := now.Add(time.Hour), now.Add(72*time.Hour)
	for _, tc := range []struct {
		name, limits, window string
		used                 float64
		reset                time.Time
		available            bool
	}{
		{"weekly in secondary is full", `"primary":` + window(20, 300, short) + `,"secondary":` + window(97, 10080, long), "weekly", 97, long, false},
		{"weekly in primary", `"primary":` + window(86, 10080, long) + `,"secondary":null`, "weekly", 86, long, true},
		{"5-hour is higher", `"primary":` + window(60, 300, short) + `,"secondary":` + window(30, 10080, long), "5-hour", 60, short, true},
		{"only secondary", `"primary":null,"secondary":` + window(50, 300, short), "5-hour", 50, short, true},
		{"reset window does not count", `"primary":` + window(100, 300, now.Add(-time.Minute)) + `,"secondary":` + window(30, 10080, long), "weekly", 30, long, true},
		{"full weekly behind a reset window", `"primary":` + window(100, 300, now.Add(-time.Minute)) + `,"secondary":` + window(96, 10080, long), "weekly", 96, long, false},
		{"another length", `"primary":` + window(10, 1440, long) + `,"secondary":null`, "daily", 10, long, true},
		{"odd length", `"primary":` + window(10, 90, short) + `,"secondary":null`, "90-minute", 10, short, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, _ := setup(t)
			writeRollout(t, home, "rollout-a.jsonl", now, tokenCount(now.Add(-time.Minute), tc.limits))
			st := codexState(t)
			if st.Available != tc.available || !st.UsageKnown || st.UsedPercent != tc.used || st.Window != tc.window || st.ResetsAt.Unix() != tc.reset.Unix() {
				t.Errorf("state = %+v, want %s at %g%% reset %v available %v", st, tc.window, tc.used, tc.reset, tc.available)
			}
			if st.Source != SourceSessions || st.Stale || st.ReadAt.Sub(now.Add(-time.Minute)).Abs() > time.Second {
				t.Errorf("source %q stale %v read at %v", st.Source, st.Stale, st.ReadAt)
			}
		})
	}
}

// A reading that names another bucket than codex is not Codex's limit; one
// that names none is from a Codex that had only the one.
func TestRolloutBucket(t *testing.T) {
	now := time.Now()
	reset := now.Add(time.Hour)
	codexLine := func(used float64, id string) string {
		if id != "" {
			id = `"limit_id":"` + id + `",`
		}
		return tokenCount(now, id+`"primary":`+window(used, 300, reset)+`,"secondary":null`)
	}
	t.Run("last line of another bucket", func(t *testing.T) {
		home, _ := setup(t)
		writeRollout(t, home, "rollout-a.jsonl", now, codexLine(40, "codex"), codexLine(99, "other"))
		if st := codexState(t); !st.Available || st.UsedPercent != 40 {
			t.Errorf("state = %+v, want the codex bucket's 40%%", st)
		}
	})
	t.Run("a whole file of another bucket", func(t *testing.T) {
		home, _ := setup(t)
		writeRollout(t, home, "rollout-a.jsonl", now.Add(-time.Hour), codexLine(30, ""))
		writeRollout(t, home, "rollout-b.jsonl", now, codexLine(99, "other"))
		if st := codexState(t); !st.Available || st.UsedPercent != 30 {
			t.Errorf("state = %+v, want the older file's 30%%", st)
		}
	})
	t.Run("null id counts", func(t *testing.T) {
		home, _ := setup(t)
		writeRollout(t, home, "rollout-a.jsonl", now, tokenCount(now, `"limit_id":null,"primary":`+window(96, 300, reset)))
		if st := codexState(t); st.Available {
			t.Errorf("state = %+v, want off", st)
		}
	})
}

// A reading past its reset, or too old, says so; a reset one is not a known
// usage.
func TestRolloutStale(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name                    string
		at, reset               time.Time
		used                    float64
		available, known, stale bool
	}{
		{"recent", now.Add(-59 * time.Minute), now.Add(time.Hour), 40, true, true, false},
		{"old", now.Add(-61 * time.Minute), now.Add(time.Hour), 40, true, true, true},
		{"old and full", now.Add(-61 * time.Minute), now.Add(time.Hour), 99, false, true, true},
		{"reset passed", now.Add(-10 * time.Minute), now.Add(-time.Minute), 100, true, false, true},
		{"no reset stated, window not over", now.Add(-10 * time.Minute), time.Time{}, 100, false, true, false},
		{"no reset stated, window over", now.Add(-301 * time.Minute), time.Time{}, 100, true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, _ := setup(t)
			writeRollout(t, home, "rollout-a.jsonl", now, tokenCount(tc.at, `"primary":`+window(tc.used, 300, tc.reset)))
			st := codexState(t)
			if st.Available != tc.available || st.UsageKnown != tc.known || st.Stale != tc.stale {
				t.Errorf("state = %+v, want available %v known %v stale %v", st, tc.available, tc.known, tc.stale)
			}
		})
	}
}

// Out-of-range numbers read as unknown: they must not fail qm status --json.
func TestRolloutBadNumbers(t *testing.T) {
	home, _ := setup(t)
	now := time.Now()
	writeRollout(t, home, "rollout-a.jsonl", now, tokenCount(now, `"primary":{"used_percent":40,"window_minutes":99999999999999999,"resets_at":99999999999999999}`))
	st := codexState(t)
	if !st.Available || st.UsedPercent != 40 || !st.ResetsAt.IsZero() || st.Window != "" {
		t.Errorf("state = %+v", st)
	}
	// The last second of 9999, in UTC, is a year later further east.
	local := time.Local
	time.Local = time.FixedZone("east", 14*60*60)
	t.Cleanup(func() { time.Local = local })
	for _, reset := range []int64{253402300799, maxReset - 1} {
		writeRollout(t, home, "rollout-a.jsonl", now, tokenCount(now, `"primary":`+window(40, 10080, time.Unix(reset, 0))))
		if b, err := json.Marshal(codexState(t)); err != nil {
			t.Errorf("reset %d: %v %s", reset, err, b)
		}
	}
	// Without the line's time, or with one in the future, the file's counts.
	writeRollout(t, home, "rollout-a.jsonl", now.Add(-2*time.Hour), tokenCount(now.Add(time.Hour), `"primary":`+window(40, 10080, now.Add(time.Hour))))
	if st := codexState(t); st.ReadAt.Sub(now.Add(-2*time.Hour)).Abs() > time.Second || !st.Stale {
		t.Errorf("future line: state = %+v", st)
	}
}

// A usage-limit error with no reset time is held to the reset of the window
// that is full, or else to the soonest one.
func TestCodexLimitedWithoutResetTimeTwoWindows(t *testing.T) {
	now := time.Now()
	short, long := now.Add(time.Hour), now.Add(72*time.Hour)
	for _, tc := range []struct {
		name          string
		primary, week float64
		want          time.Time
	}{
		{"weekly full", 20, 99, long},
		{"5-hour full", 99, 40, short},
		{"neither full", 50, 60, short},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, _ := setup(t)
			writeRollout(t, home, "rollout-a.jsonl", now, tokenCount(now, `"primary":`+window(tc.primary, 300, short)+`,"secondary":`+window(tc.week, 10080, long)))
			if err := markNow(t, time.Time{}); err != nil {
				t.Fatal(err)
			}
			if st := codexState(t); st.Available || st.ResetsAt.Unix() != tc.want.Unix() {
				t.Errorf("state = %+v, want off until %v", st, tc.want)
			}
		})
	}
}

// A line longer than maxRolloutLine is skipped, not held: the readings
// around it still count, one inside it does not.
func TestRolloutLongLineSkipped(t *testing.T) {
	now := time.Now()
	path := filepath.Join(t.TempDir(), "rollout-long.jsonl")
	meta := `{"type":"session_meta","payload":{"creator_account_id":"acct-1"}}`
	small := tokenCount(now, `"primary":`+window(10, 300, now.Add(time.Hour)))
	long := tokenCount(now, `"primary":`+window(99, 300, now.Add(time.Hour))+`,"pad":"`+strings.Repeat("x", maxRolloutLine)+`"`)
	if err := os.WriteFile(path, []byte(strings.Join([]string{meta, small, long}, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, account, err := lastReading(path, now)
	if err != nil || r == nil || account != "acct-1" || len(r.Windows) != 1 || r.Windows[0].UsedPercent != 10 {
		t.Errorf("lastReading = %+v, %q, %v, want the 10%% reading of acct-1", r, account, err)
	}
}

// The first line is the session_meta, which names the account. One too long
// to read leaves the account untold, so the rollout gives no reading: it must
// not pass as one that names no account.
func TestRolloutLongFirstLineGivesNoReading(t *testing.T) {
	now := time.Now()
	path := filepath.Join(t.TempDir(), "rollout-long-meta.jsonl")
	meta := `{"type":"session_meta","payload":{"creator_account_id":"other","pad":"` + strings.Repeat("x", maxRolloutLine) + `"}}`
	small := tokenCount(now, `"primary":`+window(10, 300, now.Add(time.Hour)))
	if err := os.WriteFile(path, []byte(meta+"\n"+small+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r, account, err := lastReading(path, now); err != nil || r != nil || account != "" {
		t.Errorf("lastReading = %+v, %q, %v, want no reading", r, account, err)
	}
}

// A rollout whose session_meta was not read does not say whose it is: no
// reading, so it cannot pass as one that names no account.
func TestRolloutWithoutAReadMetaGivesNoReading(t *testing.T) {
	now := time.Now()
	count := tokenCount(now, `"primary":`+window(20, 300, now.Add(time.Hour)))
	for name, tc := range map[string]struct {
		lines []string
		want  bool
	}{
		"torn first line":                           {[]string{`{"type":"session_meta","payload":{"creator_account_id":"other"`, count}, false},
		"no session_meta":                           {[]string{`{"type":"event_msg"}`, count}, false},
		"over-long session_meta, line 2":            {[]string{`{"type":"event_msg"}`, `{"type":"session_meta","payload":{"x":"` + strings.Repeat("x", maxRolloutLine) + `"}}`, count}, false},
		"over-long session_meta, then a copied one": {[]string{`{"type":"session_meta","payload":{"x":"` + strings.Repeat("x", maxRolloutLine) + `"}}`, `{"type":"session_meta","payload":{}}`, count}, false},
		"torn session_meta, then a copied one":      {[]string{`{"type":"session_meta","payload":{"creator_account_id":"other"`, `{"type":"session_meta","payload":{"creator_account_id":"mine"}}`, count}, false},
		"session_meta naming no account":            {[]string{`{"type":"session_meta","payload":{}}`, count}, true},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rollout.jsonl")
			if err := os.WriteFile(path, []byte(strings.Join(tc.lines, "\n")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			got, account, err := lastReading(path, now)
			if (got != nil) != tc.want || account != "" || err != nil {
				t.Errorf("lastReading = %+v, %q, %v, want a reading: %v", got, account, err, tc.want)
			}
		})
	}
}
