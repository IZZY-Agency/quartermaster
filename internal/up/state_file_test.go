package up

import (
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
