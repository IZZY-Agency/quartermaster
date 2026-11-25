package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/plain"
	"github.com/IZZY-Agency/quartermaster/internal/route"
	"github.com/IZZY-Agency/quartermaster/internal/spawn"
	"github.com/IZZY-Agency/quartermaster/internal/up"
)

// forged is a name that ends its line and starts one that reads as qm's own.
const forged = "w1\nqm: forged"

// noForgedLine fails when a line of out is the forged one.
func noForgedLine(t *testing.T, out string) {
	t.Helper()
	if !strings.Contains(out, "forged") {
		t.Errorf("the name is not in %q", out)
	}
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "qm: forged") {
			t.Errorf("a name started a line of its own: %q", out)
		}
	}
}

// What qm up, down and spawn print of a name, a folder, a tab or a note from
// elsewhere stays on its line.
func TestPrintedNamesStayOnTheirLine(t *testing.T) {
	ref := launch.SessionRef{Launcher: "agterm", ID: forged, Name: forged, Warning: forged, TintNote: forged}
	step := up.Step{Candidate: up.Candidate{Role: up.RoleWorker, SessionID: "s1", Name: forged}, Action: up.Resume, Why: forged}
	p := &up.Project{ID: "shop-dbea7844", Root: "/work/" + forged, Workspace: forged, Sessions: map[string]launch.SessionRef{"s1": ref}}
	var b bytes.Buffer
	for name, print := range map[string]func(){
		"notes":      func() { printNotes(&b, []string{forged}) },
		"model":      func() { printModel(&b, up.ModelChoice{Text: forged}, true) },
		"model note": func() { printModelNote(&b, up.ModelChoice{Note: forged}) },
		"attach":     func() { attachHint(&b, launch.Tmux{}, forged, 1) },
		"switch":     func() { attachHint(&b, launch.Tmux{Inside: true}, forged, 1) },
		"warning":    func() { warn(&b, up.Outcome{Step: step, Ref: ref}) },
		"result":     func() { printResult(&b, up.Outcome{Step: step, Ref: ref}) },
		"left":       func() { s := step; s.Action = up.Leave; printResult(&b, up.Outcome{Step: s}) },
		"skipped":    func() { s := step; s.Action = up.Skip; printResult(&b, up.Outcome{Step: s}) },
		"here":       func() { printResult(&b, up.Outcome{Step: step, Ref: ref, Here: true}) },
		"failed":     func() { printFailed(&b, up.Outcome{Step: step, Err: errQuiet}) },
		"down":       func() { showDown(&b, p, []up.Step{step}, true, nil) },
		"down one":   func() { printDown(&b, up.Outcome{Step: step, Ref: ref}, false) },
		"down left":  func() { printDown(&b, up.Outcome{Step: step, Ref: ref}, true) },
		"started":    func() { _ = reportStarted(spawn.Result{TintNote: forged, Ref: ref}, &b, &b) },
		"decision":   func() { b.WriteString(decisionLine(route.Decision{Reasons: []string{forged}})) },
		"stale": func() {
			b.WriteString(up.RenderStale(&up.StalePlan{
				Close: []up.Stale{{Ref: ref, Step: step}}, Notes: []string{forged}, Warn: map[string][]string{"s1": {forged}},
			}))
		},
	} {
		t.Run(name, func(t *testing.T) {
			b.Reset()
			print()
			noForgedLine(t, b.String())
		})
	}
}

// An error that holds a folder or an id from elsewhere keeps the lines after
// its first indented.
func TestErrorsDoNotStartALine(t *testing.T) {
	newUpMachine(t)
	dir := filepath.Join(t.TempDir(), forged)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	swap(t, &upWorkDir, func() (string, error) { return dir, nil })
	for _, args := range [][]string{
		{"up", "done", "w9"},
		{"down"},
		{"spawn", "worker", "--type", "fix", "--size", "S", "--hint", "sonnet/low", "--cwd", filepath.Join(dir, "gone"), "--name", "w1", "--brief", filepath.Join(dir, "gone.md")},
	} {
		t.Run(args[0], func(t *testing.T) {
			code, _, errs := runQM(args...)
			if code == 0 {
				t.Fatal("exit 0, want an error")
			}
			noForgedLine(t, errs)
		})
	}
	t.Setenv("QM_PROJECT_ID", forged)
	_, _, errs := runQM("up", "handoff")
	noForgedLine(t, errs)
}

// What a program reads of qm's output holds the same names, through the writer
// that keeps control characters off the terminal.
func TestMachineOutputKeepsNames(t *testing.T) {
	const name = "a\u200fb\u009bc"
	var b bytes.Buffer
	out := plain.Writer(&b)
	d := route.Decision{Reasons: []string{name}}
	if err := printDecision(out, d, true); err != nil {
		t.Fatal(err)
	}
	var got route.Decision
	if err := json.Unmarshal(b.Bytes(), &got); err != nil || !slices.Equal(got.Reasons, d.Reasons) {
		t.Errorf("qm route --json: %q, %v, want %q", got.Reasons, err, d.Reasons)
	}

	b.Reset()
	if err := printJSON(out, map[string]string{"folder": name}); err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	if err := json.Unmarshal(b.Bytes(), &m); err != nil || m["folder"] != name {
		t.Errorf("qm status --json: %q, %v, want %q", m["folder"], err, name)
	}

	b.Reset()
	dir := t.TempDir()
	for file, text := range map[string]string{"status": spawn.StatusDone, "findings.json": "{\n\t\"a\": \"" + name + "\"\r\n}\n"} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := printReview("r1", dir, out); err != nil {
		t.Fatal(err)
	}
	_, text, _ := strings.Cut(b.String(), "\n")
	m = nil
	if err := json.Unmarshal([]byte(text), &m); err != nil || m["a"] != name {
		t.Errorf("findings: %q, %v, want %q", m["a"], err, name)
	}

	b.Reset()
	if err := printPath(out, "/state/"+name+".md"); err == nil || b.Len() > 0 {
		t.Errorf("a path that cannot be shown as it is: %v, printed %q, want an error and nothing", err, b.String())
	}
	for _, path := range []string{"/state/a\nb", "/state/a\xffb", "/state/a\u2028b", "/state/a\rb"} {
		if err := printPath(out, path); err == nil || b.Len() > 0 {
			t.Errorf("printPath(%q): %v, printed %q, want an error and nothing", path, err, b.String())
		}
	}
	for _, path := range []string{"/state/é.md", "/My\u00a0Files/\u3000a b.md", "/state/a\tb"} {
		b.Reset()
		if err := printPath(out, path); err != nil || b.String() != path+"\n" {
			t.Errorf("printPath(%q): %v, %q", path, err, b.String())
		}
	}
}
