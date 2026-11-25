package up

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IZZY-Agency/quartermaster/internal/launch"
)

func TestSaveNeedsAFileNameID(t *testing.T) {
	dir := setState(t)
	for _, id := range []string{"..", ".", "../evil", "a/b", `a\b`, "a\x00b"} {
		if err := Save(Project{ID: id, Root: "/work/shop"}); err == nil {
			t.Errorf("Save(id %q) succeeded, want an error", id)
		}
	}
	if entries, _ := os.ReadDir(filepath.Dir(dir)); len(entries) > 1 {
		t.Errorf("state folder holds %d entries after refused saves", len(entries))
	}
}

// A state file that LoadID would refuse is not written either.
func TestSaveNeedsTheRootsID(t *testing.T) {
	dir := setState(t)
	if err := Save(Project{ID: ProjectID("/work/shop"), Root: "/work/other"}); err == nil {
		t.Error("Save(another folder's id) succeeded, want an error")
	}
	if entries, _ := os.ReadDir(dir); len(entries) > 0 {
		t.Errorf("projects folder holds %d entries after a refused save", len(entries))
	}
	if err := Save(Project{ID: ProjectID("/work/shop"), Root: "/work/shop"}); err != nil {
		t.Errorf("Save: %v", err)
	}
}

// A state file holds text: a folder whose name is not UTF-8 would be read back
// as another folder, so it is refused before anything is written or started.
func TestProjectFolderNeedsUTF8(t *testing.T) {
	dir := setState(t)
	root := "/work/caf\xe9"
	if err := Save(Project{ID: ProjectID(root), Root: root}); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Errorf("Save: %v, want an error that says UTF-8", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) > 0 {
		t.Errorf("projects folder holds %d entries after a refused save", len(entries))
	}
	asked := false
	d := Deps{Trusted: func(string) (bool, error) { asked = true; return false, nil }}
	if _, _, err := Start(context.Background(), root, nil, d); err == nil || !strings.Contains(err.Error(), "UTF-8") || asked {
		t.Errorf("Start: %v, went on: %v, want an error that says UTF-8 first", err, asked)
	}
	coord := AdoptCandidate{SessionID: "c1", Name: "w", Cwd: realPath(root)}
	if _, err := PlanAdopt(context.Background(), root, nil, []AdoptCandidate{coord}, Choice{Coordinator: "c1"}, nil, false, AdoptDeps{}); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Errorf("PlanAdopt: %v, want an error that says UTF-8", err)
	}
}

// writeState writes text as the state file of id and returns its path.
func writeState(t *testing.T, dir, id, text string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, id+".json")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// wantRecovery checks that err names path, says what qm cannot tell, and
// gives the command that moves the file aside to a name not ending in .json.
func wantRecovery(t *testing.T, err error, path string) {
	t.Helper()
	if err == nil {
		t.Fatal("no error")
	}
	msg := err.Error()
	mv := "mv " + launch.Quote(path) + " "
	for _, want := range []string{path, "cannot tell which sessions", mv, ".unreadable-"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not hold %q", msg, want)
		}
	}
}

func TestUnreadableStateFileNamesRecovery(t *testing.T) {
	cases := map[string]string{
		"corrupt":       "{not json",
		"mismatched id": `{"version": 1, "id": "other-12345678", "root": "/work/other"}`,
		"no id":         `{"version": 1, "root": "/work/shop"}`,
		"relative root": `{"version": 1, "id": "shop-0a1b2c3d", "root": "work/shop"}`,
		"another root":  `{"version": 1, "id": "shop-0a1b2c3d", "root": "/work/shop"}`,
		"no root":       `{"version": 1, "id": "shop-0a1b2c3d"}`,
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			dir := setState(t)
			id := "shop-0a1b2c3d"
			path := writeState(t, dir, id, text)
			_, err := All()
			wantRecovery(t, err, path)
			_, _, err = LoadID(id)
			wantRecovery(t, err, path)
		})
	}
}

func TestNewerStateFileAsksForUpgrade(t *testing.T) {
	dir := setState(t)
	id := "shop-0a1b2c3d"
	path := writeState(t, dir, id, `{"version": 999, "id": "shop-0a1b2c3d"}`)
	for _, err := range []error{func() error { _, err := All(); return err }(), func() error { _, _, err := LoadID(id); return err }()} {
		if err == nil || !strings.Contains(err.Error(), "upgrade qm") || !strings.Contains(err.Error(), path) {
			t.Errorf("error %v, want the upgrade message naming %s", err, path)
		}
		if err != nil && strings.Contains(err.Error(), "mv ") {
			t.Errorf("error %q offers to move a newer qm's file aside", err)
		}
	}
}

func TestDanglingStateLinkNamesRecovery(t *testing.T) {
	dir := setState(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "shop-0a1b2c3d.json")
	if err := os.Symlink(filepath.Join(t.TempDir(), "gone.json"), path); err != nil {
		t.Fatal(err)
	}
	_, err := All()
	wantRecovery(t, err, path)
	_, found, err := LoadID("shop-0a1b2c3d")
	if found {
		t.Error("a link to a missing file loaded as found")
	}
	wantRecovery(t, err, path)
}
