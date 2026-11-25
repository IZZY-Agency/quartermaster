package outcomes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func claudeDir(t *testing.T) Claude {
	t.Helper()
	d := t.TempDir()
	write(t, filepath.Join(d, "sessions", "1.json"), `{"sessionId":"old","name":"w1","startedAt":1}`)
	write(t, filepath.Join(d, "sessions", "2.json"), `{"sessionId":"new","name":"w1","startedAt":2}`)
	write(t, filepath.Join(d, "sessions", "3.json"), `{"sessionId":"other","name":"w2","startedAt":3}`)
	write(t, filepath.Join(d, "sessions", "bad.json"), `not json`)
	write(t, filepath.Join(d, "sessions", "k.key"), `secret`)
	write(t, filepath.Join(d, "projects", "-repo", "new.jsonl"), `{"type":"user","timestamp":"2026-01-01T00:00:00Z","cwd":"/repo","gitBranch":"feat","message":{"content":"You are a worker.\ntype: bugfix"}}`+"\n")
	return Claude{Dir: d}
}

func TestSessionByNameNewestWins(t *testing.T) {
	c := claudeDir(t)
	s, ok, err := c.SessionByName("w1")
	if err != nil || !ok || s.SessionID != "new" {
		t.Fatalf("got %+v %v %v, want session new", s, ok, err)
	}
	if _, ok, _ := c.SessionByName("nobody"); ok {
		t.Error("found a session that does not exist")
	}
}

func TestTranscriptPath(t *testing.T) {
	c := claudeDir(t)
	if p, _ := c.TranscriptPath("new"); !strings.HasSuffix(p, filepath.Join("-repo", "new.jsonl")) {
		t.Errorf("path = %q", p)
	}
	// A glob pattern is not an id: it would find another session's
	// transcript.
	for _, id := range []string{"missing", "", "../x", "a/b", `a\b`, "*", "ne?", "[n]ew", "n*"} {
		if p, _ := c.TranscriptPath(id); p != "" {
			t.Errorf("TranscriptPath(%q) = %q, want none", id, p)
		}
	}
}

func TestNamesByID(t *testing.T) {
	got := claudeDir(t).NamesByID()
	if len(got) != 3 || got["new"] != "w1" || got["other"] != "w2" {
		t.Errorf("names = %v", got)
	}
}

func TestSessionTranscriptParsesPromptAndBranch(t *testing.T) {
	tr, ok, err := claudeDir(t).SessionTranscript("w1")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if tr.Branch != "feat" || tr.Cwd != "/repo" || BriefType(tr) != "bugfix" {
		t.Errorf("transcript = %+v", tr)
	}
}

func TestMatcher(t *testing.T) {
	m, err := NewMatcher([]string{"^ACME-BRIEF"})
	if err != nil {
		t.Fatal(err)
	}
	for prompt, want := range map[string]bool{
		"You are a worker for x": true,
		"# Brief\nbody":          true,
		"you are one WORKER":     true,
		"hello\nACME-BRIEF 12":   true,
		"fix the build please":   false,
		"Role: implementer":      true,
	} {
		if got := m.IsWorker(Transcript{FirstPrompt: prompt}); got != want {
			t.Errorf("IsWorker(%q) = %v, want %v", prompt, got, want)
		}
	}
	if !m.IsReviewer(Transcript{FirstPrompt: "You are a design reviewer"}) || m.IsReviewer(Transcript{FirstPrompt: "You are a worker"}) {
		t.Error("IsReviewer misjudged")
	}
	if _, err := NewMatcher([]string{"("}); err == nil {
		t.Error("a bad extra marker must be an error")
	}
}

func TestSyntheticModelIsNotUnknown(t *testing.T) {
	in := `{"type":"assistant","message":{"id":"a","model":"<synthetic>","usage":{"input_tokens":1}}}
{"type":"assistant","message":{"id":"b","model":"some-new-model","usage":{"input_tokens":1}}}
`
	tr, err := ParseTranscript(strings.NewReader(in))
	if err != nil || tr.UnknownModels != 1 {
		t.Errorf("unknown = %d, err %v; want 1 (the synthetic message is not unknown)", tr.UnknownModels, err)
	}
}

func TestWorkerFinder(t *testing.T) {
	d := t.TempDir()
	// A finished worker: no sessions/*.json, only its transcript.
	write(t, filepath.Join(d, "projects", "-repo", "11111111-2222-4333-8444-555555555555.jsonl"),
		`{"type":"assistant","message":{"id":"m1","model":"claude-sonnet-5-5","usage":{"input_tokens":1000000}}}`+"\n")
	// A worker logged before qm set session ids, found by its title entry.
	titled := filepath.Join(d, "projects", "-repo", "aaaa.jsonl")
	write(t, titled, `{"type":"custom-title","customTitle":"old-worker","sessionId":"aaaa"}`+"\n"+
		`{"type":"assistant","message":{"id":"m2","model":"claude-opus-5-5","usage":{"input_tokens":1000000}}}`+"\n")
	// An older transcript with the same title loses to the newer one.
	older := filepath.Join(d, "projects", "-repo", "bbbb.jsonl")
	write(t, older, `{"type":"custom-title","customTitle":"old-worker","sessionId":"bbbb"}`+"\n")
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(older, past, past); err != nil {
		t.Fatal(err)
	}
	f := &WorkerFinder{Claude: Claude{Dir: d}}
	if tr, ok := f.Find("11111111-2222-4333-8444-555555555555", "w1"); !ok || tr.Cost != 2 {
		t.Errorf("by id: %+v %v, want cost 2", tr, ok)
	}
	if tr, ok := f.Find("", "old-worker"); !ok || tr.Cost != 4 {
		t.Errorf("by title: %+v %v, want the newer transcript, cost 4", tr, ok)
	}
	if tr, ok := f.Find("99999999-0000-4000-8000-000000000000", "old-worker"); !ok || tr.Cost != 4 {
		t.Errorf("id missing, title fallback: %+v %v", tr, ok)
	}
	// The transcript belongs to another Record's session: not a launch's of
	// the same name that never ran.
	owned := &WorkerFinder{Claude: Claude{Dir: d}, Owned: map[string]bool{"aaaa": true}}
	if tr, ok := owned.Find("99999999-0000-4000-8000-000000000000", "old-worker"); ok {
		t.Errorf("found another session's transcript: %+v", tr)
	}
	if _, ok := f.Find("", "nobody"); ok {
		t.Error("found a worker that has no transcript")
	}
}
