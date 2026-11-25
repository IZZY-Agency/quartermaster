package up

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/spawn"
)

// refused are the claude arguments qm up sets itself (spec 5.4).
var refused = []string{"--resume", "-r", "--continue", "-c", "--session-id", "-n", "--name", "-p", "--print"}

// CheckClaudeArgs refuses, with ErrInput, coordinator arguments qm up sets
// itself: --resume, --continue, --session-id, -n/--name and -p/--print, in
// any spelling (-r, -c, --flag=value), and a lone "--", after which claude
// would read those as arguments.
func CheckClaudeArgs(args []string) error {
	for _, a := range args {
		if a == "--" {
			return fmt.Errorf("%w: a lone -- would end claude's options before the ones qm up sets; leave it out of the coordinator's claude arguments", ErrInput)
		}
		flag, _, _ := strings.Cut(a, "=")
		if slices.Contains(refused, flag) {
			return fmt.Errorf("%w: qm up sets %s itself; leave it out of the coordinator's claude arguments", ErrInput, flag)
		}
	}
	return nil
}

// Start is qm up's first run in root (spec 5.4): it checks the arguments
// (args; nil takes d.Coordinator, an empty list is no arguments) and the
// folder's trust, names the workspace, starts the coordinator with a new
// session id, the model of FreshModel and no prompt, and saves the project.
// Without a launcher it writes the script and saves the project with no terminal session, so the
// coordinator the printed line starts is the one the next qm up knows; it
// returns launch.ErrNoLauncher with the Outcome naming the script. It holds
// the adoption lock from the workspace's naming to the save: an adoption in
// another project rechecks under that lock which sessions and workspaces
// the saved projects hold, so it never takes this coordinator, running
// before its project is saved, as a worker, nor this workspace. With d.Here
// the coordinator is to start in the tab qm up runs in: Start resolves the
// tab, writes the script, saves the project once with the tab and logs the
// Record, launches nothing, and returns an Outcome with Here set for the
// caller to run its script in place of qm; no launcher is needed.
func Start(ctx context.Context, root string, args []string, d Deps) (Project, Outcome, error) {
	if err := checkRoot(root); err != nil {
		return Project{}, Outcome{}, err
	}
	if err := CheckClaudeArgs(args); err != nil {
		return Project{}, Outcome{}, err
	}
	if args == nil {
		args = d.Coordinator
	}
	if err := CheckClaudeArgs(args); err != nil {
		return Project{}, Outcome{}, fmt.Errorf("up.claude_args in the config: %w", err)
	}
	if d.Trusted != nil {
		ok, err := d.Trusted(root)
		switch {
		case err != nil:
			return Project{}, Outcome{}, fmt.Errorf("trust of %s not read: %w", root, err)
		case !ok:
			return Project{}, Outcome{}, fmt.Errorf("%w: run claude in %s once and accept the trust dialog", spawn.ErrUntrusted, root)
		}
	}
	unlock, err := AdoptLock()
	if err != nil {
		return Project{}, Outcome{}, fmt.Errorf("coordinator not started: %w", err)
	}
	defer unlock()
	all, err := All()
	if err != nil {
		return Project{}, Outcome{}, err
	}
	ws := WorkspaceName(root, func(name string) bool {
		return slices.ContainsFunc(all, func(o Project) bool { return o.Root != root && o.Workspace == name })
	})
	id, err := spawn.NewSessionID()
	if err != nil {
		return Project{}, Outcome{}, err
	}
	now := d.now()
	p := Project{
		ID: ProjectID(root), Root: root, Workspace: ws, Launcher: launcherKind(d.Launcher),
		ClaudeArgs:  append([]string{}, args...),
		Coordinator: Session{ID: id, Name: ws + "-coord", Started: now},
		Sessions:    map[string]launch.SessionRef{},
		Created:     now,
	}
	o := Outcome{
		Step: Step{
			Candidate: Candidate{Role: RoleCoordinator, SessionID: id, Name: p.Coordinator.Name, Cwd: root, Trusted: true, Started: now},
			Action:    Fresh, Why: "first run",
		},
		NewSessionID: id,
	}
	var here launch.SessionRef
	var note string
	if d.Here != nil {
		// The tab is resolved before the script is written.
		here, note = d.Here(ctx, &p, []string{id}, true)
		// Under tmux it may have named the workspace and the coordinator.
		o.Step.Name = p.Coordinator.Name
		if p.Launcher == "" {
			p.Launcher = here.Launcher
		}
	}
	o.Model = FreshModel(p.ClaudeArgs, d)
	p.setDefaultModel(id, o.Model.Model)
	script, err := coordinatorScript(&p, id, "", false, o.Model, d.Exe)
	if err == nil {
		o.Script, err = spawn.WriteScript(p.Coordinator.Name, script)
	}
	if err != nil {
		return p, o, err
	}
	// The project is saved, with the coordinator's id reserved and no terminal
	// session, before the coordinator starts: a start that fails, a failed
	// second save or a qm that exits after the start leaves a project that
	// knows the id, as one that never ran (Gather), not a live coordinator no
	// project holds.
	if d.Here != nil {
		// Started here: saved once with the tab, logged, and run by the
		// caller in place of qm, so nothing is launched.
		if here.ID != "" {
			p.Sessions[id] = stored(here)
		}
		if err := Save(p); err != nil {
			return p, o, err
		}
		o.Here, o.Ref = true, here
		o.Ref.Warning = note
		logUp(d, &p, o.Step, id, now, &o)
		return p, o, nil
	}
	if err := Save(p); err != nil {
		return p, o, err
	}
	if d.Launcher == nil {
		logScript(d, &p, o.Step, id, now, &o)
		return p, o, launch.ErrNoLauncher
	}
	if o.Ref, err = d.Launcher.Spawn(ctx, launch.LaunchSpec{Name: p.Coordinator.Name, Cwd: root, Script: o.Script, WorkspaceName: ws}); err != nil {
		o.Err = err
		logScript(d, &p, o.Step, id, now, &o)
		return p, o, err
	}
	p.Sessions[id] = stored(o.Ref)
	if err := Save(p); err != nil {
		logScript(d, &p, o.Step, id, now, &o)
		o.Err = fmt.Errorf("coordinator started (%s %s), but its terminal session was not saved: %w", o.Ref.Launcher, o.Ref.ID, err)
		return p, o, o.Err
	}
	logUp(d, &p, o.Step, id, now, &o)
	return p, o, nil
}

// launcherKind names a launcher for the state: agterm, tmux, or "" for none.
func launcherKind(l launch.Launcher) string {
	switch l.(type) {
	case nil:
		return ""
	case launch.Agterm, *launch.Agterm:
		return "agterm"
	case launch.Tmux, *launch.Tmux:
		return "tmux"
	}
	return fmt.Sprintf("%T", l)
}
