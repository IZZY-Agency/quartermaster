package up

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/IZZY-Agency/quartermaster/internal/paths"
	"github.com/IZZY-Agency/quartermaster/internal/route"
	"github.com/IZZY-Agency/quartermaster/internal/spawn"
)

// spawning reserves session id as qm spawn worker does, for project (""
// outside one); stop ends the qm that holds it.
func spawning(t *testing.T, id, project string) (stop func()) {
	t.Helper()
	state, err := paths.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(state, "spawning")
	mkdir(t, dir)
	b, err := json.Marshal([]route.Record{{Time: now, Kind: "worker", Name: "w-hand", ClaudeSession: id, UpProject: project}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, id+".json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return func() { _ = f.Close() }
}

// spawnDeps are w.deps reading the Records as qm up does.
func (w *adoptWorld) spawnDeps() AdoptDeps {
	d := w.deps()
	d.Records = func() ([]route.Record, error) {
		st, err := spawn.ListStarting()
		return spawn.AddStarting(w.recs, st), err
	}
	return d
}

// A worker qm spawn worker is starting is not adopted by another project,
// tied to a project or not; nor, tied, once its qm stopped in the middle.
func TestAdoptLeavesAWorkerBeingSpawned(t *testing.T) {
	for name, c := range map[string]struct {
		tied, stopped bool
		note          string // "" when it is adopted
	}{
		"tied":                 {true, false, "now held by project"},
		"untied":               {false, false, "qm spawn worker is still starting it"},
		"tied, its qm ended":   {true, true, "now held by project"},
		"untied, its qm ended": {false, true, ""},
	} {
		t.Run(name, func(t *testing.T) {
			w, p := laterWorld(t)
			o, ap := w.otherProject(t)
			project := ""
			if c.tied {
				project = p.ID
			}
			stop := spawning(t, adHand, project)
			if c.stopped {
				stop()
			}
			if err := Adopt(context.Background(), &ap, w.spawnDeps()); err != nil {
				t.Fatal(err)
			}
			cur, _, err := LoadID(o.ID)
			if err != nil {
				t.Fatal(err)
			}
			_, adopted := cur.Adopted[adHand]
			if adopted != (c.note == "") || c.note != "" && !notesHold(ap.Notes, c.note) {
				t.Errorf("adopted %v, notes %q; want note %q", adopted, ap.Notes, c.note)
			}
		})
	}
}

// Its own project does not adopt it as a hand-started worker either.
func TestAdoptLeavesAWorkerBeingSpawnedHere(t *testing.T) {
	w, _ := laterWorld(t)
	o, ap := w.otherProject(t)
	spawning(t, adHand, o.ID)
	if err := Adopt(context.Background(), &ap, w.spawnDeps()); err != nil {
		t.Fatal(err)
	}
	if cur, _, _ := LoadID(o.ID); len(cur.Adopted) != 0 {
		t.Errorf("adopted %v, notes %q", cur.Adopted, ap.Notes)
	}
}

// A session being spawned is no coordinator to adopt.
func TestAdoptCoordinatorBeingSpawned(t *testing.T) {
	w := newAdoptWorld(t)
	ap, err := w.plan(t, allThree, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	spawning(t, adCoord, "")
	if err := Adopt(context.Background(), &ap, w.spawnDeps()); err == nil || !strings.Contains(err.Error(), "qm spawn worker is starting that session") {
		t.Errorf("err %v", err)
	}
	if _, found, _ := Load(w.repo); found {
		t.Error("a project was saved")
	}
}
