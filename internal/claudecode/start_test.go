package claudecode

import (
	"testing"
	"time"
)

func TestProcStart(t *testing.T) {
	got, ok := ProcStart(Proc{Start: "Fri Nov 20  9:00:05 2026"})
	want := time.Date(2026, 11, 20, 9, 0, 5, 0, time.Local)
	if !ok || !got.Equal(want) {
		t.Fatalf("got %v %v, want %v", got, ok, want)
	}
	if _, ok := ProcStart(Proc{Start: "not a time"}); ok {
		t.Fatal("bad start parsed")
	}
}
