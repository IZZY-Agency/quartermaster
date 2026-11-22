package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/IZZY-Agency/quartermaster/internal/providers"
)

// env points every folder init reads or writes at a temporary one: HOME, the
// XDG folders, Claude Code's folder, an empty PATH, no TypeSafe key, no
// helper and no gitleaks. It returns the settings file's path (not created).
func env(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "claude"))
	t.Setenv("CLAUDE_PID", "")
	t.Setenv("PATH", t.TempDir())
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("TYPESAFE_API_KEY", "")
	t.Setenv("QM_PRIVCHECK", "")
	t.Setenv("QM_GITLEAKS", "")
	t.Setenv("QM_WORKER_PLUGIN", "")
	if err := os.MkdirAll(filepath.Join(home, "claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	fakeQM(t, home)
	return filepath.Join(home, "claude", "settings.json")
}

// qmLine is the command init writes for a bare status line: the fake qm
// binary's real path, then statusline. fakeQM sets it.
var qmLine string

// fakeQM makes qm's executable a file under home, named qm.
func fakeQM(t *testing.T, home string) {
	t.Helper()
	exe := filepath.Join(home, "bin", "qm")
	if err := os.MkdirAll(filepath.Dir(exe), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, exe, "#!/bin/sh\n")
	qmLine = exe + " statusline"
	old := executable
	executable = func() (string, error) { return exe, nil }
	t.Cleanup(func() { executable = old })
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// runInit runs Init with in as the user's answers and returns its output.
func runInit(t *testing.T, settings, in string, yes bool) string {
	t.Helper()
	var out bytes.Buffer
	if err := Init(context.Background(), Streams{In: strings.NewReader(in), Out: &out}, Options{Yes: yes, SettingsPath: settings}); err != nil {
		t.Fatalf("Init: %v\n%s", err, out.String())
	}
	return out.String()
}

func runUndo(t *testing.T, settings, in string) string {
	t.Helper()
	var out bytes.Buffer
	if err := Undo(context.Background(), Streams{In: strings.NewReader(in), Out: &out}, Options{SettingsPath: settings}); err != nil {
		t.Fatalf("Undo: %v\n%s", err, out.String())
	}
	return out.String()
}

func configPath(t *testing.T) string {
	return filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "quartermaster", "config.json")
}

func configMap(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(readFile(t, configPath(t))), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func statusCommand(t *testing.T, settings string) string {
	t.Helper()
	var s struct {
		StatusLine struct {
			Command string `json:"command"`
		} `json:"statusLine"`
	}
	if err := json.Unmarshal([]byte(readFile(t, settings)), &s); err != nil {
		t.Fatal(err)
	}
	return s.StatusLine.Command
}

func backups(t *testing.T, settings string) []string {
	t.Helper()
	m, err := filepath.Glob(settings + ".qm-backup-*")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

const userLine = `{
  "theme": "dark",
  "statusLine": {
    "type": "command",
    "command": "bash ~/line.sh 'a b'"
  },
  "enabledPlugins": {}
}
`

func TestInitWritesConfig0600(t *testing.T) {
	settings := env(t)
	runInit(t, settings, "", false)
	fi, err := os.Stat(configPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("config mode %v, want 0600", fi.Mode().Perm())
	}
	di, err := os.Stat(filepath.Dir(configPath(t)))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("config folder mode %v, want 0700", di.Mode().Perm())
	}
	m := configMap(t)
	if m["read_budget"] != float64(8) || m["launcher"] != "auto" {
		t.Errorf("config lacks the defaults: %v", m)
	}
}

func TestInitKeepsExistingConfigAddsMissingKeys(t *testing.T) {
	settings := env(t)
	if err := os.MkdirAll(filepath.Dir(configPath(t)), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, configPath(t), `{"read_budget": 3, "privacy": {"local_model": "off"}, "custom": true}`)
	runInit(t, settings, "", false)
	m := configMap(t)
	if m["read_budget"] != float64(3) || m["custom"] != true {
		t.Errorf("existing keys changed: %v", m)
	}
	if m["launcher"] != "auto" {
		t.Errorf("missing key not added: %v", m)
	}
	if p := m["privacy"].(map[string]any); p["local_model"] != "off" || p["clients"] == nil {
		t.Errorf("privacy = %v, want local_model kept and clients added", p)
	}
	rec := recordT(t)
	if len(rec.Created) != 0 {
		t.Errorf("created = %v, want nothing: the config existed", rec.Created)
	}
}

func TestInitKeyFromEnvNotStored(t *testing.T) {
	settings := env(t)
	t.Setenv("TYPESAFE_API_KEY", "env-key-value")
	out := runInit(t, settings, "", false)
	if !strings.Contains(out, "TYPESAFE_API_KEY") || !strings.Contains(out, "not stored") {
		t.Errorf("output does not say the env key is not stored:\n%s", out)
	}
	if strings.Contains(readFile(t, configPath(t)), "env-key-value") || strings.Contains(out, "env-key-value") {
		t.Error("the env key was stored or printed")
	}
}

func TestInitStoresKeyWhenTyped(t *testing.T) {
	settings := env(t)
	out := runInit(t, settings, "typed-key-value\nn\n", false)
	if configMap(t)["typesafe_api_key"] != "typed-key-value" {
		t.Errorf("typed key not stored: %s", readFile(t, configPath(t)))
	}
	if strings.Contains(out, "typed-key-value") {
		t.Error("the typed key was printed")
	}
}

func TestInitEnterSkipsKey(t *testing.T) {
	settings := env(t)
	runInit(t, settings, "\nn\n", false)
	if _, ok := configMap(t)["typesafe_api_key"]; ok {
		t.Error("Enter stored a key")
	}
}

func TestInitWrapsStatusLineAfterYes(t *testing.T) {
	settings := env(t)
	writeFile(t, settings, userLine)
	out := runInit(t, settings, "\ny\n", false)
	if !strings.Contains(out, "- statusLine.command") || !strings.Contains(out, "+ statusLine.command") || !strings.Contains(out, "absolute path") {
		t.Errorf("no diff shown:\n%s", out)
	}
	if got, want := statusCommand(t, settings), qmLine+` -- 'bash ~/line.sh '\''a b'\'''`; got != want {
		t.Errorf("command = %q, want %q", got, want)
	}
	b := backups(t, settings)
	if len(b) != 1 || readFile(t, b[0]) != userLine {
		t.Fatalf("backup = %v, want one copy of the original", b)
	}
	if !strings.Contains(readFile(t, settings), `"theme": "dark"`) {
		t.Error("other settings changed")
	}
	rec := recordT(t)
	if rec.StatusLineBefore == nil || *rec.StatusLineBefore != "bash ~/line.sh 'a b'" || rec.StatusLineAfter != statusCommand(t, settings) || rec.SettingsBackup != b[0] {
		t.Errorf("record = %+v", rec)
	}
}

func TestInitLeavesStatusLineOnNo(t *testing.T) {
	settings := env(t)
	writeFile(t, settings, userLine)
	runInit(t, settings, "\nn\n", false)
	if readFile(t, settings) != userLine {
		t.Error("settings changed after no")
	}
	if b := backups(t, settings); len(b) != 0 {
		t.Errorf("backups = %v, want none", b)
	}
}

func TestInitOffersBareStatusLine(t *testing.T) {
	settings := env(t)
	writeFile(t, settings, "{\n  \"theme\": \"dark\"\n}\n")
	out := runInit(t, settings, "\ny\n", false)
	if !strings.Contains(out, "qm statusline") {
		t.Errorf("no offer shown:\n%s", out)
	}
	if got := statusCommand(t, settings); got != qmLine {
		t.Errorf("command = %q, want %q", got, qmLine)
	}
}

func TestInitIdempotent(t *testing.T) {
	settings := env(t)
	writeFile(t, settings, userLine)
	runInit(t, settings, "", true)
	first := readFile(t, settings)
	out := runInit(t, settings, "", true)
	if readFile(t, settings) != first {
		t.Errorf("second run changed settings:\n%s", readFile(t, settings))
	}
	if n := strings.Count(statusCommand(t, settings), "qm statusline"); n != 1 {
		t.Errorf("wrapped %d times", n)
	}
	if !strings.Contains(out, "already") {
		t.Errorf("second run does not say the status line is already wrapped:\n%s", out)
	}
	if b := backups(t, settings); len(b) != 1 {
		t.Errorf("backups = %v, want one", b)
	}
	if rec := recordT(t); len(rec.Created) != 1 || rec.StatusLineBefore == nil {
		t.Errorf("second run lost the first run's record: %+v", rec)
	}
}

func TestInitSetsLocalModelRequired(t *testing.T) {
	settings := env(t)
	helper := filepath.Join(t.TempDir(), "qm-privcheck")
	writeFile(t, helper, "#!/bin/sh\n[ \"$1\" = status ] && echo available\n")
	if err := os.Chmod(helper, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QM_PRIVCHECK", helper)
	runInit(t, settings, "", true)
	if p := configMap(t)["privacy"].(map[string]any); p["local_model"] != "required" {
		t.Errorf("local_model = %v, want required", p["local_model"])
	}
}

func TestInitOffersSuperpowers(t *testing.T) {
	settings := env(t)
	writeFile(t, settings, `{"enabledPlugins": {"superpowers@claude-plugins-official": true}}`)
	runInit(t, settings, "", true)
	w := configMap(t)["worker"].(map[string]any)
	if pl, _ := w["plugins"].([]any); len(pl) != 1 || pl[0] != "superpowers@claude-plugins-official" {
		t.Errorf("worker.plugins = %v", w["plugins"])
	}
}

func TestInitPrintsPluginCommands(t *testing.T) {
	settings := env(t)
	out := runInit(t, settings, "", true)
	for _, want := range []string{"/plugin marketplace add ", "/plugin install qm-coordinator@quartermaster"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestUndoRestoresStatusLine(t *testing.T) {
	settings := env(t)
	writeFile(t, settings, userLine)
	runInit(t, settings, "", true)
	out := runUndo(t, settings, "")
	if got := readFile(t, settings); got != userLine {
		t.Errorf("settings after undo:\n%s\nwant:\n%s", got, userLine)
	}
	if _, err := os.Stat(configPath(t)); !os.IsNotExist(err) {
		t.Errorf("config still there: %v", err)
	}
	if _, err := os.Stat(recordPathT(t)); !os.IsNotExist(err) {
		t.Errorf("init record still there: %v", err)
	}
	for _, want := range []string{"/plugin uninstall qm-coordinator@quartermaster", "decisions.jsonl"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestUndoRemovesBareStatusLineAndCreatedSettings(t *testing.T) {
	settings := env(t)
	runInit(t, settings, "", true)
	if statusCommand(t, settings) != qmLine {
		t.Fatal("init did not add the status line")
	}
	runUndo(t, settings, "")
	if _, err := os.Stat(settings); !os.IsNotExist(err) {
		t.Errorf("settings init created is still there: %v", err)
	}
}

// editLine changes the theme and the status-line command init wrote, as a
// user editing settings.json after init would.
func editLine(t *testing.T, settings string) {
	t.Helper()
	s := readFile(t, settings)
	s = strings.Replace(s, "\"theme\": \"dark\"", "\"theme\": \"light\"", 1)
	after, err := json.Marshal(statusCommand(t, settings))
	if err != nil {
		t.Fatal(err)
	}
	s = strings.Replace(s, string(after), `"my-new-line"`, 1)
	writeFile(t, settings, s)
}

func TestUndoAsksWhenEdited(t *testing.T) {
	settings := env(t)
	writeFile(t, settings, userLine)
	runInit(t, settings, "", true)
	editLine(t, settings)
	out := runUndo(t, settings, "n\n")
	if !strings.Contains(out, "my-new-line") || !strings.Contains(out, "bash ~/line.sh") {
		t.Errorf("undo does not show both lines:\n%s", out)
	}
	if statusCommand(t, settings) != "my-new-line" {
		t.Error("undo changed an edited status line after no")
	}
}

func TestUndoRestoresEditedLineOnYes(t *testing.T) {
	settings := env(t)
	writeFile(t, settings, userLine)
	runInit(t, settings, "", true)
	editLine(t, settings)
	runUndo(t, settings, "y\n")
	if got := statusCommand(t, settings); got != "bash ~/line.sh 'a b'" {
		t.Errorf("command after yes = %q", got)
	}
	if !strings.Contains(readFile(t, settings), `"theme": "light"`) {
		t.Error("undo lost an unrelated edit")
	}
}

func TestWrapStatusLine(t *testing.T) {
	env(t)
	in := []byte("{\"statusLine\":{\"command\":\"a & b\"},\"x\":1}")
	got, diff, changed := WrapStatusLine(in)
	if !changed || diff == "" {
		t.Fatalf("changed=%v diff=%q", changed, diff)
	}
	if want := "{\"statusLine\":{\"command\":\"" + qmLine + " -- 'a & b'\"},\"x\":1}"; string(got) != want {
		t.Errorf("got %s, want %s", got, want)
	}
	again, _, changed := WrapStatusLine(got)
	if changed || !bytes.Equal(again, got) {
		t.Errorf("wrapped twice: %s", again)
	}
	empty, _, changed := WrapStatusLine([]byte("{}"))
	if !changed || !strings.Contains(string(empty), "\"command\": \""+qmLine+"\"") {
		t.Errorf("empty settings: %s", empty)
	}
	if !json.Valid(empty) {
		t.Errorf("invalid JSON: %s", empty)
	}
}

func recordT(t *testing.T) Record {
	t.Helper()
	var r Record
	if err := json.Unmarshal([]byte(readFile(t, recordPathT(t))), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func recordPathT(t *testing.T) string {
	return filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "quartermaster", "init.json")
}

func TestWrapQuotesQMPathWithSpace(t *testing.T) {
	env(t)
	dir := filepath.Join(t.TempDir(), "a b")
	exe := filepath.Join(dir, "qm")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, exe, "")
	executable = func() (string, error) { return exe, nil }
	got, _, _ := WrapStatusLine([]byte(`{"statusLine":{"command":"x"}}`))
	if want := `{"statusLine":{"command":"'` + exe + `' statusline -- 'x'"}}`; string(got) != want {
		t.Errorf("got %s, want %s", got, want)
	}
	if _, _, changed := WrapStatusLine(got); changed {
		t.Error("a quoted qm path is not seen as wrapped")
	}
}

// A status line wrapped by another qm (an earlier install) is repointed to
// this qm, and the undo still puts back the command from before the first init.
func TestInitRepointsStatusLineOfAnotherQM(t *testing.T) {
	settings := env(t)
	dir := t.TempDir()
	oldExe, newExe := filepath.Join(dir, "old", "qm"), filepath.Join(dir, "new", "qm")
	executable = func() (string, error) { return oldExe, nil }
	writeFile(t, settings, `{"statusLine":{"type":"command","command":"mine --x"}}`)
	run := func(undo bool) string {
		var out bytes.Buffer
		st := Streams{In: strings.NewReader(""), Out: &out}
		var err error
		if undo {
			err = Undo(context.Background(), st, Options{Yes: true, SettingsPath: settings})
		} else {
			err = Init(context.Background(), st, Options{Yes: true, SettingsPath: settings})
		}
		if err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	command := func() string {
		b, err := os.ReadFile(settings)
		if err != nil {
			t.Fatal(err)
		}
		cmd, _ := statusLineCommand(b)
		return cmd
	}
	run(false)
	if want := oldExe + " statusline -- 'mine --x'"; command() != want {
		t.Fatalf("first init wrote %q, want %q", command(), want)
	}
	executable = func() (string, error) { return newExe, nil }
	out := run(false)
	if want := newExe + " statusline -- 'mine --x'"; command() != want {
		t.Fatalf("second init left %q, want %q\n%s", command(), want, out)
	}
	if out = run(false); !strings.Contains(out, "already runs qm statusline") {
		t.Errorf("third init does not say the status line is already wrapped:\n%s", out)
	}
	run(true)
	if command() != "mine --x" {
		t.Errorf("undo left %q, want the command from before the first init", command())
	}
}

// Repointing keeps the first init's backup as the record's reference, gives
// the extra backup a name of its own (even within the same second), and the
// undo puts back the original bytes, an explicit statusLine null included.
func TestRepointKeepsFirstBackupAndUndoRestoresOriginalBytes(t *testing.T) {
	settings := env(t)
	dir := t.TempDir()
	exeA, exeB := filepath.Join(dir, "a", "qm"), filepath.Join(dir, "b", "qm")
	original := "{\n  \"statusLine\": null,\n  \"theme\": \"dark\"\n}\n"
	writeFile(t, settings, original)
	old := backupStamp
	backupStamp = func() int64 { return 1_800_000_000 } // every backup in the same second
	t.Cleanup(func() { backupStamp = old })
	init := func(exe string) {
		t.Helper()
		executable = func() (string, error) { return exe, nil }
		if err := Init(context.Background(), Streams{In: strings.NewReader(""), Out: &bytes.Buffer{}}, Options{Yes: true, SettingsPath: settings}); err != nil {
			t.Fatal(err)
		}
	}
	init(exeA)
	rec, _ := readRecord()
	first := rec.SettingsBackup
	init(exeB)
	rec, _ = readRecord()
	if rec.SettingsBackup != first {
		t.Fatalf("repoint replaced the backup reference %q with %q", first, rec.SettingsBackup)
	}
	if b, err := os.ReadFile(first); err != nil || string(b) != original {
		t.Fatalf("first backup %q: %q, %v", first, b, err)
	}
	if all := backups(t, settings); len(all) != 2 {
		t.Fatalf("backups %v, want the first and one more under its own name", all)
	}
	// Undone by yet another qm: the wrapper names exeB, not this one.
	executable = func() (string, error) { return filepath.Join(dir, "c", "qm"), nil }
	if err := Undo(context.Background(), Streams{In: strings.NewReader(""), Out: &bytes.Buffer{}}, Options{Yes: true, SettingsPath: settings}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(settings); string(b) != original {
		t.Fatalf("undo left %q, want the original bytes %q", b, original)
	}
}

func TestRepointStatusLineLeavesOthersAlone(t *testing.T) {
	env(t)
	executable = func() (string, error) { return "/opt/x/bin/qm", nil }
	for _, cmd := range []string{"mine", "qm statusline -- 'mine'", "/opt/x/bin/qm statusline -- 'mine'", "/a/qmx statusline"} {
		in := []byte(`{"statusLine":{"command":` + jsonString(cmd) + `}}`)
		if _, _, changed := RepointStatusLine(in); changed {
			t.Errorf("%q was repointed", cmd)
		}
	}
	in := []byte(`{"statusLine":{"command":"'/old dir/qm' statusline"}}`)
	got, _, changed := RepointStatusLine(in)
	if want := `{"statusLine":{"command":"/opt/x/bin/qm statusline"}}`; !changed || string(got) != want {
		t.Errorf("got %s (changed %v), want %s", got, changed, want)
	}
}

// Every wrapper qm init emits is recognised again: a path with an apostrophe
// (shellWord's '\” escape) and a wrapped command with a newline.
func TestWrapperParserAcceptsWhatShellWordEmits(t *testing.T) {
	env(t)
	apos := filepath.Join(t.TempDir(), "it's here", "qm")
	executable = func() (string, error) { return apos, nil }
	inner := "one\ntwo 'three'"
	wrapped, _, changed := WrapStatusLine([]byte(`{"statusLine":{"command":` + jsonString(inner) + `}}`))
	if !changed {
		t.Fatal("not wrapped")
	}
	// The same qm again: already wrapped, never wrapped a second time.
	if again, _, changed := WrapStatusLine(wrapped); changed {
		t.Fatalf("wrapped twice: %s", again)
	}
	if again, _, changed := RepointStatusLine(wrapped); changed {
		t.Fatalf("repointed to itself: %s", again)
	}
	// Another install: repointed, the command (newline included) untouched.
	newExe := "/opt/new/bin/qm"
	executable = func() (string, error) { return newExe, nil }
	got, _, changed := RepointStatusLine(wrapped)
	if !changed {
		t.Fatalf("an apostrophe path was not recognised: %s", wrapped)
	}
	cmd, _ := statusLineCommand(got)
	if want := newExe + " statusline -- " + shellQuote(inner); cmd != want {
		t.Fatalf("got %q, want %q", cmd, want)
	}
	// A path without an apostrophe wrapping a command with a newline.
	executable = func() (string, error) { return "/opt/x/bin/qm", nil }
	in := []byte(`{"statusLine":{"command":` + jsonString("/old/qm statusline -- 'a\nb'") + `}}`)
	got, _, changed = RepointStatusLine(in)
	if cmd, _ := statusLineCommand(got); !changed || cmd != "/opt/x/bin/qm statusline -- 'a\nb'" {
		t.Fatalf("newline in the wrapped command: changed %v, got %q", changed, cmd)
	}
}

func TestInitMarketplaceFlag(t *testing.T) {
	settings := env(t)
	var out bytes.Buffer
	dir := t.TempDir()
	if err := Init(context.Background(), Streams{In: strings.NewReader(""), Out: &out}, Options{Yes: true, SettingsPath: settings, Marketplace: dir}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "/plugin marketplace add "+dir+"\n") {
		t.Errorf("folder not named:\n%s", out.String())
	}
	out.Reset()
	if err := Init(context.Background(), Streams{In: strings.NewReader(""), Out: &out}, Options{Yes: true, SettingsPath: settings, Marketplace: "someone/fork"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "/plugin marketplace add someone/fork\n") {
		t.Errorf("owner/repo not named:\n%s", out.String())
	}
}

// An installed qm run inside a checkout names the public repository; a qm
// built in the checkout (bin/qm) names the checkout.
func TestInitMarketplaceIgnoresWorkingFolder(t *testing.T) {
	settings := env(t)
	co, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{".claude-plugin", "bin"} {
		if err := os.MkdirAll(filepath.Join(co, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(co, ".claude-plugin", "marketplace.json"), `{"name":"quartermaster"}`)
	writeFile(t, filepath.Join(co, "bin", "qm"), "")
	t.Chdir(co)
	run := func() string {
		var out bytes.Buffer
		if err := Init(context.Background(), Streams{In: strings.NewReader(""), Out: &out}, Options{Yes: true, SettingsPath: settings}); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	if got := run(); !strings.Contains(got, "/plugin marketplace add "+publicMarketplace+"\n") {
		t.Errorf("installed qm in a checkout does not name the public repository:\n%s", got)
	}
	executable = func() (string, error) { return filepath.Join(co, "bin", "qm"), nil }
	if got := run(); !strings.Contains(got, "/plugin marketplace add "+co+"\n") {
		t.Errorf("qm built in the checkout does not name it:\n%s", got)
	}
}

func TestUndoUsesRecordedSettingsPath(t *testing.T) {
	settings := env(t)
	runInit(t, settings, "", true) // creates settings with a bare status line
	editedSettings := strings.Replace(readFile(t, settings), "{", "{\n  \"theme\": \"dark\",", 1)
	writeFile(t, settings, editedSettings)
	other := filepath.Join(t.TempDir(), "settings.json")
	writeFile(t, other, userLine)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Dir(other))

	runUndo(t, "", "")
	if readFile(t, other) != userLine {
		t.Error("undo touched a settings file init never worked on")
	}
	if got := readFile(t, settings); !strings.Contains(got, "theme") || strings.Contains(got, "statusLine") {
		t.Errorf("recorded settings after undo:\n%s", got)
	}
}

func TestUndoAsksBeforeRemovingEditedConfig(t *testing.T) {
	for _, tc := range []struct {
		name, in  string
		yes, kept bool
	}{
		{"no", "n\n", false, true},
		{"yes answer", "y\n", false, false},
		{"--yes keeps", "", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := env(t)
			runInit(t, settings, "", true)
			writeFile(t, configPath(t), `{"read_budget": 2}`)
			var out bytes.Buffer
			if err := Undo(context.Background(), Streams{In: strings.NewReader(tc.in), Out: &out}, Options{Yes: tc.yes, SettingsPath: settings}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "edited since qm init") {
				t.Errorf("no question:\n%s", out.String())
			}
			_, err := os.Stat(configPath(t))
			if kept := err == nil; kept != tc.kept {
				t.Errorf("config kept = %v, want %v", kept, tc.kept)
			}
		})
	}
}

func TestUndoRemovesUneditedConfigWithoutAsking(t *testing.T) {
	settings := env(t)
	runInit(t, settings, "", true)
	out := runUndo(t, settings, "")
	if strings.Contains(out, "edited since qm init") {
		t.Errorf("asked about an unedited config:\n%s", out)
	}
}

func TestUndoYesKeepsEditedLine(t *testing.T) {
	settings := env(t)
	writeFile(t, settings, userLine)
	runInit(t, settings, "", true)
	editLine(t, settings)
	var out bytes.Buffer
	if err := Undo(context.Background(), Streams{In: strings.NewReader(""), Out: &out}, Options{Yes: true, SettingsPath: settings}); err != nil {
		t.Fatal(err)
	}
	if statusCommand(t, settings) != "my-new-line" {
		t.Error("--yes overwrote an edited status line")
	}
	if !strings.Contains(out.String(), "before init it was: bash ~/line.sh 'a b'") {
		t.Errorf("does not say what would have been restored:\n%s", out.String())
	}
}

func TestInitRecordsConfigBeforeLaterFailure(t *testing.T) {
	settings := env(t)
	writeFile(t, settings, userLine)
	dir := filepath.Dir(settings)
	if err := os.Chmod(dir, 0o500); err != nil { // the backup cannot be written
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	var out bytes.Buffer
	if err := Init(context.Background(), Streams{In: strings.NewReader(""), Out: &out}, Options{Yes: true, SettingsPath: settings}); err == nil {
		t.Fatal("Init succeeded writing into a read-only folder")
	}
	if rec := recordT(t); !slices.Contains(rec.Created, configPath(t)) {
		t.Errorf("record after the failure = %+v, want the config listed", rec)
	}
}

func TestWrapKeepsSymlinkedQMPath(t *testing.T) {
	env(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "Cellar", "qm")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, target, "")
	link := filepath.Join(dir, "bin", "qm")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	executable = func() (string, error) { return link, nil }
	got, _, _ := WrapStatusLine([]byte(`{"statusLine":{"command":"x"}}`))
	if want := `{"statusLine":{"command":"` + link + ` statusline -- 'x'"}}`; string(got) != want {
		t.Errorf("got %s, want %s (the symlink, not its target)", got, want)
	}
}

func TestHintsLSPInstall(t *testing.T) {
	st := []providers.Status{{Name: "lsp", Why: "LSP off for this repository: gopls-lsp not installed", Fix: "/plugin install gopls-lsp@claude-plugins-official"}}
	got := Hints(st, "/bin/zsh", true)
	if len(got) != 1 || got[0] != "LSP: inside Claude Code, run: /plugin install gopls-lsp@claude-plugins-official" {
		t.Errorf("hints = %q", got)
	}
	if got := Hints([]providers.Status{{Name: "lsp", Why: "LSP off: no official LSP plugin installed"}}, "/bin/zsh", true); len(got) != 0 {
		t.Errorf("no repository plugin: hints = %q, want none", got)
	}
}

// TestUndoKeepsEarlierSettingsWithContent: init ran with two Claude folders
// and created a settings file in each; the record names the second. Undo
// keeps the first, which holds settings, instead of removing it.
func TestUndoKeepsEarlierSettingsWithContent(t *testing.T) {
	first := env(t)
	runInit(t, first, "", true)
	second := filepath.Join(t.TempDir(), "settings.json")
	runInit(t, second, "", true)
	writeFile(t, first, `{"theme": "dark"}`)
	runUndo(t, second, "")
	if got := readFile(t, first); got != `{"theme": "dark"}` {
		t.Errorf("first settings after undo: %q", got)
	}
	if _, err := os.Stat(second); !os.IsNotExist(err) {
		t.Errorf("second settings, emptied by undo, not removed: %v", err)
	}
}

func TestInvokedPathKeepsLink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "Cellar", "1.0", "qm")
	writeFileAll(t, target)
	link := filepath.Join(dir, "bin", "qm")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	resolved := func() (string, error) { return target, nil } // Linux: /proc/self/exe
	look := func(name string) (string, error) {
		if name == "qm" {
			return link, nil
		}
		return "", os.ErrNotExist
	}
	for _, arg0 := range []string{"qm", link} {
		if got, err := invokedPath(arg0, look, resolved); err != nil || got != link {
			t.Errorf("invokedPath(%q) = %q, %v; want the link %s", arg0, got, err, link)
		}
	}
	// argv[0] that names another file, or none found: os.Executable's path.
	other := filepath.Join(dir, "other")
	writeFileAll(t, other)
	for _, arg0 := range []string{other, "missing"} {
		if got, _ := invokedPath(arg0, look, resolved); got != target {
			t.Errorf("invokedPath(%q) = %q, want %s", arg0, got, target)
		}
	}
}

func writeFileAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o700); err != nil {
		t.Fatal(err)
	}
}
