package limits

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/lines"
	"github.com/IZZY-Agency/quartermaster/internal/policy"
)

// Codex says whether Codex can take a review now (design 5.4). look finds a
// command on PATH (exec.LookPath) and read says where its usage may be taken
// from: the answer of Codex itself while it is fresh, else the newer of the
// last one and the newest rollout. Codex is available when it is installed, no
// limit window of that reading is at policy.CodexLimitPercent or over (a
// window that has reset since does not count), a fresh answer does not say
// that ordinary usage is not allowed, and no usage-limit error with a future
// reset was recorded by MarkCodexLimited: no answer lifts that one early.
// Paid credits are ignored. Installed with no usage data at all counts as
// available, and a failure to ask Codex changes nothing but ReadFailure.
//
// Limits belong to a login (CodexLogin): a rollout, or a recorded usage-limit
// error, of another login than the one Codex has now is left out. One that
// names no account, or a login that names none, counts as before. An answer of
// Codex counts only for the login it was given under.
func Codex(look func(string) (string, error), read CodexRead) (CodexState, error) {
	path, err := look("codex")
	if err != nil {
		return CodexState{Reason: "codex not installed"}, nil
	}
	return CodexFor(CodexLogin(path, CodexSessions), path, read)
}

// CodexFor is Codex for this login of the codex at path, as CodexLogin told it.
func CodexFor(login Login, path string, read CodexRead) (CodexState, error) {
	home, err := codexHome()
	if err != nil {
		return CodexState{}, err
	}
	var r *reading
	var fresh, left bool
	var fail usageFailure
	if read != CodexSessions {
		var f usageFile
		f, fresh, fail = currentUsage(path, home, login, read == CodexAsk, time.Time{})
		r, left = f.reading(), f.other
		if fresh && !login.Known() {
			login = f.owner() // the login it was given under
		}
	}
	now := time.Now()
	if !fresh {
		rollout, skipped, err := latestRollout(login, now)
		if err != nil {
			return CodexState{}, err
		}
		left = left || skipped
		if r == nil || (rollout != nil && rollout.At.After(r.At)) {
			r = rollout
		}
	}
	st, full := r.state(now)
	st.ReadFailure = fail.text()
	switch {
	case !login.Known():
		st.AccountNote = "the login cannot be told, so a figure or a limit may be another account's"
	case r == nil && left:
		st.AccountNote = "the last figure read is another account's and is not used"
	case r != nil && r.Unowned:
		st.AccountNote = "the session files do not say which account the figure is of"
	}
	until, why, err := codexLimitedUntil(login, now)
	if err != nil {
		return CodexState{}, err
	}
	if until.After(now) {
		st.ResetsAt, st.Limited = until, true
		st.Reason = why + "; resets " + until.Format(time.DateTime)
		if !st.UsageKnown {
			st.Reason += " (usage unknown)"
		}
		return st, nil
	}
	switch {
	case fresh && r.Allowed != nil && !*r.Allowed:
		st.Reason = "Codex says ordinary usage is not allowed now"
	case r == nil:
		st.Available, st.Reason = true, "no usage data yet (read from your own Codex sessions; qm reviews save none)"
	case len(r.Windows) == 0:
		st.Available, st.Reason = true, "Codex states no usage limit"
	case !st.UsageKnown:
		st.Available, st.Reason = true, "limit window has reset since the last Codex reading"
	case full:
		st.Reason = fmt.Sprintf("Codex limit at %g%%", st.UsedPercent)
		if st.Window != "" {
			st.Reason += " (" + st.Window + ")"
		}
	default:
		st.Available = true
	}
	return st, nil
}

func codexHome() (string, error) {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("CODEX_HOME is unset and there is no home folder: %w", err)
	}
	return filepath.Join(home, ".codex"), nil
}

// maxRollouts bounds how many rollouts latestRollout reads looking for
// rate-limit data, so a history with none is not read whole on every call.
const maxRollouts = 50

// latestRollout is the last reading in the newest rollout under
// $CODEX_HOME/sessions that has one and is not of another account than
// login's, or nil; left says whether one was left out for that. A rollout
// names its account, not the member. Folders are visited newest name first
// (sessions/YYYY/MM/DD), and the rollouts in a folder newest mtime first; the
// walk stops at the first such rollout with rate-limit data.
func latestRollout(login Login, now time.Time) (found *reading, left bool, err error) {
	home, err := codexHome()
	if err != nil {
		return nil, false, err
	}
	read := 0
	err = visitRollouts(filepath.Join(home, "sessions"), func(path string) (bool, error) {
		read++
		r, account, err := lastReading(path, now)
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil // removed while walking
		}
		if err != nil {
			return true, err
		}
		if r != nil {
			if newLogin(account, "").Differs(login) {
				r, left = nil, true
			} else {
				r.Unowned = account == ""
			}
		}
		found = r
		return r != nil || read >= maxRollouts, nil
	})
	return found, left, err
}

// visitRollouts calls fn on the rollout-*.jsonl files under dir: the files of
// a folder newest mtime first, then its subfolders in reverse name order.
// fn returns true to stop. A missing dir has no rollouts.
func visitRollouts(dir string, fn func(path string) (bool, error)) error {
	_, err := visitDir(dir, fn)
	return err
}

func visitDir(dir string, fn func(path string) (bool, error)) (bool, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	type rollout struct {
		path  string
		mtime time.Time
	}
	var files []rollout
	var dirs []string
	for _, e := range entries {
		switch {
		case e.IsDir():
			dirs = append(dirs, e.Name())
		case strings.HasPrefix(e.Name(), "rollout-") && strings.HasSuffix(e.Name(), ".jsonl"):
			info, err := e.Info()
			if err != nil {
				continue // removed while walking
			}
			files = append(files, rollout{filepath.Join(dir, e.Name()), info.ModTime()})
		}
	}
	slices.SortFunc(files, func(a, b rollout) int { return b.mtime.Compare(a.mtime) })
	for _, f := range files {
		if stop, err := fn(f.path); stop || err != nil {
			return true, err
		}
	}
	slices.Sort(dirs)
	slices.Reverse(dirs)
	for _, d := range dirs {
		if stop, err := visitDir(filepath.Join(dir, d), fn); stop || err != nil {
			return true, err
		}
	}
	return false, nil
}

// rolloutWindow is a limit window as a rollout line has it.
type rolloutWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes *int64  `json:"window_minutes"`
	ResetsAt      *int64  `json:"resets_at"`
}

type rolloutLine struct {
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Payload   struct {
		Type string `json:"type"`
		// CreatorAccountID is on the session_meta line: the account the
		// session ran under.
		CreatorAccountID string `json:"creator_account_id"`
		RateLimits       *struct {
			// LimitID names the metered bucket. Codex versions that wrote
			// none had only the one, so none counts as codexBucket.
			LimitID   string         `json:"limit_id"`
			Primary   *rolloutWindow `json:"primary"`
			Secondary *rolloutWindow `json:"secondary"`
		} `json:"rate_limits"`
	} `json:"payload"`
}

// reading is the limit reading of a token_count line; nil for any other line,
// one of another bucket, or one with no window.
func (l rolloutLine) reading() *reading {
	rl := l.Payload.RateLimits
	if l.Type != "event_msg" || l.Payload.Type != "token_count" || rl == nil || (rl.LimitID != "" && rl.LimitID != codexBucket) {
		return nil
	}
	r := &reading{Source: SourceSessions}
	for _, w := range []*rolloutWindow{rl.Primary, rl.Secondary} {
		if w != nil {
			r.Windows = append(r.Windows, newWindow(w.UsedPercent, w.WindowMinutes, w.ResetsAt))
		}
	}
	if len(r.Windows) == 0 {
		return nil
	}
	if at, err := time.Parse(time.RFC3339, l.Timestamp); err == nil {
		r.At = at
	}
	return r
}

// maxRolloutLine is the longest line of a rollout lastReading reads; a
// longer one is skipped, so one line cannot take all of memory. The lines it
// looks for, session_meta and token_count, are far shorter.
const maxRolloutLine = 16 << 20

// lastReading is the reading of the last token_count event that has one, or
// nil, and the account the rollout's session_meta line names ("" when it names
// none). A reading whose line states no time, or one after now, has the
// file's. Lines longer than maxRolloutLine and torn lines are skipped. The
// session_meta is the first line, as Codex writes it. A rollout whose first
// line was not read as one has no reading, whatever a later line says: whose
// it is cannot be told, and it must not pass as one that names no account.
func lastReading(path string, now time.Time) (last *reading, account string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = f.Close() }()
	metaRead := false
	err = lines.Each(bufio.NewReaderSize(f, 64*1024), maxRolloutLine, func(n int, line []byte) {
		if n == 1 && bytes.Contains(line, []byte(`"session_meta"`)) {
			var l rolloutLine
			if json.Unmarshal(line, &l) == nil && l.Type == "session_meta" {
				account, metaRead = l.Payload.CreatorAccountID, true
			}
		}
		if bytes.Contains(line, []byte(`"token_count"`)) {
			var l rolloutLine
			if json.Unmarshal(line, &l) == nil {
				if got := l.reading(); got != nil {
					last = got
				}
			}
		}
	})
	if err != nil {
		return nil, "", err
	}
	if !metaRead {
		return nil, "", nil
	}
	if last != nil && (last.At.IsZero() || last.At.After(now)) {
		last.At = time.Time{}
		if info, err := f.Stat(); err == nil && !info.ModTime().After(now) {
			last.At = info.ModTime()
		}
	}
	return last, account, nil
}

// windowResetOrFallback is when the limit that a usage-limit error without a
// reset time hit should be over, by login's newest rollout: the reset of its
// highest window when that one is full, else the soonest reset (the cost of
// too early is one more refused run, of too late a week without Codex), else
// now plus policy.CodexLimitedFallback. Only resets the rollout states, and
// only future ones, count; a rollout that cannot be read counts as unknown.
func windowResetOrFallback(login Login, now time.Time) time.Time {
	fallback := now.Add(policy.CodexLimitedFallback)
	r, _, err := latestRollout(login, now)
	if err != nil || r == nil {
		return fallback
	}
	if top, live := r.top(now); live && top.UsedPercent >= policy.CodexLimitPercent && top.ResetsAt.After(now) {
		return top.ResetsAt
	}
	var soonest time.Time
	for _, w := range r.Windows {
		if w.ResetsAt.After(now) && (soonest.IsZero() || w.ResetsAt.Before(soonest)) {
			soonest = w.ResetsAt
		}
	}
	if soonest.IsZero() {
		return fallback
	}
	return soonest
}

// limitErrRe matches the Codex CLI's usage-limit message; resetRe its reset
// time, as in "try again at Nov 23rd, 2026 9:02 AM".
var (
	limitErrRe = regexp.MustCompile(`(?i)you[’']ve hit your usage limit`)
	resetRe    = regexp.MustCompile(`(?i)try again at ([a-z]+) (\d{1,2})(?:st|nd|rd|th)?,? (\d{4}),? (\d{1,2}:\d{2}) ?([ap]m)`)
)

// ParseCodexLimitError reports whether output is the Codex CLI's usage-limit
// error and reads its reset time, in local time. The time is zero when the
// error states none it can read; MarkCodexLimited takes that zero as is.
func ParseCodexLimitError(output string) (time.Time, bool) {
	if !limitErrRe.MatchString(output) {
		return time.Time{}, false
	}
	m := resetRe.FindStringSubmatch(output)
	if m == nil {
		return time.Time{}, true
	}
	s := strings.Join([]string{m[1], m[2] + ",", m[3], m[4], strings.ToUpper(m[5])}, " ")
	for _, layout := range []string{"Jan 2, 2006 3:04 PM", "January 2, 2006 3:04 PM"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, true
}
