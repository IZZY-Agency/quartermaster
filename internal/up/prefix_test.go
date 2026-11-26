package up

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IZZY-Agency/quartermaster/internal/config"
)

func strPtr(s string) *string { return &s }

// namedDir makes <tmp>/<base> and returns it, symlinks resolved.
func namedDir(t *testing.T, base string) string {
	t.Helper()
	dir := filepath.Join(realTempDir(t), base)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestPrefixName(t *testing.T) {
	long := strings.Repeat("n", 60)
	tests := []struct {
		name      string
		base      string // the project folder's base name
		state     bool   // a qm up project with workspace "ws" is recorded
		cfg       func(root string) config.Config
		workspace string                   // what the caller knows of the tied project
		tiedRoot  func(root string) string // the tied project's root; nil: untied
		in        string
		want      string
		wantNote  string // a substring
		wantErr   string // a substring
	}{
		{name: "map entry wins", base: "app", state: true, in: "db", want: "ut-db",
			cfg: func(root string) config.Config {
				c := config.Defaults()
				c.ProjectNamePrefix[root] = "ut"
				return c
			}},
		{name: "map entry wins over off", base: "app", in: "db", want: "ut-db",
			cfg: func(root string) config.Config {
				c := config.Defaults()
				c.Worker.NamePrefix = strPtr("")
				c.ProjectNamePrefix[root] = "ut"
				return c
			}},
		{name: "empty map entry is off for that project", base: "app", state: true, in: "db", want: "db",
			cfg: func(root string) config.Config {
				c := config.Defaults()
				c.ProjectNamePrefix[root] = ""
				return c
			}},
		{name: "the tied project's entry, not the worker's repository's", base: "app", in: "db", want: "ut-db",
			tiedRoot: func(string) string { return "/projects/tied" },
			cfg: func(root string) config.Config {
				c := config.Defaults()
				c.ProjectNamePrefix["/projects/tied"] = "ut"
				c.ProjectNamePrefix[root] = "other"
				return c
			}},
		{name: "a tied project without an entry is not given the repository's", base: "app", workspace: "tied", in: "db", want: "tied-db",
			tiedRoot: func(string) string { return "/projects/tied" },
			cfg: func(root string) config.Config {
				c := config.Defaults()
				c.ProjectNamePrefix[root] = "other"
				return c
			}},
		{name: "a bad worker.name_prefix is refused whatever the entry", base: "app", in: "db", wantErr: "worker.name_prefix",
			cfg: func(root string) config.Config {
				c := config.Defaults()
				c.Worker.NamePrefix = strPtr("short")
				c.ProjectNamePrefix[root] = "ut"
				return c
			}},
		{name: "auto from state", base: "app", state: true, in: "db", want: "ws-db"},
		{name: "auto from the tied project", base: "app", state: true, workspace: "tied", in: "db", want: "tied-db"},
		{name: "auto fallback to the folder's base name", base: "app", in: "db", want: "app-db"},
		{name: "off", base: "app", state: true, in: "db", want: "db",
			cfg: func(string) config.Config {
				c := config.Defaults()
				c.Worker.NamePrefix = strPtr("")
				return c
			}},
		{name: "auto spelled out", base: "app", in: "db", want: "app-db",
			cfg: func(string) config.Config {
				c := config.Defaults()
				c.Worker.NamePrefix = strPtr("auto")
				return c
			}},
		{name: "invalid configured prefix is an error naming the key", base: "app", in: "db",
			wantErr: "project_name_prefix",
			cfg: func(root string) config.Config {
				c := config.Defaults()
				c.ProjectNamePrefix[root] = "bad prefix!"
				return c
			}},
		{name: "configured prefix that makes the name too long", base: "app", in: long,
			wantErr: "project_name_prefix",
			cfg: func(root string) config.Config {
				c := config.Defaults()
				c.ProjectNamePrefix[root] = strings.Repeat("p", 10)
				return c
			}},
		{name: "unknown worker.name_prefix", base: "app", in: "db", wantErr: "worker.name_prefix",
			cfg: func(string) config.Config {
				c := config.Defaults()
				c.Worker.NamePrefix = strPtr("short")
				return c
			}},
		{name: "invalid derived prefix is skipped with a note", base: "_app", in: "db", want: "db", wantNote: "not prefixed"},
		{name: "derived prefix that makes the name too long is skipped", base: "application", in: long, want: long, wantNote: "too long"},
		{name: "name that is the prefix is kept", base: "app", state: true, in: "ws", want: "ws"},
		{name: "name that starts with the prefix is kept", base: "app", state: true, in: "ws-db", want: "ws-db"},
		{name: "name that only starts with the same letters is prefixed", base: "app", state: true, in: "wsx", want: "ws-wsx"},
		{name: "invalid name is left for spawn to refuse", base: "app", in: "bad name", want: "bad name"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			projectState(t)
			root := namedDir(t, tc.base)
			if tc.state {
				saveAt(t, root)
			}
			cfg := config.Defaults()
			if tc.cfg != nil {
				cfg = tc.cfg(root)
			}
			tied := ""
			if tc.tiedRoot != nil {
				tied = tc.tiedRoot(root)
			}
			got, note, err := PrefixName(cfg, root, tc.workspace, tied, tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one naming %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("name = %q, want %q", got, tc.want)
			}
			if tc.wantNote == "" && note != "" || !strings.Contains(note, tc.wantNote) {
				t.Errorf("note = %q, want one with %q", note, tc.wantNote)
			}
		})
	}
}

func TestPrefixNameLinkedWorktreeBelongsToMain(t *testing.T) {
	projectState(t)
	main, linked := projectRepo(t)

	// No state: the main repository's folder names the project, not the worktree's.
	got, _, err := PrefixName(config.Defaults(), linked, "", "", "db")
	if err != nil || got != "main-db" {
		t.Errorf("no state: name = %q, %v, want main-db", got, err)
	}

	// The main project's state answers.
	saveAt(t, main)
	if got, _, err = PrefixName(config.Defaults(), linked, "", "", "db"); err != nil || got != "ws-db" {
		t.Errorf("state: name = %q, %v, want ws-db", got, err)
	}

	// A map entry keyed by the main repository applies in the worktree.
	cfg := config.Defaults()
	cfg.ProjectNamePrefix[main] = "ut"
	if got, _, err = PrefixName(cfg, linked, "", "", "db"); err != nil || got != "ut-db" {
		t.Errorf("map: name = %q, %v, want ut-db", got, err)
	}
}

func TestResolveName(t *testing.T) {
	gone := func() bool { return true }
	w := func(name, base string) NamedWorker { return NamedWorker{Name: name, Base: base} }
	prefixes := []string{"ut"}
	tests := []struct {
		name     string
		existing []NamedWorker
		in       string
		prefixes []string
		want     string
		matched  bool
		wantErr  []string // substrings
	}{
		{name: "full name", existing: []NamedWorker{w("ut-db", "db")}, in: "ut-db", prefixes: prefixes, want: "ut-db", matched: true},
		{name: "bare name by the stored name", existing: []NamedWorker{w("ut-db", "db")}, in: "db", want: "ut-db", matched: true},
		{name: "bare name, no stored name: the prefix guess", existing: []NamedWorker{w("ut-db", "")}, in: "db", prefixes: prefixes, want: "ut-db", matched: true},
		{name: "no stored name, no prefixes: not found", existing: []NamedWorker{w("ut-db", "")}, in: "db", want: "db"},
		{name: "prefix changed after the spawn", existing: []NamedWorker{w("old-db", "db")}, in: "db", prefixes: []string{"new"}, want: "old-db", matched: true},
		{name: "the guess is not used for a worker with a stored name", existing: []NamedWorker{w("new-db", "other")}, in: "db", prefixes: []string{"new"}, want: "db"},
		{name: "legacy name without prefix", existing: []NamedWorker{w("db", "")}, in: "db", prefixes: prefixes, want: "db", matched: true},
		{name: "unprefixed worker spawned with the prefix off", existing: []NamedWorker{w("db", "db")}, in: "db", prefixes: prefixes, want: "db", matched: true},
		{name: "unknown name is returned as typed", existing: []NamedWorker{w("ut-db", "db")}, in: "nope", want: "nope"},
		{name: "ambiguous bare name", existing: []NamedWorker{w("db", ""), w("ut-db", "db")}, in: "db", prefixes: prefixes, wantErr: []string{"ambiguous", "db and ut-db"}},
		{name: "full name of an ambiguous pair", existing: []NamedWorker{w("db", ""), w("ut-db", "db")}, in: "ut-db", prefixes: prefixes, want: "ut-db", matched: true},
		{name: "an old worker that is finished is set aside", existing: []NamedWorker{{Name: "db", Finished: gone}, w("ut-db", "db")}, in: "db", prefixes: prefixes, want: "ut-db", matched: true},
		{name: "the new one finished: the old one stays", existing: []NamedWorker{w("db", ""), {Name: "ut-db", Base: "db", Finished: gone}}, in: "db", prefixes: prefixes, want: "db", matched: true},
		{name: "two unfinished with a finished third", existing: []NamedWorker{w("db", ""), w("ut-db", "db"), {Name: "old-db", Base: "db", Finished: gone}}, in: "db", prefixes: prefixes, wantErr: []string{"db and ut-db"}},
		{name: "all finished: still ambiguous", existing: []NamedWorker{{Name: "db", Finished: gone}, {Name: "ut-db", Base: "db", Finished: gone}}, in: "db", prefixes: prefixes, wantErr: []string{"ambiguous"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, matched, err := ResolveName(tc.existing, tc.in, tc.prefixes)
			if len(tc.wantErr) > 0 {
				for _, w := range tc.wantErr {
					if err == nil || !strings.Contains(err.Error(), w) {
						t.Fatalf("err = %v, want it to contain %q", err, w)
					}
				}
				return
			}
			if err != nil || got != tc.want || matched != tc.matched {
				t.Errorf("ResolveName = %q, %v, %v, want %q, %v", got, matched, err, tc.want, tc.matched)
			}
		})
	}
}

func TestPrefixesOfProject(t *testing.T) {
	p := Project{Root: "/r/app", Workspace: "ws"}
	cfg := config.Defaults()
	if got := Prefixes(cfg, p); len(got) != 1 || got[0] != "ws" {
		t.Errorf("Prefixes = %v, want [ws]", got)
	}
	cfg.ProjectNamePrefix["/r/app"] = "ut"
	if got := Prefixes(cfg, p); len(got) != 2 || got[0] != "ut" || got[1] != "ws" {
		t.Errorf("Prefixes = %v, want [ut ws]", got)
	}
}
