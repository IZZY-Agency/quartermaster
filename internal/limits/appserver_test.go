package limits

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/policy"
)

// TestMain shortens the wait every read ends with; the tests of it set it.
func TestMain(m *testing.M) {
	termGrace = 20 * time.Millisecond
	os.Exit(m.Run())
}

// The stub Codex: a shell script that logs each start and each line it gets.
// Nothing here starts the real one.
type stubCodex struct{ dir string }

// handshake answers initialize, then reads the next two messages.
const handshake = `get; say '{"id":0,"result":{"userAgent":"stub"}}'; get; get
`

// serve is a stub body that answers the usage request with reply.
func serve(reply string) string {
	return handshake + "say '{\"id\":1,\"result\":" + reply + "}'\ncat >/dev/null\n"
}

// newStub writes a codex command with this shell body. get logs a line of its
// input, say writes one, "$dir" is the stub's folder.
func newStub(t *testing.T, body string) *stubCodex {
	t.Helper()
	s := &stubCodex{dir: t.TempDir()}
	s.write(t, body)
	return s
}

func (s *stubCodex) write(t *testing.T, body string) {
	t.Helper()
	script := `#!/bin/sh
dir='` + s.dir + `'
get() { IFS= read -r line && printf '%s\n' "$line" >>"$dir/in"; }
say() { printf '%s\n' "$1"; }
printf '%s\n' "$*" >>"$dir/starts"
pwd >"$dir/pwd"
` + body
	if err := os.WriteFile(filepath.Join(s.dir, "codex"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

func (s *stubCodex) look(string) (string, error) { return filepath.Join(s.dir, "codex"), nil }

// lines is the lines of a file the stub wrote; none when it wrote none.
func (s *stubCodex) lines(name string) []string {
	b, _ := os.ReadFile(filepath.Join(s.dir, name))
	return strings.Fields(strings.ReplaceAll(string(b), " ", "_"))
}

func (s *stubCodex) starts() int { return len(s.lines("starts")) }

func (s *stubCodex) codex(t *testing.T, read CodexRead) CodexState {
	t.Helper()
	st, err := Codex(s.look, read)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// setTimeout shortens the read's timeout.
func setTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := usageTimeout
	usageTimeout = d
	t.Cleanup(func() { usageTimeout = old })
}

// usage is an account/rateLimits/read result with one codex bucket; extra
// fields go first.
func usage(extra, primary, secondary string) string {
	bucket := fmt.Sprintf(`{"limitId":"codex","limitName":null,"primary":%s,"secondary":%s,"credits":{"hasCredits":true,"unlimited":false,"balance":"12345"},"planType":"secretplan"}`, primary, secondary)
	return fmt.Sprintf(`{%s"rateLimits":%s,"rateLimitsByLimitId":{"codex":%s,"other":{"limitId":"other","primary":{"usedPercent":100,"windowDurationMins":300,"resetsAt":null},"secondary":null}},"rateLimitUpsell":{"title":"secretbanner"}}`, extra, bucket, bucket)
}

func srvWindow(used, minutes int, reset time.Time) string {
	return fmt.Sprintf(`{"usedPercent":%d,"windowDurationMins":%d,"resetsAt":%d}`, used, minutes, reset.Unix())
}

func readUsageFile(t *testing.T, state string) usageFile {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(state, "limits", "codex-usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f usageFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func putUsageFile(t *testing.T, f usageFile) {
	t.Helper()
	if err := f.save(); err != nil {
		t.Fatal(err)
	}
}

// gone waits for the process named by the stub's file to be gone.
func gone(t *testing.T, s *stubCodex, name string) {
	t.Helper()
	ids := s.lines(name)
	if len(ids) != 1 {
		t.Fatalf("%s = %v, want one process id", name, ids)
	}
	pid, err := strconv.Atoi(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	for range 300 {
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("%s: process %d is still there", name, pid)
}

func TestAppServerReading(t *testing.T) {
	home, state := setup(t)
	login(t, home, "acct-1")
	reset := time.Now().Add(72 * time.Hour).Truncate(time.Second)
	s := newStub(t, serve(usage(`"accountId":"acct-1","ordinaryUsageAllowed":true,`, srvWindow(86, 10080, reset), "null")))
	// An older rollout says something else: the fresh reading decides.
	writeRollout(t, home, "rollout-a.jsonl", time.Now(), tokenCount(time.Now(), `"primary":`+window(99, 300, reset)))

	st := s.codex(t, CodexAsk)
	if !st.Available || !st.UsageKnown || st.UsedPercent != 86 || st.Window != "weekly" || !st.ResetsAt.Equal(reset) ||
		st.Source != SourceAppServer || st.Stale || st.ReadFailure != "" || time.Since(st.ReadAt) > 10*time.Second {
		t.Errorf("state = %+v", st)
	}

	// What was sent: one start, the three messages and nothing else.
	if got := s.lines("starts"); !reflect.DeepEqual(got, []string{"app-server"}) {
		t.Errorf("starts = %q", got)
	}
	want := []string{
		`{"method":"initialize","id":0,"params":{"clientInfo":{"name":"quartermaster","version":"dev"}}}`,
		`{"method":"initialized"}`,
		`{"method":"account/rateLimits/read","id":1,"params":{"excludeResetCreditDetails":true}}`,
	}
	if got := s.lines("in"); !reflect.DeepEqual(got, want) {
		t.Errorf("sent %q\nwant %q", got, want)
	}
	if got := s.lines("pwd"); !reflect.DeepEqual(got, []string{"/"}) {
		t.Errorf("ran in %q", got)
	}

	// What is stored: the numbers, and a hash of the account.
	path := filepath.Join(state, "limits", "codex-usage.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", info.Mode())
	}
	b, _ := os.ReadFile(path)
	for _, secret := range []string{"acct-1", "secretplan", "secretbanner", "12345", "other", "balance"} {
		if strings.Contains(string(b), secret) {
			t.Errorf("the file holds %q: %s", secret, b)
		}
	}
	f := readUsageFile(t, state)
	if f.Account != hashAccount("acct-1") || len(f.Windows) != 1 || f.Allowed == nil || !*f.Allowed {
		t.Errorf("file = %+v", f)
	}

	// Within its five minutes nothing is started again.
	if st := s.codex(t, CodexAsk); st.UsedPercent != 86 || s.starts() != 1 {
		t.Errorf("second call: %+v, %d starts", st, s.starts())
	}
}

func TestAppServerBothWindows(t *testing.T) {
	setup(t)
	now := time.Now()
	s := newStub(t, serve(usage("", srvWindow(20, 300, now.Add(time.Hour)), srvWindow(96, 10080, now.Add(72*time.Hour)))))
	st := s.codex(t, CodexAsk)
	if st.Available || st.UsedPercent != 96 || st.Window != "weekly" || st.Reason != "Codex limit at 96% (weekly)" {
		t.Errorf("state = %+v", st)
	}
}

// A server may say other things on the way: they are not answers.
func TestAppServerOtherMessages(t *testing.T) {
	setup(t)
	s := newStub(t, `say 'not json'; say '[1]'; say '{"method":"account/updated","params":{}}'
get; say '{"id":0,"method":"some/request","params":{}}'; say '{"id":0,"result":{}}'; get; get
say '{"id":7,"error":{"code":1}}'; say '{"id":1,"method":"some/request"}'
say '{"id":1,"result":`+usage("", srvWindow(12, 300, time.Now().Add(time.Hour)), "null")+`}'
cat >/dev/null
`)
	if st := s.codex(t, CodexAsk); st.UsedPercent != 12 || st.Source != SourceAppServer {
		t.Errorf("state = %+v", st)
	}
}

// An older server has only the single-bucket view.
func TestAppServerSingleBucketView(t *testing.T) {
	now := time.Now()
	w := srvWindow(33, 300, now.Add(time.Hour))
	for _, tc := range []struct {
		reply string
		ok    bool
	}{
		{`{"rateLimits":{"primary":` + w + `,"secondary":null}}`, true},
		{`{"rateLimits":{"limitId":"codex","primary":` + w + `},"rateLimitsByLimitId":null}`, true},
		{`{"rateLimits":{"limitId":"other","primary":` + w + `}}`, false},
		{`{"rateLimits":{"limitId":"codex","primary":` + w + `},"rateLimitsByLimitId":{"other":{"primary":` + w + `}}}`, true},
		{`{"rateLimits":{"limitId":"other","primary":` + w + `},"rateLimitsByLimitId":{"other":{"primary":` + w + `}}}`, false},
	} {
		setup(t)
		s := newStub(t, serve(tc.reply))
		if st := s.codex(t, CodexAsk); st.UsageKnown != tc.ok || (st.ReadFailure == "") != tc.ok {
			t.Errorf("%s: state = %+v, want known %v", tc.reply, st, tc.ok)
		}
	}
}

func TestAppServerFailures(t *testing.T) {
	now := time.Now()
	w := srvWindow(33, 300, now.Add(time.Hour))
	const secret = "secret-error-text"
	for _, tc := range []struct {
		name, body string
		want       usageFailure
	}{
		{"exits at once", "exit 1\n", failEnded},
		{"exits after the handshake", handshake + "exit 0\n", failEnded},
		{"does not know the command", "echo " + secret + " >&2; exit 2\n", failEnded},
		{"hangs", "sleep 60\n", failTimeout},
		{"hangs after the handshake", handshake + "sleep 60\n", failTimeout},
		{"prints garbage and hangs", "say garbage; say '{'; sleep 60\n", failTimeout},
		{"prints garbage for ever", "while :; do say garbagegarbagegarbagegarbagegarbagegarbage || exit; done\n", failShape},
		{"one endless line", "while :; do printf garbagegarbagegarbagegarbagegarbage || exit; done\n", failShape},
		{"initialize refused", `get; say '{"id":0,"error":{"code":-32600,"message":"` + secret + `"}}'; cat >/dev/null` + "\n", failRefused},
		{"API-key login", handshake + `say '{"id":1,"error":{"code":-32600,"message":"` + secret + `"}}'; cat >/dev/null` + "\n", failRefused},
		{"unknown method", handshake + `say '{"id":1,"error":{"code":-32601,"message":"` + secret + `"}}'; cat >/dev/null` + "\n", failRefused},
		{"no network", handshake + `say '{"id":1,"error":{"code":-32603,"message":"` + secret + `"}}'; cat >/dev/null` + "\n", failFetch},
		{"no result", serve("null"), failShape},
		{"another type", serve(`{"rateLimits":{"primary":{"usedPercent":"33"}}}`), failShape},
		{"no percentage", serve(`{"rateLimits":{"primary":{"windowDurationMins":300}}}`), failShape},
		{"negative percentage", serve(`{"rateLimits":{"primary":{"usedPercent":-1}}}`), failShape},
		{"no windows and nothing else", serve(`{"rateLimits":{"primary":null,"secondary":null}}`), failShape},
		{"empty", serve(`{}`), failShape},
		{"another account", serve(usage(`"accountId":"acct-2",`, w, "null")), failAccount},
		{"logs in as another account meanwhile", handshake + `printf '{"tokens":{"account_id":"acct-3"}}' >"$CODEX_HOME/auth.json"
say '{"id":1,"result":` + usage("", w, "null") + `}'; cat >/dev/null` + "\n", failAccount},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, state := setup(t)
			login(t, home, "acct-1")
			// The read before this one, an hour old, and a newer rollout.
			putUsageFile(t, usageFile{ReadAt: now.Add(-time.Hour), Account: hashAccount("acct-1"), Windows: []limitWindow{{UsedPercent: 11, Minutes: 300, ResetsAt: now.Add(time.Hour)}}})
			writeRollout(t, home, "rollout-a.jsonl", now, tokenCount(now.Add(-10*time.Minute), `"primary":`+window(44, 300, now.Add(time.Hour))))
			timeout := 10 * time.Second
			if tc.want == failTimeout {
				timeout = 3 * time.Second
			}
			setTimeout(t, timeout)
			s := newStub(t, tc.body)
			start := time.Now()
			st := s.codex(t, CodexAsk)
			if d := time.Since(start); d > timeout+2*time.Second {
				t.Errorf("took %v", d)
			}
			if !st.Available || st.UsedPercent != 44 || st.Source != SourceSessions || st.ReadFailure != tc.want.text() {
				t.Errorf("state = %+v, want the rollout's 44%% and %q", st, tc.want.text())
			}
			// Remembered for a short time, in qm's own words, and the reading kept.
			f := readUsageFile(t, state)
			if f.Failure != tc.want || f.FailedAt.IsZero() {
				t.Errorf("file = %+v", f)
			}
			if f.Account == hashAccount("acct-1") && (len(f.Windows) != 1 || f.Windows[0].UsedPercent != 11) {
				t.Errorf("the last reading was lost: %+v", f)
			}
			b, _ := os.ReadFile(filepath.Join(state, "limits", "codex-usage.json"))
			if strings.Contains(string(b)+st.ReadFailure+st.Reason, secret) {
				t.Errorf("Codex's own message was kept: %s %+v", b, st)
			}
			if st := s.codex(t, CodexAsk); s.starts() != 1 || st.ReadFailure != tc.want.text() {
				t.Errorf("second call: %d starts, %+v", s.starts(), st)
			}
			// And tried again after it.
			f.FailedAt = time.Now().Add(-policy.CodexUsageRetry - time.Second)
			putUsageFile(t, f)
			login(t, home, "acct-1")
			setTimeout(t, 10*time.Second)
			s.write(t, serve(usage("", w, "null")))
			if st := s.codex(t, CodexAsk); s.starts() != 2 || st.UsedPercent != 33 || st.ReadFailure != "" {
				t.Errorf("after the retry time: %d starts, %+v", s.starts(), st)
			}
			if f := readUsageFile(t, state); f.Failure != "" || !f.FailedAt.IsZero() {
				t.Errorf("the failure outlived a read that worked: %+v", f)
			}
		})
	}
}

// No failure turns Codex off, with no rollout either.
func TestAppServerFailureNoRollouts(t *testing.T) {
	setup(t)
	s := newStub(t, "exit 1\n")
	if st := s.codex(t, CodexAsk); !st.Available || st.UsageKnown || st.ReadFailure == "" {
		t.Errorf("state = %+v", st)
	}
	// Nor does a command that cannot be started.
	setup(t)
	st, err := Codex(func(string) (string, error) { return filepath.Join(t.TempDir(), "codex"), nil }, CodexAsk)
	if err != nil || !st.Available || st.ReadFailure != failStart.text() {
		t.Errorf("state = %+v, %v", st, err)
	}
}

func TestAppServerLeavesNoProcess(t *testing.T) {
	w := srvWindow(33, 300, time.Now().Add(time.Hour))
	t.Run("hangs", func(t *testing.T) {
		setup(t)
		setTimeout(t, 3*time.Second)
		s := newStub(t, `echo $$ >"$dir/pid"; trap '' TERM HUP INT; sleep 60 & echo $! >"$dir/child"; wait`+"\n")
		s.codex(t, CodexAsk)
		gone(t, s, "pid")
		gone(t, s, "child")
	})
	t.Run("forks a child that outlives it", func(t *testing.T) {
		setup(t)
		s := newStub(t, `sleep 60 & echo $! >"$dir/child"`+"\n"+serve(usage("", w, "null")))
		start := time.Now()
		if st := s.codex(t, CodexAsk); st.UsedPercent != 33 || st.ReadFailure != "" {
			t.Errorf("state = %+v", st)
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Errorf("waited %v for the child", d)
		}
		gone(t, s, "child")
	})
	t.Run("forks a child and fails", func(t *testing.T) {
		setup(t)
		setTimeout(t, 3*time.Second)
		s := newStub(t, `sleep 60 & echo $! >"$dir/child"; exit 1`+"\n")
		s.codex(t, CodexAsk)
		gone(t, s, "child")
	})
	t.Run("does not exit at the end of its input", func(t *testing.T) {
		setup(t)
		s := newStub(t, `echo $$ >"$dir/pid"`+"\n"+handshake+`say '{"id":1,"result":`+usage("", w, "null")+`}'; exec sleep 60`+"\n")
		start := time.Now()
		if st := s.codex(t, CodexAsk); st.UsedPercent != 33 {
			t.Errorf("state = %+v", st)
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Errorf("waited %v for it", d)
		}
		gone(t, s, "pid")
	})
}

// What is left of the group is asked to end before it is killed.
func TestAppServerTermBeforeKill(t *testing.T) {
	setup(t)
	old := termGrace
	termGrace = time.Second
	t.Cleanup(func() { termGrace = old })
	s := newStub(t, `(trap 'echo x >"$dir/termed"; exit' TERM; i=0; while [ $i -lt 1500 ]; do sleep 0.02; i=$((i+1)); done) >/dev/null &
`+serve(usage("", srvWindow(33, 300, time.Now().Add(time.Hour)), "null")))
	if st := s.codex(t, CodexAsk); st.UsedPercent != 33 {
		t.Errorf("state = %+v", st)
	}
	if s.lines("termed") == nil {
		t.Error("the child was killed without a SIGTERM")
	}
}

// TestInterruptHelper is the process TestAppServerInterrupted interrupts.
func TestInterruptHelper(t *testing.T) {
	codex := os.Getenv("QM_TEST_INTERRUPT")
	if codex == "" {
		t.Skip("a helper of TestAppServerInterrupted")
	}
	usageTimeout = time.Minute
	exitGrace, _ = time.ParseDuration(os.Getenv("QM_TEST_EXIT_GRACE"))
	termGrace, _ = time.ParseDuration(os.Getenv("QM_TEST_TERM_GRACE"))
	_, _ = Codex(func(string) (string, error) { return codex, nil }, CodexAsk)
	t.Fatal("the signal did not end the process")
}

// A signal that would end qm ends Codex and then qm, whenever it comes: while
// the answer is awaited, after it, and while Codex is being ended.
func TestAppServerInterrupted(t *testing.T) {
	const child = `sleep 60 & echo $! >"$dir/child"; echo $$ >"$dir/pid"` + "\n"
	const ready = `echo x >"$dir/ready"` + "\n"
	const stay = "i=0; while [ $i -lt 600 ]; do sleep 0.05; i=$((i+1)); done\n" // not for ever, if the test fails
	answer := handshake + `say '{"id":1,"result":` + usage("", srvWindow(33, 300, time.Now().Add(time.Hour)), "null") + `}'` + "\n"
	ignore := "trap '' INT HUP TERM\n"
	for _, tc := range []struct {
		name, body, exit, term string
		sig                    syscall.Signal
	}{
		{"SIGINT", ignore + child + ready + stay, "500ms", "200ms", syscall.SIGINT},
		{"SIGTERM", ignore + child + ready + stay, "500ms", "200ms", syscall.SIGTERM},
		{"SIGHUP", ignore + child + ready + stay, "500ms", "200ms", syscall.SIGHUP},
		{"after the answer", ignore + child + answer + ready + stay, "20s", "200ms", syscall.SIGINT},
		{"while it is ended", child + `trap 'echo x >"$dir/ready"' TERM` + "\n" + answer + stay, "200ms", "3s", syscall.SIGINT},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if signal.Ignored(tc.sig) {
				t.Skipf("%v is ignored here (nohup, say) and so in the helper", tc.sig)
			}
			_, state := setup(t)
			s := newStub(t, tc.body)
			path, _ := s.look("")
			cmd := exec.Command(os.Args[0], "-test.run=^TestInterruptHelper$")
			cmd.Env = append(os.Environ(), "QM_TEST_INTERRUPT="+path, "QM_TEST_EXIT_GRACE="+tc.exit, "QM_TEST_TERM_GRACE="+tc.term)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			for i := 0; len(s.lines("ready")) == 0; i++ {
				if i > 3000 {
					_ = cmd.Process.Kill()
					t.Fatal("the stub did not get ready")
				}
				time.Sleep(10 * time.Millisecond)
			}
			start := time.Now()
			_ = cmd.Process.Signal(tc.sig)
			_ = cmd.Wait()
			if d := time.Since(start); d > 10*time.Second {
				t.Errorf("ended after %v", d)
			}
			if ws := cmd.ProcessState.Sys().(syscall.WaitStatus); !ws.Signaled() || ws.Signal() != tc.sig {
				t.Errorf("ended with %v, want the signal", cmd.ProcessState)
			}
			gone(t, s, "pid")
			gone(t, s, "child")
			if _, err := os.Stat(filepath.Join(state, "limits", "codex-usage.json")); err == nil {
				t.Error("an interrupted read was stored")
			}
		})
	}
}

// A holder that stores its answer just as the wait for it ends is seen.
func TestAppServerBusyThenDone(t *testing.T) {
	now := time.Now()
	_, state := setup(t)
	setTimeout(t, time.Second)
	s := newStub(t, "exit 1\n")
	if err := os.MkdirAll(filepath.Join(state, "limits"), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(state, "limits", "codex-usage.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		time.Sleep(300 * time.Millisecond)
		putUsageFile(t, usageFile{ReadAt: now, Windows: []limitWindow{{UsedPercent: 12, Minutes: 300, ResetsAt: now.Add(time.Hour)}}})
	})
	if st := s.codex(t, CodexAsk); st.UsedPercent != 12 || st.ReadFailure != "" || s.starts() != 0 {
		t.Errorf("%+v, %d starts", st, s.starts())
	}
	wg.Wait()
}

// When a cached reading is read again, and when it is not used.
func TestAppServerCache(t *testing.T) {
	now := time.Now()
	const other = "acct-2"
	week := []limitWindow{{UsedPercent: 11, Minutes: 10080, ResetsAt: now.Add(time.Hour)}}
	for _, tc := range []struct {
		name    string
		file    usageFile
		starts  int
		used    float64
		cached  float64 // what CodexCached shows; 0 for nothing
		allowed bool
	}{
		{"fresh", usageFile{ReadAt: now.Add(-4 * time.Minute), Account: hashAccount("acct-1"), Windows: week}, 0, 11, 11, true},
		{"old", usageFile{ReadAt: now.Add(-6 * time.Minute), Account: hashAccount("acct-1"), Windows: week}, 1, 33, 11, true},
		{"from the future", usageFile{ReadAt: now.Add(time.Hour), Account: hashAccount("acct-1"), Windows: week}, 1, 33, 0, true},
		{"of another account", usageFile{ReadAt: now.Add(-time.Minute), Account: hashAccount(other), Windows: week}, 1, 33, 0, true},
		{"of no account", usageFile{ReadAt: now.Add(-time.Minute), Windows: week}, 1, 33, 0, true},
		{"a window reset since", usageFile{ReadAt: now.Add(-2 * time.Minute), Account: hashAccount("acct-1"), Windows: []limitWindow{{UsedPercent: 100, Minutes: 300, ResetsAt: now.Add(-time.Minute)}}}, 1, 33, 100, true},
		{"failed for another account", usageFile{FailedAt: now.Add(-time.Second), Failure: failTimeout, Account: hashAccount(other)}, 1, 33, 0, true},
		{"failed in the future", usageFile{FailedAt: now.Add(time.Hour), Failure: failTimeout, Account: hashAccount("acct-1")}, 1, 33, 0, true},
		{"not allowed, fresh", usageFile{ReadAt: now.Add(-4 * time.Minute), Account: hashAccount("acct-1"), Windows: week, Allowed: new(bool)}, 0, 11, 11, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, _ := setup(t)
			login(t, home, "acct-1")
			s := newStub(t, serve(usage("", srvWindow(33, 300, now.Add(time.Hour)), "null")))
			putUsageFile(t, tc.file)
			if st := s.codex(t, CodexCached); st.UsedPercent != tc.cached || s.starts() != 0 || st.Available != tc.allowed {
				t.Errorf("cached: %+v, %d starts", st, s.starts())
			}
			if st := s.codex(t, CodexSessions); st.UsedPercent != 0 || st.Source != "" || s.starts() != 0 || !st.Available {
				t.Errorf("sessions only: %+v, %d starts", st, s.starts())
			}
			if st := s.codex(t, CodexAsk); st.UsedPercent != tc.used || s.starts() != tc.starts || st.Available != tc.allowed {
				t.Errorf("ask: %+v, %d starts", st, s.starts())
			}
		})
	}
	t.Run("unreadable", func(t *testing.T) {
		_, state := setup(t)
		s := newStub(t, serve(usage("", srvWindow(33, 300, now.Add(time.Hour)), "null")))
		for i, bad := range []string{"{", `{"windows":[{"used_percent":-5}],"read_at":"` + now.Format(time.RFC3339) + `"}`, `{"read_at":"` + now.Format(time.RFC3339) + `"}`} {
			if err := os.MkdirAll(filepath.Join(state, "limits"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(state, "limits", "codex-usage.json"), []byte(bad), 0o600); err != nil {
				t.Fatal(err)
			}
			if st := s.codex(t, CodexAsk); st.UsedPercent != 33 || s.starts() != i+1 {
				t.Errorf("%s: %+v, %d starts", bad, st, s.starts())
			}
		}
	})
}

// Without a fresh reading the newer of the last one and the rollouts counts.
func TestAppServerOldReadingOrRollout(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		read, rollout time.Duration
		want          float64
		stale         bool
	}{
		{-30 * time.Minute, -2 * time.Hour, 11, false},
		{-2 * time.Hour, -30 * time.Minute, 44, false},
		{-2 * time.Hour, -3 * time.Hour, 11, true},
	} {
		home, _ := setup(t)
		s := newStub(t, "exit 1\n")
		no := false
		putUsageFile(t, usageFile{ReadAt: now.Add(tc.read), Allowed: &no, Windows: []limitWindow{{UsedPercent: 11, Minutes: 10080, ResetsAt: now.Add(time.Hour)}}})
		writeRollout(t, home, "rollout-a.jsonl", now, tokenCount(now.Add(tc.rollout), `"primary":`+window(44, 10080, now.Add(time.Hour))))
		// An old "not allowed" says nothing about now.
		if st := s.codex(t, CodexAsk); st.UsedPercent != tc.want || !st.Available || st.Stale != tc.stale || st.ReadFailure == "" {
			t.Errorf("%v/%v: %+v", tc.read, tc.rollout, st)
		}
	}
}

// A fresh answer wins over any rollout: a session can write an earlier
// reading again, under a newer time.
func TestAppServerFreshWinsOverRollout(t *testing.T) {
	now := time.Now()
	home, _ := setup(t)
	s := newStub(t, "exit 1\n")
	putUsageFile(t, usageFile{ReadAt: now.Add(-4 * time.Minute), Windows: []limitWindow{{UsedPercent: 11, Minutes: 10080, ResetsAt: now.Add(time.Hour)}}})
	writeRollout(t, home, "rollout-a.jsonl", now, tokenCount(now.Add(-time.Minute), `"primary":`+window(99, 10080, now.Add(time.Hour))))
	if st := s.codex(t, CodexAsk); st.UsedPercent != 11 || !st.Available || s.starts() != 0 {
		t.Errorf("%+v, %d starts", st, s.starts())
	}
}

func TestAppServerOrdinaryUsage(t *testing.T) {
	reset := time.Now().Add(time.Hour)
	w := srvWindow(33, 300, reset)
	for _, tc := range []struct {
		name, reply string
		available   bool
		reason      string
	}{
		{"false", usage(`"ordinaryUsageAllowed":false,`, w, "null"), false, "Codex says ordinary usage is not allowed now"},
		{"null", usage(`"ordinaryUsageAllowed":null,`, w, "null"), true, ""},
		{"true does not lift the limit", usage(`"ordinaryUsageAllowed":true,`, srvWindow(95, 300, reset), "null"), false, "Codex limit at 95% (5-hour)"},
		{"null does not lift the limit", usage(`"ordinaryUsageAllowed":null,`, srvWindow(95, 300, reset), "null"), false, "Codex limit at 95% (5-hour)"},
		{"no windows, true", `{"ordinaryUsageAllowed":true,"rateLimits":{"primary":null}}`, true, "Codex states no usage limit"},
		{"no windows, false", `{"ordinaryUsageAllowed":false,"rateLimits":{"primary":null}}`, false, "Codex says ordinary usage is not allowed now"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup(t)
			s := newStub(t, serve(tc.reply))
			if st := s.codex(t, CodexAsk); st.Available != tc.available || st.Reason != tc.reason || st.Stale || st.ReadFailure != "" {
				t.Errorf("state = %+v", st)
			}
		})
	}
}

// A fresh reading with room never clears a usage-limit error before its time.
func TestAppServerMarkerWins(t *testing.T) {
	setup(t)
	until := time.Now().Add(3 * time.Hour)
	if err := markNow(t, until); err != nil {
		t.Fatal(err)
	}
	s := newStub(t, serve(usage(`"ordinaryUsageAllowed":true,`, srvWindow(3, 300, time.Now().Add(time.Hour)), "null")))
	if st := s.codex(t, CodexAsk); st.Available || !st.ResetsAt.Equal(until) || st.UsedPercent != 3 {
		t.Errorf("state = %+v", st)
	}
}

// Several qm at once start it once and all have the figure.
func TestAppServerOnce(t *testing.T) {
	setup(t)
	s := newStub(t, "sleep 0.3\n"+serve(usage("", srvWindow(33, 300, time.Now().Add(time.Hour)), "null")))
	setTimeout(t, 10*time.Second)
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			if st, err := Codex(s.look, CodexAsk); err != nil || st.UsedPercent != 33 || st.ReadFailure != "" {
				t.Errorf("state = %+v, %v", st, err)
			}
		})
	}
	wg.Wait()
	if s.starts() != 1 {
		t.Errorf("%d starts, want 1", s.starts())
	}
}

// A qm that holds the lock for too long is not waited for past the timeout,
// and is not remembered as a failure of Codex.
func TestAppServerBusy(t *testing.T) {
	_, state := setup(t)
	setTimeout(t, 300*time.Millisecond)
	if err := os.MkdirAll(filepath.Join(state, "limits"), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(state, "limits", "codex-usage.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	s := newStub(t, serve(usage("", srvWindow(33, 300, time.Now().Add(time.Hour)), "null")))
	start := time.Now()
	st := s.codex(t, CodexAsk)
	if d := time.Since(start); d > 2*time.Second || s.starts() != 0 || !st.Available || st.ReadFailure != failBusy.text() {
		t.Errorf("after %v: %+v, %d starts", d, st, s.starts())
	}
	if _, err := os.Stat(filepath.Join(state, "limits", "codex-usage.json")); err == nil {
		t.Error("a busy lock was remembered as a failure")
	}
}

// Without a place for its lock and cache nothing is started: every command
// would start it.
func TestAppServerNoState(t *testing.T) {
	_, state := setup(t)
	if err := os.MkdirAll(filepath.Join(state, "limits", "codex-usage.lock"), 0o700); err != nil {
		t.Fatal(err)
	}
	s := newStub(t, serve(usage("", srvWindow(33, 300, time.Now().Add(time.Hour)), "null")))
	if st := s.codex(t, CodexAsk); !st.Available || s.starts() != 0 || st.ReadFailure != failState.text() {
		t.Errorf("%+v, %d starts", st, s.starts())
	}
}

// Codex runs outside every project: not in one's folder, without one's git
// variables, and with the same CODEX_HOME when it is a relative one.
func TestAppServerEnvironment(t *testing.T) {
	t.Chdir(t.TempDir())
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	got := codexEnv([]string{"HOME=/h", "PWD=/h/project", "OLDPWD=/h/other", "GIT_DIR=/h/project/.git", "GIT_CONFIG_PARAMETERS='core.worktree=x'",
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.worktree", "GIT_CONFIG_VALUE_0=/h/project", "GIT_SSH_COMMAND=ssh", "CODEX_HOME=codex", "PATH=/bin"})
	want := []string{"PWD=/", "HOME=/h", "GIT_SSH_COMMAND=ssh", "CODEX_HOME=" + filepath.Join(wd, "codex"), "PATH=/bin"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	// And the process has it.
	setup(t)
	t.Setenv("GIT_DIR", "/somewhere/.git")
	s := newStub(t, `env >"$dir/env"`+"\n"+serve(usage("", srvWindow(33, 300, time.Now().Add(time.Hour)), "null")))
	s.codex(t, CodexAsk)
	b, _ := os.ReadFile(filepath.Join(s.dir, "env"))
	if env := "\n" + string(b); strings.Contains(env, "\nGIT_DIR=") || !strings.Contains(env, "\nCODEX_HOME=") {
		t.Errorf("environment: %s", env)
	}
}

func TestAppServerNotInstalled(t *testing.T) {
	_, state := setup(t)
	st, err := Codex(missing, CodexAsk)
	if err != nil || st.Available || st.ReadFailure != "" {
		t.Errorf("%+v, %v", st, err)
	}
	if _, err := os.Stat(state); err == nil {
		t.Error("state written without Codex")
	}
}

// Every failure has qm's own words; one this qm does not know still has some.
func TestUsageFailureText(t *testing.T) {
	for _, f := range []usageFailure{failStart, failEnded, failTimeout, failRefused, failFetch, failShape, failAccount, failBusy, failState, "other"} {
		if text := f.text(); text == "" || strings.Contains(text, "other") && f == "other" {
			t.Errorf("%s: %q", f, text)
		}
	}
	if CodexAsk.If(false) != CodexSessions || CodexAsk.If(true) != CodexAsk {
		t.Error("If")
	}
}
