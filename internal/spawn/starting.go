package spawn

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/declog"
	"github.com/IZZY-Agency/quartermaster/internal/paths"
	"github.com/IZZY-Agency/quartermaster/internal/route"
)

// A reservation holds a worker from before its process is visible until its
// Records are logged: the file <state>/spawning/<session id>.json, with the
// Records, locked (flock) for as long as the qm that starts the worker runs.
// It is a file of its own, not an entry in a project's state (what qm up's
// own starts reserve in): a qm up holds its project's lock, and its state in
// memory, for as long as a question it asks stays open, so a spawn could
// neither wait for that lock nor write without it. No lock is waited for
// here, and a spawn outside a qm up project is held the same way.
type reservation struct {
	path string
	f    *os.File // holds the lock
}

// What the Detail of a worker Record starts with when nothing confirmed that
// its session started: the script's path follows.
const (
	DetailNotConfirmed = "launch not confirmed: "
	DetailMaybeStarted = "launch failed, may have started: "
)

// Unconfirmed tells whether nothing confirmed that the session of worker
// Record r started: it may never have run.
func Unconfirmed(r *route.Record) bool {
	return strings.HasPrefix(r.Detail, DetailNotConfirmed) || strings.HasPrefix(r.Detail, DetailMaybeStarted)
}

// startingDir is <state>/spawning, the folder of the reservations.
func startingDir() (string, error) {
	state, err := paths.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(state, "spawning"), nil
}

// reserveWorker reserves session sid with the Records to log for it.
func reserveWorker(sid string, recs []route.Record) (*reservation, error) {
	if !briefName.MatchString(sid + ".md") {
		return nil, fmt.Errorf("reservation: session id %q is not usable in a file name", sid)
	}
	dir, err := startingDir()
	if err != nil {
		return nil, fmt.Errorf("reservation: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("reservation: %w", err)
	}
	sweepTemp(dir, ScriptAge)
	r := &reservation{path: filepath.Join(dir, sid+".json")}
	if err := r.save(recs); err != nil {
		return nil, fmt.Errorf("reservation: %w", err)
	}
	return r, nil
}

// sweepTemp removes the temporary files in dir (save) older than age.
func sweepTemp(dir string, age time.Duration) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && fi.Mode().IsRegular() && strings.HasSuffix(e.Name(), ".tmp") && time.Since(fi.ModTime()) > age {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// save replaces the reservation's file in one rename, locked before it is
// there to be read.
func (r *reservation) save(recs []route.Record) error {
	b, err := json.Marshal(recs)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(r.path), ".*.tmp") // mode 0600
	if err != nil {
		return err
	}
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		_, err = f.Write(append(b, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	if err == nil {
		err = os.Rename(f.Name(), r.path)
	}
	if err != nil {
		_ = os.Remove(f.Name())
		_ = f.Close()
		return err
	}
	if r.f != nil {
		_ = r.f.Close()
	}
	r.f = f
	return nil
}

// withdraw ends the reservation of a worker that was not started.
func (r *reservation) withdraw() error {
	defer func() { _ = r.f.Close() }()
	return os.Remove(r.path)
}

// log logs the worker's Records and ends the reservation. When one cannot be
// logged, the Records stay reserved for the next qm up to log.
func (r *reservation) log(recs []route.Record) error {
	defer func() { _ = r.f.Close() }()
	for i, rec := range recs {
		if err := declog.Append(rec); err != nil {
			return errors.Join(err, r.save(recs[i:]))
		}
	}
	// A file left here is dropped by the next qm up: its Records are logged.
	_ = os.Remove(r.path)
	return nil
}

// Starting is a worker qm spawn worker reserved and has not logged.
type Starting struct {
	SessionID string
	Records   []route.Record
	// Running: the qm that starts it still runs. Otherwise that qm stopped
	// in the middle, and the worker may run or not.
	Running bool
}

// ListStarting lists the reserved workers, by session id. It changes nothing.
// A reservation that cannot be read is an error: it may hold a running
// worker.
func ListStarting() ([]Starting, error) {
	names, dir, err := startingNames()
	if err != nil {
		return nil, err
	}
	var out []Starting
	for _, n := range names {
		st, f, err := openStarting(filepath.Join(dir, n), syscall.LOCK_SH)
		if err != nil {
			return nil, err
		}
		if f != nil {
			_ = f.Close()
			out = append(out, st)
		}
	}
	return out, nil
}

func startingNames() (names []string, dir string, err error) {
	if dir, err = startingDir(); err != nil {
		return nil, "", fmt.Errorf("reservations: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, "", fmt.Errorf("reservations: %w", err)
	}
	for _, e := range entries {
		if sid, ok := strings.CutSuffix(e.Name(), ".json"); ok && briefName.MatchString(sid+".md") {
			names = append(names, e.Name())
		}
	}
	return names, dir, nil
}

// openStarting reads the reservation at path, with the lock how when its qm
// no longer runs. f is nil when the reservation ended meanwhile, as far as
// path names no file or none that stays the same.
func openStarting(path string, how int) (st Starting, f *os.File, err error) {
	st.SessionID = strings.TrimSuffix(filepath.Base(path), ".json")
	for range 100 {
		f, err = os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if errors.Is(err, fs.ErrNotExist) {
			return st, nil, nil
		}
		if err != nil {
			return st, nil, fmt.Errorf("reservation: %w", err)
		}
		err = syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
		st.Running = errors.Is(err, syscall.EWOULDBLOCK)
		if err != nil && !st.Running {
			_ = f.Close()
			return st, nil, fmt.Errorf("reservation %s: lock: %w", path, err)
		}
		// The file may have been replaced or removed before it was locked.
		mine, err1 := f.Stat()
		now, err2 := os.Lstat(path)
		if err1 == nil && err2 == nil && os.SameFile(mine, now) {
			b, err := io.ReadAll(f)
			if err == nil {
				err = json.Unmarshal(b, &st.Records)
			}
			if err != nil {
				_ = f.Close()
				return st, nil, fmt.Errorf("reservation %s is not readable (remove it when no qm spawn worker runs): %w", path, err)
			}
			return st, f, nil
		}
		_ = f.Close()
		time.Sleep(time.Millisecond)
	}
	return st, nil, fmt.Errorf("reservation %s keeps changing", path)
}

// AddStarting is recs with the reserved Records the log does not have yet.
// st is listed before recs is read: a reservation ends only once its Records
// are logged, so one or the other has them.
func AddStarting(recs []route.Record, st []Starting) []route.Record {
	for _, s := range st {
		recs = append(slices.Clip(recs), unlogged(recs, s.Records)...)
	}
	return recs
}

// unlogged are the Records of reserved that recs does not have.
func unlogged(recs, reserved []route.Record) []route.Record {
	var out []route.Record
	for _, r := range reserved {
		if !slices.ContainsFunc(recs, func(l route.Record) bool {
			if l.Kind != r.Kind {
				return false
			}
			if r.Kind == "worker" {
				return l.ClaudeSession == r.ClaudeSession
			}
			return l.Name == r.Name && l.Time.Equal(r.Time)
		}) {
			out = append(out, r)
		}
	}
	return out
}

// RecoverStarting ends the reservations of the qm that stopped in the middle
// of starting a worker: the Records are logged with log, as for a launch that
// may have started, so the worker is left when it runs. A reservation whose
// qm still runs is left. One whose Records cannot be logged stays; err says
// so.
func RecoverStarting(log func(route.Record) error) (notes []string, err error) {
	names, dir, err := startingNames()
	if err != nil {
		return nil, err
	}
	var errs []error
	for _, n := range names {
		st, f, err := openStarting(filepath.Join(dir, n), syscall.LOCK_EX)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if f == nil {
			continue
		}
		if !st.Running {
			note, err := st.recover(filepath.Join(dir, n), log)
			if err != nil {
				errs = append(errs, err)
			}
			if note != "" {
				notes = append(notes, note)
			}
		}
		_ = f.Close()
	}
	return notes, errors.Join(errs...)
}

// recover logs the Records of st that are not logged and removes its file at
// path, whose lock the caller holds.
func (st Starting) recover(path string, log func(route.Record) error) (note string, err error) {
	recs, err := declog.Read(time.Time{})
	if err != nil {
		return "", fmt.Errorf("reservation %s: %w", path, err)
	}
	for _, r := range unlogged(recs, st.Records) {
		if err := log(r); err != nil {
			return "", fmt.Errorf("worker %s: its Record (session %.8s) was not logged: %w", r.Name, st.SessionID, err)
		}
		if r.Kind == "worker" {
			note = fmt.Sprintf("worker %s: an earlier qm spawn worker stopped while starting session %.8s: its Record is logged now", r.Name, st.SessionID)
		}
	}
	if err := os.Remove(path); err != nil {
		return note, fmt.Errorf("reservation: %w", err)
	}
	return note, nil
}
