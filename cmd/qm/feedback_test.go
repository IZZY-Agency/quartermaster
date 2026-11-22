package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/declog"
)

func TestFeedbackAnalyzeUsage(t *testing.T) {
	routeHermetic(t)
	var out, errOut bytes.Buffer
	if code := run([]string{"feedback-analyze"}, &out, &errOut); code != 2 {
		t.Errorf("no file: exit %d, want 2", code)
	}
}

func TestFeedbackAnalyzeErrorLogged(t *testing.T) {
	routeHermetic(t)
	other := filepath.Join(t.TempDir(), "s-fb-1.txt")
	if err := os.WriteFile(other, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"feedback-analyze", other}, &out, &errOut); code != 1 {
		t.Errorf("exit %d (%s), want 1", code, errOut.String())
	}
	recs, err := declog.Read(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Kind != "error" || recs[0].Rule != "feedback" {
		t.Errorf("records %+v, want one feedback error", recs)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("the file was touched: %v", err)
	}
}
