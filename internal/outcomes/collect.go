// Package outcomes turns the decision log and Claude Code transcripts into the
// trial report: what each worker cost, how large its context grew, how often
// it compacted, and what its pull requests and reviews looked like.
package outcomes

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
	"regexp"
	"sort"
	"strings"
)

// rate is dollars per million tokens: input, output, cache read, cache write.
type rate struct{ in, out, cacheRead, cacheWrite float64 }

// rates are the API-equivalent prices per model family.
var rates = map[string]rate{
	"opus":   {4, 20, 0.2, 5},
	"fable":  {8, 40, 0.2, 10},
	"sonnet": {2, 10, 0.1, 2.5},
	"haiku":  {0.1, 0.5, 0.01, 0.125},
}

// family names the model family in a model id (claude-opus-5-5 gives "opus"),
// or "" for an id of no known family.
func family(model string) string {
	for _, f := range []string{"opus", "fable", "sonnet", "haiku"} {
		if strings.Contains(model, f) {
			return f
		}
	}
	return ""
}

// Usage is the token counts of one assistant message.
type Usage struct {
	Input, Output, CacheRead, CacheWrite int
}

// Context is the prompt size the message was sent with.
func (u Usage) Context() int { return u.Input + u.CacheRead + u.CacheWrite }

// Cost is the API-equivalent dollars for u on model; zero for an unknown model.
func (u Usage) Cost(model string) float64 {
	r, ok := rates[family(model)]
	if !ok {
		return 0
	}
	return (float64(u.Input)*r.in + float64(u.Output)*r.out +
		float64(u.CacheRead)*r.cacheRead + float64(u.CacheWrite)*r.cacheWrite) / 1e6
}

// Transcript is what one Claude Code transcript adds up to.
type Transcript struct {
	Cost        float64
	PeakContext int
	Compactions int
	// PRURLs are the pull requests the session opened with gh pr create.
	PRURLs []string
	// Start is the first timestamp in the transcript, RFC 3339, or "".
	Start string
	// FirstPrompt is the first 600 characters of the first user prompt.
	FirstPrompt string
	// Cwd and Branch are the last working folder and git branch the
	// transcript names.
	Cwd, Branch string
	// UnknownModels counts messages whose model has no price; their cost is
	// not in Cost.
	UnknownModels int
}

type content struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	Text      string          `json:"text"`
}

type tline struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	Timestamp string `json:"timestamp"`
	Cwd       string `json:"cwd"`
	GitBranch string `json:"gitBranch"`
	Compact   bool   `json:"isCompactSummary"`
	Message   *struct {
		ID      string          `json:"id"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
		Usage   *struct {
			Input      int `json:"input_tokens"`
			Output     int `json:"output_tokens"`
			CacheRead  int `json:"cache_read_input_tokens"`
			CacheWrite int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

var prURL = regexp.MustCompile(`https://[^\s"']+/pull/\d+`)

// ParseTranscript reads a transcript (JSON lines). Claude Code writes one line
// per content block, so a message spans several lines with the same
// message.id; its usage is counted once, from the last line that carries it.
// A line that is not JSON is skipped.
func ParseTranscript(r io.Reader) (Transcript, error) {
	type msg struct {
		model string
		u     Usage
	}
	var (
		t       Transcript
		msgs    = map[string]msg{}
		order   []string
		createT = map[string]bool{} // tool_use ids of gh pr create calls
		seen    = map[string]bool{}
	)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 256<<20)
	for sc.Scan() {
		var l tline
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		if t.Start == "" {
			t.Start = l.Timestamp
		}
		if l.Cwd != "" {
			t.Cwd = l.Cwd
		}
		if l.GitBranch != "" {
			t.Branch = l.GitBranch
		}
		if l.Type == "system" && l.Subtype == "compact_boundary" {
			t.Compactions++
		}
		if l.Message == nil {
			continue
		}
		if l.Type == "assistant" && l.Message.Usage != nil && l.Message.ID != "" {
			if _, ok := msgs[l.Message.ID]; !ok {
				order = append(order, l.Message.ID)
			}
			u := l.Message.Usage
			msgs[l.Message.ID] = msg{l.Message.Model, Usage{u.Input, u.Output, u.CacheRead, u.CacheWrite}}
		}
		if l.Type == "user" && t.FirstPrompt == "" && !l.Compact {
			t.FirstPrompt = promptText(l.Message.Content)
		}
		var blocks []content
		if json.Unmarshal(l.Message.Content, &blocks) != nil {
			continue
		}
		for _, b := range blocks {
			switch b.Type {
			case "tool_use":
				var in struct {
					Command string `json:"command"`
				}
				if b.Name == "Bash" && json.Unmarshal(b.Input, &in) == nil && strings.Contains(in.Command, "gh pr create") {
					createT[b.ID] = true
				}
			case "tool_result":
				if !createT[b.ToolUseID] {
					continue
				}
				for _, u := range prURL.FindAllString(resultText(b.Content), -1) {
					if !seen[u] {
						seen[u] = true
						t.PRURLs = append(t.PRURLs, u)
					}
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return t, fmt.Errorf("read transcript: %w", err)
	}
	for _, id := range order {
		m := msgs[id]
		if _, ok := rates[family(m.model)]; !ok && m.model != "<synthetic>" {
			t.UnknownModels++
		}
		t.Cost += m.u.Cost(m.model)
		if c := m.u.Context(); c > t.PeakContext {
			t.PeakContext = c
		}
	}
	return t, nil
}

// promptText is the first 600 characters of a user message's text, "" for a
// message of tool results only.
func promptText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		var bs []content
		if json.Unmarshal(raw, &bs) != nil {
			return ""
		}
		var b strings.Builder
		for _, c := range bs {
			if c.Type == "text" {
				b.WriteString(c.Text)
				b.WriteByte('\n')
			}
		}
		s = b.String()
	}
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 600 {
		s = string(r[:600])
	}
	return s
}

// builtinWorkerMarkers match the first prompt of a worker or reviewer brief.
const builtinWorkerMarkers = `(You are (a |an |one )?worker|^# ?Brief|^# Worker|Role: implementer|independent reviewer|You are a (design )?reviewer|fresh reviewer)`

// reviewerMarkers are the markers of a review brief.
const reviewerMarkers = `(independent reviewer|You are a (design )?reviewer|fresh reviewer)`

// Matcher tells worker briefs from other sessions by the first prompt.
type Matcher struct{ worker, reviewer *regexp.Regexp }

// NewMatcher builds the matcher from the built-in markers and extra regexes
// (config report.worker_markers).
func NewMatcher(extra []string) (Matcher, error) {
	parts := []string{builtinWorkerMarkers}
	for _, e := range extra {
		if _, err := regexp.Compile(e); err != nil {
			return Matcher{}, fmt.Errorf("report.worker_markers: %w", err)
		}
		parts = append(parts, "(?:"+e+")")
	}
	return Matcher{
		worker:   regexp.MustCompile("(?im)" + strings.Join(parts, "|")),
		reviewer: regexp.MustCompile("(?im)" + reviewerMarkers),
	}, nil
}

// IsWorker reports whether the first prompt looks like a worker brief
// (a review brief counts: reviewers are workers too, by the 2026-11-19 method).
func (m Matcher) IsWorker(t Transcript) bool { return m.worker.MatchString(t.FirstPrompt) }

// IsReviewer reports whether the first prompt looks like a review brief.
func (m Matcher) IsReviewer(t Transcript) bool { return m.reviewer.MatchString(t.FirstPrompt) }

var typeWord = regexp.MustCompile(`(?im)(?:--type[ =]|^type:\s*)([a-z][a-z-]*)`)

// BriefType is the type word the coordinator put in the brief, or "".
func BriefType(t Transcript) string {
	if m := typeWord.FindStringSubmatch(t.FirstPrompt); m != nil {
		return strings.ToLower(m[1])
	}
	return ""
}

// resultText flattens a tool_result's content, a string or a list of text blocks.
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var bs []content
	if json.Unmarshal(raw, &bs) != nil {
		return ""
	}
	var b strings.Builder
	for _, c := range bs {
		b.WriteString(c.Text)
		b.WriteByte('\n')
	}
	return b.String()
}

// ReadTranscript parses the transcript file at path.
func ReadTranscript(path string) (Transcript, error) {
	f, err := os.Open(path)
	if err != nil {
		return Transcript{}, fmt.Errorf("read transcript: %w", err)
	}
	defer func() { _ = f.Close() }()
	return ParseTranscript(f)
}

// SessionFile is one ~/.claude/sessions/*.json entry.
type SessionFile struct {
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
	Cwd       string `json:"cwd"`
	StartedAt int64  `json:"startedAt"`
}

// Claude locates Claude Code's own files under Dir (normally ~/.claude).
type Claude struct{ Dir string }

// DefaultClaude is $CLAUDE_CONFIG_DIR, else ~/.claude.
func DefaultClaude() (Claude, error) {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return Claude{d}, nil
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return Claude{}, fmt.Errorf("claude folder: %w", err)
	}
	return Claude{filepath.Join(h, ".claude")}, nil
}

// SessionByName returns the newest session with that name, or false. The
// sessions folder holds only running sessions, so a finished worker is found
// by its session id (route.Record.ClaudeSession) or by TitleIndex instead.
func (c Claude) SessionByName(name string) (SessionFile, bool, error) {
	files, err := filepath.Glob(filepath.Join(c.Dir, "sessions", "*.json"))
	if err != nil {
		return SessionFile{}, false, err
	}
	var best SessionFile
	found := false
	for _, p := range files {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var s SessionFile
		if json.Unmarshal(b, &s) != nil || s.Name != name || s.SessionID == "" {
			continue
		}
		if !found || s.StartedAt > best.StartedAt {
			best, found = s, true
		}
	}
	return best, found, nil
}

// NamesByID maps Claude session ids to session names.
func (c Claude) NamesByID() map[string]string {
	out := map[string]string{}
	files, _ := filepath.Glob(filepath.Join(c.Dir, "sessions", "*.json"))
	for _, p := range files {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var s SessionFile
		if json.Unmarshal(b, &s) == nil && s.SessionID != "" && s.Name != "" {
			out[s.SessionID] = s.Name
		}
	}
	return out
}

// TranscriptPath finds the transcript of a Claude session id, or "".
func (c Claude) TranscriptPath(sessionID string) (string, error) {
	if sessionID == "" || strings.ContainsAny(sessionID, "/\\") {
		return "", nil
	}
	m, err := filepath.Glob(filepath.Join(c.Dir, "projects", "*", sessionID+".jsonl"))
	if err != nil || len(m) == 0 {
		return "", err
	}
	sort.Strings(m)
	return m[0], nil
}

// AllTranscripts lists every transcript under the projects folder.
func (c Claude) AllTranscripts() ([]string, error) {
	m, err := filepath.Glob(filepath.Join(c.Dir, "projects", "*", "*.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Strings(m)
	return m, nil
}

// TitleIndex maps each session name a transcript gives itself (the
// "custom-title" entry Claude Code writes for -n NAME) to the session id of
// the newest transcript with that name, by file time. It reads every
// transcript, so callers build it once and only when a lookup by id failed.
func (c Claude) TitleIndex() map[string]string {
	paths, _ := c.AllTranscripts()
	type hit struct {
		id  string
		mod int64
	}
	best := map[string]hit{}
	marker := []byte(`"custom-title"`)
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 1<<20), 256<<20)
		for sc.Scan() {
			if !bytes.Contains(sc.Bytes(), marker) {
				continue
			}
			var e struct {
				Type        string `json:"type"`
				CustomTitle string `json:"customTitle"`
			}
			if json.Unmarshal(sc.Bytes(), &e) != nil || e.Type != "custom-title" || e.CustomTitle == "" {
				continue
			}
			id := strings.TrimSuffix(filepath.Base(p), ".jsonl")
			if h, ok := best[e.CustomTitle]; !ok || fi.ModTime().UnixNano() > h.mod {
				best[e.CustomTitle] = hit{id, fi.ModTime().UnixNano()}
			}
		}
		_ = f.Close()
	}
	out := make(map[string]string, len(best))
	for name, h := range best {
		out[name] = h.id
	}
	return out
}

// SessionTranscript resolves a session name to its transcript. ok is false
// when no session of that name, or no transcript for it, exists.
func (c Claude) SessionTranscript(name string) (t Transcript, ok bool, err error) {
	s, found, err := c.SessionByName(name)
	if err != nil || !found {
		return Transcript{}, false, err
	}
	return c.byID(s.SessionID)
}

// WorkerFinder finds a worker's transcript: by the session id qm launched it
// with, else by name among running sessions, else by the transcript's
// custom-title entry. The title index is built once, on first need.
type WorkerFinder struct {
	Claude Claude
	titles map[string]string
}

// Find returns the transcript of the worker session of name launched with
// session id id ("" for a worker logged before qm set one).
func (w *WorkerFinder) Find(id, name string) (Transcript, bool) {
	if id != "" {
		if t, ok, err := w.Claude.byID(id); ok && err == nil {
			return t, true
		}
	}
	if name == "" {
		return Transcript{}, false
	}
	if t, ok, err := w.Claude.SessionTranscript(name); ok && err == nil {
		return t, true
	}
	if w.titles == nil {
		w.titles = w.Claude.TitleIndex()
	}
	if tid := w.titles[name]; tid != "" {
		if t, ok, err := w.Claude.byID(tid); ok && err == nil {
			return t, true
		}
	}
	return Transcript{}, false
}

// IDTranscript reads the transcript of a Claude session id.
func (c Claude) IDTranscript(id string) (Transcript, bool, error) { return c.byID(id) }

func (c Claude) byID(id string) (Transcript, bool, error) {
	p, err := c.TranscriptPath(id)
	if err != nil || p == "" {
		return Transcript{}, false, err
	}
	t, err := ReadTranscript(p)
	if errors.Is(err, fs.ErrNotExist) {
		return Transcript{}, false, nil
	}
	return t, err == nil, err
}
