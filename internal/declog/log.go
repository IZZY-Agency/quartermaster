// Package declog appends to and reads the decision log,
// $XDG_STATE_HOME/quartermaster/decisions.jsonl: one route.Record per line.
package declog

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
	"syscall"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/paths"
	"github.com/IZZY-Agency/quartermaster/internal/route"
)

// maxLine is the longest line Read takes for a record; a longer one is
// skipped.
const maxLine = 16 << 20

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
	// O_NOFOLLOW: a log that is a symbolic link is refused, not written
	// through.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
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
// missing log is empty; a log that is a symbolic link is an error. A line
// that is not a valid record (a write cut short by a crash, say) or is
// longer than maxLine is skipped, so one bad line does not hide the rest.
func Read(since time.Time) ([]route.Record, error) {
	path, err := Path()
	if err != nil {
		return nil, fmt.Errorf("decision log: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("decision log: %w", err)
	}
	defer func() { _ = f.Close() }()
	var out []route.Record
	err = eachLine(bufio.NewReaderSize(f, 64*1024), func(line []byte) {
		var r route.Record
		if json.Unmarshal(line, &r) == nil && !r.Time.Before(since) {
			out = append(out, r)
		}
	})
	if err != nil {
		return out, fmt.Errorf("decision log: %w", err)
	}
	return out, nil
}

// eachLine calls fn with each line rd holds, its newline included, and
// skips the lines longer than maxLine (the newline aside) without holding
// them in memory.
func eachLine(rd *bufio.Reader, fn func([]byte)) error {
	var line []byte
	long := false
	for {
		chunk, err := rd.ReadSlice('\n')
		if !long && len(line)+len(chunk) > maxLine+1 {
			long, line = true, nil
		}
		if !long {
			line = append(line, chunk...)
		}
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case err != nil && !errors.Is(err, io.EOF):
			return err
		}
		if !long && len(bytes.TrimSuffix(line, []byte("\n"))) <= maxLine && len(line) > 0 {
			fn(line)
		}
		if err != nil {
			return nil
		}
		line, long = line[:0], false
	}
}
