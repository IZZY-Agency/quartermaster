package limits

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// ClaudeFile is the content of limits/claude.json, written by the status-line
// wrapper (qm statusline) and read by the Claude limit reader. A limit the
// status line did not report is nil.
type ClaudeFile struct {
	FiveHourUsed     *float64   `json:"five_hour_used"`
	SevenDayUsed     *float64   `json:"seven_day_used"`
	FiveHourResetsAt *time.Time `json:"five_hour_resets_at"`
	SevenDayResetsAt *time.Time `json:"seven_day_resets_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

// WriteClaudeFile writes f to path (mode 0600, folder 0700) through a
// temporary file, so a reader never sees a partial file.
func WriteClaudeFile(path string, f ClaudeFile) error {
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return writeAtomic(path, append(data, '\n'))
}

// writeAtomic writes data to path (mode 0600, folder 0700) through a
// temporary file in the same folder, renamed into place.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
