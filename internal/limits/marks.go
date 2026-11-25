package limits

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/policy"
)

// A mark records a Codex usage-limit error: limits/codex-limited-<who>.json
// (0600) with the reset time, one for each login that hit its limit. Who is
// the login's hashes, <account> or <account>-<member>, or markUnknown. A mark
// of a login that could not be told counts for every login. oldMark is the one
// mark an earlier qm kept for all of them; it is read, and removed once its
// reset has passed, and not written.
const (
	markPrefix  = "codex-limited-"
	markUnknown = "unknown"
	oldMark     = "codex-limited.json"
	marksLock   = "codex-limited.lock"
)

// codexLimited is a mark's content. Account is set in oldMark only: the id
// (not a hash) of the account that hit the limit, when known.
type codexLimited struct {
	ResetsAt time.Time `json:"resets_at"`
	Account  string    `json:"account,omitempty"`
}

func markName(login Login) string {
	who := login.account
	switch {
	case who == "":
		who = markUnknown
	case login.member != "":
		who += "-" + login.member
	}
	return markPrefix + who + ".json"
}

// markLogin is the login a mark's file name is for; ok is false for a file
// that is no mark.
func markLogin(name string) (login Login, ok bool) {
	if name == oldMark {
		return Login{}, true
	}
	who, ok := strings.CutPrefix(name, markPrefix)
	if !ok {
		return Login{}, false
	}
	if who, ok = strings.CutSuffix(who, ".json"); !ok {
		return Login{}, false
	}
	if who == markUnknown {
		return Login{}, true
	}
	login.account, login.member, _ = strings.Cut(who, "-")
	return login, login.account != ""
}

// MarkCodexLimited records a Codex usage-limit error: Codex is unavailable to
// login, taken before the run that ended on it, until resetsAt. A zero
// resetsAt (the error stated no reset time) means until the limit window of
// login's newest rollout resets, or for policy.CodexLimitedFallback when that
// is unknown or past.
func MarkCodexLimited(login Login, resetsAt time.Time) error {
	dir, err := limitsDir()
	if err != nil {
		return err
	}
	now := time.Now()
	if resetsAt.IsZero() {
		resetsAt = windowResetOrFallback(login, now)
	}
	data, err := json.Marshal(codexLimited{ResetsAt: resetsAt})
	if err != nil {
		return err
	}
	// The lock is for pruneMarks; a mark is written without it all the same.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if unlock, fail := lockState(marksLock, usageTimeout); fail == "" {
		defer unlock()
	}
	if err := writeAtomic(filepath.Join(dir, markName(login)), append(data, '\n')); err != nil {
		return markError(dir, "written", err)
	}
	return nil
}

// markError is err without the name of the mark, which holds the login's
// hashes: the error is logged and shown.
func markError(dir, done string, err error) error {
	var pathErr *fs.PathError
	var linkErr *os.LinkError
	switch {
	case errors.As(err, &pathErr):
		err = pathErr.Err
	case errors.As(err, &linkErr):
		err = linkErr.Err
	}
	return fmt.Errorf("a Codex limit mark in %s cannot be %s: %w", dir, done, err)
}

// codexLimitedUntil is the latest reset of the marks that count for login, or
// zero, and the reason to give while it holds. Marks whose reset has passed
// are removed, whoever's they are.
func codexLimitedUntil(login Login, now time.Time) (until time.Time, why string, err error) {
	dir, err := limitsDir()
	if err != nil {
		return time.Time{}, "", err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, "", nil
	}
	if err != nil {
		return time.Time{}, "", err
	}
	var over []string
	for _, e := range entries {
		owner, ok := markLogin(e.Name())
		if !ok || e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name())
		reset, stated, torn, err := readMark(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue // removed meanwhile
		}
		if stated.Known() {
			owner = stated
		}
		if err != nil {
			if owner.Differs(login) {
				continue // another login's, so its state changes nothing here
			}
			return time.Time{}, "", markError(dir, "read", err)
		}
		switch {
		case !reset.After(now):
			over = append(over, path)
		case !owner.Differs(login) && reset.After(until):
			until, why = reset, "Codex usage limit reached"
			if torn {
				why += " (its mark in " + dir + " is unreadable)"
			}
		}
	}
	pruneMarks(over, now)
	return until, why, nil
}

// readMark is a mark's reset time and the login its content names (oldMark).
// One that cannot be parsed (torn) counts for policy.CodexLimitedFallback from
// its mtime, so it neither fails every call nor keeps Codex off for good.
func readMark(path string) (reset time.Time, login Login, torn bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, Login{}, false, err
	}
	var l codexLimited
	if json.Unmarshal(data, &l) == nil {
		return l.ResetsAt, newLogin(l.Account, ""), false, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}, Login{}, false, err
	}
	return info.ModTime().Add(policy.CodexLimitedFallback), Login{}, true, nil
}

// pruneMarks removes the marks whose reset has passed, read again under the
// lock that MarkCodexLimited writes under. When that is held, a later call
// removes them.
func pruneMarks(files []string, now time.Time) {
	if len(files) == 0 {
		return
	}
	unlock, fail := lockState(marksLock, 0)
	if fail != "" {
		return
	}
	defer unlock()
	for _, path := range files {
		if reset, _, _, err := readMark(path); err == nil && !reset.After(now) {
			_ = os.Remove(path)
		}
	}
}

// lockState takes the lock of this name in the limits folder, waiting for at
// most wait. The lock is free once its process is gone.
func lockState(name string, wait time.Duration) (unlock func(), fail usageFailure) {
	dir, err := limitsDir()
	if err == nil {
		err = os.MkdirAll(dir, 0o700)
	}
	if err != nil {
		return nil, failState
	}
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, failState
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = f.Close() }, ""
		}
		fail := failBusy
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			fail = failState
		}
		select {
		case <-ctx.Done():
		case <-time.After(10 * time.Millisecond):
			if fail == failBusy {
				continue
			}
		}
		_ = f.Close()
		return nil, fail
	}
}
