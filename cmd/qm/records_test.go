package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/IZZY-Agency/quartermaster/internal/declog"
	"github.com/IZZY-Agency/quartermaster/internal/route"
)

func TestReadUpRecordsClearsUnusableNames(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for _, r := range []route.Record{
		{Kind: "worker", Name: "w1", ClaudeSession: "s-good", Cwd: "/work/shop"},
		{Kind: "worker", Name: "../../escaped", ClaudeSession: "s-bad", Cwd: "/work/shop"},
	} {
		if err := declog.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	recs, err := readUpRecords(&out)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0].Name != "w1" || recs[1].ClaudeSession != "s-bad" || recs[1].Name != "" {
		t.Fatalf("records %+v, want both, the unusable name cleared", recs)
	}
	if note := out.String(); !strings.HasPrefix(note, "note: ") || !strings.Contains(note, "s-bad") || strings.Count(note, "\n") != 1 {
		t.Errorf("notes %q, want one note naming session s-bad", note)
	}
}
