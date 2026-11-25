package hooks

import (
	"io"
	"strings"
	"testing"
	"time"
)

func TestHandleStdinThatNeverEnds(t *testing.T) {
	cfg := setup(t)
	old := hookTimeout
	hookTimeout = 50 * time.Millisecond
	t.Cleanup(func() { hookTimeout = old })
	r, w := io.Pipe()
	t.Cleanup(func() { _ = w.Close() })
	go func() { _, _ = io.WriteString(w, `{"session_id":"test-session","background_tasks":[`) }()
	done := make(chan []byte, 1)
	go func() { done <- Handle("turn-stop", r, cfg) }()
	select {
	case out := <-done:
		if len(out) != 0 {
			t.Fatalf("printed %q", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stdin that never ends held the hook past its limit")
	}
	if errs := records(t, "error"); len(errs) != 1 || !strings.Contains(errs[0].Detail, "timed out") {
		t.Fatalf("errors %+v", errs)
	}
}
