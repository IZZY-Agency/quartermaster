package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/IZZY-Agency/quartermaster/internal/policy"
	"github.com/IZZY-Agency/quartermaster/internal/privacy"
	"github.com/IZZY-Agency/quartermaster/internal/transcript"
	"github.com/IZZY-Agency/quartermaster/internal/typesafe"
)

// askGateReason is what the coordinator reads when a question is denied or a
// stop is blocked.
const askGateReason = "Decide this yourself, state the decision and the reason in one line, and continue."

// askTranscriptTail is how much of the transcript's end the ask gate reads
// for the recent conversation.
const askTranscriptTail = 4 << 20

// askGate stops the coordinator from asking the user what it can decide
// itself (design 5.7): an AskUserQuestion call, or a reply whose last line is
// a question. Jev judges the question over the recent conversation, both
// through the privacy gate first; a question that is answerable from context,
// cheap to undo and not a product ruling is denied (a stop is blocked).
func askGate(c *call) (result, error) {
	if c.in.AgentID != "" {
		return result{}, nil
	}
	var question string
	switch {
	case c.in.HookEventName == "PreToolUse" && c.in.ToolName == "AskUserQuestion":
		question = askedQuestions(c.in.ToolInput)
	case c.in.HookEventName == "Stop" && !c.in.StopHookActive:
		question = endingQuestion(c.in.LastAssistantMessage)
	}
	if strings.TrimSpace(question) == "" {
		return result{}, nil
	}
	question = keepStart(question, policy.AskQuestionChars)
	conv := ""
	if c.in.TranscriptPath != "" {
		var err error
		if conv, err = recentConversation(c.in.TranscriptPath); err != nil {
			return result{}, err
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), hookTimeout)
	defer cancel()
	// Both texts go through the gate at once; the gate bounds the helper
	// processes they share.
	fields := []string{policy.AskConversationField, policy.AskQuestionField}
	texts := []string{conv, question}
	gated := make([]privacy.Result, len(texts))
	errs := make([]error, len(texts))
	var wg sync.WaitGroup
	for i := range texts {
		wg.Go(func() { gated[i], errs[i] = c.gate.Gate(ctx, texts[i]) })
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return result{}, err
	}
	state := map[string]any{}
	for i, g := range gated {
		if g.Local {
			return result{}, nil
		}
		state[fields[i]] = g.Text
	}
	qs := make(map[string]typesafe.Question, len(policy.AskQuestions))
	for id, q := range policy.AskQuestions {
		qs[id] = typesafe.Question(q)
	}
	a, err := c.ask.Ask(ctx, state, qs)
	if err != nil {
		return result{}, err
	}
	if a["answerable_from_context"].Noul >= policy.YesAt &&
		a["cheap_to_undo"].Noul >= policy.YesAt &&
		a["product_ruling"].Noul <= policy.NoAt {
		return result{deny: askGateReason}, nil
	}
	return result{}, nil
}

// askedQuestions is the text of an AskUserQuestion call: each question with
// its options.
func askedQuestions(input map[string]any) string {
	qs, _ := input["questions"].([]any)
	var b strings.Builder
	for _, q := range qs {
		m, _ := q.(map[string]any)
		text, _ := m["question"].(string)
		if text == "" {
			continue
		}
		b.WriteString(text)
		b.WriteString("\n")
		opts, _ := m["options"].([]any)
		for _, o := range opts {
			om, _ := o.(map[string]any)
			label, _ := om["label"].(string)
			desc, _ := om["description"].(string)
			if label == "" {
				continue
			}
			b.WriteString("- " + label)
			if desc != "" {
				b.WriteString(": " + desc)
			}
			b.WriteString("\n")
		}
	}
	return strings.TrimSpace(b.String())
}

// endingQuestion is the last non-empty line of msg when it ends with a
// question mark, else "".
func endingQuestion(msg string) string {
	lines := strings.Split(strings.TrimSpace(msg), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if !strings.HasSuffix(last, "?") {
		return ""
	}
	return last
}

// recentConversation is the last policy.AskTurns user and assistant text
// turns of the main thread, oldest first, each as "role: text", at most
// policy.AskContextChars characters: older turns are dropped first, and a
// single turn over the cap keeps its end. Tool calls, tool results and
// thinking are not turns.
func recentConversation(path string) (string, error) {
	lines, err := transcript.TailLines(path, askTranscriptTail)
	if err != nil {
		return "", err
	}
	var turns []string
	size := 0
	for i := len(lines) - 1; i >= 0 && len(turns) < policy.AskTurns; i-- {
		role, text := turnText(lines[i])
		if text == "" {
			continue
		}
		t := role + ": " + text
		if size > 0 && size+len(t)+2 > policy.AskContextChars {
			break
		}
		t = keepEnd(t, policy.AskContextChars)
		turns = append(turns, t)
		size += len(t) + 2
	}
	for i, j := 0, len(turns)-1; i < j; i, j = i+1, j-1 {
		turns[i], turns[j] = turns[j], turns[i]
	}
	return strings.Join(turns, "\n\n"), nil
}

// keepStart is s cut to at most n bytes from its start, on a rune boundary.
func keepStart(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// keepEnd is s cut to at most n bytes from its end, on a rune boundary.
func keepEnd(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:]
}

// turnText reads one transcript line: the role and the text of a main-thread
// user or assistant message, or "" when the line is anything else.
func turnText(line []byte) (role, text string) {
	var e struct {
		Type        string `json:"type"`
		IsSidechain bool   `json:"isSidechain"`
		IsMeta      bool   `json:"isMeta"`
		Message     struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &e) != nil || e.IsSidechain || e.IsMeta || (e.Type != "user" && e.Type != "assistant") {
		return "", ""
	}
	var s string
	if json.Unmarshal(e.Message.Content, &s) == nil {
		return e.Type, strings.TrimSpace(s)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(e.Message.Content, &blocks) != nil {
		return "", ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, strings.TrimSpace(b.Text))
		}
	}
	return e.Type, strings.Join(parts, "\n")
}
