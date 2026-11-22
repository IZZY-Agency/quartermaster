package spawn

import (
	"strings"
	"testing"

	"github.com/IZZY-Agency/quartermaster/internal/route"
)

func TestResumeScriptMatchesLaunch(t *testing.T) {
	hermetic(t)
	spawnerPath = func() string { return "/opt/homebrew/bin:/usr/bin" }
	p, _ := profileFor(t, "opus", "high", []string{"--permission-mode", "acceptEdits"})
	p.MCPConfig = "/cfg/mcp.json"
	p.Env["QM_PROJECT_ID"] = "try-00000000"
	p.Env["QM_WORKSPACE"] = "try"
	tier := route.Tier{Model: "opus", Effort: "high"}
	const id = "00000000-0000-4000-8000-000000000001"
	launchS := LaunchScript(p, tier, "task-a", id, "/work/my repo", "/briefs/a.md", "/opt/qm/bin")
	resume := ResumeScript(p, tier, "task-a", id, "/work/my repo", "/opt/qm/bin")

	prompt := " \"$(printf '\\n'; cat /briefs/a.md)\"\n"
	want := strings.Replace(strings.TrimSuffix(launchS, prompt)+"\n", " --session-id "+id, " --resume "+id, 1)
	if !strings.HasSuffix(launchS, prompt) || resume != want {
		t.Errorf("resume script =\n%s\nwant the launch script with --resume and no prompt:\n%s", resume, want)
	}
	if strings.Contains(resume, "--session-id") || strings.Contains(resume, "cat ") {
		t.Errorf("resume script still sets the id or reads a prompt:\n%s", resume)
	}
	if !strings.Contains(resume, " QM_PROJECT_ID=try-00000000 QM_WORKSPACE=try ") {
		t.Errorf("resume script lacks the project tie:\n%s", resume)
	}
}

// coordinatorLaunch is a coordinator launch with every field but Resume and
// PromptPath set.
func coordinatorLaunch(t *testing.T) (CoordinatorLaunch, string) {
	t.Helper()
	hermetic(t)
	spawnerPath = func() string { return "/usr/bin" }
	return CoordinatorLaunch{
		Name: "try-coord", SessionID: "00000000-0000-4000-8000-000000000002",
		Cwd: "/work/try", QMDir: "/opt/qm/bin", ProjectID: "try-00000000", Workspace: "try",
		HandoffPath: "/state/handoff/try-coord.md",
		Args:        []string{"--permission-mode", "acceptEdits"},
	}, "QM_HANDOFF=/state/handoff/try-coord.md"
}

func TestCoordinatorScriptFirstRun(t *testing.T) {
	c, handoff := coordinatorLaunch(t)
	want := "#!/bin/sh\n" +
		"PATH=/opt/qm/bin:/usr/bin:\"$PATH\"\nexport PATH\n" +
		"cd /work/try || exit 1\n" +
		"exec env QM_PROJECT_ID=try-00000000 QM_WORKSPACE=try QM_SESSION_NAME=try-coord " + handoff +
		" claude --permission-mode acceptEdits -n try-coord --session-id 00000000-0000-4000-8000-000000000002\n"
	if got, err := CoordinatorScript(c); err != nil || got != want {
		t.Errorf("script = %v\n%s\nwant\n%s", err, got, want)
	}
}

func TestCoordinatorScriptResume(t *testing.T) {
	c, handoff := coordinatorLaunch(t)
	c.Resume = true
	c.Args = nil
	want := "#!/bin/sh\n" +
		"PATH=/opt/qm/bin:/usr/bin:\"$PATH\"\nexport PATH\n" +
		"cd /work/try || exit 1\n" +
		"exec env QM_PROJECT_ID=try-00000000 QM_WORKSPACE=try QM_SESSION_NAME=try-coord " + handoff +
		" claude -n try-coord --resume 00000000-0000-4000-8000-000000000002\n"
	if got, err := CoordinatorScript(c); err != nil || got != want {
		t.Errorf("script = %v\n%s\nwant\n%s", err, got, want)
	}
}

func TestCoordinatorScriptPrompt(t *testing.T) {
	c, handoff := coordinatorLaunch(t)
	c.PromptPath = "/state/up/try's summary.md"
	want := "#!/bin/sh\n" +
		"PATH=/opt/qm/bin:/usr/bin:\"$PATH\"\nexport PATH\n" +
		"cd /work/try || exit 1\n" +
		"exec env QM_PROJECT_ID=try-00000000 QM_WORKSPACE=try QM_SESSION_NAME=try-coord " + handoff +
		" claude --permission-mode acceptEdits -n try-coord --session-id 00000000-0000-4000-8000-000000000002" +
		" \"$(printf '\\n'; cat '/state/up/try'\\''s summary.md')\"\n"
	if got, err := CoordinatorScript(c); err != nil || got != want {
		t.Errorf("script = %v\n%s\nwant\n%s", err, got, want)
	}
}

func TestCoordinatorScriptBadInput(t *testing.T) {
	for name, mod := range map[string]func(*CoordinatorLaunch){
		"no session id":  func(c *CoordinatorLaunch) { c.SessionID = "" },
		"empty name":     func(c *CoordinatorLaunch) { c.Name = "" },
		"dot":            func(c *CoordinatorLaunch) { c.Name = "." },
		"dot dot":        func(c *CoordinatorLaunch) { c.Name = ".." },
		"name with path": func(c *CoordinatorLaunch) { c.Name = "a/b" },
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := coordinatorLaunch(t)
			mod(&c)
			if s, err := CoordinatorScript(c); err == nil || s != "" {
				t.Errorf("CoordinatorScript() = %q, %v; want an error", s, err)
			}
		})
	}

	// No handoff path leaves QM_HANDOFF out.
	c, _ := coordinatorLaunch(t)
	c.HandoffPath = ""
	if s, err := CoordinatorScript(c); err != nil || strings.Contains(s, "QM_HANDOFF") {
		t.Errorf("script = %v\n%s\nwant no QM_HANDOFF", err, s)
	}
}

func TestPlainResumeScript(t *testing.T) {
	hermetic(t)
	old := spawnerPath
	t.Cleanup(func() { spawnerPath = old })
	spawnerPath = func() string { return "/usr/bin" }
	const id = "00000000-0000-4000-8000-000000000021"
	env := map[string]string{"QM_WORKSPACE": "repo", "QM_PROJECT_ID": "repo-1a2b3c4d", "OTHER": "x"}
	s := PlainResumeScript([]string{"--permission-mode", "acceptEdits", "--model", "opus"}, env, "w-hand", id, "/work/repo", "/opt/qm/bin")
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	want := "exec env QM_PROJECT_ID=repo-1a2b3c4d QM_WORKSPACE=repo claude --permission-mode acceptEdits --model opus -n w-hand --resume " + id
	if got := lines[len(lines)-1]; got != want {
		t.Errorf("last line =\n%s\nwant\n%s", got, want)
	}
	if !strings.HasPrefix(s, "#!/bin/sh\nPATH=/opt/qm/bin:/usr/bin:\"$PATH\"\n") || !strings.Contains(s, "cd /work/repo || exit 1\n") {
		t.Errorf("script lacks qm's PATH or the cd:\n%s", s)
	}
	if strings.Contains(s, "QM_ROLE") {
		t.Errorf("a hand-started worker's script sets QM_ROLE:\n%s", s)
	}
}

func TestValidName(t *testing.T) {
	for name, want := range map[string]bool{"w-hand": true, "proj-coord": true, "": false, "-x": false, "a b": false, "a/b": false} {
		if got := ValidName(name); got != want {
			t.Errorf("ValidName(%q) = %v, want %v", name, got, want)
		}
	}
}
