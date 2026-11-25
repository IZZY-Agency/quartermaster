package launch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// stateDir points qm's state folder at a new folder and returns it.
func stateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	return dir
}

type fakeSession struct {
	id, name, color string
	on              []string
}

// exitError is the error of a process that really ran and exited with code.
func exitError(code int) error {
	return exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
}

// fakeCtl stands in for agtermctl: it keeps windows with sessions and
// answers window list, tree, session new, session tint and session restore
// the way agtermctl does (verified live: colours fold to lower case, --on
// omitted keeps the placement else icon, placements come back as [icon name]).
type fakeCtl struct {
	windows    [][]*fakeSession // the first window is the frontmost
	calls      [][]string
	rejectTint bool   // an agterm older than --tint
	failNew    string // session new fails with this text
	failTint   string // session tint fails with this text
	onNew      func() // runs when session new has created its session
	onRestore  func(id string)
	onTint     func(id string) // runs when session tint is about to change a tab
	badTree    map[int]bool    // windows whose tree cannot be read
	next       int
}

func (f *fakeCtl) add(window int, id, name, color string, on ...string) *fakeSession {
	s := &fakeSession{id: id, name: name, color: color, on: on}
	f.windows[window] = append(f.windows[window], s)
	return s
}

func (f *fakeCtl) find(id string) *fakeSession {
	for _, w := range f.windows {
		for _, s := range w {
			if s.id == id {
				return s
			}
		}
	}
	return nil
}

func (f *fakeCtl) ctl(args []string) []string {
	var out []string
	for _, a := range args {
		if a != "--json" {
			out = append(out, a)
		}
	}
	return out
}

func flagValue(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func canonOn(on string) []string {
	var out []string
	for _, p := range []string{"icon", "name"} {
		if slices.Contains(strings.Split(on, ","), p) {
			out = append(out, p)
		}
	}
	return out
}

func (f *fakeCtl) run(_ context.Context, bin string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	a := f.ctl(args)
	switch {
	case slices.Equal(a, []string{"window", "list"}):
		var ws []string
		for i := range f.windows {
			ws = append(ws, fmt.Sprintf(`{"id":"win%d","open":true}`, i))
		}
		return []byte(`{"ok":true,"result":{"windows":[` + strings.Join(ws, ",") + `]}}`), nil
	case a[0] == "tree":
		i := 0
		if w := flagValue(a, "--window"); w != "" {
			_, _ = fmt.Sscanf(w, "win%d", &i)
		}
		if f.badTree[i] {
			return nil, errors.New(bin + ": tree failed")
		}
		var ss []string
		for _, s := range f.windows[i] {
			m := map[string]any{"id": s.id, "name": s.name}
			if s.color != "" {
				m["tint"], m["tintOn"] = s.color, s.on
			}
			b, _ := json.Marshal(m)
			ss = append(ss, string(b))
		}
		return []byte(`{"ok":true,"result":{"tree":{"workspaces":[{"id":"w","name":"alpha","sessions":[` + strings.Join(ss, ",") + `]}]}}}`), nil
	case a[0] == "session" && a[1] == "tint":
		if f.failTint != "" {
			return nil, errors.New(bin + ": " + f.failTint)
		}
		s := f.find(flagValue(a, "--target"))
		if s == nil {
			return []byte(`{"ok":false,"error":"no such session: x"}`), nil
		}
		if f.onTint != nil {
			f.onTint(s.id)
		}
		if a[2] == "clear" {
			s.color, s.on = "", nil
		} else {
			on := flagValue(a, "--on")
			s.color = strings.ToLower(a[2])
			if on != "" {
				s.on = canonOn(on)
			} else if len(s.on) == 0 {
				s.on = []string{"icon"}
			}
		}
		return []byte(`{"ok":true,"result":{"id":"` + s.id + `"}}`), nil
	case a[0] == "session" && a[1] == "new":
		if f.rejectTint && slices.Contains(a, "--tint") {
			return nil, fmt.Errorf("%s: Error: Unknown option '--tint'\nUsage: agtermctl session new <options>: %w", bin, exitError(64))
		}
		if f.failNew != "" {
			return nil, errors.New(bin + ": " + f.failNew)
		}
		f.next++
		s := &fakeSession{id: fmt.Sprintf("new-%d", f.next), name: flagValue(a, "--name")}
		if c := flagValue(a, "--tint"); c != "" {
			s.color, s.on = strings.ToLower(c), canonOn(flagValue(a, "--tint-on"))
			if len(s.on) == 0 {
				s.on = []string{"icon"}
			}
		}
		f.windows[0] = append(f.windows[0], s)
		if f.onNew != nil {
			f.onNew()
		}
		return []byte(`{"ok":true,"result":{"id":"` + s.id + `"}}`), nil
	case a[0] == "session" && a[1] == "restore":
		if f.onRestore != nil {
			f.onRestore(flagValue(a, "--target"))
		}
		return []byte(`{"ok":true,"result":{}}`), nil
	}
	return nil, fmt.Errorf("fakeCtl: unexpected %q", args)
}

func (f *fakeCtl) agterm() Agterm { return Agterm{Bin: "agtermctl", Run: f.run} }

func (f *fakeCtl) count(verb ...string) int {
	n := 0
	for _, c := range f.calls {
		if len(c) >= len(verb) && slices.Equal(c[:len(verb)], verb) {
			n++
		}
	}
	return n
}

func TestTintConstructors(t *testing.T) {
	for _, tc := range []struct {
		got, want Tint
	}{
		{CoordinatorTint("purple"), Tint{"purple", "icon,name"}},
		{WorkerTint("teal"), Tint{"teal", "icon"}},
		{AttentionTint("red"), Tint{"red", "icon,name"}},
		{CoordinatorTint(""), Tint{}},
		{WorkerTint(""), Tint{}},
		{AttentionTint(""), Tint{}},
	} {
		if tc.got != tc.want {
			t.Errorf("got %+v, want %+v", tc.got, tc.want)
		}
	}
	if !(Tint{}).IsZero() || WorkerTint("teal").IsZero() {
		t.Error("IsZero wrong")
	}
}

func TestTintOwned(t *testing.T) {
	teal, blue := WorkerTint("teal"), WorkerTint("blue")
	for _, tc := range []struct {
		name     string
		cur, mem Tint
		has      bool
		want     bool
	}{
		{"untinted tab, nothing remembered", Tint{}, Tint{}, false, true},
		{"untinted tab, something remembered (cleared by hand)", Tint{}, teal, true, true},
		{"tinted by hand, nothing remembered", blue, Tint{}, false, false},
		{"tinted by hand, other colour remembered", blue, teal, true, false},
		{"exactly what qm set", teal, teal, true, true},
		{"same colour, other placement", Tint{"teal", "icon,name"}, teal, true, false},
		{"same placement, other colour", Tint{"teal", "name"}, Tint{"blue", "name"}, true, false},
	} {
		if got := tintOwned(tc.cur, tc.mem, tc.has); got != tc.want {
			t.Errorf("%s: owned = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestAgtermWorkspacesReadsTint(t *testing.T) {
	b, err := os.ReadFile("testdata/agterm-tree-tint.json")
	if err != nil {
		t.Fatal(err)
	}
	r := &recorder{out: string(b)}
	wss, err := Agterm{Bin: "agtermctl", Run: r.run}.Workspaces(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got []Tint
	for _, s := range wss[0].Sessions {
		got = append(got, s.Tint)
	}
	want := []Tint{{"purple", "icon,name"}, {"#abcdef", "name"}, {}, {"teal", "icon,name"}}
	if !slices.Equal(got, want) {
		t.Errorf("tints = %+v, want %+v (lower case, placement in icon,name order, none when untinted)", got, want)
	}
}

func TestTintStoreConcurrentUpdatesAllLand(t *testing.T) {
	stateDir(t)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("s%d", i)
			if err := withTints(context.Background(), func(b *tintBook) error {
				b.Set(id, WorkerTint("teal"))
				return nil
			}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	for i := range 20 {
		if _, ok, err := rememberedTint(fmt.Sprintf("s%d", i)); err != nil || !ok {
			t.Errorf("s%d lost: %v %v", i, ok, err)
		}
	}
}

func TestTintStoreFileModeAndNoLeftovers(t *testing.T) {
	dir := stateDir(t)
	if err := withTints(context.Background(), func(b *tintBook) error { b.Set("a", WorkerTint("teal")); return nil }); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "quartermaster", "tints.json")
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("tints.json: %v %v", fi, err)
	}
	ents, _ := os.ReadDir(filepath.Dir(p))
	for _, e := range ents {
		if strings.Contains(e.Name(), "tmp") || strings.HasPrefix(e.Name(), ".tints-") {
			t.Errorf("leftover temporary file %s", e.Name())
		}
	}
}

func TestTintStoreCorruptFileReadsAsEmpty(t *testing.T) {
	dir := stateDir(t)
	p := filepath.Join(dir, "quartermaster", "tints.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := rememberedTint("a"); ok || err != nil {
		t.Errorf("rememberedTint = %v, %v; a corrupt file remembers nothing, so no tab looks qm's", ok, err)
	}
	if err := withTints(context.Background(), func(b *tintBook) error { b.Set("a", WorkerTint("teal")); return nil }); err != nil {
		t.Fatalf("update over a corrupt file: %v", err)
	}
	if _, ok, _ := rememberedTint("a"); !ok {
		t.Error("the update did not replace the corrupt file")
	}
}

func remember(t *testing.T, id string, tint Tint) {
	t.Helper()
	if err := withTints(context.Background(), func(b *tintBook) error { b.Set(id, tint); return nil }); err != nil {
		t.Fatal(err)
	}
}

// rememberAged remembers tint for id as set age ago.
func rememberAged(t *testing.T, id string, tint Tint, age time.Duration) {
	t.Helper()
	if err := withTints(context.Background(), func(b *tintBook) error {
		b.Set(id, tint)
		e := b.sessions[id]
		e.At = time.Now().Add(-age).Unix()
		b.sessions[id] = e
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// rememberMissing is rememberAged for a tab first missed missing ago.
func rememberMissing(t *testing.T, id string, tint Tint, age, missing time.Duration) {
	t.Helper()
	rememberAged(t, id, tint, age)
	if err := withTints(context.Background(), func(b *tintBook) error {
		e := b.sessions[id]
		e.Missing = time.Now().Add(-missing).Unix()
		b.sessions[id], b.dirty = e, true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func entry(t *testing.T, id string) tintEntry {
	t.Helper()
	path, err := tintsFile()
	if err != nil {
		t.Fatal(err)
	}
	m, err := readTints(path)
	if err != nil {
		t.Fatal(err)
	}
	e, ok := m[id]
	if !ok {
		t.Fatalf("no entry for %s", id)
	}
	return e
}

func memory(t *testing.T, id string) (Tint, bool) {
	t.Helper()
	m, ok, err := rememberedTint(id)
	if err != nil {
		t.Fatal(err)
	}
	return m, ok
}

func TestApplyTintSetsAnUntintedTabAndRemembersIt(t *testing.T) {
	stateDir(t)
	f := &fakeCtl{windows: make([][]*fakeSession, 1)}
	f.add(0, "s1", "worker", "")
	res, err := f.agterm().ApplyTint(context.Background(), "s1", WorkerTint("teal"))
	if err != nil || res != TintApplied {
		t.Fatalf("ApplyTint = %v, %v", res, err)
	}
	if s := f.find("s1"); s.color != "teal" || !slices.Equal(s.on, []string{"icon"}) {
		t.Errorf("tab = %+v", s)
	}
	if m, ok := memory(t, "s1"); !ok || m != WorkerTint("teal") {
		t.Errorf("memory = %+v %v", m, ok)
	}
	if got := flagValue(f.calls[len(f.calls)-1], "--on"); got != "icon" {
		t.Errorf("--on %q: the placement must always be passed, agterm keeps the old one otherwise", got)
	}
}

func TestApplyTintLeavesAHandTintedTabAlone(t *testing.T) {
	stateDir(t)
	for name, mem := range map[string]*Tint{"nothing remembered": nil, "other tint remembered": {Color: "teal", On: "icon"}} {
		t.Run(name, func(t *testing.T) {
			stateDir(t)
			f := &fakeCtl{windows: make([][]*fakeSession, 1)}
			f.add(0, "s1", "worker", "blue", "name")
			if mem != nil {
				remember(t, "s1", *mem)
			}
			res, err := f.agterm().ApplyTint(context.Background(), "s1", AttentionTint("red"))
			if err != nil || res != TintLeft {
				t.Fatalf("ApplyTint = %v, %v; want TintLeft", res, err)
			}
			if f.count("session", "tint") != 0 {
				t.Errorf("agtermctl session tint ran: %q", f.calls)
			}
			if s := f.find("s1"); s.color != "blue" || !slices.Equal(s.on, []string{"name"}) {
				t.Errorf("tab = %+v", s)
			}
			// clearing is the owner's too
			if res, _ := f.agterm().ApplyTint(context.Background(), "s1", Tint{}); res != TintLeft || f.find("s1").color != "blue" {
				t.Errorf("clear: %v, tab %+v", res, f.find("s1"))
			}
		})
	}
}

func TestApplyTintRecolorsItsOwnTintBothWays(t *testing.T) {
	stateDir(t)
	f := &fakeCtl{windows: make([][]*fakeSession, 1)}
	f.add(0, "s1", "worker", "teal", "icon")
	remember(t, "s1", WorkerTint("teal"))
	a := f.agterm()
	if res, err := a.ApplyTint(context.Background(), "s1", AttentionTint("orange")); err != nil || res != TintApplied {
		t.Fatalf("to orange: %v %v", res, err)
	}
	if s := f.find("s1"); s.color != "orange" || !slices.Equal(s.on, []string{"icon", "name"}) {
		t.Errorf("tab = %+v", s)
	}
	if m, _ := memory(t, "s1"); m != AttentionTint("orange") {
		t.Errorf("memory = %+v", m)
	}
	if res, err := a.ApplyTint(context.Background(), "s1", WorkerTint("teal")); err != nil || res != TintApplied {
		t.Fatalf("back to teal: %v %v", res, err)
	}
	if s := f.find("s1"); s.color != "teal" || !slices.Equal(s.on, []string{"icon"}) {
		t.Errorf("tab = %+v: the placement went back with the colour", s)
	}
}

func TestApplyTintNothingToDoRunsNothing(t *testing.T) {
	stateDir(t)
	f := &fakeCtl{windows: make([][]*fakeSession, 1)}
	f.add(0, "s1", "worker", "teal", "icon")
	remember(t, "s1", WorkerTint("teal"))
	res, err := f.agterm().ApplyTint(context.Background(), "s1", WorkerTint("teal"))
	if err != nil || res != TintUnchanged || f.count("session", "tint") != 0 {
		t.Errorf("ApplyTint = %v, %v, calls %q", res, err, f.calls)
	}
}

func TestApplyTintClearsItsOwnTintAndForgetsIt(t *testing.T) {
	stateDir(t)
	f := &fakeCtl{windows: make([][]*fakeSession, 1)}
	f.add(0, "s1", "worker", "teal", "icon")
	remember(t, "s1", WorkerTint("teal"))
	res, err := f.agterm().ApplyTint(context.Background(), "s1", Tint{})
	if err != nil || res != TintApplied || f.find("s1").color != "" {
		t.Fatalf("ApplyTint = %v, %v, tab %+v", res, err, f.find("s1"))
	}
	if _, ok := memory(t, "s1"); ok {
		t.Error("the cleared tab is still remembered")
	}
	if c := f.calls[len(f.calls)-1]; !slices.Contains(c, "clear") {
		t.Errorf("last call %q does not clear", c)
	}
}

func TestApplyTintTakesTheTabBackAfterTheOwnerClearedItByHand(t *testing.T) {
	stateDir(t)
	f := &fakeCtl{windows: make([][]*fakeSession, 1)}
	f.add(0, "s1", "worker", "")
	remember(t, "s1", WorkerTint("teal")) // qm set teal, the owner cleared it
	res, err := f.agterm().ApplyTint(context.Background(), "s1", AttentionTint("red"))
	if err != nil || res != TintApplied || f.find("s1").color != "red" {
		t.Errorf("ApplyTint = %v, %v, tab %+v", res, err, f.find("s1"))
	}
}

func TestApplyTintFindsATabInAnotherWindowAndKeepsOtherWindowsEntries(t *testing.T) {
	stateDir(t)
	f := &fakeCtl{windows: make([][]*fakeSession, 2)}
	f.add(0, "front", "front", "")
	f.add(1, "back", "back", "")
	f.add(1, "other", "other", "teal", "icon")
	remember(t, "other", WorkerTint("teal"))
	rememberMissing(t, "closed", WorkerTint("teal"), 40*24*time.Hour, 31*24*time.Hour)
	rememberAged(t, "reopenable", WorkerTint("teal"), 2*24*time.Hour)
	if res, err := f.agterm().ApplyTint(context.Background(), "back", WorkerTint("teal")); err != nil || res != TintApplied {
		t.Fatalf("ApplyTint = %v, %v", res, err)
	}
	if _, ok := memory(t, "other"); !ok {
		t.Error("the entry of a tab in the other window was pruned")
	}
	if _, ok := memory(t, "closed"); ok {
		t.Error("the entry of a tab gone for over a month was not dropped")
	}
	if _, ok := memory(t, "reopenable"); !ok {
		t.Error("the entry of a tab that may be in a closed window agterm can reopen was dropped")
	}
}

func TestApplyTintDoesNotPruneWhenAWindowCouldNotBeRead(t *testing.T) {
	stateDir(t)
	f := &fakeCtl{windows: make([][]*fakeSession, 1)}
	f.add(0, "s1", "worker", "")
	rememberAged(t, "elsewhere", WorkerTint("teal"), 90*24*time.Hour)
	a := Agterm{Bin: "agtermctl", Run: func(ctx context.Context, bin string, args ...string) ([]byte, error) {
		if len(args) > 1 && args[0] == "window" {
			return nil, errors.New("window list failed")
		}
		return f.run(ctx, bin, args...)
	}}
	if _, err := a.ApplyTint(context.Background(), "s1", WorkerTint("teal")); err != nil {
		t.Fatal(err)
	}
	if _, ok := memory(t, "elsewhere"); !ok {
		t.Error("an entry was pruned from a partial view of the tabs")
	}
}

func TestApplyTintSessionGone(t *testing.T) {
	stateDir(t)
	f := &fakeCtl{windows: make([][]*fakeSession, 1)}
	f.add(0, "s1", "worker", "")
	remember(t, "s9", WorkerTint("teal"))
	_, err := f.agterm().ApplyTint(context.Background(), "s9", WorkerTint("teal"))
	if !errors.Is(err, ErrGone) {
		t.Fatalf("err = %v, want ErrGone", err)
	}
	if _, ok := memory(t, "s9"); !ok {
		t.Error("the entry was dropped at once: agterm can bring a closed tab back")
	}
}

func TestApplyTintFailedSetKeepsTheMemory(t *testing.T) {
	stateDir(t)
	f := &fakeCtl{windows: make([][]*fakeSession, 1)}
	f.add(0, "s1", "worker", "teal", "icon")
	remember(t, "s1", WorkerTint("teal"))
	f.failTint = "boom"
	if _, err := f.agterm().ApplyTint(context.Background(), "s1", AttentionTint("red")); err == nil {
		t.Fatal("no error")
	}
	if m, _ := memory(t, "s1"); m != WorkerTint("teal") {
		t.Errorf("memory = %+v; it must still say what the tab shows", m)
	}
}

func TestAgtermSpawnTintedCreatesWithTintAndRemembersIt(t *testing.T) {
	stateDir(t)
	f := &fakeCtl{windows: make([][]*fakeSession, 1)}
	ref, err := f.agterm().Spawn(context.Background(), LaunchSpec{Name: "w", Cwd: "/x", Script: "/s.sh", Tint: WorkerTint("teal")})
	if err != nil || ref.TintNote != "" {
		t.Fatalf("Spawn = %+v, %v", ref, err)
	}
	c := f.calls[0]
	if flagValue(c, "--tint") != "teal" || flagValue(c, "--tint-on") != "icon" {
		t.Errorf("session new args %q", c)
	}
	if m, ok := memory(t, ref.ID); !ok || m != WorkerTint("teal") {
		t.Errorf("memory = %+v %v", m, ok)
	}
	if f.count("session", "tint") != 0 {
		t.Error("tinted by a second call instead of at creation")
	}
}

func TestAgtermSpawnWithoutTintTouchesNoMemory(t *testing.T) {
	dir := stateDir(t)
	f := &fakeCtl{windows: make([][]*fakeSession, 1)}
	ref, err := f.agterm().Spawn(context.Background(), LaunchSpec{Name: "w", Cwd: "/x", Script: "/s.sh"})
	if err != nil || ref.TintNote != "" {
		t.Fatalf("Spawn = %+v, %v", ref, err)
	}
	if slices.Contains(f.calls[0], "--tint") || slices.Contains(f.calls[0], "--tint-on") {
		t.Errorf("args %q", f.calls[0])
	}
	if _, err := os.Stat(filepath.Join(dir, "quartermaster")); err == nil {
		t.Error("the state folder was touched")
	}
}

func TestAgtermSpawnOldAgtermRetriesOnceWithoutTint(t *testing.T) {
	stateDir(t)
	f := &fakeCtl{windows: make([][]*fakeSession, 1), rejectTint: true}
	ref, err := f.agterm().Spawn(context.Background(), LaunchSpec{Name: "w", Cwd: "/x", Script: "/s.sh", Tint: WorkerTint("teal")})
	if err != nil {
		t.Fatal(err)
	}
	if f.count("session", "new") != 2 || slices.Contains(f.calls[1], "--tint") || slices.Contains(f.calls[1], "--tint-on") {
		t.Errorf("calls %q", f.calls)
	}
	if len(f.windows[0]) != 1 || ref.ID != f.windows[0][0].id {
		t.Errorf("sessions %d, ref %+v", len(f.windows[0]), ref)
	}
	if ref.TintNote == "" || !strings.Contains(ref.TintNote, "--tint") {
		t.Errorf("TintNote = %q", ref.TintNote)
	}
	if _, ok := memory(t, ref.ID); ok {
		t.Error("an untinted tab is remembered as tinted")
	}
}

func TestAgtermSpawnOtherFailureWithTintDoesNotRetry(t *testing.T) {
	stateDir(t)
	for _, msg := range []string{"agtermctl: timed out waiting for agterm: exit status 1", "agtermctl session new: tint rejected"} {
		f := &fakeCtl{windows: make([][]*fakeSession, 1), failNew: msg}
		_, err := f.agterm().Spawn(context.Background(), LaunchSpec{Name: "w", Cwd: "/x", Script: "/s.sh", Tint: WorkerTint("teal")})
		if err == nil {
			t.Fatalf("%q: no error", msg)
		}
		if n := f.count("session", "new"); n != 1 {
			t.Errorf("%q: %d session new calls; the first may have created a tab, so none may follow", msg, n)
		}
	}
}

func TestAgtermSpawnUnreadableStateStartsUntinted(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "afile")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", file) // a file where the folder should be
	f := &fakeCtl{windows: make([][]*fakeSession, 1)}
	ref, err := f.agterm().Spawn(context.Background(), LaunchSpec{Name: "w", Cwd: "/x", Script: "/s.sh", Tint: WorkerTint("teal")})
	if err != nil {
		t.Fatalf("Spawn failed for a tint that cannot be remembered: %v", err)
	}
	if f.count("session", "new") != 1 || slices.Contains(f.calls[0], "--tint") {
		t.Errorf("calls %q", f.calls)
	}
	if ref.TintNote == "" {
		t.Error("no TintNote")
	}
}

func TestAgtermSpawnThatWaitsTooLongForTheLockStartsUntinted(t *testing.T) {
	dir := stateDir(t)
	defer func(d time.Duration) { tintsWait = d }(tintsWait)
	tintsWait = 50 * time.Millisecond
	p := filepath.Join(dir, "quartermaster", "tints.json.lock")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	held, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600) // another qm, creating its tab
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	f := &fakeCtl{windows: make([][]*fakeSession, 1)}
	start := time.Now()
	ref, err := f.agterm().Spawn(context.Background(), LaunchSpec{Name: "w", Cwd: "/x", Script: "/s.sh", Tint: WorkerTint("teal")})
	if err != nil {
		t.Fatalf("Spawn failed because another qm held the tint memory: %v", err)
	}
	if waited := time.Since(start); waited < tintsWait {
		t.Errorf("gave up after %v, before the %v wait", waited, tintsWait)
	}
	if f.count("session", "new") != 1 || slices.Contains(f.calls[0], "--tint") || f.count("session", "restore") != 1 {
		t.Errorf("want one untinted, pinned tab; calls %q", f.calls)
	}
	if s := f.find(ref.ID); s == nil || s.color != "" {
		t.Errorf("tab %+v, ref %+v", s, ref)
	}
	if !strings.HasPrefix(ref.TintNote, "tab not tinted: tint memory: lock: ") {
		t.Errorf("TintNote = %q", ref.TintNote)
	}
}

func TestAgtermSpawnClearsTheTintItCouldNotRemember(t *testing.T) {
	dir := stateDir(t)
	f := &fakeCtl{windows: make([][]*fakeSession, 1)}
	f.onNew = func() { // the memory file becomes unwritable once the tab exists
		p := filepath.Join(dir, "quartermaster", "tints.json")
		_ = os.Remove(p)
		if err := os.MkdirAll(filepath.Join(p, "blocker"), 0o700); err != nil {
			t.Error(err)
		}
	}
	ref, err := f.agterm().Spawn(context.Background(), LaunchSpec{Name: "w", Cwd: "/x", Script: "/s.sh", Tint: WorkerTint("teal")})
	if err != nil {
		t.Fatalf("Spawn = %v", err)
	}
	if s := f.find(ref.ID); s == nil || s.color != "" {
		t.Errorf("tab %+v: an unremembered tint would read as the owner's", s)
	}
	if ref.TintNote == "" {
		t.Error("no TintNote")
	}
}

func TestTmuxIgnoresTint(t *testing.T) {
	r := &recorder{out: "@7\n"}
	spec := LaunchSpec{Name: "w", Cwd: "/x", Script: "/s.sh", WorkspaceName: "proj"}
	if _, err := (Tmux{Bin: "tmux", Run: r.run, Inside: true}).Spawn(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	want := slices.Clone(r.calls)
	r2 := &recorder{out: "@7\n"}
	spec.Tint = WorkerTint("teal")
	ref, err := (Tmux{Bin: "tmux", Run: r2.run, Inside: true}).Spawn(context.Background(), spec)
	if err != nil || ref.TintNote != "" {
		t.Fatalf("Spawn = %+v, %v", ref, err)
	}
	if fmt.Sprint(r2.calls) != fmt.Sprint(want) {
		t.Errorf("tmux calls changed with a tint:\n%q\n%q", r2.calls, want)
	}
}

func TestApplyTintDropsEveryEntryOlderThanAYear(t *testing.T) {
	stateDir(t)
	f := &fakeCtl{windows: make([][]*fakeSession, 1)}
	f.add(0, "s1", "worker", "")
	f.add(0, "s2", "worker", "teal", "icon")
	rememberAged(t, "s2", WorkerTint("teal"), 400*24*time.Hour)
	if _, err := f.agterm().ApplyTint(context.Background(), "s1", WorkerTint("teal")); err != nil {
		t.Fatal(err)
	}
	if _, ok := memory(t, "s2"); ok {
		t.Error("an entry over a year old stayed, so the file could grow for ever")
	}
}

func TestAgtermSpawnDropsEveryEntryOlderThanAYear(t *testing.T) {
	stateDir(t)
	f := &fakeCtl{windows: make([][]*fakeSession, 1)}
	rememberAged(t, "ancient", WorkerTint("teal"), 400*24*time.Hour)
	rememberAged(t, "absent", WorkerTint("teal"), 90*24*time.Hour)
	if _, err := f.agterm().Spawn(context.Background(), LaunchSpec{Name: "w", Cwd: "/x", Script: "/s.sh", Tint: WorkerTint("teal")}); err != nil {
		t.Fatal(err)
	}
	if _, ok := memory(t, "ancient"); ok {
		t.Error("an entry over a year old stayed: with only spawns running, the file grows for ever")
	}
	if _, ok := memory(t, "absent"); !ok {
		t.Error("a younger entry was dropped by a spawn, which has not looked which tabs exist")
	}
}

func TestApplyTintCountsTheMonthFromWhenTheTabWentMissing(t *testing.T) {
	stateDir(t)
	f := &fakeCtl{windows: make([][]*fakeSession, 1)}
	f.add(0, "s1", "worker", "")
	// tinted long ago, its window closed only now
	rememberAged(t, "back", WorkerTint("teal"), 90*24*time.Hour)
	a := f.agterm()
	if _, err := a.ApplyTint(context.Background(), "s1", WorkerTint("teal")); err != nil {
		t.Fatal(err)
	}
	if _, ok := memory(t, "back"); !ok {
		t.Fatal("dropped the first time it was missed: a reopened window's tab would read as the owner's")
	}
	// asking for the missing tab itself keeps it too
	if _, err := a.ApplyTint(context.Background(), "back", AttentionTint("red")); !errors.Is(err, ErrGone) {
		t.Fatalf("err = %v, want ErrGone", err)
	}
	f.add(0, "back", "back", "teal", "icon") // the window is reopened
	if res, err := a.ApplyTint(context.Background(), "back", AttentionTint("red")); err != nil || res != TintApplied {
		t.Errorf("reopened tab: %v, %v; want it still qm's", res, err)
	}
}

func TestApplyTintASeenTabStartsItsMonthAgain(t *testing.T) {
	stateDir(t)
	f := &fakeCtl{windows: make([][]*fakeSession, 1)}
	f.add(0, "s1", "worker", "")
	f.add(0, "back", "back", "teal", "icon")
	rememberMissing(t, "back", WorkerTint("teal"), 300*24*time.Hour, 40*24*time.Hour)
	if _, err := f.agterm().ApplyTint(context.Background(), "s1", WorkerTint("teal")); err != nil {
		t.Fatal(err)
	}
	e := entry(t, "back")
	if e.Missing != 0 {
		t.Error("a tab that is open again is still marked missing, so closing it once more would drop it at once")
	}
	if age := time.Since(time.Unix(e.At, 0)); age > time.Hour {
		t.Errorf("an open tab's entry is %v old: a tab kept open for a year would expire", age)
	}
}

func TestApplyTintAfterALostReplyForgetsTheTab(t *testing.T) {
	for name, lost := range map[string]func() error{
		"the run failed without a refusal": func() error { return fmt.Errorf("agtermctl: read: %w", exitError(1)) },
		"unreadable reply":                 nil,
	} {
		t.Run(name, func(t *testing.T) {
			stateDir(t)
			f := &fakeCtl{windows: make([][]*fakeSession, 1)}
			f.add(0, "s1", "worker", "teal", "icon")
			remember(t, "s1", WorkerTint("teal"))
			a := Agterm{Bin: "agtermctl", Run: func(ctx context.Context, bin string, args ...string) ([]byte, error) {
				out, err := f.run(ctx, bin, args...)
				if len(args) > 1 && args[0] == "session" && args[1] == "tint" { // applied, reply lost
					if lost != nil {
						return nil, lost()
					}
					return []byte("{\"ok\":tr"), nil
				}
				return out, err
			}}
			if _, err := a.ApplyTint(context.Background(), "s1", AttentionTint("red")); err == nil {
				t.Fatal("no error")
			}
			if m, ok := memory(t, "s1"); ok {
				t.Errorf("remembers %+v though the tab may have changed: a stale record could claim a hand-picked colour", m)
			}
		})
	}
}

func TestApplyTintTabInAnUnreadWindowIsNotGone(t *testing.T) {
	stateDir(t)
	f := &fakeCtl{windows: make([][]*fakeSession, 2), badTree: map[int]bool{1: true}}
	f.add(0, "front", "front", "")
	f.add(1, "back", "back", "teal", "icon")
	remember(t, "back", WorkerTint("teal"))
	_, err := f.agterm().ApplyTint(context.Background(), "back", AttentionTint("red"))
	if err == nil || errors.Is(err, ErrGone) {
		t.Fatalf("err = %v; a tab that may be in the window not read is not gone", err)
	}
	if _, ok := memory(t, "back"); !ok {
		t.Error("the entry was dropped on a partial view")
	}
}

func TestApplyTintForgetsBeforeItChangesTheTab(t *testing.T) {
	stateDir(t)
	f := &fakeCtl{windows: make([][]*fakeSession, 1)}
	f.add(0, "s1", "worker", "teal", "icon")
	remember(t, "s1", WorkerTint("teal"))
	f.onTint = func(id string) {
		if _, ok := memory(t, id); ok {
			t.Error("the old record was still there when the tab changed: a failed write after it would leave a record that can claim a later hand-picked teal")
		}
	}
	if _, err := f.agterm().ApplyTint(context.Background(), "s1", AttentionTint("red")); err != nil {
		t.Fatal(err)
	}
	if m, ok := memory(t, "s1"); !ok || m != AttentionTint("red") {
		t.Errorf("memory = %+v %v", m, ok)
	}
}

func TestAgtermSpawnCreatesRecordsAndPinsInTheRightOrderUnderOneLock(t *testing.T) {
	dir := stateDir(t)
	f := &fakeCtl{windows: make([][]*fakeSession, 1)}
	locked := func() bool {
		l, err := os.OpenFile(filepath.Join(dir, "quartermaster", "tints.json.lock"), os.O_RDWR, 0)
		if err != nil {
			t.Error(err)
			return false
		}
		defer func() { _ = l.Close() }()
		return syscall.Flock(int(l.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil
	}
	f.onNew = func() {
		if !locked() {
			t.Error("the tint memory is not locked while the tab is created, so another qm could see it tinted and unremembered")
		}
	}
	f.onRestore = func(id string) {
		if _, ok := memory(t, id); ok {
			t.Error("the tint was recorded before the restore pin")
		}
	}
	ref, err := f.agterm().Spawn(context.Background(), LaunchSpec{Name: "w", Cwd: "/x", Script: "/s.sh", Tint: WorkerTint("teal")})
	if err != nil {
		t.Fatal(err)
	}
	if f.count("session", "restore") != 1 {
		t.Errorf("pinned %d times", f.count("session", "restore"))
	}
	if _, ok := memory(t, ref.ID); !ok {
		t.Error("not remembered")
	}
}

func TestAgtermSpawnLeavesATintTheOwnerChangedMeanwhile(t *testing.T) {
	dir := stateDir(t)
	f := &fakeCtl{windows: make([][]*fakeSession, 1)}
	f.onNew = func() {
		f.windows[0][0].color, f.windows[0][0].on = "blue", []string{"name"} // the owner
		p := filepath.Join(dir, "quartermaster", "tints.json")
		_ = os.Remove(p)
		if err := os.MkdirAll(filepath.Join(p, "blocker"), 0o700); err != nil {
			t.Error(err)
		}
	}
	ref, err := f.agterm().Spawn(context.Background(), LaunchSpec{Name: "w", Cwd: "/x", Script: "/s.sh", Tint: WorkerTint("teal")})
	if err != nil {
		t.Fatal(err)
	}
	if s := f.find(ref.ID); s.color != "blue" {
		t.Errorf("tab %+v: the owner's colour was cleared", s)
	}
	if ref.TintNote == "" {
		t.Error("no TintNote")
	}
}

func TestAgtermSpawnRetriesOnlyAFailedRunOfTheParser(t *testing.T) {
	stateDir(t)
	exit := func(code int) error {
		return fmt.Errorf("agtermctl: Error: Unknown option '--tint'\nUsage: ...: %w", exitError(code))
	}
	for _, tc := range []struct {
		name  string
		run   func(args []string) ([]byte, error)
		calls int
	}{
		{"parser refusal, exit 64", func([]string) ([]byte, error) { return nil, exit(64) }, 2},
		{"same words, other exit status", func([]string) ([]byte, error) { return nil, exit(1) }, 1},
		{"a lost reply", func([]string) ([]byte, error) { return nil, fmt.Errorf("agtermctl: read: %w", exitError(1)) }, 1},
		{"same words on the standard output of a call that succeeded", func([]string) ([]byte, error) {
			return []byte("Error: Unknown option '--tint'\n{\"ok\":true,\"result\":{\"id\":\"x\"}}"), nil
		}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stateDir(t)
			n := 0
			a := Agterm{Bin: "agtermctl", Run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
				if len(args) > 1 && args[1] == "new" {
					n++
					if n == 2 {
						return []byte(`{"ok":true,"result":{"id":"new-1"}}`), nil
					}
					return tc.run(args)
				}
				return []byte(`{"ok":true,"result":{}}`), nil
			}}
			_, err := a.Spawn(context.Background(), LaunchSpec{Name: "w", Cwd: "/x", Script: "/s.sh", Tint: WorkerTint("teal")})
			// a failure that is not a clear refusal says the tab may exist
			if tc.calls == 1 && !errors.Is(err, ErrMaybeStarted) {
				t.Errorf("err = %v, want ErrMaybeStarted", err)
			}
			if tc.calls == 2 && err != nil {
				t.Errorf("err = %v", err)
			}
			if n != tc.calls {
				t.Errorf("%d session new calls, want %d", n, tc.calls)
			}
		})
	}
}
