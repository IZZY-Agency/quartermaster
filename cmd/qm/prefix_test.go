package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/declog"
	"github.com/IZZY-Agency/quartermaster/internal/route"
	"github.com/IZZY-Agency/quartermaster/internal/up"
)

const upWorker2ID = "00000000-0000-4000-8000-0000000000a2"

// worker2 logs a second worker Record of p, with its own session and
// transcript, so two workers can share a name or differ by the prefix.
func (m *upMachine) worker2(t *testing.T, p up.Project, name string) {
	t.Helper()
	m.worker2At(t, p, name, m.root)
}

// worker2At is worker2 with the worker's folder cwd.
func (m *upMachine) worker2At(t *testing.T, p up.Project, name, cwd string) {
	t.Helper()
	r := route.Record{
		Time: time.Now().UTC().Add(-time.Hour), Kind: "worker", Name: name, Cwd: cwd,
		UpProject: p.ID, ClaudeSession: upWorker2ID,
		Decision: &route.Decision{Kind: "worker", Tier: route.Tier{Model: "sonnet", Effort: "low"}, Profile: &route.Profile{
			Autocompact: 150000, PluginsOn: []string{"qm-worker"}, PluginDirs: []string{"/old/qm-worker"},
		}},
	}
	if err := declog.Append(r); err != nil {
		t.Fatal(err)
	}
	m.fixture(t, "coord.jsonl", filepath.Join(m.claude, "projects", "p", upWorker2ID+".jsonl"))
}

// workerBase is worker with the name the worker was spawned with, before its
// prefix.
func (m *upMachine) workerBase(t *testing.T, p up.Project, name, base string) {
	t.Helper()
	r := route.Record{
		Time: time.Now().UTC().Add(-time.Hour), Kind: "worker", Name: name, BaseName: base, Cwd: m.root,
		UpProject: p.ID, ClaudeSession: upWorkerID,
		Decision: &route.Decision{Kind: "worker", Tier: route.Tier{Model: "sonnet", Effort: "low"}, Profile: &route.Profile{
			Autocompact: 150000, PluginsOn: []string{"qm-worker"}, PluginDirs: []string{"/old/qm-worker"},
		}},
	}
	if err := declog.Append(r); err != nil {
		t.Fatal(err)
	}
	m.fixture(t, "coord.jsonl", filepath.Join(m.claude, "projects", "p", upWorkerID+".jsonl"))
}

// prefixDry runs qm spawn worker --dry-run with name in a new repository and
// returns the repository, the exit code, stdout and stderr. config, when set,
// returns the config file's text for that repository.
func prefixDry(t *testing.T, name string, config func(repo string) string) (repo string, code int, stdout, stderr string) {
	t.Helper()
	routeHermetic(t)
	t.Setenv("AGTERM_SESSION_ID", "")
	t.Setenv("TMUX", "")
	t.Setenv("CLAUDE_PID", "")
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "claude"))
	t.Setenv("QM_WORKER_PLUGIN", t.TempDir())
	repo = routeRepo(t)
	// The config keys a repository as git names it, symlinks resolved.
	if real, err := filepath.EvalSymlinks(repo); err == nil {
		repo = real
	}
	if config != nil {
		writeConfig(t, config(repo))
	}
	brief := routeFile(t, t.TempDir(), "brief.md", "Fix the typo in the readme.\n")
	var out, errb bytes.Buffer
	code = run([]string{"spawn", "worker", "--brief", brief, "--type", "docs", "--size", "S", "--hint", "sonnet/low", "--name", name, "--cwd", repo, "--dry-run"}, &out, &errb)
	return repo, code, out.String(), errb.String()
}

func TestSpawnWorkerNamePrefix(t *testing.T) {
	tied := func(t *testing.T) {
		t.Setenv("QM_PROJECT_ID", "try-00000000")
		t.Setenv("QM_WORKSPACE", "try")
	}
	tests := []struct {
		name    string
		tied    bool
		in      string
		config  func(repo string) string
		want    func(repo string) string // the name line; "" for an error
		wantErr string
	}{
		{name: "auto: the folder's base name when the project has no state", in: "w1",
			want: func(repo string) string { return filepath.Base(repo) + "-w1" }},
		{name: "auto: the tied project's workspace", tied: true, in: "w1",
			want: func(string) string { return "try-w1" }},
		{name: "a name that is the prefix is kept", tied: true, in: "try",
			want: func(string) string { return "try" }},
		{name: "a name that starts with the prefix is kept", tied: true, in: "try-w1",
			want: func(string) string { return "try-w1" }},
		{name: "the map entry wins", tied: true, in: "w1",
			config: func(repo string) string {
				return `{"project_name_prefix":{"` + repo + `":"ut"}}`
			},
			want: func(string) string { return "ut-w1" }},
		{name: "off", tied: true, in: "w1",
			config: func(string) string { return `{"worker":{"name_prefix":""}}` },
			want:   func(string) string { return "w1" }},
		{name: "a bad configured prefix is an error naming the key", in: "w1",
			config: func(repo string) string {
				return `{"project_name_prefix":{"` + repo + `":"bad prefix"}}`
			},
			wantErr: "project_name_prefix"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("QM_PROJECT_ID", "")
			t.Setenv("QM_WORKSPACE", "")
			if tc.tied {
				tied(t)
			}
			repo, code, out, errs := prefixDry(t, tc.in, tc.config)
			if tc.wantErr != "" {
				if code != 1 || !strings.Contains(errs, tc.wantErr) {
					t.Fatalf("exit %d, stderr %q, want an error naming %q", code, errs, tc.wantErr)
				}
				return
			}
			if want := "\nname: " + tc.want(repo) + "\n"; code != 0 || !strings.Contains(out, want) {
				t.Fatalf("exit %d, stderr %q, want %q in:\n%s", code, errs, want, out)
			}
		})
	}
}

func TestUpDoneBareAndFullName(t *testing.T) {
	for _, arg := range []string{"w1", "proj-w1"} {
		t.Run(arg, func(t *testing.T) {
			m := newUpMachine(t)
			m.worker(t, m.project(t), "proj-w1")
			var out, errb bytes.Buffer
			if code := run([]string{"up", "done", arg}, &out, &errb); code != 0 {
				t.Fatalf("exit %d, stderr %q", code, errb.String())
			}
			if _, ok := loadProject(t, m.root).Done[upWorkerID]; !ok {
				t.Errorf("proj-w1 not marked done")
			}
		})
	}
}

func TestUpDoneConfiguredPrefix(t *testing.T) {
	m := newUpMachine(t)
	writeConfig(t, `{"project_name_prefix":{"`+m.root+`":"ut"}}`)
	m.worker(t, m.project(t), "ut-w1")
	var out, errb bytes.Buffer
	if code := run([]string{"up", "done", "w1"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if _, ok := loadProject(t, m.root).Done[upWorkerID]; !ok {
		t.Errorf("ut-w1 not marked done")
	}
}

func TestUpDoneAmbiguousBareName(t *testing.T) {
	m := newUpMachine(t)
	p := m.project(t)
	m.worker(t, p, "proj-w1")
	m.worker2(t, p, "w1")
	var out, errb bytes.Buffer
	code := run([]string{"up", "done", "w1"}, &out, &errb)
	if code != 2 || !strings.Contains(errb.String(), "proj-w1 and w1") {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if done := loadProject(t, m.root).Done; len(done) != 0 {
		t.Errorf("done = %v, want nothing marked", done)
	}
	// The full names say which is meant.
	errb.Reset()
	if code := run([]string{"up", "done", "proj-w1"}, &out, &errb); code != 0 {
		t.Errorf("full name: exit %d, stderr %q", code, errb.String())
	}
}

func TestFinishBareName(t *testing.T) {
	tests := []struct {
		name, worker, base string
		config             func(m *finishMachine) string
		arg                string
	}{
		{name: "bare, by the stored name", worker: "proj-w1", base: "w1", arg: "w1"},
		{name: "full", worker: "proj-w1", base: "w1", arg: "proj-w1"},
		{name: "bare, no stored name: the workspace prefix", worker: "proj-w1", arg: "w1"},
		{name: "bare, no stored name: the configured prefix", worker: "ut-w1", arg: "w1",
			config: func(m *finishMachine) string { return `{"project_name_prefix":{"` + m.root + `":"ut"}}` }},
		{name: "bare, the prefix changed after the spawn", worker: "old-w1", base: "w1", arg: "w1",
			config: func(m *finishMachine) string { return `{"project_name_prefix":{"` + m.root + `":"new"}}` }},
		{name: "unprefixed worker of an earlier qm", worker: "w1", arg: "w1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := newFinishMachineNamed(t, tc.worker, tc.base)
			if tc.config != nil {
				writeConfig(t, tc.config(m))
			}
			code, out, errs := runQM("finish", "--dry-run", tc.arg)
			if code != 0 || !strings.Contains(out, "would mark done") || !strings.Contains(out, tc.worker) {
				t.Fatalf("exit %d, err %q, out:\n%s", code, errs, out)
			}
			if code, _, errs := runQM("finish", "--dry-run", "nobody"); code != 2 || !strings.Contains(errs, `no worker "nobody"`) {
				t.Errorf("unknown: exit %d, err %q", code, errs)
			}
		})
	}
}

func TestFinishAmbiguousBareName(t *testing.T) {
	m := newFinishMachineNamed(t, "proj-w1", "w1")
	m.worker2(t, loadProject(t, m.root), "w1")
	code, _, errs := runQM("finish", "--dry-run", "w1")
	if code != 2 || !strings.Contains(errs, "proj-w1 and w1") {
		t.Errorf("exit %d, err %q", code, errs)
	}
}

// An old worker of the bare name that is finished does not make it ambiguous.
func TestFinishBareNameSetsAsideAFinishedWorker(t *testing.T) {
	m := newFinishMachineNamed(t, "proj-w1", "w1")
	p := loadProject(t, m.root)
	m.worker2(t, p, "w1")
	p.Done = map[string]time.Time{upWorker2ID: time.Now()}
	if err := up.Save(p); err != nil {
		t.Fatal(err)
	}
	code, out, errs := runQM("finish", "--dry-run", "w1")
	if code != 0 || !strings.Contains(out, "proj-w1") {
		t.Fatalf("exit %d, err %q, out:\n%s", code, errs, out)
	}
}

func TestUpDoneBareNameSetsAsideAFinishedWorker(t *testing.T) {
	m := newUpMachine(t)
	p := m.project(t)
	m.worker(t, p, "proj-w1")
	m.worker2(t, p, "w1")
	p.Done = map[string]time.Time{upWorker2ID: time.Now()}
	if err := up.Save(p); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := run([]string{"up", "done", "w1"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if _, ok := loadProject(t, m.root).Done[upWorkerID]; !ok {
		t.Errorf("proj-w1 not marked done")
	}
}

// A config that does not read: a note, and only an exact full name works.
func TestUpDoneUnreadableConfigTakesFullNamesOnly(t *testing.T) {
	m := newUpMachine(t)
	m.worker(t, m.project(t), "proj-w1")
	writeConfig(t, `{not json`)
	var out, errb bytes.Buffer
	code := run([]string{"up", "done", "w1"}, &out, &errb)
	if code != 2 || !strings.Contains(errb.String(), "qm up: note: the config does not read") {
		t.Fatalf("bare: exit %d, out %q, stderr %q", code, out.String(), errb.String())
	}
	if _, ok := loadProject(t, m.root).Done[upWorkerID]; ok {
		t.Fatalf("a bare name was guessed")
	}
	out.Reset()
	errb.Reset()
	if code := run([]string{"up", "done", "proj-w1"}, &out, &errb); code != 0 {
		t.Fatalf("full: exit %d, out %q, stderr %q", code, out.String(), errb.String())
	}
}

func TestUpDoneUnreadableConfigStoredBaseStillResolves(t *testing.T) {
	m := newUpMachine(t)
	m.workerBase(t, m.project(t), "proj-w1", "w1")
	writeConfig(t, `{not json`)
	var out, errb bytes.Buffer
	if code := run([]string{"up", "done", "w1"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, out %q, stderr %q", code, out.String(), errb.String())
	}
	if _, ok := loadProject(t, m.root).Done[upWorkerID]; !ok {
		t.Fatalf("not marked done")
	}
}

func TestSpawnWorkerNotesUnreadProject(t *testing.T) {
	_, code, _, errs := prefixDry(t, "w1", func(string) string {
		t.Setenv("QM_PROJECT_ID", "nostate-00000000")
		t.Setenv("QM_WORKSPACE", "try")
		return `{}`
	})
	if code != 0 || !strings.Contains(errs, "qm spawn: note: project nostate-00000000 not read") {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
}

func TestSpawnWorkerPrefixFromTheTiedProjectsEntry(t *testing.T) {
	tiedRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// The worker's folder is another repository, with an entry of its own. The
	// project is saved once prefixDry has made its hermetic state.
	_, code, out, errs := prefixDry(t, "w1", func(repo string) string {
		p := up.Project{ID: up.ProjectID(tiedRoot), Root: tiedRoot, Workspace: "try", Launcher: "agterm"}
		if err := up.Save(p); err != nil {
			t.Fatal(err)
		}
		t.Setenv("QM_PROJECT_ID", p.ID)
		t.Setenv("QM_WORKSPACE", "try")
		return `{"project_name_prefix":{"` + tiedRoot + `":"ut","` + repo + `":"other"}}`
	})
	if code != 0 || !strings.Contains(out, "\nname: ut-w1\n") {
		t.Fatalf("exit %d, stderr %q, out:\n%s", code, errs, out)
	}
}

func TestSpawnWorkerBadNamePrefixIsRefusedWhateverTheEntry(t *testing.T) {
	_, code, _, errs := prefixDry(t, "w1", func(repo string) string {
		return `{"worker":{"name_prefix":"short"},"project_name_prefix":{"` + repo + `":"ut"}}`
	})
	if code != 1 || !strings.Contains(errs, "worker.name_prefix") {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
}

func TestWatchBareName(t *testing.T) {
	m := newUpMachine(t)
	m.worker(t, m.project(t), "proj-w")
	liveWorker(t)
	turnHook(t, "turn-start", "UserPromptSubmit")
	fakeClock(t, func(n int) {
		if n == 2 {
			turnHook(t, "turn-stop", "Stop")
		}
	})
	var out, errb bytes.Buffer
	if code := run([]string{"watch", "--silent", "100000h", "--interval", "1s", "w"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if !strings.Contains(out.String(), "proj-w stopped ") {
		t.Fatalf("out:\n%s", out.String())
	}
}

func TestWatchBareNameSetsAsideAWorkerWhoseFolderIsGone(t *testing.T) {
	m := newUpMachine(t)
	p := m.project(t)
	m.worker(t, p, "proj-w")
	m.worker2At(t, p, "w", filepath.Join(t.TempDir(), "gone"))
	liveWorker(t)
	turnHook(t, "turn-start", "UserPromptSubmit")
	fakeClock(t, func(n int) {
		if n == 2 {
			turnHook(t, "turn-stop", "Stop")
		}
	})
	var out, errb bytes.Buffer
	if code := run([]string{"watch", "--silent", "100000h", "--interval", "1s", "w"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if !strings.Contains(out.String(), "proj-w stopped ") {
		t.Fatalf("out:\n%s", out.String())
	}
}

func TestWatchAmbiguousBareName(t *testing.T) {
	m := newUpMachine(t)
	p := m.project(t)
	m.worker(t, p, "proj-w")
	m.worker2(t, p, "w")
	liveWorker(t)
	var out, errb bytes.Buffer
	code := run([]string{"watch", "--silent", "100000h", "w"}, &out, &errb)
	if code != 2 || !strings.Contains(errb.String(), "proj-w and w") {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
}

func TestWatchBareNameByConfiguredPrefixAndStoredName(t *testing.T) {
	for name, tc := range map[string]struct{ worker, config string }{
		"configured prefix": {"ut-w", `{"project_name_prefix":{"@ROOT@":"ut"}}`},
		"prefix changed":    {"old-w", `{"project_name_prefix":{"@ROOT@":"new"}}`},
	} {
		t.Run(name, func(t *testing.T) {
			m := newUpMachine(t)
			p := m.project(t)
			base := ""
			if name == "prefix changed" {
				base = "w"
			}
			m.workerBase(t, p, tc.worker, base)
			writeConfig(t, strings.ReplaceAll(tc.config, "@ROOT@", m.root))
			liveWorker(t)
			turnHook(t, "turn-start", "UserPromptSubmit")
			fakeClock(t, func(n int) {
				if n == 2 {
					turnHook(t, "turn-stop", "Stop")
				}
			})
			var out, errb bytes.Buffer
			if code := run([]string{"watch", "--silent", "100000h", "--interval", "1s", "w"}, &out, &errb); code != 0 {
				t.Fatalf("exit %d, stderr %q", code, errb.String())
			}
			if !strings.Contains(out.String(), tc.worker+" stopped ") {
				t.Fatalf("out:\n%s", out.String())
			}
		})
	}
}
