package spawn

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/config"
	"github.com/IZZY-Agency/quartermaster/internal/declog"
	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/limits"
	"github.com/IZZY-Agency/quartermaster/internal/reviewin"
	"github.com/IZZY-Agency/quartermaster/internal/route"
)

// hermetic points every folder qm and Claude Code read at temporary ones and
// gives qm-worker a folder of its own; it returns the Claude folder.
func hermetic(t *testing.T) (claudeDir string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	claudeDir = filepath.Join(home, "claude")
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	t.Setenv("TYPESAFE_API_KEY", "")
	worker := filepath.Join(home, "qm-worker")
	mkdir(t, worker)
	t.Setenv("QM_WORKER_PLUGIN", worker)
	old := spawnerPath
	t.Cleanup(func() { spawnerPath = old })
	spawnerPath = func() string { return "" }
	return claudeDir
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, text string) string {
	t.Helper()
	mkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	base := []string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}
	if out, err := exec.Command("git", append(base, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// repoWith makes a repository on branch main holding files, committed.
func repoWith(t *testing.T, files ...string) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	for _, f := range files {
		writeFile(t, filepath.Join(dir, f), "x\n")
	}
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "first")
	return dir
}

// fakeLauncher records the sessions it is asked to start.
type fakeLauncher struct{ specs []launch.LaunchSpec }

func (f *fakeLauncher) Spawn(_ context.Context, s launch.LaunchSpec) (launch.SessionRef, error) {
	f.specs = append(f.specs, s)
	return launch.SessionRef{Launcher: "fake", ID: "s-1", Name: s.Name}, nil
}

// testDeps are Deps with Jev off, no Codex, no coordinator, and l as the
// launcher whatever the choice.
func testDeps(l launch.Launcher) Deps {
	return Deps{
		Config:  config.Defaults(),
		Claude:  limits.ClaudeState{},
		Codex:   func() limits.CodexState { return limits.CodexState{Reason: "not installed"} },
		Getenv:  func(string) string { return "" },
		PS:      func(int) (string, int, error) { return "", 0, errors.New("no ps in tests") },
		ProcExe: func(int) (string, error) { return "", errors.New("no executables in tests") },
		Pick: func(string, func(string) string) (launch.Launcher, error) {
			return l, nil
		},
		Exe: "/nonexistent/bin/qm",
	}
}

func request(t *testing.T, repo, typ string) WorkerRequest {
	t.Helper()
	brief := writeFile(t, filepath.Join(t.TempDir(), "brief.md"), "Rename the helper in one file.\n")
	return WorkerRequest{
		Brief: brief, Type: typ, Size: "S", Name: "task-a", Cwd: repo,
		Hint: &route.Tier{Model: "sonnet", Effort: "medium"},
	}
}

func records(t *testing.T) []route.Record {
	t.Helper()
	recs, err := declog.Read(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func launchDirEntries(t *testing.T) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(os.Getenv("XDG_STATE_HOME"), "quartermaster", "launch"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return entries
}

func TestReviewTypeUsesReviewTable(t *testing.T) {
	hermetic(t)
	repo := repoWith(t, "README.md")
	git(t, repo, "update-ref", "refs/remotes/origin/main", "main")
	git(t, repo, "checkout", "-q", "-b", "feature")
	writeFile(t, filepath.Join(repo, "notes.txt"), "one\ntwo\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "second")
	top, branch, err := reviewin.Repo(repo)
	if err != nil {
		t.Fatal(err)
	}
	// One review already logged for this branch: the next one is round 2.
	if err := declog.Append(route.Record{Time: time.Now().UTC(), Kind: "review", Project: top, Branch: branch}); err != nil {
		t.Fatal(err)
	}

	l := &fakeLauncher{}
	req := request(t, repo, "review")
	req.Hint = &route.Tier{Model: "opus", Effort: "xhigh"}
	res, err := Worker(context.Background(), req, testDeps(l))
	if err != nil {
		t.Fatal(err)
	}
	d := res.Decision
	if d.Kind != "review" || d.Row != "fix-check" || d.Tier != (route.Tier{Model: "sonnet", Effort: "medium"}) {
		t.Fatalf("decision = %s %s %+v, want review fix-check sonnet/medium", d.Kind, d.Row, d.Tier)
	}
	if len(l.specs) != 1 {
		t.Fatalf("launched %d sessions, want 1", len(l.specs))
	}
	if !strings.Contains(res.Script, "--model claude-sonnet-5-5 --effort medium") {
		t.Errorf("script does not run the review tier:\n%s", res.Script)
	}
	recs := records(t)
	if len(recs) != 3 || recs[1].Kind != "review" || recs[1].Round != 2 || recs[1].Name != "task-a" || recs[1].Branch != "feature" {
		t.Fatalf("records = %+v, want a second review record, round 2", recs)
	}
	if recs[1].Detail != `{"verdict":"session","p1":0,"p2":0,"p3":0}` {
		t.Errorf("review detail = %q, want the review JSON with verdict session", recs[1].Detail)
	}
	// The reviewer session is a worker too, logged after its round.
	w := recs[2]
	if w.Kind != "worker" || w.Type != "review" || w.ClaudeSession != res.SessionID || !w.Time.After(recs[1].Time) {
		t.Errorf("worker record = %+v, want kind worker, type review, the session id, after the review", w)
	}
}

func TestWorkerLaunchesAndLogs(t *testing.T) {
	hermetic(t)
	repo := repoWith(t, "README.md")
	l := &fakeLauncher{}
	res, err := Worker(context.Background(), request(t, repo, "docs"), testDeps(l))
	if err != nil {
		t.Fatal(err)
	}
	if len(l.specs) != 1 || l.specs[0].Name != "task-a" || l.specs[0].Script != res.ScriptPath {
		t.Fatalf("launched %+v, want one session running %s", l.specs, res.ScriptPath)
	}
	b, err := os.ReadFile(res.ScriptPath)
	if err != nil || string(b) != res.Script {
		t.Fatalf("script file: %v, %q", err, b)
	}
	recs := records(t)
	if len(recs) != 1 || recs[0].Kind != "worker" || recs[0].Decision == nil || recs[0].Decision.Profile == nil {
		t.Fatalf("records = %+v, want one worker record with its profile", recs)
	}
	if recs[0].Type != "docs" || recs[0].Size != "S" || recs[0].Name != "task-a" || recs[0].Branch != "main" {
		t.Errorf("record = %+v", recs[0])
	}
	// The terminal session is logged, so qm down closes it and qm up pins it.
	if recs[0].TermLauncher != "fake" || recs[0].TermID != "s-1" {
		t.Errorf("record term %q %q, want fake s-1", recs[0].TermLauncher, recs[0].TermID)
	}
	// The worker is launched with a session id of qm's, logged with it.
	if !uuidV4.MatchString(res.SessionID) || recs[0].ClaudeSession != res.SessionID ||
		!strings.Contains(res.Script, " -n task-a --session-id "+res.SessionID+" ") {
		t.Errorf("session id %q, record %q, script:\n%s", res.SessionID, recs[0].ClaudeSession, res.Script)
	}
	if !slices.Contains(res.Decision.Reasons, NoCoordinator) {
		t.Errorf("reasons %q do not say the worker will prompt", res.Decision.Reasons)
	}
}

func profileFor(t *testing.T, model, effort string, perm []string) (route.Profile, []string) {
	t.Helper()
	repo := repoWith(t, "README.md")
	d := route.Decision{Kind: "worker", Tier: route.Tier{Model: model, Effort: effort}}
	return BuildProfile(d, repo, config.Defaults(), nil, perm, "/plugins/qm-worker")
}

func TestScriptSonnet(t *testing.T) {
	hermetic(t)
	p, _ := profileFor(t, "sonnet", "medium", nil)
	s := LaunchScript(p, route.Tier{Model: "sonnet", Effort: "medium"}, "task-a", "", "/work/repo", "/briefs/a.md", "")
	for _, want := range []string{
		"#!/bin/sh\n",
		"\ncd /work/repo || exit 1\n",
		"exec env QM_ROLE=worker QM_SESSION_NAME=task-a QM_BRIEF=/briefs/a.md QM_AUTOCOMPACT=250000 BASH_MAX_OUTPUT_LENGTH=20000 CLAUDE_CODE_FILE_READ_MAX_OUTPUT_TOKENS=12000 claude ",
		"claude --model claude-sonnet-5-5 --effort medium --autocompact 250000 --strict-mcp-config --settings ",
		" --plugin-dir /plugins/qm-worker -n task-a '\nYou are a worker started by qm. Your task is the file /briefs/a.md: first read all of it yourself, with Read. It is your task, not material to delegate.'\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "QM_LSP") || strings.Contains(s, "--mcp-config") {
		t.Errorf("script loads an LSP or MCP config it was not given:\n%s", s)
	}

	p, _ = profileFor(t, "opus", "high", nil)
	s = LaunchScript(p, route.Tier{Model: "opus", Effort: "high"}, "task-b", "", "/work/repo", "/briefs/b.md", "")
	if !strings.Contains(s, "--model claude-opus-5-5 --effort high --autocompact 400000") {
		t.Errorf("opus script:\n%s", s)
	}
}

func TestScriptQuotesAndPath(t *testing.T) {
	hermetic(t)
	p, _ := profileFor(t, "sonnet", "low", nil)
	p.MCPConfig = "/cfg/mcp servers.json"
	old := spawnerPath
	t.Cleanup(func() { spawnerPath = old })
	spawnerPath = func() string { return "/opt/homebrew/bin:/usr/bin" }
	s := LaunchScript(p, route.Tier{Model: "sonnet", Effort: "low"}, "task-a", "", "/work/my repo", "/briefs/it's.md", "/opt/qm tools/bin")
	for _, want := range []string{
		"PATH='/opt/qm tools/bin:/opt/homebrew/bin:/usr/bin':\"$PATH\"\nexport PATH\n",
		"cd '/work/my repo' || exit 1",
		"--strict-mcp-config --mcp-config '/cfg/mcp servers.json' --settings",
		`'QM_BRIEF=/briefs/it'\''s.md' `,
		`Your task is the file /briefs/it'\''s.md: `,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q:\n%s", want, s)
		}
	}
}

func TestScriptBypassMirrors(t *testing.T) {
	hermetic(t)
	for _, perm := range [][]string{
		{"--dangerously-skip-permissions"},
		{"--permission-mode", "acceptEdits"},
	} {
		p, _ := profileFor(t, "sonnet", "medium", perm)
		s := LaunchScript(p, route.Tier{Model: "sonnet", Effort: "medium"}, "task-a", "", "/w", "/b.md", "")
		if want := " claude " + strings.Join(perm, " ") + " --model "; !strings.Contains(s, want) {
			t.Errorf("script lacks %q:\n%s", want, s)
		}
	}
	p, _ := profileFor(t, "sonnet", "medium", nil)
	s := LaunchScript(p, route.Tier{Model: "sonnet", Effort: "medium"}, "task-a", "", "/w", "/b.md", "")
	if strings.Contains(s, "--dangerously") || strings.Contains(s, "--permission-mode") || !strings.Contains(s, " claude --model ") {
		t.Errorf("no coordinator flag, yet the script has one:\n%s", s)
	}
}

// settingsOf returns the enabledPlugins of the script's --settings JSON.
func settingsOf(t *testing.T, script string) map[string]bool {
	t.Helper()
	_, rest, ok := strings.Cut(script, "--settings '")
	js, _, ok2 := strings.Cut(rest, "'")
	if !ok || !ok2 {
		t.Fatalf("no --settings in:\n%s", script)
	}
	var s struct {
		EnabledPlugins map[string]bool `json:"enabledPlugins"`
	}
	if err := json.Unmarshal([]byte(js), &s); err != nil {
		t.Fatalf("settings %s: %v", js, err)
	}
	return s.EnabledPlugins
}

func TestSettingsDisablesCoordinator(t *testing.T) {
	hermetic(t)
	repo := repoWith(t, "README.md")
	cfg := config.Defaults()
	cfg.Worker.Plugins = []string{"superpowers"}
	cfg.ProjectPlugins[repo] = []string{"figma@example-market"}
	user := map[string]bool{
		"superpowers@claude-plugins-official": true,
		"other@example-market":                true,
		"figma@example-market":                false,
		"qm-coordinator@quartermaster":        true,
	}
	d := route.Decision{Kind: "worker", Tier: route.Tier{Model: "sonnet", Effort: "medium"}}
	p, _ := BuildProfile(d, repo, cfg, user, nil, "/plugins/qm-worker")
	got := settingsOf(t, LaunchScript(p, d.Tier, "task-a", "", "/w", "/b.md", ""))
	want := map[string]bool{
		"superpowers@claude-plugins-official": true,
		"other@example-market":                false,
		"figma@example-market":                true,
		"qm-coordinator@quartermaster":        false,
	}
	if len(got) != len(want) {
		t.Errorf("enabledPlugins = %v, want %v", got, want)
	}
	for k, v := range want {
		if on, ok := got[k]; !ok || on != v {
			t.Errorf("enabledPlugins[%s] = %v (set %v), want %v", k, on, ok, v)
		}
	}

	// With nothing enabled by the user, the coordinator is still switched off.
	p, _ = BuildProfile(d, repo, config.Defaults(), nil, nil, "/plugins/qm-worker")
	if got := settingsOf(t, LaunchScript(p, d.Tier, "task-a", "", "/w", "/b.md", "")); len(got) != 1 || got["qm-coordinator@quartermaster"] {
		t.Errorf("enabledPlugins = %v, want only the coordinator, off", got)
	}
}

// installPlugins writes Claude Code's installed_plugins.json with keys
// installed at user scope.
func installPlugins(t *testing.T, claudeDir string, keys ...string) {
	t.Helper()
	plugins := map[string]any{}
	for _, k := range keys {
		plugins[k] = []map[string]string{{"scope": "user"}}
	}
	b, err := json.Marshal(map[string]any{"version": 2, "plugins": plugins})
	if err != nil {
		t.Fatal(err)
	}
	mkdir(t, filepath.Join(claudeDir, "plugins"))
	if err := os.WriteFile(filepath.Join(claudeDir, "plugins", "installed_plugins.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLSPByMajority(t *testing.T) {
	claudeDir := hermetic(t)
	installPlugins(t, claudeDir, "gopls-lsp@claude-plugins-official", "typescript-lsp@claude-plugins-official")
	repo := repoWith(t, "a.go", "b.go", "cmd/c.go", "web/app.ts", "README.md")
	d := route.Decision{Kind: "worker", Tier: route.Tier{Model: "sonnet", Effort: "medium"}}
	p, _ := BuildProfile(d, repo, config.Defaults(), nil, nil, "/plugins/qm-worker")
	// Turned on through enabledPlugins; no --plugin-dir for it.
	if want := []string{"/plugins/qm-worker"}; !slices.Equal(p.PluginDirs, want) {
		t.Errorf("plugin dirs = %q, want %q", p.PluginDirs, want)
	}
	if p.Env["QM_LSP"] != "go" || !slices.Contains(p.PluginsOn, "gopls-lsp@claude-plugins-official") || slices.Contains(p.PluginsOn, "typescript-lsp@claude-plugins-official") {
		t.Errorf("env %v, plugins on %q: want QM_LSP=go and gopls-lsp only", p.Env, p.PluginsOn)
	}
	s := LaunchScript(p, d.Tier, "task-a", "", "/w", "/b.md", "")
	if !strings.Contains(s, "QM_AUTOCOMPACT=250000 QM_LSP=go BASH_MAX_OUTPUT_LENGTH") || !settingsOf(t, s)["gopls-lsp@claude-plugins-official"] {
		t.Errorf("script does not turn gopls-lsp on:\n%s", s)
	}
	// Already on in the user's settings: still on, once.
	p, _ = BuildProfile(d, repo, config.Defaults(), map[string]bool{"gopls-lsp@claude-plugins-official": true}, nil, "/plugins/qm-worker")
	if n := strings.Count(strings.Join(p.PluginsOn, " "), "gopls-lsp"); n != 1 {
		t.Errorf("plugins on %q: want gopls-lsp once", p.PluginsOn)
	}
}

func TestLSPMissingNotLoaded(t *testing.T) {
	claudeDir := hermetic(t)
	// A marketplace folder for the plugin is not an install.
	mkdir(t, filepath.Join(claudeDir, "plugins", "marketplaces", "claude-plugins-official", "plugins", "gopls-lsp"))
	installPlugins(t, claudeDir, "pyright-lsp@claude-plugins-official")
	repo := repoWith(t, "a.go", "b.go", "c.py")
	d := route.Decision{Kind: "worker", Tier: route.Tier{Model: "sonnet", Effort: "medium"}}
	p, notes := BuildProfile(d, repo, config.Defaults(), nil, nil, "/plugins/qm-worker")
	if !slices.Equal(p.PluginDirs, []string{"/plugins/qm-worker"}) || p.Env["QM_LSP"] != "" || slices.ContainsFunc(p.PluginsOn, func(k string) bool { return strings.Contains(k, "-lsp") }) {
		t.Errorf("profile %+v loads an LSP plugin that is not installed", p)
	}
	if !slices.ContainsFunc(notes, func(n string) bool {
		return strings.Contains(n, "gopls-lsp not loaded") && strings.Contains(n, "/plugin install gopls-lsp@claude-plugins-official")
	}) {
		t.Errorf("notes %q do not say why gopls-lsp is not loaded and how to install it", notes)
	}
	if s := LaunchScript(p, d.Tier, "task-a", "", "/w", "/b.md", ""); strings.Contains(s, "QM_LSP") {
		t.Errorf("script sets QM_LSP:\n%s", s)
	}
}

func TestInstalledPluginsScopes(t *testing.T) {
	claudeDir := hermetic(t)
	mkdir(t, filepath.Join(claudeDir, "plugins"))
	b := `{"version":2,"plugins":{
		"a@m":[{"scope":"user"}],
		"b@m":[{"scope":"project","projectPath":"/work/repo"}],
		"c@m":[{"scope":"local","projectPath":"/work/other"}]}}`
	if err := os.WriteFile(filepath.Join(claudeDir, "plugins", "installed_plugins.json"), []byte(b), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := InstalledPlugins("/work/repo")
	if err != nil || !got["a@m"] || !got["b@m"] || got["c@m"] {
		t.Errorf("installed = %v, %v; want a and b", got, err)
	}
}

func TestDryRunDoesNotLaunch(t *testing.T) {
	hermetic(t)
	repo := repoWith(t, "README.md")
	l := &fakeLauncher{}
	req := request(t, repo, "fix")
	req.DryRun = true
	res, err := Worker(context.Background(), req, testDeps(l))
	if err != nil {
		t.Fatal(err)
	}
	if len(l.specs) != 0 || res.ScriptPath != "" || !strings.HasPrefix(res.Script, "#!/bin/sh\n") {
		t.Errorf("dry run launched %d, wrote %q, script %q", len(l.specs), res.ScriptPath, res.Script)
	}
	if recs := records(t); len(recs) != 0 {
		t.Errorf("dry run logged %+v", recs)
	}
	if e := launchDirEntries(t); len(e) != 0 {
		t.Errorf("dry run wrote %d launch scripts", len(e))
	}

	// A dry run needs no launcher: it says none was found.
	deps := testDeps(l)
	deps.Pick = launch.Pick
	res, err = Worker(context.Background(), req, deps)
	if err != nil {
		t.Fatal(err)
	}
	if res.Launcher != nil || !strings.Contains(res.LauncherNote, "none found") {
		t.Errorf("launcher %v, note %q: want none found", res.Launcher, res.LauncherNote)
	}
}

func TestNoLauncherClearError(t *testing.T) {
	hermetic(t)
	repo := repoWith(t, "README.md")
	deps := testDeps(nil)
	deps.Pick = launch.Pick
	_, err := Worker(context.Background(), request(t, repo, "fix"), deps)
	if !errors.Is(err, launch.ErrNoLauncher) {
		t.Fatalf("err = %v, want ErrNoLauncher", err)
	}
	for _, w := range []string{"agterm", "tmux"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error %q does not name %s", err, w)
		}
	}
	if recs := records(t); len(recs) != 0 {
		t.Errorf("logged %+v", recs)
	}
	if e := launchDirEntries(t); len(e) != 0 {
		t.Errorf("wrote %d launch scripts", len(e))
	}
}

func TestBadRequest(t *testing.T) {
	hermetic(t)
	repo := repoWith(t, "README.md")
	for name, mod := range map[string]func(*WorkerRequest){
		"no hint":           func(r *WorkerRequest) { r.Hint = nil },
		"name with slash":   func(r *WorkerRequest) { r.Name = "a/b" },
		"empty name":        func(r *WorkerRequest) { r.Name = "" },
		"cwd missing":       func(r *WorkerRequest) { r.Cwd = filepath.Join(repo, "nope") },
		"force without why": func(r *WorkerRequest) { r.Force = &route.Tier{Model: "opus", Effort: "high"} },
		"why without force": func(r *WorkerRequest) { r.ForceWhy = "because" },
		"bad size":          func(r *WorkerRequest) { r.Size = "XL" },
		"no cwd":            func(r *WorkerRequest) { r.Cwd = "" },
		"brief missing":     func(r *WorkerRequest) { r.Brief = filepath.Join(repo, "nope.md") },
		"bad hint model":    func(r *WorkerRequest) { r.Hint = &route.Tier{Model: "gpt", Effort: "high"} },
	} {
		t.Run(name, func(t *testing.T) {
			l := &fakeLauncher{}
			req := request(t, repo, "fix")
			mod(&req)
			if _, err := Worker(context.Background(), req, testDeps(l)); !errors.Is(err, ErrInput) {
				t.Errorf("err = %v, want ErrInput", err)
			}
			if len(l.specs) != 0 || len(records(t)) != 0 {
				t.Error("launched or logged on bad input")
			}
		})
	}
}

func TestScriptNamesUnique(t *testing.T) {
	hermetic(t)
	seen := map[string]bool{}
	for range 3 {
		p, err := WriteScript("task-a", "#!/bin/sh\n")
		if err != nil {
			t.Fatal(err)
		}
		if seen[p] {
			t.Fatalf("path %s reused", p)
		}
		seen[p] = true
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o700 || filepath.Base(filepath.Dir(p)) != "launch" || !strings.HasPrefix(filepath.Base(p), "task-a-") {
			t.Errorf("script %s mode %v", p, fi.Mode().Perm())
		}
	}
	if e := launchDirEntries(t); len(e) != 3 {
		t.Errorf("%d scripts, want 3", len(e))
	}
}

func TestWorkerPluginDirLookup(t *testing.T) {
	t.Setenv("QM_WORKER_PLUGIN", "")
	real := func(t *testing.T, p string) string {
		t.Helper()
		r, err := filepath.EvalSymlinks(p)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	t.Run("share layout through a symlinked bin", func(t *testing.T) {
		prefix := t.TempDir()
		share := filepath.Join(prefix, "share", "quartermaster", "plugins", "qm-worker")
		mkdir(t, share)
		exe := writeFile(t, filepath.Join(prefix, "bin", "qm"), "")
		links := t.TempDir()
		link := filepath.Join(links, "qm")
		if err := os.Symlink(exe, link); err != nil {
			t.Fatal(err)
		}
		got, err := WorkerPluginDir(link)
		if err != nil || got != real(t, share) {
			t.Errorf("got %q, %v; want %s", got, err, share)
		}
	})
	t.Run("repository layout", func(t *testing.T) {
		repo := t.TempDir()
		plug := filepath.Join(repo, "plugins", "qm-worker")
		mkdir(t, plug)
		exe := writeFile(t, filepath.Join(repo, "bin", "qm"), "")
		got, err := WorkerPluginDir(exe)
		if err != nil || got != real(t, plug) {
			t.Errorf("got %q, %v; want %s", got, err, plug)
		}
	})
	t.Run("unpacked release archive", func(t *testing.T) {
		dir := t.TempDir()
		share := filepath.Join(dir, "share", "quartermaster", "plugins", "qm-worker")
		mkdir(t, share)
		exe := writeFile(t, filepath.Join(dir, "qm"), "")
		got, err := WorkerPluginDir(exe)
		if err != nil || got != real(t, share) {
			t.Errorf("got %q, %v; want %s", got, err, share)
		}
	})
	t.Run("neither", func(t *testing.T) {
		dir := t.TempDir()
		exe := writeFile(t, filepath.Join(dir, "bin", "qm"), "")
		_, err := WorkerPluginDir(exe)
		if err == nil {
			t.Fatal("want an error")
		}
		for _, p := range []string{"share/quartermaster/plugins/qm-worker", "plugins/qm-worker"} {
			if !strings.Contains(err.Error(), filepath.Join(real(t, dir), p)) {
				t.Errorf("error %q does not list %s", err, p)
			}
		}
	})
	t.Run("QM_WORKER_PLUGIN", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("QM_WORKER_PLUGIN", dir)
		if got, err := WorkerPluginDir("/nonexistent/qm"); err != nil || got != dir {
			t.Errorf("got %q, %v; want %s", got, err, dir)
		}
		t.Setenv("QM_WORKER_PLUGIN", filepath.Join(dir, "missing"))
		if _, err := WorkerPluginDir("/nonexistent/qm"); err == nil {
			t.Error("a missing QM_WORKER_PLUGIN folder: want an error")
		}
	})
}

func TestReviewSessionDropsCodex(t *testing.T) {
	hermetic(t)
	repo := repoWith(t, "README.md")
	git(t, repo, "update-ref", "refs/remotes/origin/main", "main")
	git(t, repo, "checkout", "-q", "-b", "feature")
	deps := testDeps(&fakeLauncher{})
	deps.Codex = func() limits.CodexState { return limits.CodexState{Available: true, UsageKnown: true, UsedPercent: 10} }
	res, err := Worker(context.Background(), request(t, repo, "review"), deps)
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision.Codex != nil || !slices.Contains(res.Decision.Reasons, ReasonReviewerSession) {
		t.Errorf("decision codex %v, reasons %q: want no Codex tier and the reason", res.Decision.Codex, res.Decision.Reasons)
	}
	recs := records(t)
	if len(recs) != 2 || recs[0].Decision == nil || recs[0].Decision.Codex != nil || recs[1].Decision == nil || recs[1].Decision.Codex != nil {
		t.Fatalf("records = %+v, want a review and a worker record without a Codex tier", recs)
	}
}

// failingLauncher fails every launch.
type failingLauncher struct{}

func (failingLauncher) Spawn(context.Context, launch.LaunchSpec) (launch.SessionRef, error) {
	return launch.SessionRef{}, errors.New("launcher down")
}

func TestSpawnFailureRemovesScript(t *testing.T) {
	hermetic(t)
	repo := repoWith(t, "README.md")
	res, err := Worker(context.Background(), request(t, repo, "fix"), testDeps(failingLauncher{}))
	if err == nil || !strings.Contains(err.Error(), "launcher down") {
		t.Fatalf("err = %v, want the launcher's error", err)
	}
	if res.ScriptPath != "" || len(launchDirEntries(t)) != 0 || len(records(t)) != 0 {
		t.Errorf("a failed launch left script %q, %d files, %d records", res.ScriptPath, len(launchDirEntries(t)), len(records(t)))
	}
}

func TestScriptPathFromExe(t *testing.T) {
	hermetic(t)
	repo := repoWith(t, "README.md")
	dir := t.TempDir()
	exe := writeFile(t, filepath.Join(dir, "bin", "qm"), "")
	deps := testDeps(&fakeLauncher{})
	deps.Exe = exe
	req := request(t, repo, "docs")
	req.DryRun = true
	res, err := Worker(context.Background(), req, deps)
	if err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(filepath.Dir(exe))
	if want := "PATH=" + launch.Quote(real) + ":"; !strings.Contains(res.Script, want) {
		t.Errorf("script lacks %q:\n%s", want, res.Script)
	}
}

func TestDroppedWorkerPluginNoted(t *testing.T) {
	hermetic(t)
	repo := repoWith(t, "README.md")
	cfg := config.Defaults()
	cfg.Worker.Plugins = []string{"superpowers", "ghost"}
	d := route.Decision{Kind: "worker", Tier: route.Tier{Model: "sonnet", Effort: "medium"}}
	_, notes := BuildProfile(d, repo, cfg, map[string]bool{"superpowers@claude-plugins-official": true}, nil, "/plugins/qm-worker")
	ghost := func(n string) bool { return strings.Contains(n, "worker plugin ghost not loaded") }
	if !slices.ContainsFunc(notes, ghost) || slices.ContainsFunc(notes, func(n string) bool { return strings.Contains(n, "superpowers") }) {
		t.Errorf("notes = %q, want ghost noted and superpowers not", notes)
	}
}

func TestLSPPluginNotSwitchedOff(t *testing.T) {
	claudeDir := hermetic(t)
	installPlugins(t, claudeDir, "gopls-lsp@claude-plugins-official")
	repo := repoWith(t, "a.go")
	d := route.Decision{Kind: "worker", Tier: route.Tier{Model: "sonnet", Effort: "medium"}}

	// On or off in the user's settings, an installed LSP plugin is on in the
	// worker, through enabledPlugins only.
	for _, user := range []bool{true, false} {
		p, _ := BuildProfile(d, repo, config.Defaults(), map[string]bool{"gopls-lsp@claude-plugins-official": user}, nil, "/plugins/qm-worker")
		s := settingsOf(t, LaunchScript(p, d.Tier, "task-a", "", "/w", "/b.md", ""))
		if !s["gopls-lsp@claude-plugins-official"] || len(p.PluginDirs) != 1 || p.Env["QM_LSP"] != "go" {
			t.Errorf("user %v: settings %v, dirs %q, env %v: want gopls on, no --plugin-dir, QM_LSP=go", user, s, p.PluginDirs, p.Env)
		}
	}
}

func TestProjectPluginsSwitchedOff(t *testing.T) {
	claudeDir := hermetic(t)
	repo := repoWith(t, "README.md")
	writeFile(t, filepath.Join(claudeDir, "settings.json"), `{"enabledPlugins":{"user-one@example-market":true}}`)
	writeFile(t, filepath.Join(repo, ".claude", "settings.json"), `{"enabledPlugins":{"proj-one@example-market":true,"proj-kept@example-market":true}}`)
	writeFile(t, filepath.Join(repo, ".claude", "settings.local.json"), `{"enabledPlugins":{"local-one@example-market":["1.0.0"]}}`)
	top, _, err := reviewin.Repo(repo)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.ProjectPlugins[top] = []string{"proj-kept"}
	deps := testDeps(&fakeLauncher{})
	deps.Config = cfg
	req := request(t, repo, "docs")
	req.DryRun = true
	res, err := Worker(context.Background(), req, deps)
	if err != nil {
		t.Fatal(err)
	}
	got := settingsOf(t, res.Script)
	want := map[string]bool{
		"user-one@example-market":      false,
		"proj-one@example-market":      false,
		"local-one@example-market":     false,
		"proj-kept@example-market":     true,
		"qm-coordinator@quartermaster": false,
	}
	if len(got) != len(want) {
		t.Errorf("enabledPlugins = %v, want %v", got, want)
	}
	for k, v := range want {
		if on, ok := got[k]; !ok || on != v {
			t.Errorf("enabledPlugins[%s] = %v (set %v), want %v", k, on, ok, v)
		}
	}
}

// writeSynced writes a claude.ai sync manifest naming the plugins (name to
// marketplace).
func writeSynced(t *testing.T, claudeDir string, plugins map[string]string) {
	t.Helper()
	type entry struct {
		Name            string `json:"name"`
		MarketplaceName string `json:"marketplaceName"`
	}
	var m struct {
		Plugins []entry `json:"plugins"`
	}
	for n, mk := range plugins {
		m.Plugins = append(m.Plugins, entry{n, mk})
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(claudeDir, "plugins", "synced", "account-a", "manifest.json"), string(b))
}

// settingsDoc is the whole --settings JSON of a script.
func settingsDoc(t *testing.T, script string) map[string]any {
	t.Helper()
	_, rest, _ := strings.Cut(script, "--settings '")
	js, _, _ := strings.Cut(rest, "'")
	var s map[string]any
	if err := json.Unmarshal([]byte(js), &s); err != nil {
		t.Fatalf("settings %q: %v", js, err)
	}
	return s
}

func TestClaudeAiPluginsSwitchedOff(t *testing.T) {
	claudeDir := hermetic(t)
	writeSynced(t, claudeDir, map[string]string{"synced-one": "example-market", "synced-two": "example-market"})
	repo := repoWith(t, "README.md")
	d := route.Decision{Kind: "worker", Tier: route.Tier{Model: "sonnet", Effort: "medium"}}

	p, _ := BuildProfile(d, repo, config.Defaults(), nil, nil, "/plugins/qm-worker")
	s := LaunchScript(p, d.Tier, "task-a", "", "/w", "/b.md", "")
	if doc := settingsDoc(t, s); doc["syncClaudeAiPlugins"] != false {
		t.Errorf("settings %v: want syncClaudeAiPlugins false", doc)
	}
	got := settingsOf(t, s)
	for _, k := range []string{"synced-one@example-market", "synced-two@example-market"} {
		if on, ok := got[k]; !ok || on {
			t.Errorf("enabledPlugins[%s] = %v (set %v), want false", k, on, ok)
		}
	}

	// A worker plugin that comes from the sync keeps the sync on.
	cfg := config.Defaults()
	cfg.Worker.Plugins = []string{"synced-one"}
	p, notes := BuildProfile(d, repo, cfg, nil, nil, "/plugins/qm-worker")
	s = LaunchScript(p, d.Tier, "task-a", "", "/w", "/b.md", "")
	if _, set := settingsDoc(t, s)["syncClaudeAiPlugins"]; set || !p.KeepClaudeAiPlugins {
		t.Errorf("sync switched off although a worker plugin needs it:\n%s", s)
	}
	got = settingsOf(t, s)
	if !got["synced-one@example-market"] || got["synced-two@example-market"] {
		t.Errorf("enabledPlugins = %v, want synced-one on and synced-two off", got)
	}
	if !slices.ContainsFunc(notes, func(n string) bool { return strings.Contains(n, "sync left on") }) {
		t.Errorf("notes = %q", notes)
	}

	// The same plugin also enabled in the local settings loads without the
	// sync: the sync goes off.
	p, _ = BuildProfile(d, repo, cfg, map[string]bool{"synced-one@example-market": true}, nil, "/plugins/qm-worker")
	s = LaunchScript(p, d.Tier, "task-a", "", "/w", "/b.md", "")
	if doc := settingsDoc(t, s); doc["syncClaudeAiPlugins"] != false || p.KeepClaudeAiPlugins || !settingsOf(t, s)["synced-one@example-market"] {
		t.Errorf("settings %v: want synced-one on and the sync off", doc)
	}
}

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestNewSessionIDIsV4AndUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		id, err := NewSessionID()
		if err != nil || !uuidV4.MatchString(id) || seen[id] {
			t.Fatalf("id %q, err %v: want a new v4 UUID", id, err)
		}
		seen[id] = true
	}
}

// TestScriptPromptPointsToTheBrief: the argument claude gets is one line that
// names the brief file; the brief's text is nowhere in the script.
func TestScriptPromptPointsToTheBrief(t *testing.T) {
	hermetic(t)
	dir := t.TempDir()
	brief := writeFile(t, filepath.Join(dir, "it's a brief.md"), "--model is not a flag here")
	p, _ := profileFor(t, "sonnet", "medium", nil)
	s := LaunchScript(p, route.Tier{Model: "sonnet", Effort: "medium"}, "task-a", "", dir, brief, "")
	_, prompt, ok := strings.Cut(s, " -n task-a ")
	if !ok {
		t.Fatalf("no prompt in:\n%s", s)
	}
	// Run through sh with claude replaced by printf, to see the argument
	// claude would get.
	out, err := exec.Command("/bin/sh", "-c", "printf '[%s]' "+prompt).Output()
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.HasPrefix(got, "[\nYou are a worker") || strings.Count(got, "\n") != 1 || !strings.Contains(got, " "+brief+":") {
		t.Errorf("prompt argument = %q, want one line naming %s", got, brief)
	}
	if strings.Contains(s, "not a flag") {
		t.Errorf("the brief's text is in the script:\n%s", s)
	}
}

func TestWriteScriptSweepsOldScripts(t *testing.T) {
	hermetic(t)
	first, err := WriteScript("old", "#!/bin/sh\n")
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-ScriptAge - time.Hour)
	if err := os.Chtimes(first, past, past); err != nil {
		t.Fatal(err)
	}
	second, err := WriteScript("new", "#!/bin/sh\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Errorf("old script kept: %v", err)
	}
	if _, err := os.Stat(second); err != nil {
		t.Errorf("new script: %v", err)
	}
}

func TestWorkerTiedToProject(t *testing.T) {
	hermetic(t)
	repo := repoWith(t, "README.md")
	l := &fakeLauncher{}
	req := request(t, repo, "docs")
	req.UpProject, req.UpWorkspace, req.WorkspaceName = "try-00000000", "try", "try"
	res, err := Worker(context.Background(), req, testDeps(l))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Script, " QM_PROJECT_ID=try-00000000 QM_WORKSPACE=try ") {
		t.Errorf("script lacks the project tie:\n%s", res.Script)
	}
	if len(l.specs) != 1 || l.specs[0].WorkspaceName != "try" || l.specs[0].Workspace != "" {
		t.Errorf("launched %+v, want one session in workspace named try", l.specs)
	}
	recs := records(t)
	if len(recs) != 1 || recs[0].UpProject != "try-00000000" || recs[0].Cwd != repo {
		t.Fatalf("records = %+v, want up_project try-00000000 and cwd %s", recs, repo)
	}
	if env := recs[0].Decision.Profile.Env; env["QM_PROJECT_ID"] != "try-00000000" || env["QM_WORKSPACE"] != "try" {
		t.Errorf("logged profile env = %v, want the project tie", env)
	}

	// An explicit workspace moves only where the worker opens: its launch
	// still carries the project's workspace for its own workers.
	l = &fakeLauncher{}
	req = request(t, repo, "docs")
	req.UpProject, req.UpWorkspace, req.Workspace = "try-00000000", "try", "ws-7"
	res, err = Worker(context.Background(), req, testDeps(l))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Script, " QM_PROJECT_ID=try-00000000 QM_WORKSPACE=try ") {
		t.Errorf("script lacks the project tie:\n%s", res.Script)
	}
	if len(l.specs) != 1 || l.specs[0].Workspace != "ws-7" || l.specs[0].WorkspaceName != "" {
		t.Errorf("launched %+v, want one session in workspace ws-7", l.specs)
	}
}

func TestWorkerUntied(t *testing.T) {
	hermetic(t)
	repo := repoWith(t, "README.md")
	l := &fakeLauncher{}
	res, err := Worker(context.Background(), request(t, repo, "docs"), testDeps(l))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Script, "QM_PROJECT_ID") || strings.Contains(res.Script, "QM_WORKSPACE") {
		t.Errorf("untied script carries a project tie:\n%s", res.Script)
	}
	if len(l.specs) != 1 || l.specs[0].WorkspaceName != "" {
		t.Errorf("launched %+v, want no workspace name", l.specs)
	}
	recs := records(t)
	if len(recs) != 1 || recs[0].UpProject != "" || recs[0].Cwd != repo {
		t.Fatalf("records = %+v, want no up_project and cwd %s", recs, repo)
	}
	if _, ok := recs[0].Decision.Profile.Env["QM_PROJECT_ID"]; ok {
		t.Errorf("logged profile env = %v, want no project tie", recs[0].Decision.Profile.Env)
	}
}

func TestWorkerBothWorkspacesBadInput(t *testing.T) {
	hermetic(t)
	repo := repoWith(t, "README.md")
	req := request(t, repo, "docs")
	req.Workspace, req.WorkspaceName = "ws-1", "try"
	if _, err := Worker(context.Background(), req, testDeps(&fakeLauncher{})); !errors.Is(err, ErrInput) {
		t.Errorf("err = %v, want ErrInput", err)
	}
}

func TestSpawnUntrustedFails(t *testing.T) {
	hermetic(t)
	repo := repoWith(t, "README.md")
	l := &fakeLauncher{}
	deps := testDeps(l)
	var asked string
	deps.Trusted = func(dir string) (bool, error) { asked = dir; return false, nil }
	_, err := Worker(context.Background(), request(t, repo, "docs"), deps)
	if !errors.Is(err, ErrUntrusted) || !strings.Contains(err.Error(), "run claude there once") || !strings.Contains(err.Error(), repo) {
		t.Fatalf("err = %v, want ErrUntrusted naming %s", err, repo)
	}
	if asked != repo {
		t.Errorf("trust asked for %q, want %q", asked, repo)
	}
	if len(l.specs) != 0 || len(launchDirEntries(t)) != 0 || len(records(t)) != 0 {
		t.Errorf("an untrusted folder launched %d, left %d scripts, logged %d records", len(l.specs), len(launchDirEntries(t)), len(records(t)))
	}

	// A trust file that cannot be read stops the launch too.
	deps.Trusted = func(string) (bool, error) { return false, errors.New("bad json") }
	if _, err := Worker(context.Background(), request(t, repo, "docs"), deps); err == nil || !strings.Contains(err.Error(), "bad json") {
		t.Errorf("err = %v, want the trust read error", err)
	}

	// A dry run says it instead of failing.
	deps.Trusted = func(string) (bool, error) { return false, nil }
	req := request(t, repo, "docs")
	req.DryRun = true
	res, err := Worker(context.Background(), req, deps)
	if err != nil || res.TrustNote != "not trusted by Claude Code: "+repo || res.Script == "" {
		t.Errorf("dry run: err %v, trust note %q; want no error and the note", err, res.TrustNote)
	}

	// A trusted folder launches, with no note.
	deps.Trusted = func(string) (bool, error) { return true, nil }
	res, err = Worker(context.Background(), request(t, repo, "docs"), deps)
	if err != nil || res.TrustNote != "" || len(l.specs) != 1 {
		t.Errorf("trusted: err %v, note %q, launched %d", err, res.TrustNote, len(l.specs))
	}
}

// An inherited GIT_DIR must not make the LSP pick count another
// repository's files.
func TestRepoLSPIgnoresInheritedGitVariables(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo, other := repoWith(t, "a.go"), repoWith(t, "a.py", "b.py")
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_INDEX_FILE", filepath.Join(other, ".git", "index"))
	l, note := RepoLSP(repo)
	if l.Plugin != "gopls-lsp" {
		t.Errorf("RepoLSP = %q (%q), want gopls-lsp", l.Plugin, note)
	}
}
