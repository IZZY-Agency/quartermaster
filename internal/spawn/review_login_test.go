package spawn

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/limits"
	"github.com/IZZY-Agency/quartermaster/internal/route"
)

// How a stub codex's run ends: as Codex does on an error, or with findings.
const (
	runLimit    = "echo '" + limitError + "' >&2; exit 1\n"
	runLoginErr = "echo 'ERROR: please sign in again.' >&2; exit 1\n"
	runBadReply = "printf 'looks good' > \"$last\"; exit 0\n"
	runNoReply  = "exit 0\n"
	runFindings = "printf '%s' '" + cleanFindings + "' > \"$last\"; exit 0\n"
)

// loginStub is a codex with a folder of logins to switch to, <name>.json.
type loginStub struct{ home, dir, path string }

// newLoginStub makes a codex that does bodies[i] in its i-th run; $last is the
// file it was told to write its last message to, $home the Codex folder and
// "to a" switches the login. It logs each run's arguments, login and stdin.
// Asked for its usage it answers with usage ("": it fails).
func newLoginStub(t *testing.T, usage string, bodies ...string) loginStub {
	t.Helper()
	s := loginStub{home: os.Getenv("CODEX_HOME"), dir: t.TempDir()}
	if err := os.MkdirAll(s.home, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, l := range []string{"a", "b"} {
		s.put(t, l, fmt.Sprintf(`{"tokens":{"account_id":"acct-%s"}}`, l))
	}
	// Two members of one account.
	for _, l := range []string{"m1", "m2"} {
		payload := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_user_id":"user-` + l + `"}}`))
		s.put(t, l, `{"tokens":{"account_id":"acct-m","id_token":"e30.`+payload+`.x"}}`)
	}
	s.put(t, "key", `{"OPENAI_API_KEY":"none"}`)
	s.put(t, "torn", `{"tokens":`)
	answer := "exit 1"
	if usage != "" {
		answer = `read -r x; echo '{"id":0,"result":{}}'; read -r x; read -r x; echo '{"id":1,"result":` + usage + `}'; cat >/dev/null; exit 0`
	}
	body := `dir='` + s.dir + `'; home='` + s.home + `'
to() { cp "$dir/$1.json" "$home/auth.tmp"; mv "$home/auth.tmp" "$home/auth.json"; }
if [ "$1" = app-server ]; then echo x >> "$dir/asked"; [ ! -f "$dir/onask" ] || . "$dir/onask"; ` + answer + `; fi
echo run >> "$dir/runs"; n=$(wc -l < "$dir/runs" | tr -d ' ')
cp "$home/auth.json" "$dir/login-$n.json" 2>/dev/null
cat > "$dir/stdin-$n"
while [ "$1" != "-o" ]; do shift; done; last="$2"; echo "$last" > "$dir/last-$n"
case $n in
`
	for i, b := range bodies {
		body += fmt.Sprintf("%d) %s;;\n", i+1, b)
	}
	s.path = fakeBin(t, "codex", body+"esac\nexit 9\n")
	return s
}

func (s loginStub) put(t *testing.T, name, data string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.dir, name+".json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// login logs Codex in as one of the names, and is that login.
func (s loginStub) login(t *testing.T, name string) limits.Login {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s.dir, name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.home, "auth.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return limits.CodexLogin(s.path, limits.CodexSessions)
}

// onAsk makes the stub run a command when it is asked for its usage.
func (s loginStub) onAsk(t *testing.T, command string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.dir, "onask"), []byte(command+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (s loginStub) runs() int {
	b, _ := os.ReadFile(filepath.Join(s.dir, "runs"))
	return strings.Count(string(b), "\n")
}

// limited says whether Codex is limited for the login of this name.
func (s loginStub) limited(t *testing.T, name string) bool {
	t.Helper()
	st, err := limits.CodexFor(s.login(t, name), s.path, limits.CodexSessions)
	if err != nil {
		t.Fatal(err)
	}
	return st.Limited
}

// review runs a review on the stub, logged in as start, and is its logged
// decision, its job folder and whether Claude was run.
func (s loginStub) review(t *testing.T, start string, read limits.CodexRead) (d route.Decision, dir string, onClaude bool) {
	t.Helper()
	before := len(loggedReviews(t))
	s.login(t, start)
	dir = newJob(t, reviewDecision(t, 1, limits.CodexState{Available: true}), 1)
	out := t.TempDir()
	r := ReviewRunner{Codex: s.path, Claude: fakeClaude(t, out, cleanFindings), CodexRead: read}
	if err := r.Run(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	recs := loggedReviews(t)
	if len(recs) != before+1 || recs[before].Decision == nil {
		t.Fatalf("records %+v, want one more with its decision", recs)
	}
	_, err := os.Stat(filepath.Join(out, "args.txt"))
	if onClaude = err == nil; onClaude != (recs[before].Decision.Codex == nil) {
		t.Errorf("on Claude: %v, logged decision %+v", onClaude, recs[before].Decision)
	}
	return *recs[before].Decision, dir, onClaude
}

// said checks that the job's log and the decision's reasons both say want and
// that neither names an account.
func said(t *testing.T, d route.Decision, dir, want string) {
	t.Helper()
	log, err := os.ReadFile(filepath.Join(dir, logFile))
	if err != nil {
		t.Fatal(err)
	}
	reasons := strings.Join(d.Reasons, "\n")
	for name, text := range map[string]string{"log": string(log), "reasons": reasons} {
		if !strings.Contains(text, want) {
			t.Errorf("the %s lacks %q:\n%s", name, want, text)
		}
		if strings.Contains(text, "acct-") || strings.Contains(text, "user-") {
			t.Errorf("the %s names an account:\n%s", name, text)
		}
	}
}

const (
	saidRetry  = "its login was switched while it ran: the review was tried once more on Codex"
	saidNoTry  = "its login was switched while it ran: no second try, as "
	saidClaude = "the review went to Claude"
)

// Every way a first run fails, with the login switched or not and the new
// account available or limited.
func TestReviewLoginSwitched(t *testing.T) {
	for _, fail := range []struct {
		name, body string
		limit      bool
	}{{"limit", runLimit, true}, {"login error", runLoginErr, false}, {"bad reply", runBadReply, false}, {"no reply", runNoReply, false}} {
		for _, to := range []string{"", "to b\n", "to a\n", "to key\n", "to torn\n", "rm \"$home/auth.json\"\n"} {
			for _, bLimited := range []bool{false, true} {
				if to != "to b\n" && (bLimited || fail.name == "no reply") {
					continue // told by the others
				}
				t.Run(fmt.Sprintf("%s/%q/b limited %v", fail.name, to, bLimited), func(t *testing.T) {
					reviewHermetic(t)
					s := newLoginStub(t, "", to+fail.body, runFindings)
					if bLimited {
						if err := limits.MarkCodexLimited(s.login(t, "b"), time.Now().Add(time.Hour)); err != nil {
							t.Fatal(err)
						}
					}
					d, dir, onClaude := s.review(t, "a", limits.CodexSessions)
					switched := to == "to b\n"
					wantRuns := 1
					if switched && !bLimited {
						wantRuns = 2
					}
					if s.runs() != wantRuns || onClaude != (wantRuns == 1) {
						t.Errorf("%d runs, on Claude %v; want %d runs", s.runs(), onClaude, wantRuns)
					}
					switch {
					case wantRuns == 2:
						said(t, d, dir, saidRetry)
						if strings.Contains(strings.Join(d.Reasons, "\n"), saidClaude) {
							t.Errorf("reasons %q", d.Reasons)
						}
					case switched:
						said(t, d, dir, saidNoTry+"the new login is not available (Codex usage limit reached")
					}
					if !switched && strings.Contains(strings.Join(d.Reasons, "\n"), "switched") {
						t.Errorf("reasons %q", d.Reasons)
					}
					if got := s.limited(t, "a"); got != fail.limit {
						t.Errorf("a limited: %v, want %v", got, fail.limit)
					}
					if got := s.limited(t, "b"); got != bLimited {
						t.Errorf("b limited: %v, want %v", got, bLimited)
					}
				})
			}
		}
	}
}

// A second run that fails too goes to Claude, whatever the login is by then;
// its limit is the second login's.
func TestReviewSecondRunFails(t *testing.T) {
	for _, tc := range []struct {
		name, second string
		bLimited     bool
	}{{"limit", runLimit, true}, {"login error", runLoginErr, false}, {"switched back, limit", "to a\n" + runLimit, true}} {
		t.Run(tc.name, func(t *testing.T) {
			reviewHermetic(t)
			s := newLoginStub(t, "", "to b\n"+runLoginErr, tc.second, runFindings)
			d, dir, onClaude := s.review(t, "a", limits.CodexSessions)
			if s.runs() != 2 || !onClaude {
				t.Errorf("%d runs, on Claude %v; want two and Claude", s.runs(), onClaude)
			}
			said(t, d, dir, saidRetry)
			if !strings.Contains(strings.Join(d.Reasons, "\n"), saidClaude) {
				t.Errorf("reasons %q", d.Reasons)
			}
			if s.limited(t, "a") || s.limited(t, "b") != tc.bLimited {
				t.Errorf("limited: a %v, b %v; want false, %v", s.limited(t, "a"), s.limited(t, "b"), tc.bLimited)
			}
		})
	}
}

// After switching back the first account is still known to be limited, and the
// second run's limit is kept beside it.
func TestReviewSwitchingBack(t *testing.T) {
	reviewHermetic(t)
	s := newLoginStub(t, "", "to b\n"+runLimit, runLimit)
	if _, _, onClaude := s.review(t, "a", limits.CodexSessions); !onClaude || s.runs() != 2 {
		t.Errorf("on Claude %v after %d runs", onClaude, s.runs())
	}
	for _, l := range []string{"a", "b", "a"} {
		if !s.limited(t, l) {
			t.Errorf("%s is not limited", l)
		}
	}
	// A review under b that is switched to the limited a is not tried again.
	s = newLoginStub(t, "", "to a\n"+runLoginErr, runFindings)
	d, dir, _ := s.review(t, "b", limits.CodexSessions)
	if s.runs() != 1 {
		t.Errorf("%d runs, want one", s.runs())
	}
	said(t, d, dir, saidNoTry)
}

// Two members of one account.
func TestReviewMemberSwitched(t *testing.T) {
	reviewHermetic(t)
	s := newLoginStub(t, "", "to m2\n"+runLimit, runFindings)
	d, dir, onClaude := s.review(t, "m1", limits.CodexSessions)
	if s.runs() != 2 || onClaude {
		t.Errorf("%d runs, on Claude %v", s.runs(), onClaude)
	}
	said(t, d, dir, saidRetry)
	if !s.limited(t, "m1") || s.limited(t, "m2") {
		t.Errorf("limited: m1 %v, m2 %v; want true, false", s.limited(t, "m1"), s.limited(t, "m2"))
	}
}

// The second run has the new login, the whole prompt and an output file of its
// own: a reply the first left behind is not read as its.
func TestReviewSecondRunIsFresh(t *testing.T) {
	reviewHermetic(t)
	s := newLoginStub(t, "", "to b\nprintf '%s' '"+cleanFindings+"' > \"$last\"\n"+runLimit, runNoReply)
	if _, _, onClaude := s.review(t, "a", limits.CodexSessions); !onClaude {
		t.Error("the first run's reply was taken as the second's")
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(s.dir, name))
		if err != nil {
			t.Error(err)
		}
		return string(b)
	}
	if read("last-1") == read("last-2") {
		t.Errorf("both runs write to %s", read("last-1"))
	}
	if read("stdin-1") != read("stdin-2") || read("stdin-2") == "" {
		t.Errorf("prompts %q and %q", read("stdin-1"), read("stdin-2"))
	}
	if read("login-1.json") != read("a.json") || read("login-2.json") != read("b.json") {
		t.Errorf("logins %s and %s", read("login-1.json"), read("login-2.json"))
	}
}

// A reply left in the job folder by a run before is not this one's.
func TestReviewOldReplyNotRead(t *testing.T) {
	reviewHermetic(t)
	s := newLoginStub(t, "", runNoReply)
	s.login(t, "a")
	dir := newJob(t, reviewDecision(t, 1, limits.CodexState{Available: true}), 1)
	if err := os.WriteFile(filepath.Join(dir, codexLast), []byte(cleanFindings), 0o600); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := (ReviewRunner{Codex: s.path, Claude: fakeClaude(t, out, cleanFindings)}).Run(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	readArgs(t, out)
}

// Asked, the new account's own usage decides too, and only for that login.
func TestReviewSwitchedAsksUsage(t *testing.T) {
	usage := func(account string, used int) string {
		return fmt.Sprintf(`{"accountId":"%s","rateLimits":{"limitId":"codex","primary":{"usedPercent":%d,"windowDurationMins":300,"resetsAt":%d}}}`, account, used, time.Now().Add(time.Hour).Unix())
	}
	for _, tc := range []struct {
		name, usage string
		read        limits.CodexRead
		runs        int
		asked       bool
	}{
		{"room", usage("acct-b", 10), limits.CodexAsk, 2, true},
		{"full", usage("acct-b", 99), limits.CodexAsk, 1, true},
		{"no answer", "", limits.CodexAsk, 2, true},
		{"another account's answer", usage("acct-a", 99), limits.CodexAsk, 2, true},
		{"not to be asked", usage("acct-b", 99), limits.CodexSessions, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reviewHermetic(t)
			s := newLoginStub(t, tc.usage, "to b\n"+runLimit, runFindings)
			s.review(t, "a", tc.read)
			_, err := os.Stat(filepath.Join(s.dir, "asked"))
			if s.runs() != tc.runs || (err == nil) != tc.asked {
				t.Errorf("%d runs, asked %v; want %d, %v", s.runs(), err == nil, tc.runs, tc.asked)
			}
		})
	}
}

// A login switched again while its usage was asked for is not tried.
func TestReviewSwitchedTwice(t *testing.T) {
	for _, to := range []string{"to a", "to key", `rm "$home/auth.json"`} {
		reviewHermetic(t)
		s := newLoginStub(t, "", "to b\n"+runLoginErr, runFindings)
		s.onAsk(t, to)
		d, dir, onClaude := s.review(t, "a", limits.CodexAsk)
		if s.runs() != 1 || !onClaude {
			t.Errorf("%s: %d runs, on Claude %v", to, s.runs(), onClaude)
		}
		said(t, d, dir, saidNoTry+"it was changed again since")
	}
}

// A run that ended on the time limit, or on a signal, is not tried again, and
// the limit is one for both runs.
func TestReviewNotTriedAgain(t *testing.T) {
	reviewHermetic(t)
	for _, tc := range []struct {
		name   string
		bodies []string
		runs   int
	}{
		{"time limit", []string{"to b\nsleep 30\n", runFindings}, 1},
		{"time limit in the second run", []string{"to b\n" + runLoginErr, "sleep 30\n"}, 2},
	} {
		s := newLoginStub(t, "", tc.bodies...)
		s.login(t, "a")
		dir := newJob(t, reviewDecision(t, 1, limits.CodexState{Available: true}), 1)
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		out := t.TempDir()
		start := time.Now()
		err := ReviewRunner{Codex: s.path, Claude: fakeClaude(t, out, cleanFindings)}.Run(ctx, dir)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) || s.runs() != tc.runs || time.Since(start) > 5*time.Second {
			t.Errorf("%s: %v, %d runs, took %v", tc.name, err, s.runs(), time.Since(start))
		}
		if _, err := os.Stat(filepath.Join(out, "args.txt")); err == nil {
			t.Errorf("%s: Claude was run", tc.name)
		}
	}
	for _, sig := range []string{"INT", "TERM", "KILL"} {
		for _, said := range []string{"", "echo '" + limitError + "' >&2\n"} {
			s := newLoginStub(t, "", "to b\n"+said+"kill -"+sig+" $$\n", runFindings)
			if _, _, onClaude := s.review(t, "a", limits.CodexSessions); !onClaude || s.runs() != 1 {
				t.Errorf("SIG%s after %q: on Claude %v after %d runs", sig, said, onClaude, s.runs())
			}
		}
	}
}

// With the login in a keyring, the account Codex itself names tells a switch.
func TestReviewKeyringSwitched(t *testing.T) {
	answer := func(account string) string {
		return `{"accountId":"` + account + `","rateLimits":{"limitId":"codex","primary":{"usedPercent":10}}}`
	}
	for _, read := range []limits.CodexRead{limits.CodexAsk, limits.CodexSessions} {
		reviewHermetic(t)
		s := newLoginStub(t, "", "")
		s.put(t, "answer", `{"id":1,"result":`+answer("acct-a")+`}`)
		s.put(t, "answer-b", `{"id":1,"result":`+answer("acct-b")+`}`)
		s.path = fakeBin(t, "codex", `dir='`+s.dir+`'
if [ "$1" = app-server ]; then read -r x; echo '{"id":0,"result":{}}'; read -r x; read -r x; cat "$dir/answer.json"; echo; cat >/dev/null; exit 0; fi
echo run >> "$dir/runs"; cat > /dev/null; while [ "$1" != "-o" ]; do shift; done; last="$2"
if [ "$(wc -l < "$dir/runs" | tr -d ' ')" = 1 ]; then cp "$dir/answer-b.json" "$dir/answer.json"; `+runLimit+`fi
`+runFindings)
		dir := newJob(t, reviewDecision(t, 1, limits.CodexState{Available: true}), 1)
		out := t.TempDir()
		if err := (ReviewRunner{Codex: s.path, Claude: fakeClaude(t, out, cleanFindings), CodexRead: read}).Run(context.Background(), dir); err != nil {
			t.Fatal(err)
		}
		want := 1
		if read == limits.CodexAsk {
			want = 2
		}
		if s.runs() != want {
			t.Errorf("read %v: %d runs, want %d", read, s.runs(), want)
		}
	}
}
