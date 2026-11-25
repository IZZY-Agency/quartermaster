package spawn

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/paths"
)

// BriefAge is how long a worker's copy of its brief is kept: as long as
// Claude Code keeps a transcript by default, so a worker that had no turn yet
// still finds its task when its session is resumed.
const BriefAge = 30 * 24 * time.Hour

// maxBrief is the largest brief: what Claude Code's Read opens whole.
const maxBrief = 256 << 10

const (
	// readLines is how many lines Claude Code's Read returns without a limit.
	readLines = 2000
	// defaultReadTokens is Claude Code's own cap on a Read's output, which
	// qm does not set for a coordinator.
	defaultReadTokens = 25000
	// readTokensEnv sets that cap.
	readTokensEnv = "CLAUDE_CODE_FILE_READ_MAX_OUTPUT_TOKENS"
	// BriefEnv names, in a worker's environment, the file that holds its
	// task: the read-budget rule does not count a Read of it.
	BriefEnv = "QM_BRIEF"
)

// briefName is a brief copy's file name: a Claude session id and .md.
var briefName = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\.md$`)

// launchDir is <state>/launch, the folder of the launch scripts and the
// copies of the briefs.
func launchDir() (string, error) {
	state, err := paths.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(state, "launch"), nil
}

// BriefPath is where the copy of the brief of session sessionID is kept:
// <state>/launch/<session id>.md.
func BriefPath(sessionID string) (string, error) {
	if !briefName.MatchString(sessionID + ".md") {
		return "", fmt.Errorf("brief copy: session id %q is not usable in a file name", sessionID)
	}
	dir, err := launchDir()
	if err != nil {
		return "", fmt.Errorf("brief copy: %w", err)
	}
	return filepath.Join(dir, sessionID+".md"), nil
}

// WriteBrief writes the copy of session sessionID's brief (BriefPath), mode
// 0600, never over an existing file, and returns its path. WriteScript
// removes the copies older than BriefAge; nothing else removes one, but a
// launch that clearly failed.
func WriteBrief(sessionID string, text []byte) (path string, err error) {
	if path, err = BriefPath(sessionID); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("brief copy: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("brief copy: %w", err)
	}
	_, werr := f.Write(text)
	if err := errors.Join(werr, f.Close()); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("brief copy: %w", err)
	}
	return path, nil
}

// CopyBrief writes the text of the file from as the brief copy of session
// sessionID: what qm up starts a session in place of another with, kept as
// long as the copy of a spawned worker's brief. A line no Read returns is broken
// into several.
func CopyBrief(sessionID, from string) (path string, err error) {
	b, err := os.ReadFile(from)
	if err != nil {
		return "", fmt.Errorf("brief copy: %w", err)
	}
	return WriteBrief(sessionID, []byte(strings.Join(linesOf(b), "")))
}

// readBrief reads the brief file at path: a regular file of at most maxBrief
// bytes, with no line longer than maxLine.
func readBrief(path string) ([]byte, error) {
	fi, err := os.Stat(path)
	switch {
	case err != nil:
		return nil, fmt.Errorf("spawn: brief: %w", err)
	case !fi.Mode().IsRegular():
		return nil, fmt.Errorf("spawn: brief %s is not a file", path)
	case fi.Size() > maxBrief:
		return nil, fmt.Errorf("spawn: brief %s has %d bytes, more than a worker reads at once (%d)", path, fi.Size(), maxBrief)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("spawn: brief: %w", err)
	}
	n := 0
	for line := range bytes.Lines(b) {
		if n++; len(line) > maxLine {
			return nil, fmt.Errorf("spawn: brief %s: line %d has %d bytes, more than one Read returns (%d): break it into lines", path, n, len(line), maxLine)
		}
	}
	return b, nil
}

// briefOf is the copy of session sessionID's brief, "" when there is none.
func briefOf(sessionID string) string {
	path, err := BriefPath(sessionID)
	if err != nil {
		return ""
	}
	if fi, err := os.Lstat(path); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	return path
}

// briefFile is a file a session is told to read: its path, and the file that
// has its text now (the original, for a copy not written yet).
type briefFile struct{ path, text string }

// workerPrompt and coordinatorPrompt are first prompts: one line that points
// to the file holding the text, which is so never on a command line, where
// every process of the user reads it. "You are a worker" is what qm report
// knows a worker's transcript by.
func workerPrompt(f briefFile) string {
	return fmt.Sprintf("You are a worker started by qm. Your task is the file %s: first read all of it yourself, with Read%s. It is your task, not material to delegate.", f.path, readParts(f.text, defaultReadTokens))
}

func coordinatorPrompt(f briefFile) string {
	return fmt.Sprintf("You continue an earlier session. First read all of the file %s, with Read%s: it is qm's summary of that session.", f.path, readParts(f.text, defaultReadTokens))
}

// readParts says how to read the file at path when one Read of at most limit
// tokens does not return all of it.
func readParts(path string, limit int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	parts := partsOf(linesOf(b), limit)
	if len(parts) < 2 {
		return ""
	}
	size, equal := parts[0].lines, true
	ranges := make([]string, len(parts))
	for i, p := range parts {
		equal = equal && (p.lines == size || i == len(parts)-1 && p.lines < size)
		ranges[i] = fmt.Sprintf("%d and %d", p.from+1, p.lines)
	}
	if equal {
		return fmt.Sprintf(", in parts of %d lines (offset and limit)", size)
	}
	return fmt.Sprintf(", in %d parts (offset and limit): %s", len(parts), strings.Join(ranges, ", "))
}

// part is a run of a file's lines, from the line with index from.
type part struct{ from, lines int }

// partsOf are the parts of lines, each as long as a Read of at most limit
// tokens returns whole. A line over limit is a part of its own.
func partsOf(lines []string, limit int) (parts []part) {
	size := 0
	for i, line := range lines {
		k := len(parts) - 1
		if k < 0 || parts[k].lines == readLines || readTokens(size+len(line), parts[k].lines+1) > limit {
			parts = append(parts, part{from: i})
			size, k = 0, k+1
		}
		parts[k].lines++
		size += len(line)
	}
	return parts
}

// readTokens is what a Read of size bytes in lines lines takes at most: two
// bytes a token, more than any text takes, and four tokens a line for its
// number.
func readTokens(size, lines int) int { return size/2 + 4*lines }

// maxLine is the longest line, with its newline, that a Read returns.
const maxLine = 2 * (defaultReadTokens - 4)

// linesOf are the lines of b with their newlines, one longer than maxLine
// broken into several.
func linesOf(b []byte) (lines []string) {
	for _, line := range strings.SplitAfter(string(b), "\n") {
		for len(line) > maxLine {
			cut := maxLine - 1
			for cut > maxLine-utf8.UTFMax && !utf8.RuneStart(line[cut]) {
				cut--
			}
			lines = append(lines, line[:cut]+"\n")
			line = line[cut:]
		}
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// readCost is the tokens a Read of the whole file at path takes at most.
func readCost(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return readTokens(len(b), bytes.Count(b, []byte("\n"))+1)
}

// withReadCap is env with the Read output cap raised to what the file at
// path needs, to Claude Code's own cap at most: it holds for every Read of
// the session, so a larger file is read in parts instead. env itself is not
// changed.
func withReadCap(env map[string]string, path string) map[string]string {
	limit, err := strconv.Atoi(env[readTokensEnv])
	tokens := readCost(path)
	if err != nil || limit >= defaultReadTokens || tokens <= limit {
		return env
	}
	out := maps.Clone(env)
	out[readTokensEnv] = strconv.Itoa(min(tokens, defaultReadTokens))
	return out
}

// readRules are the permission rules that let a session read the file at
// path without asking: the file is outside its working folder. Claude Code
// checks a symlink's target too.
func readRules(path string) []string {
	if path == "" {
		return nil
	}
	rules := []string{"Read(/" + path + ")"}
	if real := realPath(path); real != path {
		rules = append(rules, "Read(/"+real+")")
	}
	return rules
}

// realPath is path with the symlinks of what exists of it resolved: the file,
// or its folder, may not be there yet.
func realPath(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	dir := filepath.Dir(path)
	if dir == path {
		return path
	}
	return filepath.Join(realPath(dir), filepath.Base(path))
}

// quotePrompt is the prompt as a script's word. It starts with a newline, so
// what reads a process's arguments knows where they end.
func quotePrompt(prompt string) string { return launch.Quote("\n" + prompt) }
