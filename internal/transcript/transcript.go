// Package transcript reads facts from the end of a Claude Code transcript
// (a JSON-lines file): the context size, what the owner last wrote, and the
// working folder. It only reads; qm never writes Claude Code's files.
package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"time"
)

// Tail is the bytes LastContext reads from the end of a transcript.
const Tail = 1 << 20

// factsTail is the bytes OwnerMessages and Cwd read from the end.
const factsTail = 4 << 20

// Last is the context of a transcript's last main-thread assistant message
// and when it was written. The zero value means none was found.
type Last struct {
	Context int
	At      time.Time
}

// TailLines returns the whole lines in the last n bytes of the file at path.
func TailLines(path string, n int64) ([][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	off := max(fi.Size()-n, 0)
	b, err := io.ReadAll(io.NewSectionReader(f, off, fi.Size()-off))
	if err != nil {
		return nil, err
	}
	if off > 0 {
		// Drop the line cut by the offset.
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		}
	}
	var lines [][]byte
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64*1024), int(n)+1)
	for sc.Scan() {
		// Scanner reuses its buffer: keep a copy of each line.
		lines = append(lines, bytes.Clone(sc.Bytes()))
	}
	return lines, sc.Err()
}

// LastContext is the context size of the last main-thread assistant message
// in the transcript: input plus cache-write plus cache-read tokens, with the
// message's timestamp (an unparseable timestamp leaves At zero while Context
// is still set). It reads at most the last Tail bytes.
func LastContext(path string) (Last, error) {
	lines, err := TailLines(path, Tail)
	if err != nil {
		return Last{}, err
	}
	for i := len(lines) - 1; i >= 0; i-- {
		var e struct {
			Type        string `json:"type"`
			IsSidechain bool   `json:"isSidechain"`
			Timestamp   string `json:"timestamp"`
			Message     struct {
				Usage *struct {
					Input      int `json:"input_tokens"`
					CacheWrite int `json:"cache_creation_input_tokens"`
					CacheRead  int `json:"cache_read_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(lines[i], &e) != nil || e.Type != "assistant" || e.IsSidechain || e.Message.Usage == nil {
			continue
		}
		u := e.Message.Usage
		at, _ := time.Parse(time.RFC3339, e.Timestamp)
		return Last{Context: u.Input + u.CacheWrite + u.CacheRead, At: at}, nil
	}
	return Last{}, nil
}

// OwnerMessages returns the last n main-thread user messages whose content is
// text (a string, or text blocks; tool results and meta lines are skipped),
// oldest first, each cut to limit characters with an ellipsis.
func OwnerMessages(path string, n, limit int) ([]string, error) {
	lines, err := TailLines(path, factsTail)
	if err != nil {
		return nil, err
	}
	var out []string
	for i := len(lines) - 1; i >= 0 && len(out) < n; i-- {
		var e struct {
			Type        string `json:"type"`
			IsSidechain bool   `json:"isSidechain"`
			IsMeta      bool   `json:"isMeta"`
			Message     struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(lines[i], &e) != nil || e.Type != "user" || e.IsSidechain || e.IsMeta {
			continue
		}
		text := messageText(e.Message.Content)
		if strings.TrimSpace(text) == "" {
			continue
		}
		out = append(out, cut(text, limit))
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// messageText is the text of a message content: a string, or the text blocks
// joined; "" when it holds none (a tool result, say).
func messageText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func cut(s string, limit int) string {
	r := []rune(s)
	if limit < 0 || len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "…"
}

// Cwd is the last working folder a transcript line names; "" when none.
func Cwd(path string) (string, error) {
	lines, err := TailLines(path, factsTail)
	if err != nil {
		return "", err
	}
	for i := len(lines) - 1; i >= 0; i-- {
		var e struct {
			Cwd string `json:"cwd"`
		}
		if json.Unmarshal(lines[i], &e) == nil && e.Cwd != "" {
			return e.Cwd, nil
		}
	}
	return "", nil
}

// StartCwd is the first working folder a main-thread transcript line names:
// the folder the session was started in, which is the one claude --resume
// finds it from. A session that changed folder later names other folders on
// later lines (and in its session file). "" when no line has one.
func StartCwd(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 64<<20)
	for sc.Scan() {
		var e struct {
			Cwd         string `json:"cwd"`
			IsSidechain bool   `json:"isSidechain"`
		}
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.Cwd != "" && !e.IsSidechain {
			return e.Cwd, nil
		}
	}
	return "", sc.Err()
}

// FirstAt is the first timestamp a transcript line carries: when the session
// started. Zero when no line has one.
func FirstAt(path string) (time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 64<<20)
	for sc.Scan() {
		var e struct {
			Timestamp string `json:"timestamp"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Timestamp == "" {
			continue
		}
		if at, err := time.Parse(time.RFC3339Nano, e.Timestamp); err == nil {
			return at, nil
		}
	}
	return time.Time{}, sc.Err()
}
