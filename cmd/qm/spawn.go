package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
	"github.com/IZZY-Agency/quartermaster/internal/config"
	"github.com/IZZY-Agency/quartermaster/internal/declog"
	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/paths"
	"github.com/IZZY-Agency/quartermaster/internal/plain"
	"github.com/IZZY-Agency/quartermaster/internal/spawn"
	"github.com/IZZY-Agency/quartermaster/internal/up"
)

const spawnUsage = `usage:
  qm spawn worker --brief FILE --type T --size S|M|L --hint m/e --name NAME --cwd DIR [--workspace ID] [--launcher agterm|tmux] [--force m/e --why TEXT] [--dry-run]
  qm spawn review --prompt FILE [--round N] [--base REF] [--diff A..B] [--repo DIR] [--wait] [--force m/e --why TEXT]
  qm spawn review --status ID [--wait]
`

// runSpawn launches a worker or a review after routing it.
func runSpawn(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, spawnUsage)
		return 2
	}
	if isHelp(args) {
		_, _ = fmt.Fprint(stdout, spawnUsage)
		return 0
	}
	switch args[0] {
	case "worker":
		return runSpawnWorker(args[1:], stdout, stderr)
	case "review":
		return spawnReview(args[1:], stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "qm spawn: unknown kind %q\n%s", args[0], spawnUsage)
		return 2
	}
}

// runSpawnWorker runs qm spawn worker. Exit 2 on bad input, 1 when routing
// or launching fails.
func runSpawnWorker(args []string, stdout, stderr io.Writer) int {
	err := spawnWorker(args, stdout, stderr)
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errUsage), errors.Is(err, spawn.ErrInput):
		printErr(stderr, "qm spawn", err)
		_, _ = fmt.Fprint(stderr, spawnUsage)
		return 2
	default:
		printErr(stderr, "qm spawn", err)
		return 1
	}
}

func spawnWorker(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("qm spawn worker", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var req spawn.WorkerRequest
	fs.StringVar(&req.Brief, "brief", "", "the worker brief file")
	fs.StringVar(&req.Type, "type", "", "task type: "+strings.Join(workerTypes, ", "))
	fs.StringVar(&req.Size, "size", "", "S, M or L")
	hint := fs.String("hint", "", "the coordinator's pick, model/effort (required)")
	fs.StringVar(&req.Name, "name", "", "the session's name")
	fs.StringVar(&req.Cwd, "cwd", "", "the folder the worker starts in")
	fs.StringVar(&req.Workspace, "workspace", "", "agterm workspace (default: the coordinator's, next to its session)")
	fs.StringVar(&req.Launcher, "launcher", "", "agterm or tmux (default: the config's launcher)")
	force := fs.String("force", "", "override the table, model/effort (needs --why)")
	fs.StringVar(&req.ForceWhy, "why", "", "why the tier is forced")
	fs.BoolVar(&req.DryRun, "dry-run", false, "print the decision and the launch script; launch and log nothing")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	// spawn.Worker checks the rest of the request (ErrInput).
	switch {
	case !slices.Contains(workerTypes, req.Type):
		return fmt.Errorf("%w: --type %q is not one of %s", errUsage, req.Type, strings.Join(workerTypes, ", "))
	case req.Launcher != "" && req.Launcher != "agterm" && req.Launcher != "tmux":
		return fmt.Errorf("%w: --launcher %q is not agterm or tmux", errUsage, req.Launcher)
	}
	var err error
	if req.Hint, err = parseTier("--hint", *hint); err != nil {
		return err
	}
	if req.Force, err = parseTier("--force", *force); err != nil {
		return err
	}
	// A coordinator started by qm up ties its workers to its project and
	// opens them in its workspace; an explicit --workspace changes only where
	// this worker opens.
	// Without the variables (an adopted coordinator was not started by qm),
	// the spawner's folder names the project; the worker's --cwd does not.
	// The folder ties only a spawner that is one of the project's sessions
	// (its coordinator, an adopted session, a tied worker): a side session
	// the user did not adopt spawns untied (adopt design 4.5).
	upWorkspace := os.Getenv("QM_WORKSPACE")
	if req.UpProject = os.Getenv("QM_PROJECT_ID"); req.UpProject == "" {
		wd, err := upWorkDir()
		var p up.Project
		var found bool
		if err == nil {
			p, found, err = up.ProjectFor(wd)
		}
		switch {
		case err != nil:
			printLine(stderr, "qm spawn: warning: project not read: %v; worker not tied to a project", err)
		case found:
			if why := spawnerTie(&p); why != "" {
				printLine(stderr, "qm spawn: note: %s: worker not tied to project %s", why, p.ID)
				break
			}
			req.UpProject, upWorkspace = p.ID, p.Workspace
		}
	}
	if req.UpProject != "" {
		req.UpWorkspace = upWorkspace
		if req.Workspace == "" {
			req.WorkspaceName = req.UpWorkspace
		}
	}

	rd, err := routeDeps()
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	running, exe, err := paths.Executable()
	if err != nil {
		return err
	}
	// The project's prefix goes in front of the name, here, so the dry run, the
	// launch script, the log and the summary all carry the final name.
	projectRoot := ""
	if req.UpProject != "" {
		if tied, ok, err := up.LoadID(req.UpProject); err == nil && ok {
			projectRoot = tied.Root
		} else {
			printLine(stderr, "qm spawn: note: project %s not read: the prefix is read from the worker's own repository", req.UpProject)
		}
	}
	full, prefixNote, err := up.PrefixName(cfg, req.Cwd, upWorkspace, projectRoot, req.Name)
	if err != nil {
		return err
	}
	if prefixNote != "" {
		printLine(stderr, "qm spawn: note: %s", prefixNote)
	}
	req.BaseName, req.Name = req.Name, full
	deps := spawn.Deps{
		Config: cfg, Ask: rd.ask, Gate: rd.gate, Claude: claudeState(stderr),
		Codex:  rd.cachedCodex,
		Getenv: os.Getenv, PS: spawn.PS, ProcExe: spawn.ProcExe, Pick: launch.Pick, Exe: exe, Running: running, Mark: rd.mark,
		Trusted: func(dir string) (bool, error) {
			cj, err := paths.ClaudeJSON()
			if err != nil {
				return false, err
			}
			return claudecode.Trusted(cj, dir)
		},
	}
	res, err := spawn.Worker(context.Background(), req, deps)
	if err != nil {
		if res.Ref.ID != "" {
			printLine(stdout, "started %s %s (%s)", res.Ref.Launcher, res.Ref.ID, res.Ref.Name)
		}
		return err
	}
	if req.DryRun {
		trust := ""
		if res.TrustNote != "" {
			trust = "trust: " + plain.Line(res.TrustNote) + "\n"
		}
		if res.TintNote != "" {
			printLine(stderr, "qm spawn: note: %s", res.TintNote)
		}
		_, err = fmt.Fprintf(stdout, "%s\nname: %s\nlauncher: %s\n%stint: %s\nbrief: %s, to be copied to %s\nscript (dry run, not written):\n%s", decisionLine(res.Decision), plain.Line(req.Name), plain.Line(res.LauncherNote), trust, plain.Line(tintLine(res.Tint)), plain.Line(req.Brief), plain.Line(res.BriefPath), res.Script)
		return err
	}
	return reportStarted(res, stdout, stderr)
}

// spawnerTie says why the session running qm spawn worker is not one of
// p's sessions ("" when it is, so its folder ties the worker to p).
func spawnerTie(p *up.Project) string {
	ids, err := callerSessions()
	if err != nil {
		return "the spawning session not read (" + err.Error() + ")"
	}
	if len(ids) == 0 {
		return "no Claude session found above qm"
	}
	recs, err := declog.Read(time.Time{})
	if err != nil {
		return "the decision log not read (" + err.Error() + ")"
	}
	if !slices.ContainsFunc(ids, func(id string) bool { return projectHolds(p, recs, id) }) {
		return "this session is not the project's coordinator or one of its sessions"
	}
	return ""
}

// tintLine says what a worker's tab is tinted: "teal on icon", or "none".
func tintLine(t launch.Tint) string {
	if t.IsZero() {
		return "none"
	}
	return t.Color + " on " + t.On
}

// reportStarted prints the started worker and its decision, the tint notes
// (a colour the config got wrong; a tab started untinted) and the
// launcher's warning (a failed agterm pin) to stderr.
func reportStarted(res spawn.Result, stdout, stderr io.Writer) error {
	for _, n := range []string{res.TintNote, res.Ref.TintNote} {
		if n != "" {
			printLine(stderr, "qm spawn: note: %s", n)
		}
	}
	if res.Ref.Warning != "" {
		printLine(stderr, "qm spawn: warning: %s", res.Ref.Warning)
	}
	_, err := fmt.Fprintf(stdout, "started %s %s (%s)\n%s\nwatch it: qm watch\n", res.Ref.Launcher, plain.Line(res.Ref.ID), plain.Line(res.Ref.Name), decisionLine(res.Decision))
	return err
}
