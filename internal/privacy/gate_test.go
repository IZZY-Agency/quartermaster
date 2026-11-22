package privacy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/config"
	"github.com/IZZY-Agency/quartermaster/internal/policy"
)

// The test binary doubles as a fake qm-privcheck and a fake gitleaks: when
// fakeEnv is "1" it acts as gitleaks when started with "stdin", else as the
// helper, and exits instead of running tests.
const fakeEnv = "QM_TEST_FAKE"

// Fake helper settings, read from the environment of the fake process.
const (
	fakeNames   = "QM_TEST_NAMES"   // comma-separated candidates names returns
	fakePersons = "QM_TEST_PERSONS" // comma-separated candidates ask confirms
	fakeAsk     = "QM_TEST_ASK"     // "unavailable": ask exits 3; else the leftover answer
	fakeSecret  = "QM_TEST_SECRET"  // comma-separated secrets the fake gitleaks reports
	fakeAskLog  = "QM_TEST_ASK_LOG" // file the fake helper appends each ask prompt to
	fakeRunLog  = "QM_TEST_RUN_LOG" // file the fake helper appends "start" and "end" lines to, per ask process
)

func TestMain(m *testing.M) {
	if os.Getenv(fakeEnv) == "1" {
		if slices.Contains(os.Args[1:], "stdin") {
			os.Exit(fakeGitleaks())
		}
		os.Exit(fakePrivcheck())
	}
	os.Exit(m.Run())
}

func fakePrivcheck() int {
	switch os.Args[len(os.Args)-1] {
	case "names":
		var in []struct{ ID, Text string }
		if err := json.NewDecoder(os.Stdin).Decode(&in); err != nil {
			return 1
		}
		out := []map[string]any{}
		for _, it := range in {
			names := []string{}
			for _, n := range strings.Split(os.Getenv(fakeNames), ",") {
				if n != "" && strings.Contains(it.Text, n) {
					names = append(names, n)
				}
			}
			out = append(out, map[string]any{"id": it.ID, "names": names})
		}
		_ = json.NewEncoder(os.Stdout).Encode(out)
		return 0
	case "ask":
		if os.Getenv(fakeAsk) == "unavailable" {
			fmt.Fprintln(os.Stderr, "unavailable")
			return 3
		}
		var in []struct{ Key, Instructions, Prompt string }
		if err := json.NewDecoder(os.Stdin).Decode(&in); err != nil {
			return 1
		}
		if p := os.Getenv(fakeRunLog); p != "" {
			logRun(p, "start")
			time.Sleep(200 * time.Millisecond)
			defer logRun(p, "end")
		}
		out := []map[string]string{}
		for _, it := range in {
			if p := os.Getenv(fakeAskLog); p != "" {
				f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
				if err == nil {
					_, _ = f.WriteString(it.Prompt + "\n----\n")
					_ = f.Close()
				}
			}
			answer := os.Getenv(fakeAsk)
			if strings.HasPrefix(it.Prompt, "Text around the word:") {
				answer = "no"
				for _, p := range strings.Split(os.Getenv(fakePersons), ",") {
					if p != "" && strings.Contains(it.Prompt, "Is \""+p+"\"") {
						answer = "Yes."
					}
				}
			}
			out = append(out, map[string]string{"key": it.Key, "answer": answer})
		}
		_ = json.NewEncoder(os.Stdout).Encode(out)
		return 0
	}
	return 2
}

// logRun appends one line to the run log, in a single write.
func logRun(path, line string) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err == nil {
		_, _ = f.WriteString(line + "\n")
		_ = f.Close()
	}
}

func fakeGitleaks() int {
	s := os.Getenv(fakeSecret)
	if s == "" {
		fmt.Println("[]")
		return 0
	}
	findings := []map[string]string{}
	for _, one := range strings.Split(s, ",") {
		findings = append(findings, map[string]string{"RuleID": "generic", "Secret": one})
	}
	_ = json.NewEncoder(os.Stdout).Encode(findings)
	return 1
}

// fakes points QM_PRIVCHECK and QM_GITLEAKS at the test binary (or at a
// missing path when the provider should be absent) and sets the fake's mode.
func fakes(t *testing.T, helper, gitleaks bool, env map[string]string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	t.Setenv("QM_PRIVCHECK", missing)
	t.Setenv("QM_GITLEAKS", missing)
	t.Setenv(fakeEnv, "1")
	if helper {
		t.Setenv("QM_PRIVCHECK", self)
	}
	if gitleaks {
		t.Setenv("QM_GITLEAKS", self)
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
}

// checker builds a Checker from the discovered providers.
func checker(localModel string) *Checker {
	return New(Options{Helper: FindHelper(), Gitleaks: FindGitleaks(), LocalModel: localModel})
}

func TestFindHelperMissing(t *testing.T) {
	fakes(t, false, false, nil)
	if h := FindHelper(); h != "" {
		t.Errorf("FindHelper = %q, want none", h)
	}
	if g := FindGitleaks(); g != "" {
		t.Errorf("FindGitleaks = %q, want none", g)
	}
}

func TestGateHelperMissing(t *testing.T) {
	fakes(t, false, true, nil)
	res, err := checker("required").Gate(context.Background(), "Ask Pat Example about it.")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Local || res.Reason != "privacy helper unavailable" {
		t.Errorf("result = %+v, want Local with reason", res)
	}
	if res.Text != "" {
		t.Errorf("local result carries text %q", res.Text)
	}
}

func TestGateHelperAskUnavailableRequired(t *testing.T) {
	fakes(t, true, true, map[string]string{fakeNames: "Pat Example", fakeAsk: "unavailable"})
	res, err := checker("required").Gate(context.Background(), "Ask Pat Example about it.")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Local || res.Reason != "privacy helper unavailable" {
		t.Errorf("result = %+v", res)
	}
}

func TestGateWithoutFoundationModel(t *testing.T) {
	fakes(t, true, true, map[string]string{fakeNames: "Pat Example,Astro", fakeAsk: "unavailable"})
	res, err := checker("auto").Gate(context.Background(), "Ask Pat Example about Astro.")
	if err != nil {
		t.Fatal(err)
	}
	if res.Local {
		t.Errorf("Local = true, want false: %+v", res)
	}
	if res.Text != "Ask <PERSON_1> about <PERSON_2>." {
		t.Errorf("Text = %q, want every candidate redacted", res.Text)
	}
	for _, off := range []string{"name confirmation", "leftover check"} {
		if !slices.Contains(res.Off, off) {
			t.Errorf("Off = %v, want %q", res.Off, off)
		}
	}
	if res.Counts["PERSON"] != 2 {
		t.Errorf("Counts = %v", res.Counts)
	}
}

func TestGateLocalModelOff(t *testing.T) {
	log := filepath.Join(t.TempDir(), "asks")
	fakes(t, true, true, map[string]string{fakeNames: "Astro", fakeAsk: "no", fakeAskLog: log})
	res, err := checker("off").Gate(context.Background(), "Build Astro.")
	if err != nil {
		t.Fatal(err)
	}
	if res.Local || res.Text != "Build <PERSON_1>." {
		t.Errorf("result = %+v", res)
	}
	if _, err := os.Stat(log); err == nil {
		t.Error("local_model off still asked the model")
	}
	if !slices.Contains(res.Off, "leftover check") || !slices.Contains(res.Off, "name confirmation") {
		t.Errorf("Off = %v", res.Off)
	}
}

func TestGateConfirmsNames(t *testing.T) {
	log := filepath.Join(t.TempDir(), "asks")
	fakes(t, true, true, map[string]string{
		fakeNames: "Pat Example,Astro", fakePersons: "Pat Example", fakeAsk: "no", fakeAskLog: log,
	})
	res, err := checker("auto").Gate(context.Background(), "Ask Pat Example about Astro.")
	if err != nil {
		t.Fatal(err)
	}
	if res.Local || res.Text != "Ask <PERSON_1> about Astro." || len(res.Off) != 0 {
		t.Errorf("result = %+v", res)
	}
	asked, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(asked), "Excerpt:\n\"\"\"\nAsk [x] about Astro.\n\"\"\"") {
		t.Errorf("leftover check did not read the redacted text with neutral placeholders:\n%s", asked)
	}
}

func TestGateLeftoverYesIsLocal(t *testing.T) {
	fakes(t, true, true, map[string]string{fakeAsk: "yes"})
	res, err := checker("auto").Gate(context.Background(), "Plain text with nothing in it.")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Local || res.Reason != "still private after redaction" || res.Text != "" {
		t.Errorf("result = %+v", res)
	}
}

func TestGateLeftoverChunks(t *testing.T) {
	log := filepath.Join(t.TempDir(), "asks")
	fakes(t, true, true, map[string]string{fakeAsk: "no", fakeAskLog: log})
	text := strings.Repeat("word ", 2400) // 12,000 characters: three chunks
	if _, err := checker("auto").Gate(context.Background(), text); err != nil {
		t.Fatal(err)
	}
	asked, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(asked), "Excerpt:"); n != 3 {
		t.Errorf("leftover check read %d chunks, want 3", n)
	}
}

func TestGateWithoutGitleaks(t *testing.T) {
	fakes(t, true, false, map[string]string{fakeAsk: "no"})
	res, err := checker("auto").Gate(context.Background(), "token="+testToken)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "token=<TOKEN_1>" {
		t.Errorf("Text = %q", res.Text)
	}
	if !slices.Contains(res.Off, "gitleaks") {
		t.Errorf("Off = %v, want gitleaks", res.Off)
	}
}

func TestGateGitleaksSecret(t *testing.T) {
	fakes(t, true, true, map[string]string{fakeAsk: "no", fakeSecret: "hunter2-but-longer"})
	res, err := checker("auto").Gate(context.Background(), "password: hunter2-but-longer")
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "password: <SECRET_1>" || res.Counts["SECRET"] != 1 {
		t.Errorf("result = %+v", res)
	}
}

func TestGateClients(t *testing.T) {
	fakes(t, true, true, map[string]string{fakeAsk: "no"})
	c := New(Options{
		Helper: FindHelper(), Gitleaks: FindGitleaks(), LocalModel: "auto",
		Clients: []config.Pseudonym{{Pattern: "example corp", Replace: "ClientA"}},
	})
	res, err := c.Gate(context.Background(), "Fix the Example Corp shop.")
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "Fix the ClientA shop." {
		t.Errorf("Text = %q", res.Text)
	}
}

func TestGateHelperFailureIsError(t *testing.T) {
	fakes(t, true, true, nil)
	broken := filepath.Join(t.TempDir(), "broken")
	if err := os.WriteFile(broken, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	c := New(Options{Helper: broken, Gitleaks: FindGitleaks(), LocalModel: "auto"})
	if _, err := c.Gate(context.Background(), "text"); err == nil {
		t.Error("a failing helper returned no error")
	}
}

func TestGateGitleaksSecretWithDigits(t *testing.T) {
	secret := "zz-1234567890123-QwErTyUiOpAsDfGh"
	fakes(t, true, true, map[string]string{fakeAsk: "no", fakeSecret: secret})
	res, err := checker("auto").Gate(context.Background(), "token "+secret+" here")
	if err != nil {
		t.Fatal(err)
	}
	if res.Local || res.Text != "token <SECRET_1> here" {
		t.Errorf("result = %+v", res)
	}
}

func TestGateSecretNotFoundIsLocal(t *testing.T) {
	fakes(t, true, true, map[string]string{fakeAsk: "no", fakeSecret: "not-in-the-text-secret"})
	res, err := checker("auto").Gate(context.Background(), "plain text")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Local || res.Reason != "a secret could not be redacted" || res.Text != "" {
		t.Errorf("result = %+v", res)
	}
}

func TestGateLeftoverNotSureIsLocal(t *testing.T) {
	fakes(t, true, true, map[string]string{fakeAsk: "not sure"})
	res, err := checker("auto").Gate(context.Background(), "Plain text with nothing in it.")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Local || res.Reason != "still private after redaction" {
		t.Errorf("result = %+v", res)
	}
}

func TestGateLeftoverNoWithPunctuation(t *testing.T) {
	fakes(t, true, true, map[string]string{fakeAsk: " No."})
	res, err := checker("auto").Gate(context.Background(), "Plain text with nothing in it.")
	if err != nil {
		t.Fatal(err)
	}
	if res.Local {
		t.Errorf("result = %+v", res)
	}
}

func TestGateOverlappingSecrets(t *testing.T) {
	whole := "user:Zq8vR2mK9xT4wB7nL3pF6hJ1cY5dG0sE"
	part := "Zq8vR2mK9xT4wB7nL3pF6hJ1cY5dG0sE"
	fakes(t, true, true, map[string]string{fakeAsk: "no", fakeSecret: part + "," + whole})
	res, err := checker("auto").Gate(context.Background(), "login "+whole+" now")
	if err != nil {
		t.Fatal(err)
	}
	if res.Local || res.Text != "login <SECRET_1> now" {
		t.Errorf("result = %+v", res)
	}
}

func TestGateSecretWithHomePath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || len(home) < 2 {
		t.Skip("no home folder")
	}
	secret := strings.TrimRight(home, "/") + "/.keys/Zq8vR2mK9xT4wB7nL3pF6hJ1cY5dG0sE"
	fakes(t, true, true, map[string]string{fakeAsk: "no", fakeSecret: secret})
	res, err := checker("auto").Gate(context.Background(), "key file "+secret)
	if err != nil {
		t.Fatal(err)
	}
	if res.Local || res.Text != "key file <SECRET_1>" {
		t.Errorf("result = %+v", res)
	}
}

// TestAskRunsHelpersConcurrently: a batch is split across at most
// policy.HelperConcurrency helper processes, run at once, and the bound holds
// across concurrent Gate calls of one Checker.
func TestAskRunsHelpersConcurrently(t *testing.T) {
	runLog := filepath.Join(t.TempDir(), "runs")
	fakes(t, true, true, map[string]string{fakeAsk: "no", fakeRunLog: runLog})
	c := checker("auto")
	items := make([]askItem, 10)
	for i := range items {
		items[i] = askItem{Key: fmt.Sprintf("k%d", i), Instructions: "i", Prompt: "p"}
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	answers := make([]map[string]string, 2)
	for i := range 2 {
		wg.Go(func() { answers[i], errs[i] = c.ask(context.Background(), items) })
	}
	wg.Wait()
	for i := range 2 {
		if errs[i] != nil || len(answers[i]) != len(items) {
			t.Fatalf("ask %d: %v, %d answers; want %d", i, errs[i], len(answers[i]), len(items))
		}
	}
	b, err := os.ReadFile(runLog)
	if err != nil {
		t.Fatal(err)
	}
	running, peak, starts := 0, 0, 0
	for _, l := range strings.Fields(string(b)) {
		if l == "start" {
			running++
			starts++
		} else {
			running--
		}
		peak = max(peak, running)
	}
	if starts != 8 || peak < 2 || peak > policy.HelperConcurrency {
		t.Errorf("%d helper processes, at most %d at once; want 8 (4 per ask), between 2 and %d at once", starts, peak, policy.HelperConcurrency)
	}
}
