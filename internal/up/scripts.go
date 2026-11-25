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

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
	"github.com/IZZY-Agency/quartermaster/internal/paths"
	"github.com/IZZY-Agency/quartermaster/internal/policy"
	"github.com/IZZY-Agency/quartermaster/internal/route"
	"github.com/IZZY-Agency/quartermaster/internal/spawn"
	"github.com/IZZY-Agency/quartermaster/internal/transcript"
)

// coordinatorScript is the coordinator's script: resume sid, or start a new
// session sid with prompt (a summary file, "" for none). What m adds comes
// before the project's own arguments, so no flag left without a value there
// takes it as one.
func coordinatorScript(p *Project, sid, prompt string, resume bool, m ModelChoice, exe string) (string, error) {
	note, err := HandoffPath(p.ID, p.Coordinator.Name)
	if err != nil {
		return "", err
	}
	return spawn.CoordinatorScript(spawn.CoordinatorLaunch{
		Name: p.Coordinator.Name, SessionID: sid, Cwd: p.Root, QMDir: qmDirOf(exe),
		ProjectID: p.ID, Workspace: p.Workspace, HandoffPath: note,
		PromptPath: prompt, Args: append(m.flags(), p.ClaudeArgs...), Resume: resume,
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
// for today (spec 5.5): the qm-worker plugin folder found again, and no
// other plugin folder (BuildProfile writes that one only, so another in the
// Record is not qm's); the MCP config only when it is one of mcpConfigs, the
// config's project_mcp files, else none, with a warning; the permission
// flags capped at the coordinator's current ones (coordinator: its claude
// arguments, coordinatorCap); and the project tie in the environment.
func workerProfile(p *Project, s Step, exe string, coordinator, mcpConfigs []string) (prof route.Profile, warning string, err error) {
	if !hasProfile(s.Record) {
		return route.Profile{}, "", fmt.Errorf("%s: no launch profile in its Record", s.Name)
	}
	prof = *s.Record.Decision.Profile
	prof.PluginsOn = slices.Clone(prof.PluginsOn)
	prof.PluginsOff = slices.Clone(prof.PluginsOff)
	prof.Env = maps.Clone(prof.Env)
	dir, err := spawn.WorkerPluginDir(exe)
	if err != nil {
		return route.Profile{}, "", err
	}
	prof.PluginDirs = []string{dir}
	if prof.MCPConfig != "" && !slices.Contains(mcpConfigs, prof.MCPConfig) {
		prof.MCPConfig = ""
		warning = "its recorded MCP config is not a project_mcp file of the config: started without MCP servers"
	}
	// Never raised: the weaker of the worker's own flags and the
	// coordinator's, as for a hand-started worker; not started when the two
	// have no mode in common.
	perm, err := spawn.LowerPermission(prof.Permission, coordinator)
	if err != nil {
		return route.Profile{}, "", fmt.Errorf("%s: not started, its own permission flags against the coordinator's: %w", s.Name, err)
	}
	prof.Permission = perm
	if prof.Env == nil {
		prof.Env = map[string]string{}
	}
	prof.Env["QM_PROJECT_ID"], prof.Env["QM_WORKSPACE"] = p.ID, p.Workspace
	return prof, warning, nil
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
// nothing outside the kept list reaches the resume line. An error when its
// own permission flags and the coordinator's have no mode in common.
func adoptedArgs(args, coordinator []string) ([]string, error) {
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
	lowered, err := spawn.LowerPermission(perm, coordinator)
	if err != nil {
		return nil, fmt.Errorf("its own permission flags against the coordinator's: %w", err)
	}
	out := slices.Clone(lowered)
	if limit, _ := spawn.PermissionOf(coordinator, 0); allow && (slices.Contains(limit, bypassFlag) || slices.Contains(coordinator, allowFlag)) {
		out = append(out, allowFlag)
	}
	return append(out, rest...), nil
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
// mode 0600 in a 0700 folder, never over an existing file. projectID must
// be a file name (safeName) and prefix a session name spawn.FileName takes
// followed by -. Notes there older
// than spawn.ScriptAge are removed first.
func writeUpFile(projectID, prefix, text string) (string, error) {
	if !safeName(projectID) {
		return "", fmt.Errorf("prompt file: bad project id %q", projectID)
	}
	if name, ok := strings.CutSuffix(prefix, "-"); !ok || !spawn.FileName(name) {
		return "", fmt.Errorf("prompt file: name %q is not usable in a file name", prefix)
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

// LiveArgs are the permission flags of running session id, read off its
// process in procs (livePermission): live is false when id is not in live; an error when its
// process is not listed or does not run claude.
func LiveArgs(id string, live map[string]claudecode.LiveSession, procs []claudecode.Proc) (args []string, ok bool, err error) {
	l, ok := live[id]
	if !ok {
		return nil, false, nil
	}
	for _, pr := range procs {
		if pr.PID != l.PID {
			continue
		}
		if args, isClaude := claudecode.ClaudeArgs(pr.Cmd); isClaude {
			flags, err := livePermission(args)
			if err != nil {
				return nil, true, fmt.Errorf("the claude arguments of its process %d: %w", l.PID, err)
			}
			return flags, true, nil
		}
		break
	}
	return nil, true, fmt.Errorf("the claude arguments of its process %d not read", l.PID)
}

// livePermission is the permission flags read off a running claude's words:
// the words up to --, and of every permission reading among them the weakest
// (spawn.LowerPermission), so a prompt that names a stronger flag cannot
// raise it. The process list keeps no argument boundaries, so a coordinator
// with no permission flag whose prompt (without --) names one is still read
// as having it: the same as reading its saved arguments did before.
// --allow-dangerously-skip-permissions is never passed on from a live read.
// An error when two readings have no mode in common.
func livePermission(words []string) ([]string, error) {
	return spawn.WeakestPermission(words)
}
