package up

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/paths"
	"github.com/IZZY-Agency/quartermaster/internal/policy"
	"github.com/IZZY-Agency/quartermaster/internal/route"
	"github.com/IZZY-Agency/quartermaster/internal/spawn"
	"github.com/IZZY-Agency/quartermaster/internal/transcript"
)

// coordinatorScript is the coordinator's script: resume sid, or start a new
// session sid with prompt (a summary file, "" for none).
func coordinatorScript(p *Project, sid, prompt string, resume bool, exe string) (string, error) {
	note, err := HandoffPath(p.ID, p.Coordinator.Name)
	if err != nil {
		return "", err
	}
	return spawn.CoordinatorScript(spawn.CoordinatorLaunch{
		Name: p.Coordinator.Name, SessionID: sid, Cwd: p.Root, QMDir: qmDirOf(exe),
		ProjectID: p.ID, Workspace: p.Workspace, HandoffPath: note,
		PromptPath: prompt, Args: p.ClaudeArgs, Resume: resume,
	})
}

// HandoffDir is <state>/handoff, the folder of the handoff notes: one
// folder per project, <state>/handoff/<project id>/<name>.md.
func HandoffDir() (string, error) {
	state, err := paths.StateDir()
	if err != nil {
		return "", fmt.Errorf("handoff folder: %w", err)
	}
	return filepath.Join(state, "handoff"), nil
}

// HandoffPath is <state>/handoff/<projectID>/NAME.md; it creates the
// folder (0700).
func HandoffPath(projectID, name string) (string, error) {
	if !safeName(projectID) || !safeName(name) {
		return "", fmt.Errorf("handoff note: bad project id %q or name %q", projectID, name)
	}
	base, err := HandoffDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, projectID)
	if err := ensureDir(base); err != nil {
		return "", fmt.Errorf("handoff folder: %w", err)
	}
	if err := ensureDir(dir); err != nil {
		return "", fmt.Errorf("handoff folder: %w", err)
	}
	return filepath.Join(dir, name+".md"), nil
}

// qmDirOf is the folder of qm's executable, symlinks resolved; "" when it
// cannot be read.
func qmDirOf(exe string) string {
	if exe == "" {
		return ""
	}
	real, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return ""
	}
	return filepath.Dir(real)
}

// workerProfile is the logged launch profile of a worker step, re-resolved
// for today (spec 5.5): the qm-worker plugin folder found again (BuildProfile
// puts it first), the permission flags capped at the coordinator's current
// ones, and the project tie in the environment.
func workerProfile(p *Project, s Step, exe string) (route.Profile, error) {
	if !hasProfile(s.Record) {
		return route.Profile{}, fmt.Errorf("%s: no launch profile in its Record", s.Name)
	}
	prof := *s.Record.Decision.Profile
	prof.PluginsOn = slices.Clone(prof.PluginsOn)
	prof.PluginsOff = slices.Clone(prof.PluginsOff)
	prof.PluginDirs = slices.Clone(prof.PluginDirs)
	prof.Env = maps.Clone(prof.Env)
	dir, err := spawn.WorkerPluginDir(exe)
	if err != nil {
		return route.Profile{}, err
	}
	if len(prof.PluginDirs) > 0 {
		prof.PluginDirs[0] = dir
	} else {
		prof.PluginDirs = []string{dir}
	}
	prof.Permission = spawn.CapPermission(prof.Permission, p.ClaudeArgs)
	if prof.Env == nil {
		prof.Env = map[string]string{}
	}
	prof.Env["QM_PROJECT_ID"], prof.Env["QM_WORKSPACE"] = p.ID, p.Workspace
	return prof, nil
}

// The flags adoptedArgs sorts out of a hand-started worker's arguments.
const (
	bypassFlag = "--dangerously-skip-permissions"
	allowFlag  = "--allow-dangerously-skip-permissions"
	modeFlag   = "--permission-mode"
)

// adoptedArgs is a hand-started worker's claude arguments at its resume
// (adopt design 4.3): the weaker of its own permission flags and the
// coordinator's (spawn.LowerPermission), then --allow-dangerously-skip-permissions when it
// had it and the coordinator runs with bypass or carries it too, then its
// --model and --effort in order. Every other argument is dropped, so
// nothing outside the kept list reaches the resume line.
func adoptedArgs(args, coordinator []string) []string {
	var perm, rest []string
	allow := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == bypassFlag || strings.HasPrefix(a, modeFlag+"="):
			perm = append(perm, a)
		case a == modeFlag:
			if i+1 < len(args) {
				perm = append(perm, a, args[i+1])
				i++
			}
		case a == allowFlag:
			allow = true
		case a == "--model" || a == "--effort":
			if i+1 < len(args) {
				rest = append(rest, a, args[i+1])
				i++
			}
		case strings.HasPrefix(a, "--model=") || strings.HasPrefix(a, "--effort="):
			rest = append(rest, a)
		}
	}
	out := slices.Clone(spawn.LowerPermission(perm, coordinator))
	if limit, _ := spawn.PermissionOf(coordinator, 0); allow && (slices.Contains(limit, bypassFlag) || slices.Contains(coordinator, allowFlag)) {
		out = append(out, allowFlag)
	}
	return append(out, rest...)
}

// writeSummary builds the restart summary of a fresh coordinator (spec 7)
// and writes it; it returns the file's path.
func writeSummary(p *Project, steps []Step, s Step, d Deps) (string, error) {
	in := SummaryInput{Project: *p, Old: s.Candidate, Threshold: policy.UpCoordinatorSmall, Steps: steps, Git: ReadGit(p.Root, d.Run), Now: d.now()}
	if s.Handoff != "" {
		if b, err := os.ReadFile(s.Handoff); err == nil {
			in.HandoffNote = string(b)
		}
	}
	if s.Transcript != "" {
		// Without the owner's messages the summary still serves; it says "none".
		in.OwnerMessages, _ = transcript.OwnerMessages(s.Transcript, summaryMessages, summaryMessageLen)
	}
	return WriteSummary(p.ID, Summary(in))
}

// writeNote writes a fresh worker's first prompt (spec 5.5): three lines from
// qm, then its handoff note. It returns the file's path.
func writeNote(p *Project, s Step) (string, error) {
	note, err := os.ReadFile(s.Handoff)
	if err != nil {
		return "", fmt.Errorf("handoff note of %s: %w", s.Name, err)
	}
	var b strings.Builder
	if s.Last.At.IsZero() {
		fmt.Fprintf(&b, "You continue the task of worker %s, whose session ended at an unknown context.\n", s.Name)
	} else {
		fmt.Fprintf(&b, "You continue the task of worker %s, whose session ended at %s tokens of context.\n", s.Name, kTokensUp(s.Last.Context))
	}
	b.WriteString("Its handoff note follows.\n")
	if s.Transcript == "" {
		b.WriteString("Its transcript is missing; the note is all there is.\n\n")
	} else {
		fmt.Fprintf(&b, "Its full transcript is %s; read from it only what the note lacks.\n\n", s.Transcript)
	}
	b.Write(note)
	return writeUpFile(p.ID, s.Name+"-", b.String())
}

// writeUpFile writes text to <state>/up/<projectID>/<prefix><unixnano>.md,
// mode 0600 in a 0700 folder, never over an existing file. Notes there older
// than spawn.ScriptAge are removed first.
func writeUpFile(projectID, prefix, text string) (string, error) {
	if projectID == "" || strings.ContainsAny(projectID, "/\x00") || projectID == "." || projectID == ".." {
		return "", fmt.Errorf("prompt file: bad project id %q", projectID)
	}
	state, err := paths.StateDir()
	if err != nil {
		return "", fmt.Errorf("prompt file: %w", err)
	}
	dir := filepath.Join(state, "up", projectID)
	if err := ensureDir(dir); err != nil {
		return "", fmt.Errorf("prompt file: %w", err)
	}
	sweepNotes(dir, spawn.ScriptAge)
	for range 100 {
		path := filepath.Join(dir, fmt.Sprintf("%s%d.md", prefix, time.Now().UnixNano()))
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("prompt file: %w", err)
		}
		if _, err := f.WriteString(text); err != nil {
			_ = f.Close()
			return "", fmt.Errorf("prompt file: %w", err)
		}
		if err := f.Close(); err != nil {
			return "", fmt.Errorf("prompt file: %w", err)
		}
		return path, nil
	}
	return "", errors.New("prompt file: no free file name")
}

// sweepNotes removes the .md files in dir older than age.
func sweepNotes(dir string, age time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		fi, err := e.Info()
		if err == nil && fi.Mode().IsRegular() && strings.HasSuffix(e.Name(), ".md") && time.Since(fi.ModTime()) > age {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
