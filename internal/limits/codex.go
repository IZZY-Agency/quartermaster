package limits

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/paths"
	"github.com/IZZY-Agency/quartermaster/internal/policy"
)

// Codex says whether Codex can take a review now (design 5.4). look finds a
// command on PATH (exec.LookPath). Codex is available when it is installed,
// the newest rollout's last primary limit is under policy.CodexLimitPercent
// (or its window has reset since), and no usage-limit error with a future
// reset was recorded by MarkCodexLimited. Paid credits are ignored. Installed
// with no usage data at all counts as available.
//
// Limits belong to an account: a rollout, or a recorded usage-limit error,
// of another account than the one Codex is logged in to now (auth.json) is
// not this account's reading and is left out. One that names no account, or
// a login that names none, counts as before.
func Codex(look func(string) (string, error)) (CodexState, error) {
	if _, err := look("codex"); err != nil {
		return CodexState{Reason: "codex not installed"}, nil
	}
	now := time.Now()
	p, err := latestPrimary()
	if err != nil {
		return CodexState{}, err
	}
	var st CodexState
	if p != nil {
		st.UsedPercent, st.UsageKnown = p.UsedPercent, true
		if p.ResetsAt != nil {
			st.ResetsAt = time.Unix(*p.ResetsAt, 0)
		}
	}
	until, why, err := codexLimitedUntil()
	if err != nil {
		return CodexState{}, err
	}
	if until.After(now) {
		st.ResetsAt = until
		st.Reason = why + "; resets " + until.Format(time.DateTime)
		if !st.UsageKnown {
			st.Reason += " (usage unknown)"
		}
		return st, nil
	}
	switch {
	case p == nil:
		st.Available, st.Reason = true, "no usage data yet (read from your own Codex sessions; qm reviews save none)"
	case p.UsedPercent < policy.CodexLimitPercent:
		st.Available = true
	case !st.ResetsAt.IsZero() && !st.ResetsAt.After(now):
		st.Available = true
		st.Reason = "limit window has reset since the last Codex session"
	default:
		st.Reason = fmt.Sprintf("Codex limit at %g%%", p.UsedPercent)
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

// maxRollouts bounds how many rollouts latestPrimary reads looking for
// rate-limit data, so a history with none is not read whole on every call.
const maxRollouts = 50

// latestPrimary is the last primary limit in the newest rollout under
// $CODEX_HOME/sessions that has one and belongs to the account Codex is
// logged in to, or nil. Folders are visited newest name first
// (sessions/YYYY/MM/DD), and the rollouts in a folder newest mtime first; the
// walk stops at the first such rollout with rate-limit data.
func latestPrimary() (*primaryLimit, error) {
	home, err := codexHome()
	if err != nil {
		return nil, err
	}
	account := currentAccount(home)
	var found *primaryLimit
	read := 0
	err = visitRollouts(filepath.Join(home, "sessions"), func(path string) (bool, error) {
		read++
		p, owner, err := lastPrimary(path)
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil // removed while walking
		}
		if err != nil {
			return true, err
		}
		if p != nil && otherAccount(owner, account) {
			p = nil
		}
		found = p
		return p != nil || read >= maxRollouts, nil
	})
	return found, err
}

// currentAccount is the account id Codex is logged in to: tokens.account_id
// of $CODEX_HOME/auth.json, or "" when the file is missing, unreadable or
// names none (an API-key login). Nothing else of the file is used.
func currentAccount(home string) string {
	data, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if err != nil {
		return ""
	}
	var a struct {
		Tokens struct {
			AccountID string `json:"account_id"`
		} `json:"tokens"`
	}
	if json.Unmarshal(data, &a) != nil {
		return ""
	}
	return a.Tokens.AccountID
}

// otherAccount reports whether a reading made under account owner is not
// the current account's: both are known and they differ.
func otherAccount(owner, current string) bool {
	return owner != "" && current != "" && owner != current
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

type primaryLimit struct {
	UsedPercent float64 `json:"used_percent"`
	ResetsAt    *int64  `json:"resets_at"`
}

type rolloutLine struct {
	Type    string `json:"type"`
	Payload struct {
		Type string `json:"type"`
		// CreatorAccountID is on the session_meta line: the account the
		// session ran under.
		CreatorAccountID string `json:"creator_account_id"`
		RateLimits       *struct {
			Primary *primaryLimit `json:"primary"`
		} `json:"rate_limits"`
	} `json:"payload"`
}

// lastPrimary is the primary limit of the last token_count event that has
// one, or nil, and the account the rollout's session_meta line names ("" when
// it names none). Lines are read whole, however long; torn lines are skipped.
func lastPrimary(path string) (last *primaryLimit, account string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if account == "" && bytes.Contains(line, []byte(`"session_meta"`)) {
			var l rolloutLine
			if json.Unmarshal(line, &l) == nil && l.Type == "session_meta" {
				account = l.Payload.CreatorAccountID
			}
		}
		if bytes.Contains(line, []byte(`"token_count"`)) {
			var l rolloutLine
			if json.Unmarshal(line, &l) == nil && l.Type == "event_msg" && l.Payload.Type == "token_count" &&
				l.Payload.RateLimits != nil && l.Payload.RateLimits.Primary != nil {
				last = l.Payload.RateLimits.Primary
			}
		}
		if errors.Is(err, io.EOF) {
			return last, account, nil
		}
		if err != nil {
			return nil, "", err
		}
	}
}

type codexLimited struct {
	ResetsAt time.Time `json:"resets_at"`
	// Account is the Codex account that hit the limit, when known.
	Account string `json:"account,omitempty"`
}

func codexLimitedPath() (string, error) {
	dir, err := paths.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "limits", "codex-limited.json"), nil
}

// MarkCodexLimited records a Codex usage-limit error: Codex is unavailable
// until resetsAt. A zero resetsAt (the error stated no reset time) means until
// the newest rollout's limit window resets, or for policy.CodexLimitedFallback
// when that is unknown or past. It writes limits/codex-limited.json (mode
// 0600).
func MarkCodexLimited(resetsAt time.Time) error {
	path, err := codexLimitedPath()
	if err != nil {
		return err
	}
	if resetsAt.IsZero() {
		resetsAt = windowResetOrFallback(time.Now())
	}
	account := ""
	if home, err := codexHome(); err == nil {
		account = currentAccount(home)
	}
	data, err := json.Marshal(codexLimited{ResetsAt: resetsAt, Account: account})
	if err != nil {
		return err
	}
	return writeAtomic(path, append(data, '\n'))
}

// windowResetOrFallback is the newest rollout's primary limit reset when it
// is after now, else now plus policy.CodexLimitedFallback. A rollout that
// cannot be read counts as unknown.
func windowResetOrFallback(now time.Time) time.Time {
	fallback := now.Add(policy.CodexLimitedFallback)
	p, err := latestPrimary()
	if err != nil || p == nil || p.ResetsAt == nil {
		return fallback
	}
	if reset := time.Unix(*p.ResetsAt, 0); reset.After(now) {
		return reset
	}
	return fallback
}

// codexLimitedUntil is the reset time MarkCodexLimited recorded, or zero
// (also when it was recorded for another account than the current one),
// and the reason to give while it holds. A file that cannot be parsed counts
// as limited for policy.CodexLimitedFallback from its mtime, so a corrupt
// file neither fails every call nor keeps Codex off for good.
func codexLimitedUntil() (time.Time, string, error) {
	path, err := codexLimitedPath()
	if err != nil {
		return time.Time{}, "", err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, "", nil
	}
	if err != nil {
		return time.Time{}, "", err
	}
	var l codexLimited
	if json.Unmarshal(data, &l) == nil {
		if home, err := codexHome(); err == nil && otherAccount(l.Account, currentAccount(home)) {
			return time.Time{}, "", nil // another account's limit
		}
		return l.ResetsAt, "Codex usage limit reached", nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}, "", err
	}
	return info.ModTime().Add(policy.CodexLimitedFallback), "Codex usage limit reached (" + path + " is unreadable)", nil
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
