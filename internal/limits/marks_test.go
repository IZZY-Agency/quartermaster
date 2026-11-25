package limits

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/policy"
)

func markFiles(t *testing.T, state string) (names []string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(state, "limits"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if _, ok := markLogin(e.Name()); ok {
			names = append(names, e.Name())
		}
	}
	return names
}

// putOldMark writes the single mark of an earlier qm.
func putOldMark(t *testing.T, state, data string) string {
	t.Helper()
	path := filepath.Join(state, "limits", oldMark)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func oldMarkJSON(reset time.Time, account string) string {
	return `{"resets_at":"` + reset.Format(time.RFC3339Nano) + `","account":"` + account + `"}`
}

// wantLimited checks until when Codex is limited (zero: it is not).
func wantLimited(t *testing.T, who string, until time.Time) {
	t.Helper()
	st := codexState(t)
	switch {
	case until.IsZero() && (!st.Available || st.Limited):
		t.Errorf("%s: %+v, want available", who, st)
	case !until.IsZero() && (st.Available || !st.ResetsAt.Equal(until)):
		t.Errorf("%s: %+v, want limited until %v", who, st, until)
	}
}

// A usage-limit error holds for the login that hit it only, whichever login
// Codex has by then, and there is a mark for each.
func TestMarkPerAccount(t *testing.T) {
	home, state := setup(t)
	login(t, home, "acct-b")
	inA, inB := time.Now().Add(3*time.Hour).Truncate(time.Second), time.Now().Add(5*time.Hour).Truncate(time.Second)
	if err := MarkCodexLimited(newLogin("acct-a", ""), inA); err != nil {
		t.Fatal(err)
	}
	wantLimited(t, "b", time.Time{})
	if err := markNow(t, inB); err != nil {
		t.Fatal(err)
	}
	wantLimited(t, "b marked", inB)
	login(t, home, "acct-a")
	wantLimited(t, "a", inA)
	login(t, home, "acct-c")
	wantLimited(t, "c", time.Time{})

	names := markFiles(t, state)
	if len(names) != 2 {
		t.Errorf("marks %v, want two", names)
	}
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(state, "limits", n))
		if err != nil || strings.Contains(n+string(b), "acct-") {
			t.Errorf("%s = %s, %v; want no account id", n, b, err)
		}
		if info, err := os.Stat(filepath.Join(state, "limits", n)); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v, %v", n, info, err)
		}
	}

	// A login that cannot be told: any mark may be its own.
	if err := os.Remove(filepath.Join(home, "auth.json")); err != nil {
		t.Fatal(err)
	}
	wantLimited(t, "unknown", inB)
	// And its own counts for every login.
	in9 := time.Now().Add(9 * time.Hour).Truncate(time.Second)
	if err := markNow(t, in9); err != nil {
		t.Fatal(err)
	}
	login(t, home, "acct-c")
	wantLimited(t, "c after an unknown login's limit", in9)
}

func TestMarkPerMember(t *testing.T) {
	home, _ := setup(t)
	reset := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	one := loginMember(t, home, "acct-1", "user-1")
	if err := MarkCodexLimited(one, reset); err != nil {
		t.Fatal(err)
	}
	wantLimited(t, "the member", reset)
	loginMember(t, home, "acct-1", "user-2")
	wantLimited(t, "another member", time.Time{})
	login(t, home, "acct-1")
	wantLimited(t, "a member that cannot be told", reset)
	loginMember(t, home, "acct-2", "user-1")
	wantLimited(t, "the user in another account", time.Time{})
}

// The single mark of an earlier qm is honoured, next to the new ones.
func TestOldMark(t *testing.T) {
	home, state := setup(t)
	old, later := time.Now().Add(2*time.Hour).Truncate(time.Second), time.Now().Add(4*time.Hour).Truncate(time.Second)
	path := putOldMark(t, state, oldMarkJSON(old, "acct-a"))
	login(t, home, "acct-a")
	wantLimited(t, "a", old)
	loginMember(t, home, "acct-a", "user-1")
	wantLimited(t, "a member of a", old)
	login(t, home, "acct-b")
	wantLimited(t, "b", time.Time{})
	if err := markNow(t, later); err != nil {
		t.Fatal(err)
	}
	wantLimited(t, "b marked", later)
	login(t, home, "acct-a")
	wantLimited(t, "a still", old)

	putOldMark(t, state, oldMarkJSON(old, ""))
	login(t, home, "acct-c")
	wantLimited(t, "c, by one that names no account", old)

	putOldMark(t, state, oldMarkJSON(time.Now().Add(-time.Second), "acct-a"))
	wantLimited(t, "c, past", time.Time{})
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the old mark is still there: %v", err)
	}
}

// Marks whose reset has passed are removed, also another login's; live ones
// are not.
func TestMarksRemoved(t *testing.T) {
	home, state := setup(t)
	login(t, home, "acct-a")
	live := newLogin("acct-b", "")
	for l, reset := range map[Login]time.Time{
		newLogin("acct-a", ""): time.Now().Add(-time.Hour), newLogin("acct-c", "user-1"): time.Now().Add(-time.Hour),
		{}: time.Now().Add(-time.Hour), live: time.Now().Add(time.Hour),
	} {
		if err := MarkCodexLimited(l, reset); err != nil {
			t.Fatal(err)
		}
	}
	wantLimited(t, "a", time.Time{})
	if got := markFiles(t, state); len(got) != 1 || got[0] != markName(live) {
		t.Errorf("marks %v, want only %s", got, markName(live))
	}
}

// A mark renewed between the look at it and its removal stays.
func TestPruneRereads(t *testing.T) {
	_, state := setup(t)
	l := newLogin("acct-a", "")
	if err := MarkCodexLimited(l, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(state, "limits", markName(l))
	pruneMarks([]string{path}, time.Now())
	if _, err := os.Stat(path); err != nil {
		t.Errorf("a live mark was removed: %v", err)
	}
	if err := MarkCodexLimited(l, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	unlock, fail := lockState(marksLock, 0)
	if fail != "" {
		t.Fatal(fail)
	}
	pruneMarks([]string{path}, time.Now())
	if _, err := os.Stat(path); err != nil {
		t.Errorf("removed while a writer holds the lock: %v", err)
	}
	unlock()
	pruneMarks([]string{path}, time.Now())
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("still there: %v", err)
	}
}

// A mark that cannot be read keeps only its own login off, for the fallback
// time from its mtime, and the reason names no account.
func TestMarkCorrupt(t *testing.T) {
	home, state := setup(t)
	l := newLogin("acct-a", "")
	if err := MarkCodexLimited(l, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(state, "limits", markName(l))
	if err := os.WriteFile(path, []byte("{torn"), 0o600); err != nil {
		t.Fatal(err)
	}
	login(t, home, "acct-b")
	wantLimited(t, "b", time.Time{})
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	wantLimited(t, "b, that of a cannot be opened", time.Time{})
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	login(t, home, "acct-a")
	if os.Geteuid() != 0 { // root opens it all the same
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := Codex(installed, CodexSessions); err == nil || !errors.Is(err, fs.ErrPermission) || strings.Contains(err.Error(), l.account) {
			t.Errorf("a's own cannot be opened: %v", err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Dir(path)
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		err := MarkCodexLimited(l, time.Now().Add(time.Hour))
		if err == nil || strings.Contains(err.Error(), l.account) {
			t.Errorf("a mark that cannot be written: %v", err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if st := codexState(t); st.Available || !strings.Contains(st.Reason, "unreadable") || strings.Contains(st.Reason, l.account) {
		t.Errorf("state = %+v", st)
	}
	old := time.Now().Add(-policy.CodexLimitedFallback - time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	wantLimited(t, "a, later", time.Time{})
	if got := markFiles(t, state); len(got) != 0 {
		t.Errorf("marks %v", got)
	}
}

// Without a reset time the window is read from the rollouts of the login that
// hit the limit, not of the one Codex has by then.
func TestMarkFallbackOfThatAccount(t *testing.T) {
	home, _ := setup(t)
	a, b := time.Now().Add(30*time.Hour).Truncate(time.Second), time.Now().Add(60*time.Hour).Truncate(time.Second)
	addAccountRollout(t, home, "rollout-limited.jsonl", "rollout-a.jsonl", "acct-a", a, time.Now().Add(-time.Hour))
	addAccountRollout(t, home, "rollout-limited.jsonl", "rollout-b.jsonl", "acct-b", b, time.Now())
	login(t, home, "acct-b")
	if err := MarkCodexLimited(newLogin("acct-a", ""), time.Time{}); err != nil {
		t.Fatal(err)
	}
	login(t, home, "acct-a")
	wantLimited(t, "a", a)
}
