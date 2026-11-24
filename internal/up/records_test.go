package up

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/IZZY-Agency/quartermaster/internal/route"
	"github.com/IZZY-Agency/quartermaster/internal/spawn"
)

func TestUsableRecordsClearsUnsafeFields(t *testing.T) {
	good := route.Record{Kind: "worker", Name: "w1", ClaudeSession: "s-good", Branch: "feature/résumé", Cwd: "/work/shop", Project: "/work/shop"}
	cases := map[string]struct {
		rec     route.Record
		started bool
	}{
		"name with a folder":  {route.Record{Kind: "worker", Name: "../../evil", ClaudeSession: "s-1", Cwd: "/w"}, false},
		"name with backslash": {route.Record{Kind: "worker", Name: `a\b`, ClaudeSession: "s-2", Cwd: "/w"}, false},
		"no name":             {route.Record{Kind: "worker", ClaudeSession: "s-3", Cwd: "/w"}, false},
		"relative cwd":        {route.Record{Kind: "worker", Name: "w6", ClaudeSession: "s-8", Cwd: "shop"}, false},
		"branch like a flag":  {route.Record{Kind: "worker", Name: "w2", ClaudeSession: "s-4", Cwd: "/w", Branch: "--output=/tmp/x"}, true},
		"branch with ..":      {route.Record{Kind: "worker", Name: "w3", ClaudeSession: "s-5", Cwd: "/w", Branch: "a..b"}, true},
		"relative project":    {route.Record{Kind: "worker", Name: "w5", ClaudeSession: "s-7", Cwd: "/w", Project: "work/shop"}, true},
		"project like a flag": {route.Record{Kind: "worker", Name: "w7", ClaudeSession: "s-9", Cwd: "/w", Project: "-C/tmp"}, true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			review := route.Record{Kind: "review", Name: "../not-a-worker"}
			c.rec.Replaces = "s-old"
			got, notes := UsableRecords([]route.Record{good, c.rec, review})
			if len(got) != 3 || got[0].Name != "w1" || got[0].Branch != good.Branch || got[2].Name != review.Name {
				t.Fatalf("records %+v, want the good worker and the review unchanged", got)
			}
			r := got[1]
			if r.ClaudeSession != c.rec.ClaudeSession || r.Replaces != "s-old" || r.Kind != "worker" {
				t.Errorf("record %+v lost its session, kind or replacement", r)
			}
			if r.Name != "" && !spawn.ValidName(r.Name) || r.Cwd != "" && !filepath.IsAbs(r.Cwd) ||
				r.Branch != "" && !validBranch(r.Branch) || r.Project != "" && !filepath.IsAbs(r.Project) {
				t.Errorf("record %+v keeps an unusable field", r)
			}
			if started := r.Name != ""; started != c.started {
				t.Errorf("name %q: started = %v, want %v", r.Name, started, c.started)
			}
			if len(notes) != 1 || !strings.Contains(notes[0], c.rec.ClaudeSession) {
				t.Fatalf("notes %q, want one naming session %s", notes, c.rec.ClaudeSession)
			}
		})
	}
}

func TestValidBranch(t *testing.T) {
	for _, b := range []string{"main", "feature/x", "fix-1.2", "sec_spawn", "a/b/c", "feature/résumé"} {
		if !validBranch(b) {
			t.Errorf("validBranch(%q) = false", b)
		}
	}
	for _, b := range []string{"", "-x", "--head", "a..b", "a b", "a~1", "a^", "a:b", "a?", "a*", "a[", `a\b`, "a\nb", "/a", "a/", "a//b", "a.lock", ".a", "a/.b", "@", "a@{1}", "HEAD\x7f"} {
		if validBranch(b) {
			t.Errorf("validBranch(%q) = true", b)
		}
	}
}

func TestFallbackWorkerName(t *testing.T) {
	cases := map[string]string{
		"0a1b2c3d-4e5f-6789-abcd-ef0123456789": "w-0a1b2c3d",
		"ab":                                   "w-ab",
	}
	for id, want := range cases {
		if got := fallbackName(id); got != want {
			t.Errorf("fallbackName(%q) = %q, want %q", id, got, want)
		}
	}
	for _, id := range []string{"../../evil-session", `a\b/c`, "", "////////", "..", "é-ü"} {
		got := fallbackName(id)
		if !strings.HasPrefix(got, "w-") || !spawn.ValidName(got) || len(got) < 3 {
			t.Errorf("fallbackName(%q) = %q, want w- and letters or digits", id, got)
		}
	}
}

func TestClearedNameNamesNoTerminal(t *testing.T) {
	r := route.Record{Kind: "worker", ClaudeSession: "s-1", TermLauncher: "tmux", TermID: "@3"}
	if ref, ok := termRef(&r); ok {
		t.Errorf("termRef = %+v, want none for a Record with no name", ref)
	}
	r.Name = "w1"
	if ref, ok := termRef(&r); !ok || ref.Name != "w1" || ref.ID != "@3" {
		t.Errorf("termRef = %+v, %v, want the named window", ref, ok)
	}
}
