package spawn

import (
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/route"
)

// The worker's Record keeps the name as given with --name, before the prefix.
func TestWorkerRecordsKeepTheBaseName(t *testing.T) {
	req := WorkerRequest{Name: "proj-w1", BaseName: "w1", Cwd: "/r", Type: "docs", Size: "S"}
	recs := workerRecords(route.Decision{Kind: "worker"}, req, "/r", "main", 0, "sid", time.Now())
	if got := recs[len(recs)-1]; got.Name != "proj-w1" || got.BaseName != "w1" {
		t.Errorf("Record name %q, base %q, want proj-w1 and w1", got.Name, got.BaseName)
	}
}
