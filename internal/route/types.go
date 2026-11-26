// Package route decides the model and effort for a worker or a review. This
// file holds the types other packages share; the routing itself comes later.
package route

import (
	"context"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/privacy"
	"github.com/IZZY-Agency/quartermaster/internal/typesafe"
)

// Tier is a Claude model and effort. Model: sonnet|opus|fable|haiku.
// Effort: low|medium|high|xhigh|max. It must stay convertible to and from
// policy.Tier (same fields, same order).
type Tier struct {
	Model  string `json:"model"`
	Effort string `json:"effort"`
}

// CodexTier is a Codex model and reasoning effort (effort at most xhigh).
type CodexTier struct {
	Model  string `json:"model"`
	Effort string `json:"effort"`
}

// Profile is how a worker session is launched. PluginsOn are the plugins on
// in the session; PluginsOff the user's plugins switched off for it.
type Profile struct {
	Autocompact int               `json:"autocompact"`
	PluginsOn   []string          `json:"plugins_on,omitempty"`
	PluginsOff  []string          `json:"plugins_off,omitempty"`
	PluginDirs  []string          `json:"plugin_dirs,omitempty"`
	MCPConfig   string            `json:"mcp_config,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	// Permission holds the permission flags copied from the coordinator.
	Permission []string `json:"permission,omitempty"`
	// KeepClaudeAiPlugins leaves claude.ai plugin sync on: a plugin the
	// session needs comes from it. False switches the sync off.
	KeepClaudeAiPlugins bool `json:"keep_claude_ai_plugins,omitempty"`
}

// Decision is what route picked and why. Kind: worker|review.
type Decision struct {
	Kind      string         `json:"kind"`
	Tier      Tier           `json:"tier"`
	Codex     *CodexTier     `json:"codex,omitempty"`
	Row       string         `json:"row,omitempty"`
	Reasons   []string       `json:"reasons,omitempty"`
	Local     bool           `json:"local,omitempty"`
	JevOff    bool           `json:"jev_off,omitempty"`
	Judgments map[string]any `json:"judgments,omitempty"`
	Profile   *Profile       `json:"profile,omitempty"`
}

// Record is one line of the decision log. Kind: worker|review|guard|error.
// Project is the repository's top-level path and Branch its current branch;
// review rounds are counted per project and branch. For a review record,
// Detail is compact JSON: {"verdict":"...","p1":n,"p2":n,"p3":n}. Session is
// the Claude session id of the session the record came from (a coordinator's,
// for a hook); ClaudeSession, on a worker record, is the session id qm gave
// the worker it launched (claude --session-id), which names its transcript.
type Record struct {
	Time    time.Time `json:"time"`
	Kind    string    `json:"kind"`
	Project string    `json:"project,omitempty"`
	Branch  string    `json:"branch,omitempty"`
	Session string    `json:"session,omitempty"`
	// ClaudeSession is the worker's own Claude session id (worker records).
	ClaudeSession string    `json:"claude_session,omitempty"`
	Name          string    `json:"name,omitempty"`
	Type          string    `json:"type,omitempty"`
	Size          string    `json:"size,omitempty"`
	Round         int       `json:"round,omitempty"`
	Decision      *Decision `json:"decision,omitempty"`
	Override      string    `json:"override,omitempty"`
	Rule          string    `json:"rule,omitempty"`
	Action        string    `json:"action,omitempty"`
	Detail        string    `json:"detail,omitempty"`
	// Cwd is the folder a worker or coordinator session started in.
	Cwd string `json:"cwd,omitempty"`
	// UpProject ties a session to the qm up project it belongs to.
	UpProject string `json:"up_project,omitempty"`
	// BaseName is the name a worker was given with --name, before qm added the
	// project prefix (Name has it); "" for a worker spawned without one.
	BaseName string `json:"base_name,omitempty"`
	// Replaces is the Claude session id a fresh session took over from.
	Replaces string `json:"replaces,omitempty"`
	// TermLauncher and TermID name the terminal session a worker was
	// started in (worker records): the launcher (agterm, tmux) and its id
	// for the session, so qm down closes it and qm up pins it again.
	TermLauncher string `json:"term_launcher,omitempty"`
	TermID       string `json:"term_id,omitempty"`
	// DefaultModel is the model qm itself gave the coordinator session the
	// Record is about (up.coordinator_model); "" when the user chose one, or
	// none was given (up records).
	DefaultModel string `json:"default_model,omitempty"`
}

// Asker asks Jev a set of typed questions about one state. A nil Asker means
// Jev is off (no key).
type Asker interface {
	Ask(ctx context.Context, state any, qs map[string]typesafe.Question) (map[string]typesafe.Answer, error)
}

// Gater runs the privacy gate over text before it leaves the machine.
type Gater interface {
	Gate(ctx context.Context, text string) (privacy.Result, error)
}
