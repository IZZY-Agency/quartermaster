package hooks

import (
	"bytes"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// stopWith runs turn-stop on a Stop input with extra fields (JSON, "" for
// none) and returns the turn it left.
func stopWith(t *testing.T, extra string) Turn {
	t.Helper()
	cfg := setup(t)
	_, restore := workerTurns(t)
	defer restore()
	in := `{"session_id":"test-session","hook_event_name":"Stop"`
	if extra != "" {
		in += "," + extra
	}
	if out := Handle("turn-stop", strings.NewReader(in+"}"), cfg); len(out) != 0 {
		t.Fatalf("turn-stop printed %q", out)
	}
	if errs := records(t, "error"); len(errs) != 0 {
		t.Fatalf("turn-stop logged %+v", errs)
	}
	tr, err := ReadTurn("test-session")
	if err != nil || !tr.Known || tr.Open {
		t.Fatalf("turn %+v %v", tr, err)
	}
	return tr
}

func TestTurnStopRecordsPendingWork(t *testing.T) {
	cfg := setup(t)
	_, restore := workerTurns(t)
	defer restore()
	b, err := os.ReadFile("testdata/stop-pending.json")
	if err != nil {
		t.Fatal(err)
	}
	if out := Handle("turn-stop", bytes.NewReader(b), cfg); len(out) != 0 {
		t.Fatalf("turn-stop printed %q", out)
	}
	tr, err := ReadTurn("test-session")
	if err != nil || tr.Open || tr.Tasks != 3 || tr.Crons != 1 || !slices.Equal(tr.Types, []string{"shell", "subagent"}) {
		t.Fatalf("turn %+v %v", tr, err)
	}
	// The mark holds counts and types only, never the session's text.
	path, err := turnPath("test-session", "stop")
	if err != nil {
		t.Fatal(err)
	}
	mark, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"SECRET", "0 9", "b1", "c1"} {
		if strings.Contains(string(mark), text) {
			t.Errorf("the mark holds %q: %s", text, mark)
		}
	}
}

func TestTurnStopPendingShapes(t *testing.T) {
	cases := []struct {
		name, extra  string
		tasks, crons int
		types        []string
	}{
		{"missing (older Claude Code)", "", 0, 0, nil},
		{"empty", `"background_tasks":[],"session_crons":[]`, 0, 0, nil},
		{"null", `"background_tasks":null,"session_crons":null`, 0, 0, nil},
		{"objects, not arrays", `"background_tasks":{"id":"b"},"session_crons":{"id":"c"}`, 0, 0, nil},
		{"string and number", `"background_tasks":"x","session_crons":7`, 0, 0, nil},
		{"entries not objects", `"background_tasks":[1,"x"],"session_crons":[null]`, 0, 0, nil},
		{"null entry", `"background_tasks":[null]`, 0, 0, nil},
		{"type not a string", `"background_tasks":[{"type":3}]`, 0, 0, nil},
		{"any status counts", `"background_tasks":[{"type":"shell","status":"new-status"},{"type":"monitor"}]`, 2, 0, []string{"monitor", "shell"}},
		{"one bad array leaves the other", `"background_tasks":7,"session_crons":[{"id":"c"}]`, 0, 1, nil},
		{"the other way round", `"background_tasks":[{"type":"MCP task"}],"session_crons":7`, 1, 0, []string{"MCP task"}},
		{"a type that is no known label", `"background_tasks":[{"type":"npm run build"},{"type":"shell; echo x"},{}]`, 3, 0, []string{"other"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := stopWith(t, tc.extra)
			if tr.Tasks != tc.tasks || tr.Crons != tc.crons || !slices.Equal(tr.Types, tc.types) {
				t.Fatalf("turn %+v, want %d tasks, %d crons, types %v", tr, tc.tasks, tc.crons, tc.types)
			}
		})
	}
}

func TestTurnStartCarriesNothingPending(t *testing.T) {
	cfg := setup(t)
	at, restore := workerTurns(t)
	defer restore()
	b, err := os.ReadFile("testdata/stop-pending.json")
	if err != nil {
		t.Fatal(err)
	}
	Handle("turn-stop", bytes.NewReader(b), cfg)
	// A prompt reopens the turn.
	*at = at.Add(time.Second)
	fireEvent(t, cfg, "turn-start", "UserPromptSubmit", "", nil, "")
	if tr, _ := ReadTurn("test-session"); !tr.Open || tr.Tasks != 0 || tr.Crons != 0 || tr.Types != nil {
		t.Fatalf("after a prompt: %+v", tr)
	}
	// The next stop replaces the mark whole.
	*at = at.Add(time.Second)
	fireEvent(t, cfg, "turn-stop", "Stop", "", nil, "")
	if tr, _ := ReadTurn("test-session"); tr.Open || tr.Tasks != 0 || tr.Crons != 0 || tr.Types != nil {
		t.Fatalf("after a stop with nothing pending: %+v", tr)
	}
}

func TestReadTurnMarkOfOlderQM(t *testing.T) {
	setup(t)
	path, err := turnPath("test-session", "stop")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(strings.TrimSuffix(path, "/test-session.stop"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"at":1800000000000000000,"pid":4242}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if tr, err := ReadTurn("test-session"); err != nil || !tr.Known || tr.Open || tr.Tasks != 0 || tr.Crons != 0 {
		t.Fatalf("turn %+v %v", tr, err)
	}
}
