// Package spawn launches worker sessions: it routes the brief, builds the
// launch profile and script, starts the session through a launcher and logs
// the decision (design 5.3, 5.5, 5.12).
package spawn

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/config"
	"github.com/IZZY-Agency/quartermaster/internal/declog"
	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/limits"
	"github.com/IZZY-Agency/quartermaster/internal/paths"
	"github.com/IZZY-Agency/quartermaster/internal/reviewin"
	"github.com/IZZY-Agency/quartermaster/internal/route"
)

// WorkerRequest is one qm spawn worker call.
type WorkerRequest struct {
	Brief     string // the brief file
	Type      string // task type; review routes with the review table
	Size      string // S, M or L
	Name      string // the session's name
	Cwd       string // the folder the worker starts in
	Workspace string // agterm workspace; "" is the current one
	// WorkspaceName names the workspace instead (agterm, created when
	// missing; tmux, the session): the qm up project's. Setting both
	// Workspace and WorkspaceName is bad input.
	WorkspaceName string
	// UpProject ties the worker to a qm up project (QM_PROJECT_ID): its
	// launch carries the project and UpWorkspace, its Record up_project.
	UpProject string
	// UpWorkspace is the project's workspace (QM_WORKSPACE), passed on to
	// the worker's own workers whatever workspace this worker opens in.
	UpWorkspace string
	Launcher    string // agterm, tmux or auto; "" is the config's
	Hint        *route.Tier
	Force       *route.Tier
	ForceWhy    string
	DryRun      bool // route and build the script; launch and log nothing
}

// Deps is what Worker needs from outside the request.
type Deps struct {
	Config config.Config
	Ask    route.Asker // nil: Jev off
	Gate   route.Gater
	Claude limits.ClaudeState
	Codex  func() limits.CodexState // read only for --type review
	Getenv func(string) string
	PS     func(pid int) (cmd string, ppid int, err error)
	// ProcExe returns the executable a process runs, as the system
	// reports it (spawn.ProcExe).
	ProcExe func(pid int) (string, error)
	Pick    func(choice string, env func(string) string) (launch.Launcher, error)
	// Trusted reports whether Claude Code trusts a folder; nil skips the
	// check.
	Trusted func(dir string) (bool, error)
	Exe     string // qm's own executable
	// Mark, when set, amends the decision before it is used (fake Jev).
	Mark func(route.Decision) route.Decision
}

// Result is what Worker did.
type Result struct {
	Decision route.Decision
	// Launcher is nil only in a dry run that found none. LauncherNote names
	// the launcher, or says why there is none.
	Launcher     launch.Launcher
	LauncherNote string
	Script       string
	ScriptPath   string // "" in a dry run
	// SessionID is the Claude session id the worker is launched with.
	SessionID string
	Ref       launch.SessionRef
	// TrustNote, in a dry run only, says why the launch would stop: the
	// folder is not trusted, or its trust could not be read.
	TrustNote string
}

// ErrInput marks bad input: the request, or a routing input route rejects.
var ErrInput = errors.New("bad input")

// ErrUntrusted: Claude Code has not accepted its trust dialog for the
// worker's folder, so the worker would stop at the dialog (design 5.7).
var ErrUntrusted = errors.New("folder not trusted")

// ReasonReviewerSession is added to a --type review decision: the session
// runs the Claude tier, so the Codex tier is dropped.
const ReasonReviewerSession = "reviewer session runs Claude: Codex tier not used"

// validName is a session name: it becomes a file name, a tmux target and an
// environment value.
var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// ValidName says whether name can be a session name (validName).
func ValidName(name string) bool { return validName.MatchString(name) }

// Worker spawns a worker session. The launcher is picked before routing, so
// a missing launcher stops it before anything is written or logged; a dry
// run needs none. The decision is logged only once the session has started.
func Worker(ctx context.Context, req WorkerRequest, deps Deps) (Result, error) {
	var res Result
	if err := checkRequest(&req); err != nil {
		return res, fmt.Errorf("%w: %w", ErrInput, err)
	}
	choice := req.Launcher
	if choice == "" {
		choice = deps.Config.Launcher
	}
	l, err := deps.Pick(choice, deps.Getenv)
	switch {
	case err == nil:
		res.Launcher, res.LauncherNote = l, launcherName(l)
	case req.DryRun:
		res.LauncherNote = "none found: " + err.Error()
	default:
		return res, err
	}

	repo, branch := req.Cwd, ""
	if top, b, err := reviewin.Repo(req.Cwd); err == nil {
		repo, branch = top, b
	}
	d, round, err := Route(ctx, req, repo, deps)
	if err != nil {
		return res, err
	}

	perm, why, err := ParentPermission(deps.Getenv, deps.PS, deps.ProcExe)
	if err != nil {
		return res, err
	}
	workerDir, err := WorkerPluginDir(deps.Exe)
	if err != nil {
		return res, err
	}
	qmDir := ""
	if exe, err := filepath.EvalSymlinks(deps.Exe); err == nil {
		qmDir = filepath.Dir(exe)
	}
	enabled, err := EnabledPlugins(repo)
	if err != nil {
		return res, err
	}
	p, notes := BuildProfile(d, repo, deps.Config, enabled, perm, workerDir)
	if req.UpProject != "" {
		p.Env["QM_PROJECT_ID"] = req.UpProject
		if req.UpWorkspace != "" {
			p.Env["QM_WORKSPACE"] = req.UpWorkspace
		}
	}
	d.Profile = &p
	d.Reasons = append(append(d.Reasons, notes...), why)
	res.Decision = d
	if res.SessionID, err = NewSessionID(); err != nil {
		return res, err
	}
	res.Script = LaunchScript(p, d.Tier, req.Name, res.SessionID, req.Cwd, req.Brief, qmDir)
	if res.TrustNote, err = checkTrust(req.Cwd, deps.Trusted, req.DryRun); err != nil {
		return res, err
	}
	if req.DryRun {
		return res, nil
	}

	if res.ScriptPath, err = WriteScript(req.Name, res.Script); err != nil {
		return res, err
	}
	res.Ref, err = l.Spawn(ctx, launch.LaunchSpec{Name: req.Name, Cwd: req.Cwd, Script: res.ScriptPath, Workspace: req.Workspace, WorkspaceName: req.WorkspaceName})
	if err != nil {
		_ = os.Remove(res.ScriptPath)
		res.ScriptPath = ""
		return res, err
	}
	if err := logWorker(d, req, repo, branch, round, res); err != nil {
		return res, fmt.Errorf("session %s started, but the decision was not logged: %w", res.Ref.ID, err)
	}
	return res, nil
}

// checkTrust returns ErrUntrusted when trusted says Claude Code has not
// trusted cwd, or the error reading it; nil trusted checks nothing. In a dry
// run it returns no error but a note saying the same.
func checkTrust(cwd string, trusted func(string) (bool, error), dryRun bool) (note string, err error) {
	if trusted == nil {
		return "", nil
	}
	ok, err := trusted(cwd)
	switch {
	case err != nil:
		err = fmt.Errorf("trust of %s not read: %w", cwd, err)
		if dryRun {
			return err.Error(), nil
		}
		return "", err
	case !ok && dryRun:
		return "not trusted by Claude Code: " + cwd, nil
	case !ok:
		return "", fmt.Errorf("%w: Claude Code has not trusted %s: run claude there once and accept the trust dialog", ErrUntrusted, cwd)
	}
	return "", nil
}

// SessionVerdict is the verdict of the review Record a --type review worker
// logs: a review round whose findings qm does not read.
const SessionVerdict = "session"

// logWorker logs the worker Record of a launched session (kind "worker",
// whatever table routed it, so a reviewer session is a worker in qm report).
// A --type review session also logs a review Record, which counts its round;
// it is written first, so the report never assigns it to the reviewer's own
// worker Record.
func logWorker(d route.Decision, req WorkerRequest, repo, branch string, round int, res Result) error {
	now := time.Now().UTC()
	if req.Type == "review" {
		if err := declog.Append(route.Record{
			Time: now, Kind: "review", Project: repo, Branch: branch, Name: req.Name,
			Round: round, Decision: &d,
			Detail: fmt.Sprintf(`{"verdict":%q,"p1":0,"p2":0,"p3":0}`, SessionVerdict),
		}); err != nil {
			return err
		}
		now = now.Add(time.Microsecond)
	}
	return declog.Append(route.Record{
		Time: now, Kind: "worker", Project: repo, Branch: branch, Name: req.Name,
		Cwd: req.Cwd, UpProject: req.UpProject,
		ClaudeSession: res.SessionID, Type: req.Type, Size: req.Size,
		Round: round, Override: req.ForceWhy, Decision: &d,
		Detail:       res.Ref.Launcher + " " + res.Ref.ID,
		TermLauncher: res.Ref.Launcher, TermID: res.Ref.ID,
	})
}

// NewSessionID is a random (version 4) UUID, the worker's Claude session id.
func NewSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("session id: %w", err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// launcherName says which launcher l is, for a dry run.
func launcherName(l launch.Launcher) string {
	switch v := l.(type) {
	case launch.Agterm:
		return "agterm"
	case launch.Tmux:
		if v.Inside {
			return "tmux (a new window in this session)"
		}
		return "tmux (a new detached session)"
	}
	return fmt.Sprintf("%T", l)
}

// checkRequest rejects bad input and makes the brief and cwd absolute.
func checkRequest(req *WorkerRequest) error {
	switch {
	case req.Brief == "":
		return errors.New("spawn: a brief file is required")
	case req.Cwd == "":
		return errors.New("spawn: a working folder (cwd) is required")
	case req.Hint == nil:
		return errors.New("spawn: a hint (model/effort) is required")
	case !validName.MatchString(req.Name):
		return fmt.Errorf("spawn: name %q must be 1 to 64 letters, digits, - or _, starting with a letter or digit", req.Name)
	case req.Size != "S" && req.Size != "M" && req.Size != "L":
		return fmt.Errorf("spawn: size %q is not S, M or L", req.Size)
	case req.Force != nil && strings.TrimSpace(req.ForceWhy) == "":
		return errors.New("spawn: a forced tier needs a reason (--why)")
	case req.Force == nil && req.ForceWhy != "":
		return errors.New("spawn: --why goes with --force")
	case req.Workspace != "" && req.WorkspaceName != "":
		return errors.New("spawn: a workspace id and a workspace name are both set")
	}
	var err error
	if req.Brief, err = filepath.Abs(req.Brief); err != nil {
		return fmt.Errorf("spawn: %w", err)
	}
	if _, err := os.Stat(req.Brief); err != nil {
		return fmt.Errorf("spawn: brief: %w", err)
	}
	if req.Cwd, err = filepath.Abs(req.Cwd); err != nil {
		return fmt.Errorf("spawn: %w", err)
	}
	if !isDir(req.Cwd) {
		return fmt.Errorf("spawn: cwd %s is not a folder", req.Cwd)
	}
	return nil
}

// Route is the routing decision for a worker request, exactly as Worker
// takes it (the Mark amendment, and a --type review session's Codex tier
// dropped), and the review round it counts. It writes and launches nothing;
// qm route calls it for a preview.
func Route(ctx context.Context, req WorkerRequest, repo string, deps Deps) (route.Decision, int, error) {
	d, round, err := decide(ctx, req, repo, deps)
	if err != nil {
		return d, round, err
	}
	if deps.Mark != nil {
		d = deps.Mark(d)
	}
	if d.Codex != nil {
		d.Codex = nil
		d.Reasons = append(d.Reasons, ReasonReviewerSession)
	}
	return d, round, nil
}

// decide routes the request: --type review with the review table (its round
// counted from the log), every other type with the worker table.
func decide(ctx context.Context, req WorkerRequest, repo string, deps Deps) (route.Decision, int, error) {
	if req.Type == "review" {
		in, err := reviewin.For(req.Cwd, "", req.Brief, 0)
		if err != nil {
			return route.Decision{}, 0, err
		}
		in.Force = req.Force
		var codex limits.CodexState
		if deps.Codex != nil {
			codex = deps.Codex()
		}
		d, err := route.DecideReview(ctx, in, deps.Ask, deps.Gate, codex, deps.Claude)
		return d, in.Round, inputError(err)
	}
	brief, err := os.ReadFile(req.Brief)
	if err != nil {
		return route.Decision{}, 0, fmt.Errorf("spawn: brief: %w", err)
	}
	in := route.WorkerInput{
		Brief: string(brief), Type: req.Type, Size: req.Size, Repo: repo,
		Hint: req.Hint, Force: req.Force, ForceWhy: strings.TrimSpace(req.ForceWhy),
	}
	d, err := route.DecideWorker(ctx, in, deps.Ask, deps.Gate, deps.Claude)
	return d, 0, inputError(err)
}

// inputError marks a route error as bad input: route fails only on it once
// it has a gate.
func inputError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrInput, err)
}

// ScriptAge is how long launch scripts are kept: a script runs once, at the
// launch, and stays only to be read when a launch went wrong.
const ScriptAge = 7 * 24 * time.Hour

// fileName is what a session name must be to become part of a file name:
// letters, digits, - and _ only, so it never names another folder. It
// holds every ValidName and a qm up coordinator's name, which may start
// with - or _.
var fileName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// FileName says whether s may be part of a file name qm writes (fileName).
func FileName(s string) bool { return fileName.MatchString(s) }

// WriteScript writes a launch script to <state>/launch/<name>-<unixnano>.sh,
// mode 0700, never over an existing file, and returns its path. Scripts
// older than ScriptAge are removed first. A name FileName refuses is an
// error, and nothing is written.
func WriteScript(name, script string) (string, error) {
	if !FileName(name) {
		return "", fmt.Errorf("launch script: session name %q is not usable in a file name", name)
	}
	state, err := paths.StateDir()
	if err != nil {
		return "", fmt.Errorf("launch script: %w", err)
	}
	dir := filepath.Join(state, "launch")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("launch script: %w", err)
	}
	sweepScripts(dir, ScriptAge)
	for range 100 {
		path := filepath.Join(dir, fmt.Sprintf("%s-%d.sh", name, time.Now().UnixNano()))
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("launch script: %w", err)
		}
		if _, err := f.WriteString(script); err != nil {
			_ = f.Close()
			return "", fmt.Errorf("launch script: %w", err)
		}
		if err := f.Close(); err != nil {
			return "", fmt.Errorf("launch script: %w", err)
		}
		return path, nil
	}
	return "", errors.New("launch script: no free file name")
}

// sweepScripts removes the launch scripts in dir older than age.
func sweepScripts(dir string, age time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		fi, err := e.Info()
		if err == nil && fi.Mode().IsRegular() && strings.HasSuffix(e.Name(), ".sh") && time.Since(fi.ModTime()) > age {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
