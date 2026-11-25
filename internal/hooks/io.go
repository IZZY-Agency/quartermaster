// Package hooks reads Claude Code hook input and writes hook output.
package hooks

import (
	"encoding/json"
	"fmt"
	"io"
)

// Input is the JSON a Claude Code hook receives on stdin.
type Input struct {
	SessionID            string         `json:"session_id"`
	TranscriptPath       string         `json:"transcript_path"`
	CWD                  string         `json:"cwd"`
	HookEventName        string         `json:"hook_event_name"`
	ToolName             string         `json:"tool_name"`
	ToolInput            map[string]any `json:"tool_input"`
	ToolUseID            string         `json:"tool_use_id"`
	Prompt               string         `json:"prompt"`
	StopHookActive       bool           `json:"stop_hook_active"`
	LastAssistantMessage string         `json:"last_assistant_message"`
	// AgentID and AgentType are set when the hook fires inside a subagent.
	AgentID   string `json:"agent_id"`
	AgentType string `json:"agent_type"`
	// BackgroundTasks and SessionCrons are a Stop input's in-flight tasks and
	// scheduled wakeups, absent before the Claude Code that added them. They
	// stay undecoded here, so a malformed one never fails a hook.
	BackgroundTasks json.RawMessage `json:"background_tasks"`
	SessionCrons    json.RawMessage `json:"session_crons"`
}

// Read decodes one hook input.
func Read(r io.Reader) (Input, error) {
	var in Input
	if err := json.NewDecoder(r).Decode(&in); err != nil {
		return Input{}, fmt.Errorf("read hook input: %w", err)
	}
	return in, nil
}

type specific struct {
	HookEventName            string         `json:"hookEventName"`
	PermissionDecision       string         `json:"permissionDecision,omitempty"`
	PermissionDecisionReason string         `json:"permissionDecisionReason,omitempty"`
	UpdatedInput             map[string]any `json:"updatedInput,omitempty"`
	AdditionalContext        string         `json:"additionalContext,omitempty"`
}

type output struct {
	HookSpecificOutput *specific `json:"hookSpecificOutput,omitempty"`
	Decision           string    `json:"decision,omitempty"`
	Reason             string    `json:"reason,omitempty"`
}

// Deny refuses the tool call with a reason Claude reads.
func Deny(event, reason string) []byte {
	return encode(output{HookSpecificOutput: &specific{
		HookEventName:            event,
		PermissionDecision:       "deny",
		PermissionDecisionReason: reason,
	}})
}

// Note adds context for Claude without changing the call.
func Note(event, text string) []byte {
	return encode(output{HookSpecificOutput: &specific{HookEventName: event, AdditionalContext: text}})
}

// Rewrite replaces the tool input, with an optional note for Claude. It fails
// when the updated input cannot be encoded as JSON (a channel or a function in
// it, say); the caller then lets the action through and logs the error.
func Rewrite(event string, updated map[string]any, note string) ([]byte, error) {
	b, err := json.Marshal(output{HookSpecificOutput: &specific{
		HookEventName:     event,
		UpdatedInput:      updated,
		AdditionalContext: note,
	}})
	if err != nil {
		return nil, fmt.Errorf("encode hook rewrite: %w", err)
	}
	return b, nil
}

// BlockStop keeps a session from stopping, with a reason Claude reads.
func BlockStop(reason string) []byte {
	return encode(output{Decision: "block", Reason: reason})
}

// encode marshals output that holds only strings, which cannot fail; a
// failure is a programming error.
func encode(o output) []byte {
	b, err := json.Marshal(o)
	if err != nil {
		panic("hooks: encode string-only output: " + err.Error())
	}
	return b
}
