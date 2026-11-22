package hooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/paths"
)

// State is one worker session's hook state, kept in
// <state>/sessions/<session_id>.json.
type State struct {
	// Reads counts exploration reads in the current phase.
	Reads int `json:"reads"`
	// Phase counts the phases so far; a Write or Edit starts the next one.
	Phase int `json:"phase"`
	// Denials and Soft are per rule and reset with each phase.
	Denials map[string]int  `json:"denials"`
	Soft    map[string]bool `json:"soft"`
	// SoftAt is when each rule softened, Unix seconds; softening ends
	// softenFor after it, for sessions whose phases never move (a
	// coordinator has no read-budget, so no Write starts a phase there).
	SoftAt map[string]int64 `json:"soft_at,omitempty"`
	// LastNoteCtx is the context size at the last context-limit note.
	LastNoteCtx int `json:"last_note_ctx"`
}

// newPhase starts the next phase: reads, denials and softening reset.
func (s *State) newPhase() {
	s.Phase++
	s.Reads = 0
	s.Denials = map[string]int{}
	s.Soft = map[string]bool{}
	s.SoftAt = map[string]int64{}
}

// stateAge is how long a session's state file outlives its last change.
const stateAge = 7 * 24 * time.Hour

var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// withState loads the session's state under an exclusive flock, calls fn, and
// saves the state when fn succeeds. Concurrent hooks of one session (two
// PreToolUse hooks on one Bash call, say) see each other's updates.
func withState(sessionID string, fn func(*State) error) (err error) {
	if !sessionIDPattern.MatchString(sessionID) {
		return fmt.Errorf("session state: bad session id %q", sessionID)
	}
	dir, err := paths.StateDir()
	if err != nil {
		return fmt.Errorf("session state: %w", err)
	}
	dir = filepath.Join(dir, "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("session state: %w", err)
	}
	path := filepath.Join(dir, sessionID+".json")
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		// A new session: sweep the state of sessions long gone.
		sweepOld(dir, stateAge)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("session state: %w", err)
	}
	defer func() {
		if cerr := f.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("session state: %w", cerr)
		}
	}()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("session state: lock: %w", err)
	}
	// Closing the file releases the lock.
	b, err := io.ReadAll(f)
	if err != nil {
		return fmt.Errorf("session state: %w", err)
	}
	var s State
	if len(b) > 0 {
		if err := json.Unmarshal(b, &s); err != nil {
			// A state cut short (a hook stopped mid-write) must not turn the
			// rules off for the rest of the session: log it, start fresh.
			logRecord(Input{SessionID: sessionID}, "error", "session-state", "", fmt.Sprintf("bad state replaced: %v", err))
			s = State{}
		}
	}
	if s.Denials == nil {
		s.Denials = map[string]int{}
	}
	if s.Soft == nil {
		s.Soft = map[string]bool{}
	}
	if s.SoftAt == nil {
		s.SoftAt = map[string]int64{}
	}
	if err := fn(&s); err != nil {
		return err
	}
	out, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("session state: %w", err)
	}
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("session state: %w", err)
	}
	if _, err := f.WriteAt(out, 0); err != nil {
		return fmt.Errorf("session state: %w", err)
	}
	return nil
}
