package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("TYPESAFE_API_KEY", "")
	return dir
}

func TestConfigDefaults(t *testing.T) {
	isolate(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.ReadBudget != 8 || c.WorkerWindow["sonnet"] != 250000 || c.WorkerWindow["opus"] != 400000 || c.WorkerWindow["fable"] != 400000 {
		t.Errorf("budget %d window %v", c.ReadBudget, c.WorkerWindow)
	}
	if c.Launcher != "auto" || c.Privacy.LocalModel != "auto" {
		t.Errorf("launcher %q local model %q", c.Launcher, c.Privacy.LocalModel)
	}
	if len(c.Worker.Plugins) != 0 || len(c.Privacy.Clients) != 0 || len(c.Notify) != 0 {
		t.Errorf("want empty lists: %+v", c)
	}
	if c.Rule("any-rule") != "on" {
		t.Errorf("rule default %q, want on", c.Rule("any-rule"))
	}
}

func TestConfigXDG(t *testing.T) {
	dir := isolate(t)
	want := filepath.Join(dir, "quartermaster", "config.json")
	if got, err := Path(); err != nil || got != want {
		t.Fatalf("Path() = %q, %v; want %q", got, err, want)
	}
	c, _ := Load()
	if err := Save(c); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("saved file not under XDG_CONFIG_HOME: %v", err)
	}
}

func TestConfigSaveMode(t *testing.T) {
	dir := isolate(t)
	c, _ := Load()
	if err := Save(c); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "quartermaster", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode %v, want 0600", fi.Mode().Perm())
	}
	di, err := os.Stat(filepath.Join(dir, "quartermaster"))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("folder mode %v, want 0700", di.Mode().Perm())
	}
}

func TestConfigRoundTrip(t *testing.T) {
	isolate(t)
	c, _ := Load()
	c.Privacy.Clients = []Pseudonym{{Pattern: "Acme", Replace: "client-a"}}
	c.Worker.Plugins = []string{"example-plugin"}
	c.Rules = map[string]string{"read-budget": "note"}
	c.WorkerWindow["sonnet"] = 200000
	c.TypeSafeKey = "file-key"
	if err := Save(c); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Privacy.Clients) != 1 || got.Privacy.Clients[0].Replace != "client-a" || got.Worker.Plugins[0] != "example-plugin" {
		t.Errorf("lists lost: %+v", got)
	}
	if got.Rule("read-budget") != "note" || got.Rule("other") != "on" {
		t.Errorf("rules %v", got.Rules)
	}
	if got.WorkerWindow["sonnet"] != 200000 || got.WorkerWindow["opus"] != 400000 {
		t.Errorf("window %v", got.WorkerWindow)
	}
	if got.ReadBudget != 8 || got.Launcher != "auto" {
		t.Errorf("defaults lost: %+v", got)
	}
}

func TestConfigBadFile(t *testing.T) {
	dir := isolate(t)
	p := filepath.Join(dir, "quartermaster", "config.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err == nil {
		t.Fatal("want an error for a broken config file")
	}
	if !reflect.DeepEqual(c, Defaults()) {
		t.Errorf("broken file: got %+v, want the defaults", c)
	}
}

func TestConfigNullKeepsDefaults(t *testing.T) {
	dir := isolate(t)
	p := filepath.Join(dir, "quartermaster", "config.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"privacy":{"clients":null},"worker":{"plugins":null},"rules":null,"project_plugins":null,"project_mcp":null,"worker_window":null,"notify":null,"up":{"claude_args":null}}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c, Defaults()) {
		t.Errorf("nulls: got %+v, want the defaults", c)
	}
}

func TestKeyEnvFirst(t *testing.T) {
	isolate(t)
	c := Config{TypeSafeKey: "from-file"}
	if c.Key() != "from-file" {
		t.Errorf("file key: got %q", c.Key())
	}
	t.Setenv("TYPESAFE_API_KEY", "from-env")
	if c.Key() != "from-env" {
		t.Errorf("env key: got %q", c.Key())
	}
}

func TestReportWorkerMarkersDefaultEmptyAndLoad(t *testing.T) {
	if m := Defaults().Report.WorkerMarkers; m == nil || len(m) != 0 {
		t.Errorf("default markers = %#v, want an empty list", m)
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"report":{"worker_markers":["^X"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil || len(c.Report.WorkerMarkers) != 1 {
		t.Errorf("markers = %v, err %v", c.Report.WorkerMarkers, err)
	}
}

func TestUpClaudeArgsDefaultEmptyAndLoad(t *testing.T) {
	if a := Defaults().Up.ClaudeArgs; a == nil || len(a) != 0 {
		t.Errorf("default up.claude_args = %#v, want an empty list", a)
	}
	dir := isolate(t)
	p := filepath.Join(dir, "quartermaster", "config.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"up":{"claude_args":["--permission-mode","acceptEdits"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"--permission-mode", "acceptEdits"}; !reflect.DeepEqual(c.Up.ClaudeArgs, want) {
		t.Errorf("up.claude_args = %#v, want %#v", c.Up.ClaudeArgs, want)
	}
}

func TestUpCoordinatorTab(t *testing.T) {
	if got, err := Defaults().CoordinatorTab(); err != nil || got != "new" {
		t.Fatalf("default = %q, %v; want new", got, err)
	}
	for _, tc := range []struct {
		json string
		want string
		bad  bool
	}{
		{`{"up":{"coordinator_tab":"here"}}`, "here", false},
		{`{"up":{"coordinator_tab":"new"}}`, "new", false},
		{`{"up":{"coordinator_tab":""}}`, "new", false},
		{`{"up":{"coordinator_tab":null}}`, "new", false},
		{`{"up":{"coordinator_tab":"Here"}}`, "", true},
		{`{"up":{"coordinator_tab":"tab"}}`, "", true},
	} {
		dir := isolate(t)
		p := filepath.Join(dir, "quartermaster", "config.json")
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(tc.json), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := Load()
		if err != nil {
			t.Fatalf("%s: load: %v", tc.json, err)
		}
		got, err := c.CoordinatorTab()
		if (err != nil) != tc.bad || got != tc.want {
			t.Errorf("%s: CoordinatorTab() = %q, %v; want %q, bad=%v", tc.json, got, err, tc.want, tc.bad)
		}
	}
}

func TestHandoffShare(t *testing.T) {
	c := Defaults()
	if v, err := c.HandoffShare(); err != nil || v != 0.85 {
		t.Fatalf("unset: %v %v", v, err)
	}
	for _, tc := range []struct {
		json string
		want float64
		bad  bool
	}{
		{`{"worker":{"handoff_percent":70}}`, 0.70, false},
		{`{"worker":{"handoff_percent":99}}`, 0.99, false},
		{`{"worker":{"handoff_percent":1}}`, 0.01, false},
		{`{"worker":{"handoff_percent":0}}`, 0.85, true},
		{`{"worker":{"handoff_percent":100}}`, 0.85, true},
		{`{"worker":{"handoff_percent":-5}}`, 0.85, true},
		{`{"worker":{}}`, 0.85, false},
	} {
		var c Config
		if err := json.Unmarshal([]byte(tc.json), &c); err != nil {
			t.Fatalf("%s: %v", tc.json, err)
		}
		v, err := c.HandoffShare()
		if (err != nil) != tc.bad || v != tc.want {
			t.Fatalf("%s: %v %v", tc.json, v, err)
		}
		if tc.bad && !strings.Contains(err.Error(), "worker.handoff_percent") {
			t.Errorf("%s: error %q does not name the key", tc.json, err)
		}
	}
}

func TestSilentThreshold(t *testing.T) {
	c := Defaults()
	if d, err := c.SilentThreshold(); err != nil || d != 10*time.Minute {
		t.Fatalf("unset: %v %v", d, err)
	}
	for _, tc := range []struct {
		json string
		want time.Duration
		bad  bool
	}{
		{`{"watch":{"silent_minutes":3}}`, 3 * time.Minute, false},
		{`{"watch":{"silent_minutes":0}}`, 0, true},
		{`{"watch":{"silent_minutes":-1}}`, 0, true},
		{`{"watch":{"silent_minutes":9223372036854775807}}`, 0, true},
		{`{"watch":{}}`, 10 * time.Minute, false},
	} {
		var c Config
		if err := json.Unmarshal([]byte(tc.json), &c); err != nil {
			t.Fatalf("%s: %v", tc.json, err)
		}
		d, err := c.SilentThreshold()
		if (err != nil) != tc.bad || (!tc.bad && d != tc.want) {
			t.Fatalf("%s: %v %v", tc.json, d, err)
		}
	}
}

func TestSilentMinutesWrongTypeFailsLikeOtherKeys(t *testing.T) {
	// A wrongly typed key fails the whole file, as read_budget does.
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	for _, body := range []string{`{"read_budget":"8"}`, `{"watch":{"silent_minutes":"7"}}`} {
		if err := os.MkdirAll(filepath.Join(dir, "quartermaster"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "quartermaster", "config.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(); err == nil {
			t.Fatalf("%s: no error", body)
		}
	}
}

func TestConfigBadPseudonym(t *testing.T) {
	cases := []struct{ name, clients, want string }{
		{"empty replace", `[{"pattern":"acme","replace":""}]`, `privacy.clients[0]: replace is empty`},
		{"pattern matching empty text", `[{"pattern":"acme","replace":"ClientA"},{"pattern":"a*","replace":"ClientB"}]`, `privacy.clients[1]: pattern matches empty text`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := isolate(t)
			p := filepath.Join(dir, "quartermaster", "config.json")
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				t.Fatal(err)
			}
			body := `{"privacy":{"local_model":"required","clients":` + c.clients + `}}`
			if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := Load()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %v, want one saying %q", err, c.want)
			}
			// The config comes back as read, so a caller that runs on it after
			// the error keeps every client name and the local-model setting.
			if got.Privacy.LocalModel != "required" || len(got.Privacy.Clients) == 0 {
				t.Errorf("config = %+v, want it as read", got.Privacy)
			}
		})
	}
}
