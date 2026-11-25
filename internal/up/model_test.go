package up

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/route"
)

var fableOpus = CoordinatorModel{Model: "fable", Fallback: "opus"}

func TestFreshModel(t *testing.T) {
	const old = "claude is too old"
	for _, tc := range []struct {
		name            string
		args            []string
		config          CoordinatorModel
		check           string // what ModelCheck answers
		model, fallback string
		text, note      string
	}{
		{"default", nil, fableOpus, "", "fable", "opus", "model fable (up.coordinator_model), fallback opus (up.coordinator_fallback)", ""},
		{"other arguments", []string{"--permission-mode", "plan"}, fableOpus, "", "fable", "opus", "model fable (up.coordinator_model), fallback opus (up.coordinator_fallback)", ""},
		{"no fallback", nil, CoordinatorModel{Model: "opus"}, "", "opus", "", "model opus (up.coordinator_model), fallback not set", ""},
		{"no model is no fallback", nil, CoordinatorModel{Fallback: "opus", ModelNote: "bad", FallbackNote: "bad too"}, old, "", "", "model not set, fallback not set", "bad"},
		{"fallback same as model", nil, CoordinatorModel{Model: "opus", Fallback: "opus"}, "", "opus", "", "model opus (up.coordinator_model), fallback not set", ""},
		{"explicit model", []string{"--model", "sonnet"}, fableOpus, old, "", "", "model sonnet (its claude arguments), fallback not set", ""},
		{"explicit model, one word", []string{"--model=sonnet"}, fableOpus, "", "", "", "model sonnet (its claude arguments), fallback not set", ""},
		{"explicit both", []string{"--model", "sonnet", "--fallback-model=haiku"}, fableOpus, "", "", "", "model sonnet (its claude arguments), fallback haiku (its claude arguments)", ""},
		{"explicit fallback", []string{"--fallback-model", "sonnet"}, fableOpus, "", "fable", "", "model fable (up.coordinator_model), fallback sonnet (its claude arguments)", ""},
		{"explicit fallback names the model", []string{"--fallback-model", "opus, fable"}, fableOpus, "", "", "", "model not set, fallback opus, fable (its claude arguments)", ""},
		{"explicit fallback names the model's id", []string{"--fallback-model", "claude-fable-5-2"}, fableOpus, "", "", "", "model not set, fallback claude-fable-5-2 (its claude arguments)", ""},
		{"bad values", nil, CoordinatorModel{Model: "fable", Fallback: "opus", ModelNote: "bad", FallbackNote: "bad too"}, "", "fable", "opus", "model fable (up.coordinator_model), fallback opus (up.coordinator_fallback)", "bad; bad too"},
		{"bad values, explicit fallback", []string{"--fallback-model", "sonnet"}, CoordinatorModel{Model: "fable", Fallback: "opus", ModelNote: "bad", FallbackNote: "bad too"}, "", "fable", "", "model fable (up.coordinator_model), fallback sonnet (its claude arguments)", "bad"},
		{"bad values, explicit model", []string{"--model", "sonnet"}, CoordinatorModel{Model: "fable", Fallback: "opus", ModelNote: "bad", FallbackNote: "bad too"}, "", "", "", "model sonnet (its claude arguments), fallback not set", ""},
		{"old claude", nil, CoordinatorModel{Model: "fable", Fallback: "opus", ModelNote: "bad"}, old, "", "", "model not set, fallback not set", "bad; " + old},
		{"old claude, explicit fallback", []string{"--fallback-model", "sonnet"}, fableOpus, old, "", "", "model not set, fallback sonnet (its claude arguments)", old},
		{"old claude, model without fallback", nil, CoordinatorModel{Model: "opus"}, old, "", "", "model not set, fallback not set", old},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := FreshModel(tc.args, Deps{Model: tc.config, ModelCheck: func() string { return tc.check }})
			if got.Model != tc.model || got.Fallback != tc.fallback {
				t.Errorf("model %q fallback %q, want %q %q", got.Model, got.Fallback, tc.model, tc.fallback)
			}
			if got.Text != tc.text || got.Note != tc.note {
				t.Errorf("text %q note %q\nwant %q, %q", got.Text, got.Note, tc.text, tc.note)
			}
		})
	}
}

func TestFreshModelWithoutCheck(t *testing.T) {
	if got := FreshModel(nil, Deps{Model: fableOpus}); got.Model != "fable" || got.Fallback != "opus" {
		t.Errorf("got %+v", got)
	}
}

// claudeLine is the claude command line of a coordinator's script, up to its
// name.
func claudeLine(t *testing.T, script string) string {
	t.Helper()
	_, line, ok := strings.Cut(script, " 'claude' ")
	if !ok {
		if _, line, ok = strings.Cut(script, " claude "); !ok {
			t.Fatalf("no claude line:\n%s", script)
		}
	}
	line, _, _ = strings.Cut(line, "\n")
	return line
}

func TestStartGivesTheDefaultModel(t *testing.T) {
	f := &fakeAgterm{}
	var logged []route.Record
	d := upDeps(t, f, &logged)
	d.Model = fableOpus
	p, o, err := Start(context.Background(), t.TempDir(), []string{"--permission-mode", "plan"}, d)
	if err != nil {
		t.Fatal(err)
	}
	line := claudeLine(t, readFile(t, o.Script))
	if !strings.HasPrefix(line, "--model fable --fallback-model opus --permission-mode plan -n ") {
		t.Errorf("line = %q", line)
	}
	// Not among the arguments the user gave, so it never looks chosen.
	if !slices.Equal(p.ClaudeArgs, []string{"--permission-mode", "plan"}) {
		t.Errorf("claude args %q", p.ClaudeArgs)
	}
	if got := savedProject(t, p).DefaultModels[p.Coordinator.ID]; got != "fable" {
		t.Errorf("saved default model %q, want fable", got)
	}
	if o.Model.Text != "model fable (up.coordinator_model), fallback opus (up.coordinator_fallback)" {
		t.Errorf("outcome model %q", o.Model.Text)
	}
	if r := upRecords(logged)[p.Coordinator.Name]; r.DefaultModel != "fable" {
		t.Errorf("record %+v", r)
	}
}

func TestStartExplicitModelWins(t *testing.T) {
	for _, given := range []bool{true, false} {
		f := &fakeAgterm{}
		var logged []route.Record
		d := upDeps(t, f, &logged)
		d.Model = fableOpus
		var args []string
		if given {
			args = []string{"--model", "sonnet"}
		} else {
			d.Coordinator = []string{"--model", "sonnet"}
		}
		p, o, err := Start(context.Background(), t.TempDir(), args, d)
		if err != nil {
			t.Fatal(err)
		}
		if line := claudeLine(t, readFile(t, o.Script)); !strings.HasPrefix(line, "--model sonnet -n ") {
			t.Errorf("given %v: line = %q", given, line)
		}
		if len(p.DefaultModels) != 0 || upRecords(logged)[p.Coordinator.Name].DefaultModel != "" {
			t.Errorf("given %v: recorded as a default: %v", given, p.DefaultModels)
		}
	}
}

func TestStartHereGivesTheDefaultModel(t *testing.T) {
	f := &fakeAgterm{}
	var logged []route.Record
	d := upDeps(t, f, &logged)
	d.Model, d.Launcher = fableOpus, nil
	d.Here = func(context.Context, *Project, []string, bool) (launch.SessionRef, string) {
		return launch.SessionRef{Launcher: "agterm", ID: "t1"}, ""
	}
	p, o, err := Start(context.Background(), t.TempDir(), nil, d)
	if err != nil {
		t.Fatal(err)
	}
	if line := claudeLine(t, readFile(t, o.Script)); !strings.HasPrefix(line, "--model fable --fallback-model opus -n ") {
		t.Errorf("line = %q", line)
	}
	if savedProject(t, p).DefaultModels[p.Coordinator.ID] != "fable" {
		t.Error("the default model is not saved")
	}
}

// The default is read from the config at each fresh start, not from the
// project: another value there gives the next fresh start another model.
func TestApplyFreshCoordinatorFollowsTheConfig(t *testing.T) {
	w, _, d, steps, logged := coordFresh(t)
	d.Model = CoordinatorModel{Model: "opus", Fallback: "sonnet"}
	w.project.DefaultModels = map[string]string{coordID: "fable"}
	outs := Apply(context.Background(), &w.project, steps, d)
	if outs[0].Err != nil || outs[0].Step.Action != Fresh {
		t.Fatalf("outcome %+v", outs[0])
	}
	if line := claudeLine(t, readFile(t, outs[0].Script)); !strings.HasPrefix(line, "--model opus --fallback-model sonnet --settings ") {
		t.Errorf("line = %q", line)
	}
	if got := savedProject(t, w.project).DefaultModels[outs[0].NewSessionID]; got != "opus" {
		t.Errorf("saved default model %q", got)
	}
	if r := upRecords(*logged)["repo-coord"]; r.DefaultModel != "opus" {
		t.Errorf("record %+v", r)
	}
	if !strings.HasPrefix(outs[0].Model.Text, "model opus (up.coordinator_model)") {
		t.Errorf("outcome model %q", outs[0].Model.Text)
	}
}

// The session's model is saved before it starts, with its id, and forgotten
// when the launch failed.
func TestApplyDefaultModelIsReservedAndWithdrawn(t *testing.T) {
	w, f, d, steps, _ := coordFresh(t)
	d.Model = fableOpus
	var during map[string]string
	d = onNew(d, f, func() { during = savedProject(t, w.project).DefaultModels }, nil)
	f.fail = map[string]bool{"repo-coord": true}
	outs := Apply(context.Background(), &w.project, steps, d)
	if outs[0].Err == nil {
		t.Fatal("the launch did not fail")
	}
	if len(during) != 1 {
		t.Errorf("while it started, the saved default models were %v", during)
	}
	if got := savedProject(t, w.project).DefaultModels; len(got) != 0 {
		t.Errorf("after the failed launch: %v", got)
	}
}

func TestApplyResumedCoordinatorModel(t *testing.T) {
	for _, tc := range []struct {
		name   string
		saved  map[string]string
		args   []string
		config CoordinatorModel
		check  string
		prefix string
		text   string
	}{
		{"not started with a default", nil, nil, fableOpus, "", "-n ", ""},
		{"another session was", map[string]string{"other": "fable"}, nil, fableOpus, "", "-n ", ""},
		{"started with the default", map[string]string{coordID: "fable"}, nil, fableOpus, "", "--fallback-model opus -n ", "fallback opus (up.coordinator_fallback)"},
		{"the config's current fallback", map[string]string{coordID: "fable"}, nil, CoordinatorModel{Fallback: "sonnet"}, "", "--fallback-model sonnet -n ", "fallback sonnet (up.coordinator_fallback)"},
		{"no fallback now", map[string]string{coordID: "fable"}, nil, CoordinatorModel{Model: "fable"}, "", "-n ", ""},
		{"the fallback is its model", map[string]string{coordID: "opus"}, nil, fableOpus, "", "-n ", ""},
		{"explicit model", map[string]string{coordID: "fable"}, []string{"--model", "sonnet"}, fableOpus, "", "--model sonnet -n ", ""},
		{"explicit fallback", map[string]string{coordID: "fable"}, []string{"--fallback-model", "haiku"}, fableOpus, "", "--fallback-model haiku -n ", ""},
		{"old claude", map[string]string{coordID: "fable"}, nil, fableOpus, "too old", "-n ", ""},
		{"an explicit choice since", map[string]string{coordID: ""}, nil, fableOpus, "", "-n ", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logged []route.Record
			d := upDeps(t, &fakeAgterm{ws: "repo"}, &logged)
			d.Model, d.ModelCheck = tc.config, func() string { return tc.check }
			w := newWorld(t)
			fixture(t, "warm.jsonl", w.root, w.transcripts[coordID])
			w.project.DefaultModels, w.project.ClaudeArgs = tc.saved, tc.args
			steps := w.plan(t, nil, nil)
			outs := Apply(context.Background(), &w.project, steps, d)
			if outs[0].Err != nil || outs[0].Step.Action != Resume {
				t.Fatalf("outcome %+v", outs[0])
			}
			if line := claudeLine(t, readFile(t, outs[0].Script)); !strings.HasPrefix(line, tc.prefix) {
				t.Errorf("line = %q, want prefix %q", line, tc.prefix)
			}
			if outs[0].Model.Text != tc.text {
				t.Errorf("text %q, want %q", outs[0].Model.Text, tc.text)
			}
		})
	}
}

func TestReplaceClaudeArgsForgetsDefaultsOnAnExplicitChoice(t *testing.T) {
	for _, tc := range []struct {
		args []string
		kept bool
	}{
		{[]string{}, true},
		{[]string{"--permission-mode", "plan"}, true},
		{[]string{"--model", "opus"}, false},
		{[]string{"--model=opus"}, false},
		{[]string{"--fallback-model", "opus"}, false},
	} {
		p := Project{DefaultModels: map[string]string{"c1": "fable"}}
		p.ReplaceClaudeArgs(tc.args)
		if model, ok := p.DefaultModels["c1"]; !slices.Equal(p.ClaudeArgs, tc.args) || !ok || (model == "fable") != tc.kept {
			t.Errorf("%q: args %q default models %v", tc.args, p.ClaudeArgs, p.DefaultModels)
		}
	}
}

// On a resume only the fallback comes from the config, so only what is wrong
// with it is said.
func TestResumedModelNote(t *testing.T) {
	p := Project{DefaultModels: map[string]string{"c1": "fable"}}
	d := Deps{Model: CoordinatorModel{Model: "fable", Fallback: "opus", ModelNote: "bad", FallbackNote: "bad too"}}
	s := Step{Candidate: Candidate{Role: RoleCoordinator, SessionID: "c1"}, Action: Resume}
	if got := StepModel(&p, s, nil, d); got.Note != "bad too" || got.Fallback != "opus" {
		t.Errorf("got %+v", got)
	}
	// A bad value read as the model the session started on adds nothing, and
	// is said all the same.
	p.DefaultModels["c1"] = "opus"
	d.ModelCheck = func() string {
		t.Error("the version was read, and nothing is added")
		return ""
	}
	if got := StepModel(&p, s, nil, d); got != (ModelChoice{Note: "bad too"}) {
		t.Errorf("got %+v", got)
	}
}

// A coordinator that runs with the model qm gave it by default, found by an
// adoption (its project's state was moved aside): that model is not taken
// for one the user chose.
func TestDetectDropsTheDefaultModel(t *testing.T) {
	const id = "00000000-0000-4000-8000-0000000000c9"
	const cleared = "00000000-0000-4000-8000-0000000000ca"
	up := func(action, model string) route.Record {
		return route.Record{Kind: "up", Action: action, ClaudeSession: id, DefaultModel: model}
	}
	dropped := []string{"--permission-mode", "plan"}
	const line = "claude --model fable --fallback-model opus --permission-mode plan -n repo-coord --session-id " + id
	given := []string{"--model", "fable"}
	for _, tc := range []struct {
		name string
		live string // the session's id now
		cmd  string
		recs []route.Record
		want []string
	}{
		{"default", id, line, []route.Record{up("fresh", "fable")}, dropped},
		{"its launch line was printed", id, line, []route.Record{up("script", "fable")}, dropped},
		{"after /clear", cleared, line, []route.Record{up("fresh", "fable")}, dropped},
		{"recorded by qm down", id, line, []route.Record{up("fresh", "fable"), up("down", "")}, dropped},
		{"an adoption that was not saved", id, line, []route.Record{up("fresh", "fable"), up("adopt", "")}, dropped},
		{"left running", id, line, []route.Record{up("fresh", "fable"), up("leave", "")}, dropped},
		{"no record", id, "claude --model fable", nil, given},
		{"another model", id, "claude --model opus", []route.Record{up("fresh", "fable")}, []string{"--model", "opus"}},
		{"resumed with it given", id, "claude --model fable", []route.Record{up("fresh", "fable"), up("resume", "")}, given},
		{"its line printed with it given", id, "claude --model fable", []route.Record{up("fresh", "fable"), up("script", "")}, given},
		{"another session's", id, "claude --model fable", []route.Record{{Kind: "up", Action: "fresh", ClaudeSession: "x", DefaultModel: "fable"}}, given},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			det := Detect(root, nil, tc.recs, DetectEnv{
				Live:      map[string]claudecode.LiveSession{tc.live: {PID: 901, Cwd: root, Name: "repo-coord"}},
				Procs:     []claudecode.Proc{{PID: 901, Cmd: tc.cmd}},
				Worktrees: []string{root},
			})
			if len(det.Candidates) != 1 {
				t.Fatalf("candidates %+v", det.Candidates)
			}
			c := det.Candidates[0]
			if !slices.Equal(c.Kept, tc.want) {
				t.Errorf("kept %q, want %q", c.Kept, tc.want)
			}
			if slices.Contains(c.Dropped, "--model") == slices.Contains(c.Kept, "--model") {
				t.Errorf("kept %q, dropped %q", c.Kept, c.Dropped)
			}
		})
	}
}

// A coordinator qm wrote a launch line for, and does not know to have
// started, has a Record of its default model too: an adoption reads it.
func TestDefaultModelIsLoggedForAPrintedLine(t *testing.T) {
	var logged []route.Record
	d := upDeps(t, &fakeAgterm{}, &logged)
	d.Model, d.Launcher = fableOpus, nil
	p, _, err := Start(context.Background(), t.TempDir(), nil, d)
	if !errors.Is(err, launch.ErrNoLauncher) {
		t.Fatal(err)
	}
	check := func(what, id, model string) {
		t.Helper()
		if len(logged) != 1 || logged[0].Kind != "up" || logged[0].Action != "script" || logged[0].ClaudeSession != id || logged[0].DefaultModel != model {
			t.Errorf("%s: logged %+v", what, logged)
		}
		logged = nil
	}
	check("first run", p.Coordinator.ID, "fable")

	d.Model = CoordinatorModel{}
	if _, _, err := Start(context.Background(), t.TempDir(), nil, d); !errors.Is(err, launch.ErrNoLauncher) || len(logged) != 0 {
		t.Errorf("without a default: %v, logged %+v", err, logged)
	}

	f := &fakeAgterm{fail: map[string]bool{}}
	d = upDeps(t, f, &logged)
	d.Model = fableOpus
	root := t.TempDir()
	f.fail[WorkspaceName(root, func(string) bool { return false })+"-coord"] = true
	p, _, err = Start(context.Background(), root, nil, d)
	if err == nil {
		t.Fatal("the launch did not fail")
	}
	check("a failed first launch", p.Coordinator.ID, "fable")

	w, _, d, steps, restored := coordFresh(t)
	d.Model, d.Launcher = fableOpus, nil
	outs := Apply(context.Background(), &w.project, steps, d)
	logged = *restored
	check("a restore", outs[0].NewSessionID, "fable")

	// A resume by a printed line, with an explicit model since.
	w, _, d, _, restored = coordFresh(t)
	fixture(t, "warm.jsonl", w.root, w.transcripts[coordID])
	d.Model, d.Launcher = fableOpus, nil
	w.project.DefaultModels = map[string]string{coordID: "fable"}
	w.project.ReplaceClaudeArgs([]string{"--model", "fable"})
	Apply(context.Background(), &w.project, w.plan(t, nil, nil), d)
	logged = *restored
	check("a resume", coordID, "")
}
