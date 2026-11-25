package limits

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/paths"
	"github.com/IZZY-Agency/quartermaster/internal/policy"
)

// CodexRead says where Codex may take the usage from.
type CodexRead int

const (
	// CodexSessions reads only the sessions Codex saved.
	CodexSessions CodexRead = iota
	// CodexCached also uses the last answer of Codex itself, and starts
	// nothing.
	CodexCached
	// CodexAsk asks Codex when that answer is too old.
	CodexAsk
)

// If is r when Codex may be asked at all (config codex.usage_read), else
// CodexSessions.
func (r CodexRead) If(on bool) CodexRead {
	if on {
		return r
	}
	return CodexSessions
}

// usageTimeout is policy.CodexUsageTimeout; tests shorten it.
var usageTimeout = policy.CodexUsageTimeout

// usageFailure is why Codex could not be asked, as it is stored.
type usageFailure string

const (
	failStart       usageFailure = "start"
	failEnded       usageFailure = "ended"
	failTimeout     usageFailure = "timeout"
	failRefused     usageFailure = "refused"
	failFetch       usageFailure = "fetch"
	failShape       usageFailure = "shape"
	failAccount     usageFailure = "account"
	failBusy        usageFailure = "busy"
	failState       usageFailure = "state"
	failInterrupted usageFailure = "interrupted"
)

// text is the failure in qm's words. What Codex said is never read: it can
// quote the reply of its backend.
func (f usageFailure) text() string {
	switch f {
	case "":
		return ""
	case failStart:
		return "codex app-server could not be started"
	case failEnded:
		return "codex app-server ended without an answer"
	case failTimeout:
		return "codex app-server did not answer in time"
	case failRefused:
		return "Codex refused to say (an API-key login, or a Codex too old for it)"
	case failFetch:
		return "Codex could not get it (no network, or its login has lapsed)"
	case failShape:
		return "Codex's answer is not one this qm knows"
	case failAccount:
		return "Codex answered for another account than its login"
	case failBusy:
		return "another qm is still asking"
	case failState:
		return "qm's state folder cannot be written"
	}
	return "codex app-server failed"
}

// usageFile is limits/codex-usage.json: the last usage Codex stated, and the
// last failure to ask it when that came after. Account and Member are the
// login auth.json named ("" when it named none), to tell whether it still does;
// Named is, for one that named none, the hash of the account Codex named.
type usageFile struct {
	Account  string        `json:"account,omitempty"`
	Member   string        `json:"member,omitempty"`
	Named    string        `json:"named,omitempty"`
	ReadAt   time.Time     `json:"read_at,omitzero"`
	Windows  []limitWindow `json:"windows,omitempty"`
	Allowed  *bool         `json:"ordinary_usage_allowed"`
	FailedAt time.Time     `json:"failed_at,omitzero"`
	Failure  usageFailure  `json:"failure,omitempty"`

	other bool // the stored reading is another login's, and left out
}

func emptyUsage(login Login) usageFile {
	return usageFile{Account: login.account, Member: login.member}
}

// owner is the login the file is of, as far as it can be told.
func (f usageFile) owner() Login {
	if f.Account == "" {
		return Login{account: f.Named, asked: f.Named != ""}
	}
	return Login{account: f.Account, member: f.Member}
}

// of says whether the file is login's. Where auth.json named no account it is
// that of a login which names none either, or which Codex itself named.
func (f usageFile) of(login Login) bool {
	if f.Account == "" && login.account != "" {
		return f.Named == login.account && login.member == ""
	}
	return f.Account == login.account && !f.owner().Differs(login)
}

func limitsDir() (string, error) {
	dir, err := paths.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "limits"), nil
}

// loadUsage is the file's content for this login at now. One that is missing,
// unreadable or another login's is empty; so is a reading or a failure dated
// after now.
func loadUsage(login Login, now time.Time) usageFile {
	empty := emptyUsage(login)
	dir, err := limitsDir()
	if err != nil {
		return empty
	}
	b, err := os.ReadFile(filepath.Join(dir, "codex-usage.json"))
	if err != nil {
		return empty
	}
	var f usageFile
	if json.Unmarshal(b, &f) != nil {
		return empty
	}
	if !f.of(login) {
		empty.other = !f.ReadAt.IsZero() && f.owner().Differs(login)
		return empty
	}
	valid := !f.ReadAt.After(now) && (len(f.Windows) > 0 || f.Allowed != nil)
	for _, w := range f.Windows {
		valid = valid && w.UsedPercent >= 0 && w.Minutes >= 0 && w.Minutes <= maxWindowMinutes
	}
	if !valid {
		f.ReadAt, f.Windows, f.Allowed = time.Time{}, nil, nil
	}
	if f.FailedAt.After(now) {
		f.FailedAt, f.Failure = time.Time{}, ""
	}
	return f
}

func (f usageFile) save() error {
	dir, err := limitsDir()
	if err != nil {
		return err
	}
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, "codex-usage.json"), append(data, '\n'))
}

// reading is the file's reading, or nil.
func (f usageFile) reading() *reading {
	if f.ReadAt.IsZero() {
		return nil
	}
	return &reading{At: f.ReadAt, Source: SourceAppServer, Windows: f.Windows, Allowed: f.Allowed}
}

// fresh says whether the reading still stands for now: it is at most
// policy.CodexUsageMaxAge old and no window has reset since it was made.
func (f usageFile) fresh(now time.Time) bool {
	if f.ReadAt.IsZero() || now.Sub(f.ReadAt) > policy.CodexUsageMaxAge {
		return false
	}
	for _, w := range f.Windows {
		if w.ResetsAt.After(f.ReadAt) && !w.ResetsAt.After(now) {
			return false
		}
	}
	return true
}

// failure is the failure that still stands at now, or "".
func (f usageFile) failure(now time.Time) usageFailure {
	if f.FailedAt.IsZero() || now.Sub(f.FailedAt) > policy.CodexUsageRetry {
		return ""
	}
	return f.Failure
}

// currentUsage is what Codex, logged in as login at home, last said about its
// usage: with ask and nothing fresh, what it says now. What was asked for
// before since does not count. The failure is why there is nothing fresh, when there was
// a try.
func currentUsage(path, home string, login Login, ask bool, since time.Time) (f usageFile, fresh bool, fail usageFailure) {
	settled := func() bool {
		now := time.Now()
		f = loadUsage(login, now)
		fresh, fail = f.fresh(now) && !f.ReadAt.Before(since), ""
		if !fresh && !f.FailedAt.Before(since) {
			fail = f.failure(now)
		}
		return fresh || fail != "" || !ask
	}
	if settled() {
		return f, fresh, fail
	}
	wait := usageTimeout // the longest the holder of the lock needs to ask
	if !since.IsZero() {
		// Its question may be an earlier one, and the answer that counts that
		// of the next in line.
		wait = 2*usageTimeout + exitGrace
	}
	unlock, fail := lockState("codex-usage.lock", wait)
	if fail != "" {
		if busy := fail; !settled() { // its holder may just have been done
			fail = busy
		}
		return f, fresh, fail
	}
	defer unlock()
	if settled() { // by the qm that held the lock
		return f, fresh, fail
	}
	f, fail = askAndSave(f, login, path, home)
	return f, fail == "", fail
}

// askAndSave asks Codex and stores the answer, or the failure next to the
// last reading of the same login. A file that cannot be written loses no
// answer: it is used, and asked for again the next time. When auth.json already
// names another login than the caller's, nothing is asked or stored.
func askAndSave(last usageFile, login Login, path, home string) (usageFile, usageFailure) {
	before := readLogin(home)
	if before.Differs(login) {
		return emptyUsage(login), failAccount
	}
	at := time.Now() // what it says, and whose login it read, may be from its start
	r, fail := askUsage(path, usageTimeout)
	after := readLogin(home)
	named := newLogin(r.AccountID, "")
	windows, ok := r.windows()
	switch {
	case fail != "":
	case !ok:
		fail = failShape
	case before != after, named.Differs(after), named.Differs(login):
		fail = failAccount
	}
	if fail != "" {
		if !last.of(after) {
			last = emptyUsage(after)
		}
		if fail != failInterrupted { // which is not Codex's
			last.FailedAt, last.Failure = at, fail
			_ = last.save()
		}
		return last, fail
	}
	f := emptyUsage(after)
	f.ReadAt, f.Windows, f.Allowed = at, windows, r.Allowed
	if !after.Known() {
		f.Named = named.account
	}
	_ = f.save()
	return f, ""
}
