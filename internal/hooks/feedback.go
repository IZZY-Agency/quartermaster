package hooks

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/IZZY-Agency/quartermaster/internal/config"
	"github.com/IZZY-Agency/quartermaster/internal/paths"
	"github.com/IZZY-Agency/quartermaster/internal/policy"
	"github.com/IZZY-Agency/quartermaster/internal/typesafe"
)

// feedbackNoteText is the note the coordinator reads after the user stated a
// lasting rule that nothing written down covers yet.
const feedbackNoteText = "The user's last message looks like a new lasting rule not covered by CLAUDE.md or memory: save it (CLAUDE.md or a feedback memory)."

// feedbackLocalNoteText is the note when the user's message had to stay on
// this machine, so it was never checked.
const feedbackLocalNoteText = "The user's message stayed on this machine (the privacy gate kept it local); it was not checked for a lasting rule."

// FeedbackTimeout bounds one analysis: it runs in the background, so it may
// take longer than a hook, but never forever.
const FeedbackTimeout = 2 * time.Minute

// Leftovers in the sessions folder are swept: a prompt file (or a note's
// temporary or taken copy) older than staleFeedbackFile belongs to an
// analyzer that died; a pending note older than staleFeedbackNote belongs to
// a session that ended.
const (
	staleFeedbackFile = FeedbackTimeout + time.Minute
	staleFeedbackNote = 24 * time.Hour
)

// feedbackNoteSuffix names a session's pending note: <state>/sessions/<id>-fb-note.txt.
const feedbackNoteSuffix = "-fb-note.txt"

// promptFileName is the name of a prompt the feedback hook saved for the
// analyzer: <session id>-fb-<n>.txt.
var promptFileName = regexp.MustCompile(`^(.+)-fb-[0-9]+\.txt$`)

// feedbackFileName matches every file the feedback hook leaves in the
// sessions folder: a prompt file, a pending note, and a note's taken copy
// (<note>.<n>) or temporary file (<note>.tmp-<n>), each named after a valid
// session id. The sweep removes nothing else, so a session id that holds
// "-fb-" does not expose that session's other files.
var feedbackFileName = regexp.MustCompile(`^(.+)-fb-([0-9]+\.txt|note\.txt(\.[0-9]+|\.tmp-[0-9]+)?)$`)

// isFeedbackFile tells whether name is one of the feedback hook's files.
func isFeedbackFile(name string) bool {
	m := feedbackFileName.FindStringSubmatch(name)
	return m != nil && sessionIDPattern.MatchString(m[1])
}

// analyzerArgv is the command that analyzes one saved prompt: this qm's
// feedback-analyze. Tests replace it.
var analyzerArgv = func(file, memory string) ([]string, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("feedback: find qm: %w", err)
	}
	argv := []string{exe, "feedback-analyze"}
	if memory != "" {
		argv = append(argv, "--memory", memory)
	}
	return append(argv, file), nil
}

// startDetached starts argv in its own session with no standard streams and
// does not wait for it. Tests replace it.
var startDetached = func(argv []string) (*os.Process, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd.Process, nil
}

// feedbackDir is <state>/sessions, made when missing.
func feedbackDir() (string, error) {
	dir, err := paths.StateDir()
	if err != nil {
		return "", err
	}
	dir = filepath.Join(dir, "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// feedback saves the user's message and starts the analyzer in the
// background, so the message is never held up (design 5.7). The analyzer's
// note, if any, reaches the coordinator through feedback-deliver.
func feedback(c *call) (result, error) {
	if c.in.AgentID != "" || strings.TrimSpace(c.in.Prompt) == "" {
		return result{}, nil
	}
	if !sessionIDPattern.MatchString(c.in.SessionID) {
		return result{}, fmt.Errorf("feedback: bad session id %q", c.in.SessionID)
	}
	dir, err := feedbackDir()
	if err != nil {
		return result{}, fmt.Errorf("feedback: %w", err)
	}
	file := filepath.Join(dir, fmt.Sprintf("%s-fb-%d.txt", c.in.SessionID, time.Now().UnixNano()))
	if err := writeNew(file, c.in.Prompt); err != nil {
		return result{}, fmt.Errorf("feedback: %w", err)
	}
	memory := ""
	if c.in.TranscriptPath != "" {
		// The transcript sits in the project's folder, next to its memory.
		memory = filepath.Join(filepath.Dir(c.in.TranscriptPath), "memory", "MEMORY.md")
	}
	argv, err := analyzerArgv(file, memory)
	if err == nil {
		var p *os.Process
		if p, err = startDetached(argv); err == nil && p != nil {
			err = p.Release()
		}
	}
	if err != nil {
		_ = os.Remove(file)
		return result{}, fmt.Errorf("feedback: start the analyzer: %w", err)
	}
	return result{}, nil
}

// writeNew writes text to a new 0600 file.
func writeNew(path, text string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(text)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// feedbackDeliver prints the session's pending feedback note, once: the note
// is taken (renamed) before it is read, so two hooks never both print it.
func feedbackDeliver(c *call) (result, error) {
	if c.in.AgentID != "" || !sessionIDPattern.MatchString(c.in.SessionID) {
		return result{}, nil
	}
	dir, err := paths.StateDir()
	if err != nil {
		return result{}, err
	}
	sweepFeedback(filepath.Join(dir, "sessions"), time.Now())
	note := filepath.Join(dir, "sessions", c.in.SessionID+feedbackNoteSuffix)
	taken := note + "." + strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := os.Rename(note, taken); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return result{}, nil
		}
		return result{}, err
	}
	b, err := os.ReadFile(taken)
	_ = os.Remove(taken)
	if err != nil {
		return result{}, err
	}
	if text := strings.TrimSpace(string(b)); text != "" {
		return result{note: text}, nil
	}
	return result{}, nil
}

// AnalyzeFeedback is qm feedback-analyze: it reads and removes a prompt the
// feedback hook saved, and writes the session's pending note when Jev says
// the message states a lasting rule that none of the closest written rules
// (~/.claude/CLAUDE.md bullets and the lines of the memory index at memory)
// already covers. Every text goes through the privacy gate first: a message
// kept local gets a note saying it was not checked, a rule kept local is
// skipped. Without Jev it does nothing. Stale leftovers are swept first.
func AnalyzeFeedback(ctx context.Context, file, memory string, cfg config.Config) error {
	dir, err := feedbackDir()
	if err != nil {
		return fmt.Errorf("feedback: %w", err)
	}
	abs, err := filepath.Abs(file)
	if err != nil {
		return fmt.Errorf("feedback: %w", err)
	}
	m := promptFileName.FindStringSubmatch(filepath.Base(abs))
	if filepath.Dir(abs) != filepath.Clean(dir) || m == nil || !sessionIDPattern.MatchString(m[1]) {
		return fmt.Errorf("feedback: %s is not a prompt saved by the feedback hook", file)
	}
	session := m[1]
	sweepFeedback(dir, time.Now())
	b, err := os.ReadFile(abs)
	if err != nil {
		return fmt.Errorf("feedback: %w", err)
	}
	defer func() { _ = os.Remove(abs) }()

	ask, gate := jev(cfg)
	msg := strings.TrimSpace(string(b))
	if ask == nil || msg == "" || len(msg) > policy.FeedbackMaxChars {
		return nil
	}
	g, err := gate.Gate(ctx, msg)
	if err != nil {
		return err
	}
	if g.Local {
		return writePending(filepath.Join(dir, session+feedbackNoteSuffix), feedbackLocalNoteText)
	}
	a, err := ask.Ask(ctx, map[string]any{policy.FeedbackMessageField: g.Text}, questions(policy.FeedbackLastingRule))
	if err != nil {
		return fmt.Errorf("feedback: %w", err)
	}
	if a["lasting_rule"].Noul < policy.YesAt {
		return nil
	}
	rules, err := writtenRules(memory)
	if err != nil {
		return fmt.Errorf("feedback: %w", err)
	}
	covered := questions(policy.FeedbackCovered)
	for _, r := range candidates(msg, rules, policy.FeedbackCandidates) {
		gr, err := gate.Gate(ctx, r)
		if err != nil {
			return err
		}
		if gr.Local {
			// A rule that must stay local cannot be checked: skip it.
			continue
		}
		a, err := ask.Ask(ctx, map[string]any{policy.FeedbackRuleField: gr.Text, policy.FeedbackMessageField: g.Text}, covered)
		if err != nil {
			return fmt.Errorf("feedback: %w", err)
		}
		if a["covered"].Noul >= policy.YesAt {
			return nil
		}
	}
	return writePending(filepath.Join(dir, session+feedbackNoteSuffix), feedbackNoteText)
}

func questions(qs map[string]policy.Question) map[string]typesafe.Question {
	out := make(map[string]typesafe.Question, len(qs))
	for id, q := range qs {
		out[id] = typesafe.Question(q)
	}
	return out
}

// writePending writes the pending note atomically: a reader sees all of it or
// nothing.
func writePending(path, text string) error {
	tmp := path + ".tmp-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := writeNew(tmp, text); err != nil {
		return fmt.Errorf("feedback: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("feedback: %w", err)
	}
	return nil
}

// writtenRules are the bullets of the user's CLAUDE.md ($CLAUDE_CONFIG_DIR,
// else ~/.claude) and of the memory index at memory; a missing file has none.
func writtenRules(memory string) ([]string, error) {
	claudeDir := os.Getenv("CLAUDE_CONFIG_DIR")
	if claudeDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		claudeDir = filepath.Join(home, ".claude")
	}
	var rules []string
	for _, p := range []string{filepath.Join(claudeDir, "CLAUDE.md"), memory} {
		if p == "" {
			continue
		}
		b, err := os.ReadFile(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		rules = append(rules, bullets(string(b))...)
	}
	return rules, nil
}

// bullets are the Markdown list items of text, without their markers.
func bullets(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		for _, marker := range []string{"- ", "* "} {
			if item, ok := strings.CutPrefix(line, marker); ok && strings.TrimSpace(item) != "" {
				out = append(out, strings.TrimSpace(item))
			}
		}
	}
	return out
}

// candidates are the n rules that share the most words with msg, most first;
// ties keep the rules' order.
func candidates(msg string, rules []string, n int) []string {
	have := words(msg)
	type scored struct {
		rule  string
		score int
	}
	all := make([]scored, len(rules))
	for i, r := range rules {
		for w := range words(r) {
			if have[w] {
				all[i].score++
			}
		}
		all[i].rule = r
	}
	slices.SortStableFunc(all, func(a, b scored) int { return b.score - a.score })
	out := make([]string, 0, min(n, len(all)))
	for _, s := range all[:min(n, len(all))] {
		out = append(out, s.rule)
	}
	return out
}

// words is the set of lower-case words of three or more letters or digits.
func words(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len([]rune(w)) >= 3 {
			out[w] = true
		}
	}
	return out
}

// sweepFeedback removes the feedback leftovers in dir that are stale at now.
// It is best effort: a file it cannot read or remove is left for next time.
func sweepFeedback(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !isFeedbackFile(name) {
			continue
		}
		maxAge := staleFeedbackFile
		if strings.HasSuffix(name, feedbackNoteSuffix) {
			maxAge = staleFeedbackNote
		}
		fi, err := e.Info()
		if err != nil || now.Sub(fi.ModTime()) <= maxAge {
			continue
		}
		_ = os.Remove(filepath.Join(dir, name))
	}
}
