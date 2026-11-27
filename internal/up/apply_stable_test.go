package up

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
	"github.com/IZZY-Agency/quartermaster/internal/route"
)

// brewPrefix is a hand-made Homebrew prefix: a keg under Cellar holding qm and
// the plugin, <prefix>/bin/qm a link into it, and <prefix>/opt/quartermaster
// a link to the keg. It returns the prefix, the keg and the invoked path.
func brewPrefix(t *testing.T) (prefix, keg, called string) {
	t.Helper()
	prefix, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	keg = filepath.Join(prefix, "Cellar", "quartermaster", "1.0")
	for _, d := range []string{filepath.Join(keg, "share", "quartermaster", "plugins", "qm-worker"), filepath.Join(keg, "bin"), filepath.Join(prefix, "bin"), filepath.Join(prefix, "opt")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(keg, "bin", "qm"), nil, 0o700); err != nil {
		t.Fatal(err)
	}
	called = filepath.Join(prefix, "bin", "qm")
	if err := os.Symlink(filepath.Join(keg, "bin", "qm"), called); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(keg, filepath.Join(prefix, "opt", "quartermaster")); err != nil {
		t.Fatal(err)
	}
	return prefix, keg, called
}

// A restore, through Apply with QM_WORKER_PLUGIN cleared, writes the Homebrew
// opt link's paths into the script of a resumed worker and of a fresh one,
// and they still resolve after an upgrade removes the old keg.
func TestUpScriptsUseStableBrewPaths(t *testing.T) {
	for _, tc := range []struct {
		name, worker string
		fresh        bool
	}{{"resume", "w-warm", false}, {"fresh", "w-large", true}} {
		t.Run(tc.name, func(t *testing.T) {
			var logged []route.Record
			f := &fakeAgterm{ws: "repo"}
			d := upDeps(t, f, &logged)
			t.Setenv("QM_WORKER_PLUGIN", "")
			prefix, keg, called := brewPrefix(t)
			d.Exe, d.Running = called, filepath.Join(keg, "bin", "qm")
			w := newWorld(t)
			var recs []route.Record
			if tc.fresh {
				start := now.Add(-4 * time.Hour)
				old := profiled(w, largeID, "w-large", "large", start)
				old.Session, old.Project, old.Type = "00000000-0000-4000-8000-0000000000c0", w.root, "feature"
				recs = []route.Record{old}
				w.note(t, "w-large", start.Add(time.Hour))
			} else {
				recs = []route.Record{profiled(w, warmID, "w-warm", "warm", now.Add(-time.Hour))}
			}
			steps := w.plan(t, recs, []claudecode.Proc{{PID: 301, Cmd: "claude -n repo-coord"}})
			o := outcomeOf(t, Apply(context.Background(), &w.project, steps, d), tc.worker)
			if o.Err != nil {
				t.Fatalf("outcome = %+v", o)
			}
			wantBin := filepath.Join(prefix, "opt", "quartermaster", "bin")
			wantPlugin := filepath.Join(prefix, "opt", "quartermaster", "share", "quartermaster", "plugins", "qm-worker")
			script := readFile(t, o.Script)
			// The PATH line is quoted only when the inherited PATH needs it.
			if !strings.Contains(script, "PATH='"+wantBin+":") && !strings.Contains(script, "PATH="+wantBin+":") {
				t.Errorf("script's PATH does not start with %s:\n%s", wantBin, script)
			}
			if want := "--plugin-dir " + wantPlugin + " "; !strings.Contains(script, want) {
				t.Errorf("script lacks %q:\n%s", want, script)
			}
			if strings.Contains(script, filepath.Join(prefix, "Cellar")) {
				t.Errorf("script names the keg:\n%s", script)
			}
			// brew upgrade: a new keg, opt repointed, the old keg removed.
			next := filepath.Join(prefix, "Cellar", "quartermaster", "2.0")
			if err := os.MkdirAll(filepath.Join(next, "bin"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(next, "share", "quartermaster", "plugins", "qm-worker"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(next, "bin", "qm"), nil, 0o700); err != nil {
				t.Fatal(err)
			}
			opt := filepath.Join(prefix, "opt", "quartermaster")
			if err := os.Remove(opt); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(next, opt); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(keg); err != nil {
				t.Fatal(err)
			}
			for _, p := range []string{wantBin, wantPlugin} {
				if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
					t.Errorf("%s no longer resolves after the upgrade: %v", p, err)
				}
			}
		})
	}
}
