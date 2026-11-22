package limits

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/paths"
	"github.com/IZZY-Agency/quartermaster/internal/policy"
)

// Claude reads the Claude limit state from limits/claude.json, written by the
// status-line wrapper. Known is false when the file is missing, is older than
// policy.ClaudeLimitMaxAge or dated in the future: then the wrapper is gone and
// there is no limit state. A fresh file with no 5-hour usage (no window has
// started since the last reset), or whose 5-hour window has reset since, is
// known with 5-hour usage 0.
// A file that cannot be read or parsed is an error.
func Claude() (ClaudeState, error) {
	dir, err := paths.StateDir()
	if err != nil {
		return ClaudeState{}, err
	}
	path := filepath.Join(dir, "limits", "claude.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return ClaudeState{}, nil
	}
	if err != nil {
		return ClaudeState{}, err
	}
	var f ClaudeFile
	if err := json.Unmarshal(data, &f); err != nil {
		return ClaudeState{}, fmt.Errorf("%s: %w", path, err)
	}
	st := ClaudeState{UpdatedAt: f.UpdatedAt}
	if f.FiveHourUsed != nil {
		st.FiveHourUsed = *f.FiveHourUsed
	}
	if f.SevenDayUsed != nil {
		st.SevenDayUsed = *f.SevenDayUsed
	}
	now := time.Now()
	age := now.Sub(f.UpdatedAt) // a future time is as stale as an old one
	st.Known = age >= 0 && age <= policy.ClaudeLimitMaxAge
	if f.FiveHourResetsAt != nil && !f.FiveHourResetsAt.After(now) {
		st.FiveHourUsed = 0 // the window has reset since the file was written
	}
	return st, nil
}
