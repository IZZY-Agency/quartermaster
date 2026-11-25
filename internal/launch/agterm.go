package launch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Agterm starts a session with agtermctl session new.
type Agterm struct {
	Bin string // agtermctl
	Run Runner
	// Anchor is the session qm runs in (AGTERM_SESSION_ID). With no workspace
	// named, a new session is placed right after it, so it lands in the
	// coordinator's workspace, not in whichever one the front window shows.
	Anchor string
}

// Spawn creates the session with agtermctl session new (create) and pins it
// (Pin) so agterm does not restore it by itself; a failed pin is a Warning on
// the returned ref, not an error. A workspace by name wins over the anchor.
// With a Tint in the spec the tab is created tinted and what qm set is
// remembered (the tint memory, see ApplyTint), and expired entries are dropped
// from it; a tint that cannot be set or remembered never fails the launch: the
// session starts without it and the ref's TintNote says why. That includes a
// wait of more than tintsWait for another qm's lock on the memory, which is
// held while a tinted tab is created.
func (a Agterm) Spawn(ctx context.Context, s LaunchSpec) (SessionRef, error) {
	if err := s.check(); err != nil {
		return SessionRef{}, err
	}
	if s.Tint.IsZero() {
		ref, _, err := a.create(ctx, s, Tint{})
		return ref, err
	}
	// The tab is created and remembered under one lock, so no other qm
	// sees it tinted before it is remembered. When the memory cannot be
	// opened or locked, the tab is created untinted.
	var (
		ref    SessionRef
		tinted bool
		cerr   error
		ran    bool
	)
	err := withTints(ctx, func(b *tintBook) error {
		ran = true
		b.Expire()
		if ref, tinted, cerr = a.create(ctx, s, s.Tint); cerr != nil || !tinted {
			return nil
		}
		b.Set(ref.ID, s.Tint)
		return nil
	})
	switch {
	case !ran:
		ref, _, cerr = a.create(ctx, s, Tint{})
		if cerr != nil {
			return SessionRef{}, cerr
		}
		ref.TintNote = "tab not tinted: " + err.Error()
	case cerr != nil:
		return SessionRef{}, cerr
	case err != nil && tinted:
		ref.TintNote = a.untint(ctx, ref.ID, s.Tint, err)
	}
	return ref, nil
}

// create runs: agtermctl session new [--workspace ID | --workspace-name NAME
// --create-workspace | --after ANCHOR] --cwd DIR --name NAME --no-select
// --command "/bin/sh SCRIPT" [--tint COLOR --tint-on PLACES] --json, reads
// the new session id from the response, {"ok":true,"result":{"id":...}}, and
// pins the session (Pin) at once. An agterm older than --tint refuses the
// flag in agtermctl's argument parser, before anything is sent, so no tab
// exists: the session is then created once more without a tint (tinted is
// false, TintNote says why). Any other failure may have created the tab and
// is not retried.
func (a Agterm) create(ctx context.Context, s LaunchSpec, tint Tint) (ref SessionRef, tinted bool, err error) {
	newArgs := func(tint Tint) []string {
		args := []string{"session", "new"}
		switch {
		case s.Workspace != "":
			args = append(args, "--workspace", s.Workspace)
		case s.WorkspaceName != "":
			args = append(args, "--workspace-name", s.WorkspaceName, "--create-workspace")
		case a.Anchor != "":
			args = append(args, "--after", a.Anchor)
		}
		args = append(args, "--cwd", s.Cwd, "--name", s.Name, "--no-select", "--command", Command(s.Script))
		if !tint.IsZero() {
			args = append(args, "--tint", tint.Color, "--tint-on", tint.On)
		}
		return append(args, "--json")
	}
	note := ""
	raw, err := a.call(ctx, "session new", newArgs(tint)...)
	if !tint.IsZero() && unknownTintOption(err) {
		tint, note = Tint{}, "tab not tinted: this agterm does not know --tint; update agterm"
		raw, err = a.call(ctx, "session new", newArgs(tint)...)
	}
	if err != nil {
		return SessionRef{}, false, fmt.Errorf("agterm: %w", newErr(err))
	}
	var res struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &res); err != nil || res.ID == "" {
		return SessionRef{}, false, fmt.Errorf("agterm: %s session new returned no session id: %w", a.Bin, ErrMaybeStarted)
	}
	ref = SessionRef{Launcher: "agterm", ID: res.ID, Name: s.Name, TintNote: note}
	if err := a.Pin(ctx, ref.ID); err != nil {
		ref.Warning = "agterm restore pin failed: " + err.Error()
	}
	return ref, !tint.IsZero(), nil
}

// untint takes a tint off the new tab id after the memory could not record it
// (cause), as an unremembered tint would read as the owner's. It clears the
// tab only while it still shows the tint it was created with, so a colour the
// owner picked meanwhile stays. It returns the TintNote.
func (a Agterm) untint(ctx context.Context, id string, tint Tint, cause error) string {
	refs, _, err := a.allSessions(ctx)
	if err == nil {
		for _, r := range refs {
			if r.ID != id {
				continue
			}
			if r.Tint != tint {
				return "tab tinted, but not remembered, and left alone: its tint changed meanwhile (" + cause.Error() + ")"
			}
			if err = a.setTint(ctx, id, Tint{}); err == nil {
				return "tab not tinted: " + cause.Error()
			}
			break
		}
	}
	if err != nil {
		cause = errors.Join(cause, err)
	}
	return "tab tinted, but not remembered, so qm will leave its tint alone: " + cause.Error()
}

// usageExit is agtermctl's exit code for arguments it does not take.
const usageExit = 64

// newErr is a failed session new, marked ErrMaybeStarted unless agtermctl
// says the session was not opened: agterm answered ok:false, it could not
// connect to agterm, it refused its arguments, or it did not run at all. Any
// other failed run (a lost reply, a kill) may have opened it.
func newErr(err error) error {
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return err
	}
	msg := strings.ReplaceAll(err.Error(), " ", "")
	if exit.ExitCode() == usageExit || !killed(err) && (strings.Contains(msg, `"ok":false`) || strings.Contains(msg, "Error:connect(")) {
		return err
	}
	return fmt.Errorf("%w (%w)", err, ErrMaybeStarted)
}

// call runs agtermctl with args (verb names the command in errors), decodes
// the {"ok":...,"error":...,"result":...} envelope and returns its result. A
// runner error, unreadable output or ok:false is an error.
func (a Agterm) call(ctx context.Context, verb string, args ...string) (json.RawMessage, error) {
	out, err := a.Run(ctx, a.Bin, args...)
	if err != nil {
		return nil, err
	}
	var resp struct {
		OK     *bool           `json:"ok"`
		Error  string          `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("%s %s: unexpected output %q: %w (%w)", a.Bin, verb, out, err, ErrMaybeStarted)
	}
	if resp.OK == nil && resp.Error == "" {
		return nil, fmt.Errorf("%s %s: unexpected output %q, neither ok nor an error (%w)", a.Bin, verb, out, ErrMaybeStarted)
	}
	if resp.OK == nil || !*resp.OK {
		msg := resp.Error
		if msg == "" {
			msg = "agtermctl reported failure"
		}
		return nil, fmt.Errorf("%s %s: %s", a.Bin, verb, msg)
	}
	return resp.Result, nil
}

// Pin runs: agtermctl session restore --none --target ID --json, so agterm
// does not re-run the session's command when it restores its windows.
func (a Agterm) Pin(ctx context.Context, id string) error {
	_, err := a.call(ctx, "session restore", "session", "restore", "--none", "--target", id, "--json")
	return err
}

// Workspaces reads agtermctl tree --json once and returns every workspace
// with its id, name and sessions. A session whose foregroundShell is set is
// an idle shell (Idle); a pane running claude or anything else has no such
// key. A session's foreground (the argv of its pane's foreground process)
// and cwd are read into Foreground and Cwd; a tab split into several panes
// is Split.
func (a Agterm) Workspaces(ctx context.Context) ([]Workspace, error) {
	return a.workspaces(ctx, "")
}

// workspaces reads one window's tree: the frontmost window's when window is
// "", else the window with that id.
func (a Agterm) workspaces(ctx context.Context, window string) ([]Workspace, error) {
	args := []string{"tree", "--json"}
	if window != "" {
		args = append(args, "--window", window)
	}
	raw, err := a.call(ctx, "tree", args...)
	if err != nil {
		return nil, fmt.Errorf("agterm: %w", err)
	}
	var res struct {
		Tree struct {
			Workspaces []struct {
				ID       string `json:"id"`
				Name     string `json:"name"`
				Sessions []struct {
					ID              string   `json:"id"`
					Name            string   `json:"name"`
					ForegroundShell string   `json:"foregroundShell"`
					Foreground      []string `json:"foreground"`
					Cwd             string   `json:"cwd"`
					Split           bool     `json:"split"`
					Tint            string   `json:"tint"`
					TintOn          []string `json:"tintOn"`
				} `json:"sessions"`
			} `json:"workspaces"`
		} `json:"tree"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("agterm: %s tree: unexpected result: %w", a.Bin, err)
	}
	var out []Workspace
	for _, w := range res.Tree.Workspaces {
		ws := Workspace{ID: w.ID, Name: w.Name}
		for _, s := range w.Sessions {
			ws.Sessions = append(ws.Sessions, SessionRef{Launcher: "agterm", ID: s.ID, Name: s.Name, Idle: s.ForegroundShell != "", Foreground: strings.Join(s.Foreground, " "), Cwd: s.Cwd, Split: s.Split, Tint: readTint(s.Tint, s.TintOn)})
		}
		out = append(out, ws)
	}
	return out, nil
}

// Sessions returns the sessions of the workspace called workspaceName (none
// when there is no such workspace), from one Workspaces read.
func (a Agterm) Sessions(ctx context.Context, workspaceName string) ([]SessionRef, error) {
	wss, err := a.Workspaces(ctx)
	if err != nil {
		return nil, err
	}
	var refs []SessionRef
	for _, w := range wss {
		if w.Name == workspaceName {
			refs = append(refs, w.Sessions...)
		}
	}
	return refs, nil
}

// Move runs: agtermctl session move WORKSPACE_ID --target SESSION_ID --json.
func (a Agterm) Move(ctx context.Context, sessionID, workspaceID string) error {
	if _, err := a.call(ctx, "session move", "session", "move", workspaceID, "--target", sessionID, "--json"); err != nil {
		return fmt.Errorf("agterm: %w", err)
	}
	return nil
}

// WorkspaceNew runs: agtermctl workspace new --json -- NAME (a name that
// starts with a dash is not read as a flag), and returns the new
// workspace's id from the response, {"ok":true,"result":{"id":...}}
// (verified live).
func (a Agterm) WorkspaceNew(ctx context.Context, name string) (string, error) {
	raw, err := a.call(ctx, "workspace new", "workspace", "new", "--json", "--", name)
	if err != nil {
		return "", fmt.Errorf("agterm: %w", err)
	}
	var res struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &res); err != nil || res.ID == "" {
		return "", fmt.Errorf("agterm: %s workspace new returned no workspace id", a.Bin)
	}
	return res.ID, nil
}

var _ Reuser = Agterm{}

// lineKill is Ctrl-U: typed before the launch line, it clears whatever the
// shell's line holds, so leftover input never runs together with it.
// Verified live with fish on a scratch agterm tab: a half-typed command
// followed by session type of 0x15, the launch line and a newline ran the
// launch line only. Other shells are in the owner's live checks.
const lineKill = "\x15"

// notReady is an answer that the session's surface is not up yet. agterm's
// reference names "session not realized"; "not ready" is kept for older
// builds.
func notReady(msg string) bool {
	return strings.Contains(msg, "not realized") || strings.Contains(msg, "not ready")
}

// refused is a definite refusal by agterm, after which nothing was typed:
// a surface that never came up, or no such session (agterm's notFound for a
// --target that matches none, and "no such session").
func refused(msg string) bool {
	return notReady(msg) || strings.Contains(msg, "notfound") || strings.Contains(msg, "no such session")
}

// Reuse runs the script inside the idle session ref: agtermctl session type
// --target ID --json "^U/bin/sh SCRIPT\n" (^U, the line-kill byte 0x15,
// first). The line is typed, not exec'd, so the shell stays when the script
// ends. A not-ready answer ("session not realized") is retried once with
// --select. It then pins the session (Pin); a failed pin is a Warning on the
// returned ref. The returned ref is ref with its ID and Name unchanged.
// agterm's definite refusals (not realized on the retry, notFound, no such
// session) match ErrRefused: nothing was typed. Any other error does not,
// and the tab may have received the line.
func (a Agterm) Reuse(ctx context.Context, ref SessionRef, script string) (SessionRef, error) {
	text := lineKill + Command(script) + "\n"
	args := []string{"session", "type", "--target", ref.ID, "--json", text}
	_, err := a.call(ctx, "session type", args...)
	if err != nil && notReady(strings.ToLower(err.Error())) {
		args = []string{"session", "type", "--select", "--target", ref.ID, "--json", text}
		_, err = a.call(ctx, "session type", args...)
	}
	if err != nil {
		if refused(strings.ToLower(err.Error())) {
			return SessionRef{}, fmt.Errorf("agterm: %w: %v", ErrRefused, err)
		}
		return SessionRef{}, fmt.Errorf("agterm: %w", err)
	}
	out := SessionRef{Launcher: "agterm", ID: ref.ID, Name: ref.Name, Adopted: ref.Adopted}
	if err := a.Pin(ctx, ref.ID); err != nil {
		out.Warning = "agterm restore pin failed: " + err.Error()
	}
	return out, nil
}

// Close runs: agtermctl session close --target ID --json. agtermctl answers
// a session that does not exist with "no such session: ID" (ErrGone).
func (a Agterm) Close(ctx context.Context, ref SessionRef) error {
	if _, err := a.call(ctx, "session close", "session", "close", "--target", ref.ID, "--json"); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such session") {
			return fmt.Errorf("agterm: %w: %v", ErrGone, err)
		}
		return fmt.Errorf("agterm: %w", err)
	}
	return nil
}
