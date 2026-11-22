package launch

import (
	"context"
	"encoding/json"
	"fmt"
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

// Spawn runs: agtermctl session new [--workspace ID | --workspace-name NAME
// --create-workspace | --after ANCHOR] --cwd DIR --name NAME --no-select
// --command "/bin/sh SCRIPT" --json, and reads the new session id from the
// response, {"ok":true,"result":{"id":...}}. It then pins the session
// (Pin) so agterm does not restore it by itself; a failed pin is a Warning on
// the returned ref, not an error. A workspace by name wins over the anchor.
func (a Agterm) Spawn(ctx context.Context, s LaunchSpec) (SessionRef, error) {
	if err := s.check(); err != nil {
		return SessionRef{}, err
	}
	args := []string{"session", "new"}
	switch {
	case s.Workspace != "":
		args = append(args, "--workspace", s.Workspace)
	case s.WorkspaceName != "":
		args = append(args, "--workspace-name", s.WorkspaceName, "--create-workspace")
	case a.Anchor != "":
		args = append(args, "--after", a.Anchor)
	}
	args = append(args, "--cwd", s.Cwd, "--name", s.Name, "--no-select", "--command", Command(s.Script), "--json")
	raw, err := a.call(ctx, "session new", args...)
	if err != nil {
		return SessionRef{}, fmt.Errorf("agterm: %w", err)
	}
	var res struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &res); err != nil || res.ID == "" {
		return SessionRef{}, fmt.Errorf("agterm: %s session new returned no session id", a.Bin)
	}
	ref := SessionRef{Launcher: "agterm", ID: res.ID, Name: s.Name}
	if err := a.Pin(ctx, ref.ID); err != nil {
		ref.Warning = "agterm restore pin failed: " + err.Error()
	}
	return ref, nil
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
		OK     bool            `json:"ok"`
		Error  string          `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("%s %s: unexpected output %q: %w", a.Bin, verb, out, err)
	}
	if !resp.OK {
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
// and cwd are read into Foreground and Cwd.
func (a Agterm) Workspaces(ctx context.Context) ([]Workspace, error) {
	raw, err := a.call(ctx, "tree", "tree", "--json")
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
			ws.Sessions = append(ws.Sessions, SessionRef{Launcher: "agterm", ID: s.ID, Name: s.Name, Idle: s.ForegroundShell != "", Foreground: strings.Join(s.Foreground, " "), Cwd: s.Cwd})
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
