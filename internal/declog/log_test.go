package declog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/route"
)

func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	return filepath.Join(dir, "quartermaster", "decisions.jsonl")
}

func TestConcurrentAppend(t *testing.T) {
	path := isolate(t)
	var wg sync.WaitGroup
	errs := make(chan error, 1000)
	for g := 0; g < 50; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				errs <- Append(route.Record{
					Time:   time.Now(),
					Kind:   "worker",
					Name:   fmt.Sprintf("g%d-r%d", g, i),
					Detail: fmt.Sprintf("%0200d", i),
				})
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r route.Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("line %d is not a record: %v", len(seen)+1, err)
		}
		seen[r.Name] = true
	}
	if len(seen) != 1000 {
		t.Errorf("%d distinct records, want 1000", len(seen))
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("log mode %v, want 0600", fi.Mode().Perm())
	}
}

func TestAppendFolderMode(t *testing.T) {
	path := isolate(t)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Append(route.Record{Time: time.Now(), Kind: "guard"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("folder mode %v, want 0700", fi.Mode().Perm())
	}
}

func TestReadSince(t *testing.T) {
	path := isolate(t)
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for i, kind := range []string{"worker", "review", "guard"} {
		r := route.Record{Time: t0.Add(time.Duration(i) * time.Hour), Kind: kind, Branch: "main"}
		if err := Append(r); err != nil {
			t.Fatal(err)
		}
	}
	// A torn line is skipped, not fatal.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{\"time\":\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := Read(t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Kind != "review" || got[1].Kind != "guard" || got[0].Branch != "main" {
		t.Errorf("got %+v", got)
	}
}

func TestReadMissing(t *testing.T) {
	isolate(t)
	got, err := Read(time.Time{})
	if err != nil || len(got) != 0 {
		t.Errorf("got %v, %v; want nothing", got, err)
	}
}

func TestAppendDecision(t *testing.T) {
	isolate(t)
	before := time.Now().UTC().Add(-time.Second)
	d := route.Decision{Kind: "review", Tier: route.Tier{Model: "sonnet", Effort: "medium"}, Row: "fix-check"}
	if err := AppendDecision(d, route.Record{Kind: "ignored", Project: "/r", Branch: "b", Round: 2}); err != nil {
		t.Fatal(err)
	}
	recs, err := Read(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("%d records, want 1", len(recs))
	}
	r := recs[0]
	if r.Kind != "review" || r.Project != "/r" || r.Branch != "b" || r.Round != 2 ||
		r.Decision == nil || r.Decision.Row != "fix-check" || r.Time.Before(before) {
		t.Errorf("record %+v", r)
	}
}

// A line cut short (a full disk, say) does not swallow the next record: the
// append starts on a new line, and Read finds it.
func TestAppendAfterPartialLine(t *testing.T) {
	path := isolate(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"kind":"up","rule":"cut`), 0o600); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	if err := Append(route.Record{Time: at, Kind: "adopt", Rule: "after"}); err != nil {
		t.Fatal(err)
	}
	if err := Append(route.Record{Time: at, Kind: "adopt", Rule: "next"}); err != nil {
		t.Fatal(err)
	}
	got, err := Read(time.Time{})
	if err != nil || len(got) != 2 || got[0].Rule != "after" || got[1].Rule != "next" {
		t.Fatalf("Read = %+v, %v; want the two records appended after the cut line", got, err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if b[len(b)-1] != '\n' {
		t.Errorf("log does not end with a newline: %q", b)
	}
}
