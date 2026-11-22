package hooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/paths"
)

// Turn is a worker session's turn as its turn hooks recorded it. Known is
// false when no turn hook ran for the session. At is when the turn last
// opened (Open) or ended; PID is the claude process that recorded it.
type Turn struct {
	Known bool
	Open  bool
	At    time.Time
	PID   int
}

// turnMark is one turn file: <state>/turns/<session id>.start or .stop,
// written by that event's hook only, so the two never race each other.
type turnMark struct {
	At  int64 `json:"at"` // Unix nanoseconds
	PID int   `json:"pid"`
}

// turnStart records an open turn (UserPromptSubmit).
func turnStart(c *call) (result, error) { return result{}, writeTurn(c.in.SessionID, "start") }

// turnStop records the turn as ended (Stop).
func turnStop(c *call) (result, error) { return result{}, writeTurn(c.in.SessionID, "stop") }

// writeTurn writes the session's start or stop file atomically. Only worker
// sessions (QM_ROLE=worker) record turns. A mark older than the one on disk
// (a hook that ran late) is dropped. The compare and the replace are one step
// under the file's own small lock, never the session-state lock (a turn hook
// must not wait for the slower hooks), so two writers cannot both read the
// same old mark and let the older one land last.
func writeTurn(sessionID, event string) error {
	if os.Getenv("QM_ROLE") != "worker" {
		return nil
	}
	path, err := turnPath(sessionID, event)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("turn: %w", err)
	}
	m := turnMark{At: clock().UnixNano(), PID: claudePID()}
	lock, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("turn: %w", err)
	}
	defer func() { _ = lock.Close() }() // closing the file releases the lock
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("turn: lock: %w", err)
	}
	old, ok, err := readTurnMark(path)
	switch {
	case err == nil && ok && old.At > m.At:
		return nil
	case err == nil && !ok && event == "start":
		// A new session: sweep the turns of sessions long gone.
		sweepOld(dir, stateAge)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("turn: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".turn-*")
	if err != nil {
		return fmt.Errorf("turn: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("turn: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("turn: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("turn: %w", err)
	}
	return nil
}

// claudePID is the claude process the hook runs for: CLAUDE_PID when set,
// else the hook's parent (the hook command execs qm).
func claudePID() int {
	if pid, err := strconv.Atoi(os.Getenv("CLAUDE_PID")); err == nil && pid > 0 {
		return pid
	}
	return os.Getppid()
}

func turnPath(sessionID, event string) (string, error) {
	if !sessionIDPattern.MatchString(sessionID) {
		return "", fmt.Errorf("turn: bad session id %q", sessionID)
	}
	dir, err := paths.StateDir()
	if err != nil {
		return "", fmt.Errorf("turn: %w", err)
	}
	return filepath.Join(dir, "turns", sessionID+"."+event), nil
}

func readTurnMark(path string) (turnMark, bool, error) {
	var m turnMark
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return m, false, nil
	}
	if err != nil {
		return m, false, fmt.Errorf("turn: %w", err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, false, fmt.Errorf("turn %s: %w", filepath.Base(path), err)
	}
	return m, true, nil
}

// ReadTurn reads a session's turn; Known is false when no turn hook ran.
// It takes no lock and writes nothing.
func ReadTurn(sessionID string) (Turn, error) {
	var marks [2]turnMark
	var found [2]bool
	for i, event := range []string{"start", "stop"} {
		path, err := turnPath(sessionID, event)
		if err != nil {
			return Turn{}, err
		}
		if marks[i], found[i], err = readTurnMark(path); err != nil {
			return Turn{}, err
		}
	}
	start, stop := marks[0], marks[1]
	switch {
	case !found[0] && !found[1]:
		return Turn{}, nil
	case found[0] && (!found[1] || start.At > stop.At):
		return Turn{Known: true, Open: true, At: time.Unix(0, start.At), PID: start.PID}, nil
	}
	return Turn{Known: true, At: time.Unix(0, stop.At), PID: stop.PID}, nil
}
