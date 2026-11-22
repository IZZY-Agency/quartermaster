package transcript

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLastContextSumsUsage(t *testing.T) {
	got, err := LastContext("testdata/basic.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 1, 2, 10, 5, 0, 0, time.UTC)
	if got.Context != 31200 || !got.At.Equal(want) {
		t.Fatalf("got %+v, want 31200 at %v", got, want)
	}
}

func TestLastContextSkipsSidechain(t *testing.T) {
	got, err := LastContext("testdata/sidechain-last.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if got.Context != 711 {
		t.Fatalf("got %+v, want context 711", got)
	}
}

func TestLastContextNone(t *testing.T) {
	got, err := LastContext("testdata/no-assistant.jsonl")
	if err != nil || got != (Last{}) {
		t.Fatalf("got %+v, %v; want zero", got, err)
	}
}

func TestLastContextMissingFile(t *testing.T) {
	if _, err := LastContext("testdata/absent.jsonl"); err == nil {
		t.Fatal("want an error for a missing file")
	}
}

func TestOwnerMessagesTextOnly(t *testing.T) {
	got, err := OwnerMessages("testdata/basic.jsonl", 2, 100)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"second question", "third question is long"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	all, _ := OwnerMessages("testdata/basic.jsonl", 10, 100)
	if !reflect.DeepEqual(all, []string{"first question", "second question", "third question is long"}) {
		t.Fatalf("all: %q", all)
	}
}

func TestOwnerMessagesCut(t *testing.T) {
	got, err := OwnerMessages("testdata/basic.jsonl", 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"third ques…"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestCwdLast(t *testing.T) {
	got, err := Cwd("testdata/basic.jsonl")
	if err != nil || got != "/work/two" {
		t.Fatalf("got %q, %v", got, err)
	}
	none, err := Cwd("testdata/no-assistant.jsonl")
	if err != nil || none != "" {
		t.Fatalf("got %q, %v; want empty", none, err)
	}
}

func TestStartCwd(t *testing.T) {
	// basic.jsonl starts in /work/one and ends in /work/two.
	got, err := StartCwd("testdata/basic.jsonl")
	if err != nil || got != "/work/one" {
		t.Fatalf("got %q, %v", got, err)
	}
	none, err := StartCwd("testdata/no-assistant.jsonl")
	if err != nil || none != "" {
		t.Fatalf("got %q, %v; want empty", none, err)
	}
}

// bigTranscript writes a transcript over 64 KiB (the scanner's first buffer)
// whose facts sit far from its end: one assistant line and one cwd at the
// start, then many distinct user lines without a cwd.
func bigTranscript(t *testing.T, users int) (path string, want []string) {
	t.Helper()
	var b strings.Builder
	add := func(v any) {
		j, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(j)
		b.WriteByte('\n')
	}
	add(map[string]any{"type": "assistant", "cwd": "/work/far", "timestamp": "2026-01-02T10:00:00Z",
		"message": map[string]any{"usage": map[string]int{"input_tokens": 1, "cache_creation_input_tokens": 20, "cache_read_input_tokens": 300}}})
	for i := 0; i < users; i++ {
		text := fmt.Sprintf("message %05d %s", i, strings.Repeat("x", 80))
		want = append(want, text)
		add(map[string]any{"type": "user", "message": map[string]any{"content": text}})
	}
	path = filepath.Join(t.TempDir(), "big.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if b.Len() <= 3*64*1024 {
		t.Fatalf("fixture is %d bytes, want well over 64 KiB", b.Len())
	}
	return path, want
}

func TestLongTranscriptKeepsEveryLine(t *testing.T) {
	path, want := bigTranscript(t, 4000)
	got, err := OwnerMessages(path, len(want), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		for i := range want {
			if i >= len(got) || got[i] != want[i] {
				t.Fatalf("message %d: got %q, want %q", i, got[min(i, len(got)-1)], want[i])
			}
		}
	}
	if cwd, err := Cwd(path); err != nil || cwd != "/work/far" {
		t.Fatalf("Cwd far from the end: %q, %v", cwd, err)
	}
	last, err := LastContext(path)
	if err != nil || last.Context != 321 {
		t.Fatalf("LastContext far from the end: %+v, %v", last, err)
	}
}

func TestFirstAt(t *testing.T) {
	got, err := FirstAt("testdata/basic.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("FirstAt = %v, want %v", got, want)
	}
	p := filepath.Join(t.TempDir(), "none.jsonl")
	if err := os.WriteFile(p, []byte("{\"type\":\"summary\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := FirstAt(p); err != nil || !got.IsZero() {
		t.Errorf("no timestamp: %v, %v", got, err)
	}
}
