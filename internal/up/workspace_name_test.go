package up

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/launch"
)

// agtermTerms is qm down's Terminals over one agterm.
type agtermTerms struct{ a launch.Agterm }

func (x agtermTerms) Sessions(ctx context.Context, _, ws string) ([]launch.SessionRef, error) {
	return x.a.Sessions(ctx, ws)
}

func (agtermTerms) PaneRuns(context.Context, launch.SessionRef, string) (bool, error) {
	return false, nil // agterm only: no tmux window
}

func (x agtermTerms) Close(ctx context.Context, ref launch.SessionRef) error {
	return x.a.Close(ctx, ref)
}

// A workspace name with spaces and a non-ASCII character goes through
// adoption, the saved state, a worker's launch script, agtermctl and qm
// down unchanged.
func TestWorkspaceNameSpacesAndNonASCII(t *testing.T) {
	const name = "Shop v2 · next"
	w := newAdoptWorld(t)
	w.agterm.ws[0].Name = name
	w.adopt(t, allThree)
	p, ok, err := Load(w.repo)
	if err != nil || !ok || p.Workspace != name {
		t.Fatalf("saved workspace %q (%v, %v), want %q", p.Workspace, ok, err, name)
	}

	// The hand-started worker exits; qm up resumes it in a new tab.
	hand := w.procs[2]
	w.procs = w.procs[:2]
	recs := append(slices.Clone(w.recs), w.logged...)
	trusted := func(string) (bool, error) { return true, nil }
	cands, err := Gather(&p, recs, Env{Now: now, Live: w.live(t), TranscriptPath: w.transcriptPath, Trusted: trusted})
	if err != nil {
		t.Fatal(err)
	}
	steps := Plan(cands, false, now)
	run := func(ctx context.Context, bin string, args ...string) ([]byte, error) {
		if len(args) > 1 && args[0] == "session" && args[1] == "new" {
			w.agterm.calls = append(w.agterm.calls, strings.Join(args, "\x00"))
			return []byte(`{"ok":true,"result":{"id":"s-new"}}`), nil
		}
		return w.agterm.run(ctx, bin, args...)
	}
	ag := launch.Agterm{Bin: "agtermctl", Run: run}
	outs := Apply(context.Background(), &p, steps, Deps{Launcher: ag, Exe: "/opt/qm/bin/qm", Now: func() time.Time { return now }})
	var script string
	for _, o := range outs {
		if o.Err != nil {
			t.Fatalf("%s: %v", o.Step.Name, o.Err)
		}
		if o.Step.SessionID == adHand {
			script = o.Script
		}
	}
	if script == "" {
		t.Fatalf("the worker was not started: %+v", steps)
	}
	var spawned []string
	for _, c := range w.agterm.calls {
		if strings.HasPrefix(c, "session\x00new\x00") {
			spawned = strings.Split(c, "\x00")
		}
	}
	if i := slices.Index(spawned, "--workspace-name"); i < 0 || i+1 >= len(spawned) || spawned[i+1] != name {
		t.Fatalf("agtermctl arguments %q, want --workspace-name %q", spawned, name)
	}

	// The script's env line, read back by /bin/sh, gives the name exactly.
	b, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	var line string
	for l := range strings.Lines(string(b)) {
		if strings.HasPrefix(l, "exec env ") {
			line = l
		}
	}
	before, _, found := strings.Cut(line, " claude ")
	if !found {
		t.Fatalf("no env line in:\n%s", b)
	}
	out, err := exec.Command("/bin/sh", "-c", strings.TrimPrefix(before, "exec ")+" printenv QM_WORKSPACE").Output()
	if err != nil || string(out) != name+"\n" {
		t.Fatalf("QM_WORKSPACE read back as %q (%v), want %q; line %q", out, err, name, line)
	}

	// qm down lists the workspace by its name and closes every tab.
	w.procs = append(w.procs, hand)
	w.agterm.ws[0].Sessions = append(w.agterm.ws[0].Sessions, launch.SessionRef{ID: "s-new", Name: "w-hand"})
	// The new tab runs the resumed worker: qm down closes a running
	// session's tab only when its foreground is that session.
	if w.agterm.fg == nil {
		w.agterm.fg = map[string][]string{}
	}
	for _, s := range steps {
		if s.Name == "w-hand" {
			w.agterm.fg["s-new"] = []string{"claude", "-n", "w-hand", "--resume", s.SessionID}
		}
	}
	if cands, err = Gather(&p, recs, Env{Now: now, Live: w.live(t), TranscriptPath: w.transcriptPath, Trusted: trusted}); err != nil {
		t.Fatal(err)
	}
	for _, o := range Down(context.Background(), &p, Plan(cands, false, now), agtermTerms{ag}, false) {
		if o.Err != nil {
			t.Fatalf("down %s: %v", o.Step.Name, o.Err)
		}
	}
	for _, tab := range []string{"s-c", "s-q", "s-new"} {
		if !w.agterm.has("session close --target " + tab + " --json") {
			t.Fatalf("%s not closed: %q", tab, w.agterm.calls)
		}
	}
}
