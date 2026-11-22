package up

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/launch"
)

func TestProjectIDStable(t *testing.T) {
	id := ProjectID("/work/My Shop.v2")
	if id != ProjectID("/work/My Shop.v2") {
		t.Fatal("same root, different ids")
	}
	if !regexp.MustCompile(`^MyShopv2-[0-9a-f]{8}$`).MatchString(id) {
		t.Errorf("id = %q, want MyShopv2-<8 hex>", id)
	}
	if other := ProjectID("/elsewhere/My Shop.v2"); other == id {
		t.Errorf("two roots share the id %q", id)
	}
	long := ProjectID("/work/" + strings.Repeat("a", 30) + "é_-" + strings.Repeat("b", 30))
	if want := strings.Repeat("a", 30) + "_-" + strings.Repeat("b", 8) + "-"; !strings.HasPrefix(long, want) || len(long) != 40+1+8 {
		t.Errorf("long id = %q, want prefix %q and length 49", long, want)
	}
	if got := ProjectID("/work/..."); !strings.HasPrefix(got, "project-") {
		t.Errorf("id of a name with no usable character = %q, want project-<hex>", got)
	}
}

func TestWorkspaceNameCollision(t *testing.T) {
	root := "/work/shop"
	if got := WorkspaceName(root, func(string) bool { return false }); got != "shop" {
		t.Errorf("free name = %q, want shop", got)
	}
	got := WorkspaceName(root, func(n string) bool { return n == "shop" })
	want := "shop-" + strings.TrimPrefix(ProjectID(root), "shop-")[:4]
	if got != want {
		t.Errorf("taken name = %q, want %q", got, want)
	}
	if got == WorkspaceName("/other/shop", func(n string) bool { return n == "shop" }) {
		t.Errorf("two roots got the same fallback name %q", got)
	}
}

func setState(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	t.Setenv("XDG_STATE_HOME", d)
	return filepath.Join(d, "quartermaster", "projects")
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := setState(t)
	root := "/work/shop"
	at := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	p := Project{
		ID: ProjectID(root), Root: root, Workspace: "shop", Launcher: "agterm",
		ClaudeArgs:       []string{"--permission-mode", "acceptEdits"},
		Coordinator:      Session{ID: "00000000-0000-4000-8000-0000000000c1", Name: "shop-coord", Started: at},
		PastCoordinators: []string{"00000000-0000-4000-8000-0000000000c0"},
		Sessions:         map[string]launch.SessionRef{"00000000-0000-4000-8000-0000000000c1": {Launcher: "agterm", ID: "s1", Name: "shop-coord"}},
		Done:             map[string]time.Time{"00000000-0000-4000-8000-0000000000a1": at.Add(time.Hour)},
		Created:          at,
	}
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	got, ok, err := Load(root)
	if err != nil || !ok {
		t.Fatalf("Load = %v, %v", ok, err)
	}
	if got.Updated.IsZero() || got.Version != 1 {
		t.Errorf("Updated %v, Version %d: want set", got.Updated, got.Version)
	}
	p.Version, p.Updated = got.Version, got.Updated
	if !reflect.DeepEqual(got, p) {
		t.Errorf("round trip:\n got %+v\nwant %+v", got, p)
	}
	fi, err := os.Stat(filepath.Join(dir, p.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode %v, want 0600", fi.Mode().Perm())
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("folder mode %v, want 0700", di.Mode().Perm())
	}
	b, _ := os.ReadFile(filepath.Join(dir, p.ID+".json"))
	for _, key := range []string{"version", "id", "root", "workspace", "launcher", "claude_args", "coordinator", "session_id", "past_coordinators", "sessions", "done", "created", "updated"} {
		if !strings.Contains(string(b), `"`+key+`":`) {
			t.Errorf("state file lacks key %q", key)
		}
	}
	if strings.Contains(string(b), "down_at") {
		t.Error("zero down_at written")
	}
	if _, ok, err := Load("/work/other"); ok || err != nil {
		t.Errorf("Load of an unrecorded root = %v, %v; want false, nil", ok, err)
	}
	all, err := All()
	if err != nil || len(all) != 1 || all[0].ID != p.ID {
		t.Errorf("All = %v, %v", all, err)
	}
}

func TestLoadRefusesNewerVersion(t *testing.T) {
	dir := setState(t)
	root := "/work/shop"
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"version":2,"id":"` + ProjectID(root) + `","root":"/work/shop"}`
	if err := os.WriteFile(filepath.Join(dir, ProjectID(root)+".json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(root); err == nil || !strings.Contains(err.Error(), "version 2") {
		t.Errorf("Load of a version 2 file: err %v, want a refusal", err)
	}
}

func TestLoadID(t *testing.T) {
	setState(t)
	p := Project{ID: ProjectID("/work/shop"), Root: "/work/shop"}
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	got, ok, err := LoadID(p.ID)
	if err != nil || !ok || got.Root != p.Root {
		t.Errorf("LoadID = %+v, %v, %v", got, ok, err)
	}
	if _, ok, err := LoadID("none-00000000"); ok || err != nil {
		t.Errorf("LoadID of an unknown id = %v, %v", ok, err)
	}
	for _, bad := range []string{"", "..", "a/b"} {
		if _, _, err := LoadID(bad); err == nil {
			t.Errorf("LoadID(%q): want an error", bad)
		}
	}
}

func TestAllSkipsTempFiles(t *testing.T) {
	dir := setState(t)
	p := Project{ID: ProjectID("/work/shop"), Root: "/work/shop"}
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	// A temporary file left by a crash, half written.
	for _, stray := range []string{".project-123.json", ".project-456.tmp"} {
		if err := os.WriteFile(filepath.Join(dir, stray), []byte(`{"id": "sh`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	all, err := All()
	if err != nil || len(all) != 1 || all[0].ID != p.ID {
		t.Errorf("All = %v, %v; want the one project", all, err)
	}
	left, _ := filepath.Glob(filepath.Join(dir, ".project-*"))
	if len(left) != 2 {
		t.Errorf("Save left temporary files: %v", left)
	}
}

func TestAllEmpty(t *testing.T) {
	setState(t)
	if all, err := All(); err != nil || len(all) != 0 {
		t.Errorf("All with no state = %v, %v", all, err)
	}
}

func TestUpLockRefusesSecondRun(t *testing.T) {
	setState(t)
	unlock, err := Lock("shop-1a2b3c4d")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock("shop-1a2b3c4d"); !errors.Is(err, ErrBusy) {
		t.Fatalf("second Lock = %v, want ErrBusy", err)
	}
	if u, err := Lock("other-1a2b3c4d"); err != nil {
		t.Errorf("another project's lock = %v", err)
	} else {
		u()
	}
	unlock()
	again, err := Lock("shop-1a2b3c4d")
	if err != nil {
		t.Fatalf("Lock after unlock = %v", err)
	}
	again()
}

func TestRoot(t *testing.T) {
	plain := t.TempDir()
	realPlain, err := filepath.EvalSymlinks(plain)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := Root(plain); err != nil || got != realPlain {
		t.Errorf("Root outside git = %q, %v; want %q", got, err, realPlain)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(plain, link); err != nil {
		t.Fatal(err)
	}
	if got, err := Root(link); err != nil || got != realPlain {
		t.Errorf("Root through a symlink = %q, %v; want %q", got, err, realPlain)
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	sub := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := Root(sub)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(repo)
	if got != want {
		t.Errorf("Root in a repository = %q, want %q", got, want)
	}
}

func TestSaveLoadAdoptedRoundTrip(t *testing.T) {
	setState(t)
	root := "/work/shop"
	at := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	p := Project{
		ID: ProjectID(root), Root: root, Workspace: "shop", Launcher: "agterm",
		ClaudeArgs:  []string{"--permission-mode", "acceptEdits"},
		Coordinator: Session{ID: "00000000-0000-4000-8000-0000000000c1", Name: "shop-coord", Started: at, Adopted: true},
		Adopted: map[string]AdoptedSession{
			"00000000-0000-4000-8000-0000000000a1": {},
			"00000000-0000-4000-8000-0000000000a2": {Name: "w-hand", Cwd: "/work/shop-wt", Model: "opus", Args: []string{"--model", "opus"}, Started: at.Add(time.Hour)},
		},
		Created: at,
	}
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	got, ok, err := Load(root)
	if err != nil || !ok {
		t.Fatalf("Load = %v, %v", ok, err)
	}
	if got.Version != 1 {
		t.Errorf("Version %d, want 1", got.Version)
	}
	p.Version, p.Updated = got.Version, got.Updated
	if !reflect.DeepEqual(got, p) {
		t.Errorf("round trip:\n got %+v\nwant %+v", got, p)
	}
}

func TestProjectWithoutAdoptionKeys(t *testing.T) {
	at := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	p := Project{
		Version: 1, ID: "shop-1a2b3c4d", Root: "/work/shop", Workspace: "shop",
		Coordinator: Session{ID: "00000000-0000-4000-8000-0000000000c1", Name: "shop-coord", Started: at},
		Created:     at,
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "adopted") {
		t.Errorf("a project that adopted nothing writes an adopted key:\n%s", b)
	}
}
