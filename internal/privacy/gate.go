package privacy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"unicode"

	"github.com/IZZY-Agency/quartermaster/internal/config"
	"github.com/IZZY-Agency/quartermaster/internal/policy"
)

// Options configures a Checker. Helper and Gitleaks are executable paths; an
// empty one means that provider is missing.
type Options struct {
	Helper     string
	Gitleaks   string
	LocalModel string // auto | required | off
	Clients    []config.Pseudonym
}

// Checker runs the privacy gate. It is safe for concurrent use; all its
// calls share one bound of policy.HelperConcurrency helper processes.
type Checker struct {
	o   Options
	sem chan struct{}
}

// New returns a Checker.
func New(o Options) *Checker {
	return &Checker{o: o, sem: make(chan struct{}, policy.HelperConcurrency)}
}

// Gate reasons when the text must stay on this machine.
const (
	ReasonHelperUnavailable = "privacy helper unavailable"
	ReasonStillPrivate      = "still private after redaction"
	ReasonSecretNotRedacted = "a secret could not be redacted"
)

// errUnavailable: the helper says the Foundation Model is not available.
var errUnavailable = errors.New("foundation model unavailable")

// Gate redacts text and decides whether it may leave the machine. A missing
// provider turns its check off (listed in Off); a provider that fails is an
// error. A Local result carries no text.
func (c *Checker) Gate(ctx context.Context, text string) (Result, error) {
	var res Result
	required := c.o.LocalModel == "required"
	if c.o.Helper == "" {
		if required {
			return Result{Local: true, Reason: ReasonHelperUnavailable}, nil
		}
		res.Off = append(res.Off, "name detection", "name confirmation", "leftover check")
	}

	var candidates []string
	if c.o.Helper != "" {
		var err error
		if candidates, err = c.names(ctx, text); err != nil {
			return Result{}, err
		}
	}

	model := c.o.Helper != "" && c.o.LocalModel != "off"
	persons := candidates
	if model {
		confirmed, err := c.confirm(ctx, text, candidates)
		switch {
		case errors.Is(err, errUnavailable):
			model = false
		case err != nil:
			return Result{}, err
		default:
			persons = confirmed
		}
	}
	if !model && c.o.Helper != "" {
		if required {
			return Result{Local: true, Reason: ReasonHelperUnavailable}, nil
		}
		res.Off = append(res.Off, "name confirmation", "leftover check")
	}

	var secrets []string
	if c.o.Gitleaks == "" {
		res.Off = append(res.Off, "gitleaks")
	} else {
		var err error
		if secrets, err = c.gitleaks(ctx, text); err != nil {
			return Result{}, err
		}
	}

	redacted, found, missing := redact(text, secrets, c.o.Clients, persons)
	if missing {
		return Result{Local: true, Reason: ReasonSecretNotRedacted}, nil
	}

	if model {
		// Placeholders are not private; the model reads them as a neutral
		// word, so it judges only what redaction left.
		private, err := c.leftover(ctx, placeholderRe.ReplaceAllString(redacted, policy.LeftoverPlaceholder))
		switch {
		case errors.Is(err, errUnavailable):
			if required {
				return Result{Local: true, Reason: ReasonHelperUnavailable}, nil
			}
			res.Off = append(res.Off, "leftover check")
		case err != nil:
			return Result{}, err
		case private:
			return Result{Local: true, Reason: ReasonStillPrivate}, nil
		}
	}

	res.Text = redacted
	res.Counts = counts(found)
	return res, nil
}

// counts tallies placeholders by kind; client pseudonyms count as CLIENT.
func counts(found map[string]string) map[string]int {
	if len(found) == 0 {
		return nil
	}
	out := map[string]int{}
	for p := range found {
		if m := placeholderRe.FindStringSubmatch(p); m != nil && m[0] == p {
			out[m[1]]++
		} else {
			out["CLIENT"]++
		}
	}
	return out
}

// names asks the helper for person-name candidates.
func (c *Checker) names(ctx context.Context, text string) ([]string, error) {
	in := []map[string]string{{"id": "1", "text": text}}
	var out []struct {
		ID    string   `json:"id"`
		Names []string `json:"names"`
	}
	if err := c.helper(ctx, "names", in, &out); err != nil {
		return nil, err
	}
	var names []string
	for _, o := range out {
		names = append(names, o.Names...)
	}
	return longestFirst(names), nil
}

type askItem struct {
	Key          string `json:"key"`
	Instructions string `json:"instructions"`
	Prompt       string `json:"prompt"`
}

// ask puts the items to the Foundation Model and returns each answer's first
// word, lowercased, by key. The items are split across up to
// policy.HelperConcurrency helper processes run at once; the helper answers
// one item at a time. A model that is unavailable in any of them makes the
// whole ask unavailable; any other failure is the error.
func (c *Checker) ask(ctx context.Context, items []askItem) (map[string]string, error) {
	type reply struct {
		Key    string `json:"key"`
		Answer string `json:"answer"`
	}
	n := min(policy.HelperConcurrency, len(items))
	groups := make([][]askItem, n)
	for i, it := range items {
		groups[i%n] = append(groups[i%n], it)
	}
	outs := make([][]reply, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range groups {
		wg.Go(func() { errs[i] = c.helper(ctx, "ask", groups[i], &outs[i]) })
	}
	wg.Wait()
	for _, err := range errs {
		if errors.Is(err, errUnavailable) {
			return nil, err
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	answers := map[string]string{}
	for _, out := range outs {
		for _, o := range out {
			answers[o.Key] = firstWord(o.Answer)
		}
	}
	for _, it := range items {
		if _, ok := answers[it.Key]; !ok {
			return nil, fmt.Errorf("privacy helper: no answer for %s", it.Key)
		}
	}
	return answers, nil
}

// confirm keeps the candidates the model says are real people's names.
func (c *Checker) confirm(ctx context.Context, text string, candidates []string) ([]string, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	items := make([]askItem, len(candidates))
	for i, n := range candidates {
		items[i] = askItem{
			Key:          fmt.Sprintf("name%d", i),
			Instructions: policy.NameCheck.Instructions,
			Prompt:       policy.NameCheck.Prompt(around(text, n, policy.NameContext), n),
		}
	}
	answers, err := c.ask(ctx, items)
	if err != nil {
		return nil, err
	}
	var persons []string
	for i, n := range candidates {
		if answers[items[i].Key] == "yes" {
			persons = append(persons, n)
		}
	}
	return persons, nil
}

// leftover reports whether any chunk of the redacted text is still private:
// any answer whose first word is not exactly "no" counts as private.
func (c *Checker) leftover(ctx context.Context, text string) (bool, error) {
	chunks := chunk(text, policy.LeftoverChunk)
	if len(chunks) == 0 {
		return false, nil
	}
	items := make([]askItem, len(chunks))
	for i, ch := range chunks {
		items[i] = askItem{
			Key:          fmt.Sprintf("chunk%d", i),
			Instructions: policy.LeftoverCheck.Instructions,
			Prompt:       policy.LeftoverCheck.Prompt(ch),
		}
	}
	answers, err := c.ask(ctx, items)
	if err != nil {
		return false, err
	}
	for _, it := range items {
		if answers[it.Key] != "no" {
			return true, nil
		}
	}
	return false, nil
}

// firstWord is the answer's first run of letters, lowercased ("" when none).
func firstWord(answer string) string {
	words := strings.FieldsFunc(answer, func(r rune) bool { return !unicode.IsLetter(r) })
	if len(words) == 0 {
		return ""
	}
	return strings.ToLower(words[0])
}

// helper runs one helper subcommand with in as JSON on stdin and decodes its
// stdout into out. Exit 3 means the Foundation Model is unavailable.
func (c *Checker) helper(ctx context.Context, sub string, in, out any) error {
	if c.sem != nil {
		select {
		case c.sem <- struct{}{}:
			defer func() { <-c.sem }()
		case <-ctx.Done():
			return fmt.Errorf("privacy helper %s: %w", sub, ctx.Err())
		}
	}
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("privacy helper %s: %w", sub, err)
	}
	cmd := exec.CommandContext(ctx, c.o.Helper, sub)
	cmd.Stdin = bytes.NewReader(body)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 3 {
			return errUnavailable
		}
		return fmt.Errorf("privacy helper %s: %w: %s", sub, err, strings.TrimSpace(stderr.String()))
	}
	if err := json.Unmarshal(stdout.Bytes(), out); err != nil {
		return fmt.Errorf("privacy helper %s: bad output: %w", sub, err)
	}
	return nil
}

// gitleaks returns the secrets gitleaks finds in text. It runs in a temp
// folder so the current repository's .gitleaks.toml cannot change its rules.
func (c *Checker) gitleaks(ctx context.Context, text string) ([]string, error) {
	cmd := exec.CommandContext(ctx, c.o.Gitleaks, "stdin",
		"--report-format", "json", "--report-path", "-", "--no-banner", "--log-level", "error", "--redact=0")
	cmd.Dir = os.TempDir()
	cmd.Stdin = strings.NewReader(text)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	// Exit 1 means gitleaks found secrets; any other failure is an error.
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 1 {
			return nil, fmt.Errorf("gitleaks: %w: %s", err, strings.TrimSpace(stderr.String()))
		}
	}
	var findings []struct {
		Secret string `json:"Secret"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &findings); err != nil {
		return nil, fmt.Errorf("gitleaks: bad report: %w", err)
	}
	secrets := make([]string, 0, len(findings))
	for _, f := range findings {
		secrets = append(secrets, f.Secret)
	}
	return secrets, nil
}

// around returns up to n characters on each side of the first occurrence of
// word in text.
func around(text, word string, n int) string {
	i := strings.Index(text, word)
	if i < 0 {
		return text
	}
	before := []rune(text[:i])
	after := []rune(text[i+len(word):])
	if len(before) > n {
		before = before[len(before)-n:]
	}
	if len(after) > n {
		after = after[:n]
	}
	return string(before) + word + string(after)
}

// chunk splits text into pieces of at most n characters.
func chunk(text string, n int) []string {
	r := []rune(text)
	var out []string
	for len(r) > 0 {
		k := min(n, len(r))
		out = append(out, string(r[:k]))
		r = r[k:]
	}
	return out
}

// FindHelper returns the qm-privcheck path: QM_PRIVCHECK when set (empty or
// not executable means none), else next to the running binary, else on PATH.
// "" means the helper is missing.
func FindHelper() string {
	if p, ok := os.LookupEnv("QM_PRIVCHECK"); ok {
		return executable(p)
	}
	if self, err := os.Executable(); err == nil {
		if p := executable(filepath.Join(filepath.Dir(self), "qm-privcheck")); p != "" {
			return p
		}
	}
	p, _ := exec.LookPath("qm-privcheck")
	return p
}

// FindGitleaks returns the gitleaks path: QM_GITLEAKS when set, else on PATH.
// "" means gitleaks is missing.
func FindGitleaks() string {
	if p, ok := os.LookupEnv("QM_GITLEAKS"); ok {
		return executable(p)
	}
	p, _ := exec.LookPath("gitleaks")
	return p
}

func executable(p string) string {
	fi, err := os.Stat(p)
	if p == "" || err != nil || fi.IsDir() || fi.Mode().Perm()&0o111 == 0 {
		return ""
	}
	return p
}
