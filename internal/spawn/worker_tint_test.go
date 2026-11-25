package spawn

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/IZZY-Agency/quartermaster/internal/launch"
)

func TestWorkerLaunchesWithTheWorkerTint(t *testing.T) {
	for _, tc := range []struct {
		name, cfg string
		want      launch.Tint
	}{
		{"default palette", `{}`, launch.WorkerTint("teal")},
		{"configured colour", `{"tint":{"worker":"#AABBCC"}}`, launch.WorkerTint("#aabbcc")},
		{"empty means none", `{"tint":{"worker":""}}`, launch.Tint{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hermetic(t)
			repo := repoWith(t, "README.md")
			l := &fakeLauncher{}
			deps := testDeps(l)
			if err := json.Unmarshal([]byte(tc.cfg), &deps.Config); err != nil {
				t.Fatal(err)
			}
			res, err := Worker(context.Background(), request(t, repo, "docs"), deps)
			if err != nil {
				t.Fatal(err)
			}
			if len(l.specs) != 1 || l.specs[0].Tint != tc.want {
				t.Errorf("launched %+v, want tint %+v", l.specs, tc.want)
			}
			if res.Tint != tc.want || res.TintNote != "" {
				t.Errorf("Result.Tint = %+v, note %q", res.Tint, res.TintNote)
			}
		})
	}
}

func TestWorkerBadTintColourStillLaunchesWithTheDefault(t *testing.T) {
	hermetic(t)
	repo := repoWith(t, "README.md")
	l := &fakeLauncher{}
	deps := testDeps(l)
	if err := json.Unmarshal([]byte(`{"tint":{"worker":"chartreuse"}}`), &deps.Config); err != nil {
		t.Fatal(err)
	}
	res, err := Worker(context.Background(), request(t, repo, "docs"), deps)
	if err != nil {
		t.Fatalf("a bad tint colour stopped the launch: %v", err)
	}
	if len(l.specs) != 1 || l.specs[0].Tint != launch.WorkerTint("teal") {
		t.Errorf("launched %+v, want the default tint", l.specs)
	}
	if !strings.Contains(res.TintNote, "tint.worker") || !strings.Contains(res.TintNote, "chartreuse") {
		t.Errorf("TintNote = %q, want it to name the key and the value", res.TintNote)
	}
}

func TestDryRunReportsTheTintAndTmuxHasNone(t *testing.T) {
	hermetic(t)
	repo := repoWith(t, "README.md")
	req := request(t, repo, "fix")
	req.DryRun = true
	l := &fakeLauncher{}
	res, err := Worker(context.Background(), req, testDeps(l))
	if err != nil {
		t.Fatal(err)
	}
	if res.Tint != launch.WorkerTint("teal") || len(l.specs) != 0 {
		t.Errorf("Tint = %+v, launched %d", res.Tint, len(l.specs))
	}

	deps := testDeps(l)
	deps.Pick = func(string, func(string) string) (launch.Launcher, error) {
		return launch.Tmux{Bin: "tmux", Inside: true}, nil
	}
	res, err = Worker(context.Background(), req, deps)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Tint.IsZero() {
		t.Errorf("tmux dry run shows tint %+v; tmux does not tint", res.Tint)
	}
}
