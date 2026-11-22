// Package declog appends to and reads the decision log,
// $XDG_STATE_HOME/quartermaster/decisions.jsonl: one route.Record per line.
package declog

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/paths"
	"github.com/IZZY-Agency/quartermaster/internal/route"
)

// Path is the decision log's path.
func Path() (string, error) {
	d, err := paths.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "decisions.jsonl"), nil
}

// Append writes r as one line. The line goes out in a single write to a file
// opened with O_APPEND, so lines from concurrent writers do not interleave.
// When the file does not end with a newline (a write cut short by a full
// disk or a crash), the line starts with one, so the cut line does not
// swallow it. The look at the file's end and the write happen under an
// exclusive lock on the file: without it a writer can see another's record
// half written (the file's size grows before the record is complete) and
// take it for a cut line.
func Append(r route.Record) error {
	path, err := Path()
	if err != nil {
		return fmt.Errorf("decision log: %w", err)
	}
	b, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("decision log: encode record: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("decision log: %w", err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("decision log: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("decision log: %w", err)
	}
	// Closing the file releases the lock.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return fmt.Errorf("decision log: lock: %w", err)
	}
	cut, err := endsMidLine(f)
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("decision log: %w", err)
	}
	b = append(b, '\n')
	if cut {
		b = append([]byte{'\n'}, b...)
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return fmt.Errorf("decision log: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("decision log: %w", err)
	}
	return nil
}

// endsMidLine reports whether f is not empty and its last byte is not a
// newline.
func endsMidLine(f *os.File) (bool, error) {
	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return false, err
	}
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, fi.Size()-1); err != nil {
		return false, err
	}
	return last[0] != '\n', nil
}

// AppendDecision logs a routing decision: rec carries the fields of the
// launch (project, branch, type, size, round, override...); its Kind becomes
// d.Kind, its Decision d, and a zero Time becomes now, in UTC. The spawn
// commands call it when they launch; qm route is a preview and does not.
func AppendDecision(d route.Decision, rec route.Record) error {
	rec.Kind, rec.Decision = d.Kind, &d
	if rec.Time.IsZero() {
		rec.Time = time.Now().UTC()
	}
	return Append(rec)
}

// Read returns the records whose Time is not before since, in file order. A
// missing log is empty. A line that is not a valid record (a write cut short
// by a crash, say) is skipped, so one bad line does not hide the rest.
func Read(since time.Time) ([]route.Record, error) {
	path, err := Path()
	if err != nil {
		return nil, fmt.Errorf("decision log: %w", err)
	}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("decision log: %w", err)
	}
	defer func() { _ = f.Close() }()
	var out []route.Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var r route.Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			continue
		}
		if !r.Time.Before(since) {
			out = append(out, r)
		}
	}
	if err := sc.Err(); err != nil {
		return out, fmt.Errorf("decision log: %w", err)
	}
	return out, nil
}
