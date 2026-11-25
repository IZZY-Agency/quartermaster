package limits

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// markNow is MarkCodexLimited for the login Codex has now.
func markNow(t *testing.T, resetsAt time.Time) error {
	t.Helper()
	home, err := codexHome()
	if err != nil {
		t.Fatal(err)
	}
	return MarkCodexLimited(readLogin(home), resetsAt)
}

// idToken is an unsigned token with these claims of the auth namespace.
func idToken(claims string) string {
	payload := `{"email":"nobody","https://api.openai.com/auth":` + claims + `}`
	return "e30." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".x"
}

// authJSON writes auth.json whole.
func authJSON(t *testing.T, home, data string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// loginMember logs Codex in to an account as one of its members.
func loginMember(t *testing.T, home, account, user string) Login {
	t.Helper()
	authJSON(t, home, fmt.Sprintf(`{"tokens":{"account_id":%q,"id_token":%q}}`, account, idToken(`{"chatgpt_user_id":"`+user+`"}`)))
	return readLogin(home)
}

func TestReadLogin(t *testing.T) {
	home, _ := setup(t)
	if l := readLogin(home); l.Known() {
		t.Errorf("no auth.json: %+v", l)
	}
	token := func(tok string) string { return `{"tokens":{"account_id":"acct-1","id_token":"` + tok + `"}}` }
	for _, tc := range []struct {
		name, auth string
		want       Login
	}{
		{"an API key", `{"OPENAI_API_KEY":"none","tokens":null}`, Login{}},
		{"not JSON", `{"tokens":{"acc`, Login{}},
		{"another shape", `{"tokens":"x"}`, Login{}},
		{"no id", `{"tokens":{"id_token":"` + idToken(`{"user_id":"user-1"}`) + `"}}`, Login{}},
		{"an account", `{"tokens":{"account_id":"acct-1"}}`, newLogin("acct-1", "")},
		{"a member", token(idToken(`{"chatgpt_user_id":"user-1","user_id":"user-2"}`)), newLogin("acct-1", "user-1")},
		{"a member by the other claim", token(idToken(`{"user_id":"user-2"}`)), newLogin("acct-1", "user-2")},
		{"no user in the token", token(idToken(`{}`)), newLogin("acct-1", "")},
		{"another claim shape", token(idToken(`[1]`)), newLogin("acct-1", "")},
		{"a token of two parts", token("e30.e30"), newLogin("acct-1", "")},
		{"a token that is not base64", token("a.!.c"), newLogin("acct-1", "")},
		{"a token that is no string", `{"tokens":{"account_id":"acct-1","id_token":7}}`, newLogin("acct-1", "")},
	} {
		authJSON(t, home, tc.auth)
		if got := readLogin(home); got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
	l := newLogin("acct-1", "user-1")
	if s := fmt.Sprintf("%+v", l); strings.Contains(s, "acct-1") || strings.Contains(s, "user-1") {
		t.Errorf("a login holds an id: %s", s)
	}
	if l.account != hashAccount("acct-1") || newLogin("acct-2", "user-1").member == l.member {
		t.Error("the member's hash must be of the account too")
	}
}

// A file being copied over is read once more.
func TestReadLoginHalfWritten(t *testing.T) {
	home, _ := setup(t)
	authJSON(t, home, "")
	go func() {
		time.Sleep(10 * time.Millisecond)
		_ = os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"tokens":{"account_id":"acct-1"}}`), 0o600)
	}()
	if l := readLogin(home); l != newLogin("acct-1", "") {
		t.Errorf("login = %+v", l)
	}
}

func TestLoginDiffers(t *testing.T) {
	a, a1, a2, b1 := newLogin("a", ""), newLogin("a", "1"), newLogin("a", "2"), newLogin("b", "1")
	for _, tc := range []struct {
		x, y Login
		want bool
	}{
		{a, a, false}, {a, a1, false}, {a1, a1, false}, {a1, a2, true}, {a, b1, true}, {a1, b1, true},
		{Login{}, a, false}, {Login{}, Login{}, false},
	} {
		if tc.x.Differs(tc.y) != tc.want || tc.y.Differs(tc.x) != tc.want {
			t.Errorf("%+v, %+v: want %v", tc.x, tc.y, tc.want)
		}
	}
}

// Where auth.json is missing Codex may be asked who it is logged in as. The
// answer is asked for now, not one stored before.
func TestCodexLoginKeyring(t *testing.T) {
	home, state := setup(t)
	week := srvWindow(40, weekMinutes, time.Now().Add(time.Hour))
	s := newStub(t, serve(usage(`"accountId":"acct-1",`, week, "null")))
	path := filepath.Join(s.dir, "codex")
	if l := CodexLogin(path, CodexCached); l.Known() || s.starts() != 0 {
		t.Errorf("not asked: %+v, %d starts", l, s.starts())
	}
	// Nor is a failure of before: it says nothing about now.
	putUsageFile(t, usageFile{FailedAt: time.Now().Add(-time.Second), Failure: failTimeout})
	a := CodexLogin(path, CodexAsk)
	if a.account != hashAccount("acct-1") || a.member != "" {
		t.Errorf("login = %+v", a)
	}
	if b, _ := os.ReadFile(filepath.Join(state, "limits", "codex-usage.json")); strings.Contains(string(b), "acct-1") {
		t.Errorf("the id is stored: %s", b)
	}
	// Its answer is the usage of that login too, and of "whoever it is".
	if st, err := CodexFor(a, path, CodexAsk); err != nil || st.UsedPercent != 40 || st.AccountNote != "" || s.starts() != 1 {
		t.Errorf("state = %+v, %v, %d starts", st, err, s.starts())
	}
	if st := s.codex(t, CodexAsk); st.UsedPercent != 40 || st.AccountNote != "" || s.starts() != 1 {
		t.Errorf("state = %+v, %d starts", st, s.starts())
	}
	// A mark of the account counts while Codex names that one only.
	if err := MarkCodexLimited(a, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if st := s.codex(t, CodexAsk); st.Available || !st.Limited {
		t.Errorf("marked: %+v", st)
	}
	s.write(t, serve(usage(`"accountId":"acct-2",`, week, "null")))
	b := CodexLogin(path, CodexAsk)
	if !b.Differs(a) {
		t.Errorf("login = %+v after a switch from %+v", b, a)
	}
	if st := s.codex(t, CodexAsk); !st.Available {
		t.Errorf("the other account: %+v", st)
	}
	// Codex that says nothing leaves the login untold.
	s.write(t, "exit 1\n")
	if l := CodexLogin(path, CodexAsk); l.Known() {
		t.Errorf("login = %+v", l)
	}
	// With an auth.json nothing is asked.
	n := s.starts()
	authJSON(t, home, `{"OPENAI_API_KEY":"none"}`)
	if l := CodexLogin(path, CodexAsk); l.Known() || s.starts() != n {
		t.Errorf("an API key: %+v, %d starts, want %d", l, s.starts(), n)
	}
}

// An auth.json that appears while Codex is asked names the login: the one a
// run started next has.
func TestCodexLoginMadeMeanwhile(t *testing.T) {
	home, _ := setup(t)
	write := `printf '%s' '{"tokens":{"account_id":"acct-2"}}' >'` + home + `/auth.json'` + "\n"
	body := serve(usage(`"accountId":"acct-2",`, srvWindow(40, weekMinutes, time.Now().Add(time.Hour)), "null"))
	s := newStub(t, write+body)
	if l := CodexLogin(filepath.Join(s.dir, "codex"), CodexAsk); l != newLogin("acct-2", "") {
		t.Errorf("login = %+v", l)
	}
}

// An answer whose question another qm asked earlier says nothing about now:
// the login may have been another one then.
func TestCodexLoginNotAnEarlierQuestions(t *testing.T) {
	_, state := setup(t)
	answer := func(account string) string {
		return serve(usage(`"accountId":"`+account+`",`, srvWindow(40, weekMinutes, time.Now().Add(time.Hour)), "null"))
	}
	s := newStub(t, `: >"$dir/started"; sleep 0.5`+"\n"+answer("acct-1"))
	path := filepath.Join(s.dir, "codex")
	first := make(chan Login)
	go func() { first <- CodexLogin(path, CodexAsk) }()
	for range 500 {
		if _, err := os.Stat(filepath.Join(s.dir, "started")); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	s2 := newStub(t, answer("acct-2"))
	if l := CodexLogin(filepath.Join(s2.dir, "codex"), CodexAsk); l.account != hashAccount("acct-2") {
		t.Errorf("login = %+v, want the one named after it was asked for", l)
	}
	if l := <-first; l.account != hashAccount("acct-1") {
		t.Errorf("first = %+v", l)
	}
	if f := readUsageFile(t, state); f.Named != hashAccount("acct-2") {
		t.Errorf("stored %+v", f)
	}
}

// Several runners that ask at once, behind a question that is already under
// way, all get an answer: they wait for that one and share the next.
func TestCodexLoginSeveralAtOnce(t *testing.T) {
	setup(t)
	s := newStub(t, `: >"$dir/started"; sleep 2.3`+"\n"+
		serve(usage(`"accountId":"acct-1",`, srvWindow(40, weekMinutes, time.Now().Add(time.Hour)), "null")))
	path := filepath.Join(s.dir, "codex")
	logins := make(chan Login)
	ask := func() { logins <- CodexLogin(path, CodexAsk) }
	go ask()
	for range 500 {
		if _, err := os.Stat(filepath.Join(s.dir, "started")); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	go ask()
	go ask()
	for range 3 {
		if l := <-logins; l.account != hashAccount("acct-1") {
			t.Errorf("login = %+v", l)
		}
	}
	if s.starts() != 2 {
		t.Errorf("%d starts, want two", s.starts())
	}
}

// Current is whether a login is still the one of the moment.
func TestLoginCurrent(t *testing.T) {
	home, _ := setup(t)
	keyring := Login{account: hashAccount("acct-1"), asked: true}
	if !keyring.Current() || newLogin("acct-1", "").Current() {
		t.Error("no auth.json: a keyring's login holds, auth.json's does not")
	}
	login(t, home, "acct-1")
	if keyring.Current() || !newLogin("acct-1", "").Current() || newLogin("acct-2", "").Current() {
		t.Error("auth.json: only the login it names holds")
	}
	authJSON(t, home, `{"OPENAI_API_KEY":"none"}`)
	if keyring.Current() || newLogin("acct-1", "").Current() || (Login{}).Current() {
		t.Error("an API key: no login holds")
	}
}

// The login is the caller's: when auth.json names another by the time Codex
// would be asked, nothing is started or stored.
func TestCodexForSwitchedLogin(t *testing.T) {
	home, state := setup(t)
	s := newStub(t, serve(usage("", srvWindow(99, weekMinutes, time.Now().Add(time.Hour)), "null")))
	login(t, home, "acct-2")
	st, err := CodexFor(newLogin("acct-1", ""), filepath.Join(s.dir, "codex"), CodexAsk)
	if err != nil || !st.Available || st.UsageKnown || st.ReadFailure != failAccount.text() || s.starts() != 0 {
		t.Errorf("state = %+v, %v, %d starts", st, err, s.starts())
	}
	if _, err := os.Stat(filepath.Join(state, "limits", "codex-usage.json")); err == nil {
		t.Error("a usage file was written")
	}
}

// An answer for another account than the login's, or auth.json's, is none.
func TestAnswerOfAnotherAccount(t *testing.T) {
	setup(t)
	s := newStub(t, serve(usage(`"accountId":"acct-2",`, srvWindow(99, weekMinutes, time.Now().Add(time.Hour)), "null")))
	check := func(name string, l Login) {
		t.Helper()
		st, err := CodexFor(l, filepath.Join(s.dir, "codex"), CodexAsk)
		if err != nil || !st.Available || st.UsageKnown || st.ReadFailure != failAccount.text() {
			t.Errorf("%s: %+v, %v", name, st, err)
		}
	}
	check("a keyring's login", newLogin("acct-1", ""))
	home, _ := setup(t)
	login(t, home, "acct-1")
	check("a login made since", Login{})
}

// Two members of one workspace have the same account id and their own usage.
func TestUsageOfMember(t *testing.T) {
	home, _ := setup(t)
	reset := time.Now().Add(time.Hour)
	s := newStub(t, serve(usage(`"accountId":"acct-1",`, srvWindow(99, weekMinutes, reset), "null")))
	loginMember(t, home, "acct-1", "user-1")
	if st := s.codex(t, CodexAsk); st.Available || st.UsedPercent != 99 {
		t.Errorf("first: %+v", st)
	}
	loginMember(t, home, "acct-1", "user-2")
	if st := s.codex(t, CodexCached); !st.Available || st.UsageKnown || !strings.Contains(st.AccountNote, "another account's") {
		t.Errorf("second, not asked: %+v", st)
	}
	s.write(t, serve(usage(`"accountId":"acct-1",`, srvWindow(7, weekMinutes, reset), "null")))
	if st := s.codex(t, CodexAsk); !st.Available || st.UsedPercent != 7 {
		t.Errorf("second: %+v", st)
	}
	// A login whose member cannot be told falls back to the account id.
	login(t, home, "acct-1")
	if st := s.codex(t, CodexCached); st.UsedPercent != 7 {
		t.Errorf("no member: %+v", st)
	}
}

func TestAccountNote(t *testing.T) {
	home, _ := setup(t)
	reset := time.Now().Add(time.Hour)
	const unknown = "the login cannot be told, so a figure or a limit may be another account's"
	note := func(want string) {
		t.Helper()
		if st := codexState(t); st.AccountNote != want {
			t.Errorf("note %q, want %q", st.AccountNote, want)
		}
	}
	note(unknown)
	login(t, home, "acct-1")
	note("")
	addAccountRollout(t, home, "rollout-under.jsonl", "rollout-a.jsonl", "acct-2", reset, time.Now().Add(-time.Hour))
	note("the last figure read is another account's and is not used")
	addRollout(t, home, "rollout-under.jsonl", "rollout-b.jsonl", reset, time.Now())
	note("the session files do not say which account the figure is of")
	addAccountRollout(t, home, "rollout-under.jsonl", "rollout-b.jsonl", "acct-1", reset, time.Now())
	note("")
	authJSON(t, home, `{"OPENAI_API_KEY":"none"}`)
	note(unknown)
	if st, err := Codex(missing, CodexSessions); err != nil || st.AccountNote != "" {
		t.Errorf("not installed: %+v, %v", st, err)
	}
}
