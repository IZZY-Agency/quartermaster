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
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("decision log: %w", err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("decision log: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("decision log: %w", err)
	}
	return nil
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
