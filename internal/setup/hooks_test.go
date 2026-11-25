package setup

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IZZY-Agency/quartermaster/internal/launch"
)

// fakeAgterm answers init's agtermctl calls for a running agterm whose
// hooks.conf is conf.
type fakeAgterm struct {
	conf    string
	down    bool // agterm is not running
	old     bool // it does not know app.launched
	refuses string
	noLoad  bool // hooks reload fails
	reloads int
}

func (f *fakeAgterm) run(_ context.Context, bin string, args ...string) ([]byte, error) {
	if bin != "agtermctl" {
		return nil, errors.New("not here")
	}
	if f.down {
		return nil, errors.New("Error: connect(/a.sock) failed")
	}
	switch strings.Join(args[:2], " ") {
	case "hooks list":
		diags := []map[string]any{}
		if b, err := os.ReadFile(f.conf); f.refuses != "" && err == nil {
			for i, l := range strings.Split(string(b), "\n") {
				if strings.Contains(l, "app.launched") {
					diags = append(diags, map[string]any{"line": i + 1, "message": f.refuses})
				}
			}
		}
		return json.Marshal(map[string]any{"ok": true, "result": map[string]any{"hooks": map[string]any{"path": f.conf, "diagnostics": diags, "hooks": []any{}}}})
	case "hooks reload":
		f.reloads++
		if f.noLoad {
			return nil, errors.New("boom")
		}
		return []byte(`{"ok":true,"result":{"count":0}}`), nil
	case "events --json":
		if f.old {
			return nil, errors.New("Error: invalid event kind: app.launched")
		}
		return nil, errors.New("Error: event run changed")
	}
	return nil, errors.New("unexpected " + strings.Join(args, " "))
}

// agtermEnv is env with a running agterm, which is the launcher, and its
// hooks.conf at the path returned (its folder exists, the file does not).
func agtermEnv(t *testing.T) (settings string, f *fakeAgterm) {
	t.Helper()
	settings = env(t)
	f = &fakeAgterm{conf: filepath.Join(t.TempDir(), "hooks.conf")}
	old := agtermCtl
	agtermCtl = launch.Agterm{Bin: "agtermctl", Run: f.run}
	t.Cleanup(func() { agtermCtl = old })
	t.Setenv("AGTERM_SESSION_ID", "s-1")
	t.Setenv("TMUX", "")
	return settings, f
}

// ourLine is the line init appends.
func ourLine(t *testing.T) string {
	t.Helper()
	exe, err := executable()
	if err != nil {
		t.Fatal(err)
	}
	return "on app.launched " + launch.Quote(exe) + " up note --notify " + hookMark + "\n"
}

// answers: Enter for the TypeSafe key, no status line, then the hook.
const (
	hookYes = "\nn\ny\n"
	hookNo  = "\nn\nn\n"
)

func TestInitAddsAgtermHook(t *testing.T) {
	for _, before := range []string{"", "# mine\non notify say hi\n", "on notify say hi", "a\r\n\xff\xfe\n"} {
		settings, f := agtermEnv(t)
		if before != "" {
			writeFile(t, f.conf, before)
			if err := os.Chmod(f.conf, 0o640); err != nil {
				t.Fatal(err)
			}
		}
		out := runInit(t, settings, hookYes, false)
		want := before + ourLine(t)
		if before != "" && !strings.HasSuffix(before, "\n") {
			want = before + "\n" + ourLine(t)
		}
		if got := readFile(t, f.conf); got != want {
			t.Errorf("hooks.conf %q, want %q\n%s", got, want, out)
		}
		if f.reloads != 1 || !strings.Contains(out, f.conf) || !strings.Contains(out, "qm up note") {
			t.Errorf("reloads %d, output %s", f.reloads, out)
		}
		// Again: the line is there, nothing is asked and nothing written.
		out = runInit(t, settings, "\nn\n", false)
		if got := readFile(t, f.conf); got != want || strings.Contains(out, "Add this line?") {
			t.Errorf("again: hooks.conf %q\n%s", got, out)
		}
		// The undo puts the file back as it was, byte for byte.
		out = runUndo(t, settings, "")
		b, err := os.ReadFile(f.conf)
		switch {
		case before == "" && !errors.Is(err, os.ErrNotExist):
			t.Errorf("undo left the file init created: %q, %v\n%s", b, err, out)
		case before != "" && string(b) != before:
			t.Errorf("after undo %q, want %q\n%s", b, before, out)
		}
		if before != "" {
			if fi, err := os.Stat(f.conf); err != nil || fi.Mode().Perm() != 0o640 {
				t.Errorf("mode after undo: %v, %v", fi.Mode(), err)
			}
		}
		if f.reloads != 2 {
			t.Errorf("reloads %d after undo, want 2", f.reloads)
		}
	}
}

func TestUndoRemovesOnlyItsHookLine(t *testing.T) {
	settings, f := agtermEnv(t)
	writeFile(t, f.conf, "on notify say hi")
	runInit(t, settings, hookYes, false)
	// Since then: a line before and one after, and one that looks like ours.
	mine := "on app.launched qm up note --notify\n"
	writeFile(t, f.conf, mine+readFile(t, f.conf)+"on session.created true\n")
	runUndo(t, settings, "")
	if got, want := readFile(t, f.conf), mine+"on notify say hi\non session.created true\n"; got != want {
		t.Errorf("after undo %q, want %q", got, want)
	}
}

// The line above init's is not the one it found: its line break stays.
func TestUndoKeepsALineBreakThatIsNotItsOwn(t *testing.T) {
	settings, f := agtermEnv(t)
	writeFile(t, f.conf, "a")
	runInit(t, settings, hookYes, false)
	writeFile(t, f.conf, "a\nb\n"+ourLine(t))
	runUndo(t, settings, "")
	if got := readFile(t, f.conf); got != "a\nb\n" {
		t.Errorf("after undo %q", got)
	}
}

// A file that was there and empty stays, empty.
func TestUndoKeepsAnEmptyHooksFile(t *testing.T) {
	settings, f := agtermEnv(t)
	writeFile(t, f.conf, "")
	runInit(t, settings, hookYes, false)
	runUndo(t, settings, "")
	if got := readFile(t, f.conf); got != "" {
		t.Errorf("after undo %q", got)
	}
}

// editReader is the user's answers; before the last one is read, it edits
// hooks.conf.
type editReader struct {
	lines []string
	edit  func()
}

func (r *editReader) Read(b []byte) (int, error) {
	if len(r.lines) == 0 {
		return 0, io.EOF
	}
	if len(r.lines) == 1 {
		r.edit()
	}
	n := copy(b, r.lines[0])
	r.lines = r.lines[1:]
	return n, nil
}

func TestInitHookFileEditedWhileAsked(t *testing.T) {
	byHand := "on app.launched qm up note --notify\n"
	for _, c := range []struct{ name, edited, want string }{
		{"no final line break", "a\nb", "a\nb\n+"},
		{"added by hand", byHand, byHand},
	} {
		settings, f := agtermEnv(t)
		writeFile(t, f.conf, "a\n")
		in := &editReader{lines: []string{"\n", "n\n", "y\n"}, edit: func() { writeFile(t, f.conf, c.edited) }}
		var out bytes.Buffer
		if err := Init(context.Background(), Streams{In: in, Out: &out}, Options{SettingsPath: settings}); err != nil {
			t.Fatal(err)
		}
		if got, want := readFile(t, f.conf), strings.Replace(c.want, "+", ourLine(t), 1); got != want {
			t.Errorf("%s: hooks.conf %q, want %q\n%s", c.name, got, want, out.String())
		}
		runUndo(t, settings, "")
		if got := readFile(t, f.conf); got != c.edited {
			t.Errorf("%s: after undo %q, want %q", c.name, got, c.edited)
		}
	}
}

// qm was installed another way since: its own line follows, with the answer.
func TestInitRepointsItsHookLine(t *testing.T) {
	settings, f := agtermEnv(t)
	writeFile(t, f.conf, "a")
	runInit(t, settings, hookYes, false)
	old := ourLine(t)
	writeFile(t, f.conf, readFile(t, f.conf)+"b\n")
	fakeQM(t, t.TempDir())
	out := runInit(t, settings, "\nn\nn\n", false)
	if got := readFile(t, f.conf); got != "a\n"+old+"b\n" || !strings.Contains(out, "another qm") {
		t.Errorf("after no: %q\n%s", got, out)
	}
	out = runInit(t, settings, hookYes, false)
	if got := readFile(t, f.conf); got != "a\n"+ourLine(t)+"b\n" {
		t.Errorf("after yes: %q\n%s", got, out)
	}
	runUndo(t, settings, "")
	if got := readFile(t, f.conf); got != "a\nb\n" {
		t.Errorf("after undo: %q", got)
	}
}

// What the undo could not remove stays recorded, for the next undo.
func TestUndoHookNotWritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes every file")
	}
	settings, f := agtermEnv(t)
	writeFile(t, f.conf, "a\n")
	runInit(t, settings, hookYes, false)
	if err := os.Chmod(f.conf, 0o400); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		out := runUndo(t, settings, "")
		if got := readFile(t, f.conf); got != "a\n"+ourLine(t) || !strings.Contains(out, "qm init --undo again") {
			t.Errorf("after undo %q\n%s", got, out)
		}
	}
	if err := os.Chmod(f.conf, 0o600); err != nil {
		t.Fatal(err)
	}
	runUndo(t, settings, "")
	if got := readFile(t, f.conf); got != "a\n" {
		t.Errorf("after the last undo %q", got)
	}
	if out := runUndo(t, settings, ""); !strings.Contains(out, "Nothing to undo") {
		t.Errorf("once more: %s", out)
	}
}

// The record cannot be written: no line is added, and no file is left.
func TestInitHookRecordFails(t *testing.T) {
	_, f := agtermEnv(t)
	path, err := recordPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path, 0o700); err != nil { // a folder in its place
		t.Fatal(err)
	}
	p := prompter{in: bufio.NewReader(strings.NewReader("y\n")), out: io.Discard}
	if err := offerHook(context.Background(), p, "agterm", &Record{}); err == nil {
		t.Error("no error")
	}
	if _, err := os.Stat(f.conf); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("hooks.conf: %v", err)
	}
}

func TestUndoHookLineEdited(t *testing.T) {
	settings, f := agtermEnv(t)
	runInit(t, settings, hookYes, false)
	edited := "on app.launched true # edited\n"
	writeFile(t, f.conf, edited)
	out := runUndo(t, settings, "")
	if got := readFile(t, f.conf); got != edited || !strings.Contains(out, "left alone") || strings.Contains(out, "--undo again") {
		t.Errorf("after undo %q\n%s", got, out)
	}
}

func TestInitKeepsHookRecordOfFirstRun(t *testing.T) {
	settings, f := agtermEnv(t)
	runInit(t, settings, hookYes, false)
	runInit(t, settings, "\nn\n", false)
	runUndo(t, settings, "")
	if _, err := os.Stat(f.conf); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the created hooks.conf is still there: %v", err)
	}
}

func TestInitHookNo(t *testing.T) {
	settings, f := agtermEnv(t)
	out := runInit(t, settings, hookNo, false)
	if _, err := os.Stat(f.conf); !errors.Is(err, os.ErrNotExist) || f.reloads != 0 || !strings.Contains(out, "Add this line?") {
		t.Errorf("%v, reloads %d\n%s", err, f.reloads, out)
	}
	if rec := recordT(t); rec.HookLine != "" {
		t.Errorf("recorded %+v", rec)
	}
}

func TestInitOffersNoHook(t *testing.T) {
	for _, c := range []struct {
		name string
		set  func(*testing.T, *fakeAgterm)
		says string
	}{
		{"agterm is not the launcher: tmux", func(t *testing.T, _ *fakeAgterm) {
			t.Setenv("AGTERM_SESSION_ID", "")
			t.Setenv("TMUX", "/tmp/tmux,1,0")
		}, ""},
		{"agterm is not the launcher: the config says tmux", func(t *testing.T, _ *fakeAgterm) {
			path := configPath(t)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			writeFile(t, path, `{"launcher":"tmux"}`)
		}, ""},
		{"no agterm", func(t *testing.T, f *fakeAgterm) {
			f.down = true
			t.Setenv("AGTERM_SESSION_ID", "")
		}, ""},
		{"agterm is the launcher and does not answer", func(_ *testing.T, f *fakeAgterm) { f.down = true }, "needs a running agterm"},
		{"an agterm that does not know app.launched", func(_ *testing.T, f *fakeAgterm) { f.old = true }, "does not know app.launched"},
		{"a symlink", func(t *testing.T, f *fakeAgterm) {
			target := filepath.Join(t.TempDir(), "dotfiles.conf")
			writeFile(t, target, "# mine\n")
			if err := os.Symlink(target, f.conf); err != nil {
				t.Fatal(err)
			}
		}, "is a symbolic link"},
		{"a folder", func(t *testing.T, f *fakeAgterm) {
			if err := os.Mkdir(f.conf, 0o700); err != nil {
				t.Fatal(err)
			}
		}, "left alone"},
		{"not writable", func(t *testing.T, f *fakeAgterm) {
			if os.Geteuid() == 0 {
				t.Skip("root writes every file")
			}
			writeFile(t, f.conf, "# mine\n")
			if err := os.Chmod(f.conf, 0o400); err != nil {
				t.Fatal(err)
			}
		}, "left alone"},
		{"no folder", func(_ *testing.T, f *fakeAgterm) { f.conf = filepath.Join(f.conf, "none", "hooks.conf") }, "left alone"},
		{"added by hand", func(t *testing.T, f *fakeAgterm) {
			writeFile(t, f.conf, "on app.launched qm up note --notify\n")
		}, "already"},
	} {
		t.Run(c.name, func(t *testing.T) {
			settings, f := agtermEnv(t)
			c.set(t, f)
			before, _ := os.ReadFile(f.conf)
			out := runInit(t, settings, "\nn\ny\n", false)
			after, _ := os.ReadFile(f.conf)
			if string(before) != string(after) || strings.Contains(out, "Add this line?") || f.reloads != 0 {
				t.Errorf("hooks.conf %q, was %q; reloads %d\n%s", after, before, f.reloads, out)
			}
			if !strings.Contains(out, c.says) || c.says == "" && strings.Contains(out, "agterm hook") {
				t.Errorf("output: want %q\n%s", c.says, out)
			}
			if rec := recordT(t); rec.HookLine != "" {
				t.Errorf("recorded %+v", rec)
			}
		})
	}
}

// A comment that shows the line is not the line.
func TestInitHookCommentedOut(t *testing.T) {
	settings, f := agtermEnv(t)
	writeFile(t, f.conf, "# on app.launched qm up note --notify\n")
	runInit(t, settings, hookYes, false)
	if got := readFile(t, f.conf); !strings.HasSuffix(got, ourLine(t)) {
		t.Errorf("hooks.conf %q", got)
	}
}

// auto: agterm runs, and qm init was started from another terminal.
func TestInitHookAutoAgtermRunning(t *testing.T) {
	settings, f := agtermEnv(t)
	t.Setenv("AGTERM_SESSION_ID", "")
	runInit(t, settings, hookYes, false)
	if got := readFile(t, f.conf); got != ourLine(t) {
		t.Errorf("hooks.conf %q", got)
	}
}

func TestInitHookYes(t *testing.T) {
	settings, f := agtermEnv(t)
	runInit(t, settings, "", true)
	if got := readFile(t, f.conf); got != ourLine(t) {
		t.Errorf("hooks.conf %q", got)
	}
}

// agterm does not take the line after all: it is taken back.
func TestInitHookRefused(t *testing.T) {
	settings, f := agtermEnv(t)
	f.refuses = "unknown event kind"
	writeFile(t, f.conf, "# mine\n")
	out := runInit(t, settings, hookYes, false)
	if got := readFile(t, f.conf); got != "# mine\n" || !strings.Contains(out, "unknown event kind") || f.reloads != 2 {
		t.Errorf("hooks.conf %q, reloads %d\n%s", got, f.reloads, out)
	}
	if rec := recordT(t); rec.HookLine != "" {
		t.Errorf("recorded %+v", rec)
	}
}

func TestInitHookReloadFails(t *testing.T) {
	settings, f := agtermEnv(t)
	f.noLoad = true
	out := runInit(t, settings, hookYes, false)
	if got := readFile(t, f.conf); got != ourLine(t) || !strings.Contains(out, "next launch") {
		t.Errorf("hooks.conf %q\n%s", got, out)
	}
}

func TestUndoHookFileGoneOrLinked(t *testing.T) {
	settings, f := agtermEnv(t)
	runInit(t, settings, hookYes, false)
	if err := os.Remove(f.conf); err != nil {
		t.Fatal(err)
	}
	runUndo(t, settings, "")

	settings, f = agtermEnv(t)
	runInit(t, settings, hookYes, false)
	target := f.conf + ".target"
	if err := os.Rename(f.conf, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, f.conf); err != nil {
		t.Fatal(err)
	}
	out := runUndo(t, settings, "")
	if got := readFile(t, target); got != ourLine(t) || !strings.Contains(out, "left alone") {
		t.Errorf("linked file %q\n%s", got, out)
	}
}

// The user removed the line by hand, from a file init cannot write: there is
// nothing left to undo.
func TestUndoHookRemovedByHandFromALinkOrReadOnlyFile(t *testing.T) {
	for _, link := range []bool{true, false} {
		settings, f := agtermEnv(t)
		runInit(t, settings, hookYes, false)
		target := f.conf
		if link {
			target = f.conf + ".target"
			if err := os.Rename(f.conf, target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, f.conf); err != nil {
				t.Fatal(err)
			}
		}
		writeFile(t, target, "# mine\n")
		if err := os.Chmod(target, 0o400); err != nil {
			t.Fatal(err)
		}
		out := runUndo(t, settings, "")
		if got := readFile(t, target); got != "# mine\n" || strings.Contains(out, "--undo again") {
			t.Errorf("link %v: %q\n%s", link, got, out)
		}
		if out = runUndo(t, settings, ""); !strings.Contains(out, "Nothing to undo") {
			t.Errorf("link %v: once more: %s", link, out)
		}
	}
}

// A qm whose path holds a space is quoted for the hook's shell.
func TestHookLineQuotes(t *testing.T) {
	if got := hookLine("/opt/my tools/qm"); got != "on app.launched '/opt/my tools/qm' up note --notify "+hookMark {
		t.Errorf("line %q", got)
	}
	if got := hookLine("/opt/a\nb/qm"); got != "" {
		t.Errorf("a path with a line break: %q", got)
	}
}
