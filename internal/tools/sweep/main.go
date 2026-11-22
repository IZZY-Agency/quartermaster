// Command sweep is the optional semantic pre-publish sweep: it asks Jev four
// questions about every comment and documentation paragraph of the export
// set (the ALLOWLIST in scripts/publish.sh), as committed at HEAD:
//
//   - process_narrative: the text tells the story of the development process
//     (review rounds, finding labels, planning documents) instead of what the
//     code does
//   - private_reference: it points at something the public cannot open
//   - personal_identifier: it identifies a private person or their accounts
//   - security_sensitive: it would materially help an attacker
//
// It complements scripts/check-private.sh, which matches words: the sweep
// reads meaning. Run from the repository root (make sweep):
//
//	go run ./internal/tools/sweep [-since <ref>] [-out <file>] [-workers N]
//
// With -since only the files changed between <ref> and HEAD are read (git
// diff <ref>...HEAD); without it, every exported file. Without
// TYPESAFE_API_KEY it says so and exits 0: the sweep is optional.
//
// Each answer is written to the -out file as one JSON line. The hits at or
// above a question's review threshold are listed for a person to read. It
// exits 1 when a chunk could not be judged, or when a security_sensitive
// answer reaches 0.6 or a personal_identifier answer reaches 0.8; the other
// two questions only list hits.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/scanner"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/IZZY-Agency/quartermaster/internal/typesafe"
)

// minChunk and maxChunk bound the text sent for one chunk, in bytes.
const (
	minChunk = 25
	maxChunk = 2400
)

// question is one sweep question with its thresholds: hits at or above
// review are listed; at or above fail (when set) the sweep fails.
type question struct {
	id     string
	q      typesafe.Question
	review float64
	fail   float64 // 0: never fails the sweep
}

func noul(instructions, yes, no string) typesafe.Question {
	return typesafe.Question{
		Type:         "noul",
		Instructions: instructions,
		Criteria:     map[string]string{"true": yes, "false": no},
	}
}

var questions = []question{
	{id: "security_sensitive", review: 0.4, fail: 0.6, q: noul(
		"Does `text` disclose something that would materially help an attacker?",
		"A credential, token, key material, a secret value, or a concrete unpatched weakness with enough detail to exploit it",
		"Describes how security, signing, sandboxing or privacy works, names public identifiers, or discusses a weakness that the code already closes")},
	{id: "personal_identifier", review: 0.5, fail: 0.8, q: noul(
		"Does `text` contain information that identifies a specific private individual or their personal accounts?",
		"A person's name, username or handle, e-mail address, a personal (non-company) account id, a home-directory path, a machine name, or a personal account label",
		"Only company, product, vendor or provider names (for example IZZY Agency, Claude, Codex, TypeSafe, Apple), placeholder values like example.com, or no identity at all")},
	{id: "private_reference", review: 0.8, q: noul(
		"Does `text` point the reader to something a member of the public could not open?",
		"Cites a numbered issue or pull request such as '#71', a private repository, an internal document path, a chat thread, a dashboard or an internal host",
		"Refers only to public things (public URLs, vendor documentation, files that ship in this repository) or to nothing external at all")},
	{id: "process_narrative", review: 0.85, q: noul(
		"Does `text` talk about the project's internal development process instead of what the code or product does?",
		"Credits or describes a code-review round or reviewer (a person or an AI tool such as Codex, Claude or Copilot), cites an audit or review finding label like 'M1' or 'L2(b)', refers to a planning or design document, a spike, or tells the story of how a defect was discovered during review",
		"Explains behaviour, rationale, constraints, platform quirks or history of the code itself without crediting a review process or internal planning artefact")},
}

// chunk is one comment run or documentation paragraph.
type chunk struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// result is a judged chunk: one probability per question, or the error.
type result struct {
	chunk
	Scores map[string]float64 `json:"scores,omitempty"`
	Error  string             `json:"error,omitempty"`
}

// skipped: files that hold no prose of the project's own (licences, data,
// the generated third-party notices, whose names must stay).
func skipped(f string) bool {
	base := path.Base(f)
	if base == "LICENSE" || base == "THIRD_PARTY_NOTICES.md" || strings.HasSuffix(base, ".sha256") {
		return true
	}
	switch path.Ext(base) {
	case ".json", ".jsonl", ".sum", ".mod", ".txt":
		return true
	}
	return false
}

// srcLine is one line of a comment run or paragraph, with its line number.
type srcLine struct {
	n    int
	text string
}

var (
	reHashComment  = regexp.MustCompile(`^\s*#(?:[^!]|$)`)
	reSlashComment = regexp.MustCompile(`^\s*(?://|/\*|\*)`)
)

// chunks splits one file into comment runs (code) or paragraphs (Markdown),
// each cut into chunks of at most maxChunk bytes, so nothing goes unjudged.
func chunks(f, content string) []chunk {
	kind := "code comment"
	var runs [][]srcLine
	switch path.Ext(f) {
	case ".md":
		kind = "documentation"
		runs = lineRuns(content, func(l string) (bool, string) { return strings.TrimSpace(l) != "", "" })
	case ".go":
		runs = goComments(content)
	case ".swift":
		runs = lineRuns(content, func(l string) (bool, string) {
			return reSlashComment.MatchString(l), trailing(l, "//", `"`)
		})
	default: // shell, Makefile, YAML, templates, .gitignore
		runs = lineRuns(content, func(l string) (bool, string) {
			return reHashComment.MatchString(l), trailing(l, " #", `"'`)
		})
	}
	var out []chunk
	for _, r := range runs {
		out = append(out, split(f, kind, r)...)
	}
	return out
}

// lineRuns groups the lines in returns true for into runs of consecutive
// lines; a trailing comment on another line (the second result, when at
// least minChunk long) is a run of its own.
func lineRuns(content string, in func(string) (bool, string)) [][]srcLine {
	var runs [][]srcLine
	var cur []srcLine
	for i, l := range strings.Split(content, "\n") {
		ok, tail := in(l)
		if ok {
			cur = append(cur, srcLine{i + 1, l})
			continue
		}
		if len(cur) > 0 {
			runs = append(runs, cur)
			cur = nil
		}
		if len(strings.TrimSpace(tail)) >= minChunk {
			runs = append(runs, []srcLine{{i + 1, tail}})
		}
	}
	if len(cur) > 0 {
		runs = append(runs, cur)
	}
	return runs
}

// trailing returns the comment that ends a code line: from the first marker
// outside quotes (any of quotes) to the end, or "".
func trailing(l, marker, quotes string) string {
	var open rune
	for i, r := range l {
		switch {
		case open != 0:
			if r == open {
				open = 0
			}
		case strings.ContainsRune(quotes, r):
			open = r
		case strings.HasPrefix(l[i:], marker):
			return strings.TrimSpace(l[i:])
		}
	}
	return ""
}

// goComments returns the comments of a Go file, trailing and block comments
// included, as go/scanner finds them; comments on consecutive lines form one
// run.
func goComments(content string) [][]srcLine {
	fset := token.NewFileSet()
	src := []byte(content)
	file := fset.AddFile("f.go", fset.Base(), len(src))
	var sc scanner.Scanner
	sc.Init(file, src, nil, scanner.ScanComments)
	var runs [][]srcLine
	var cur []srcLine
	last := -1
	for {
		pos, tok, lit := sc.Scan()
		if tok == token.EOF {
			break
		}
		if tok != token.COMMENT {
			continue
		}
		line := fset.Position(pos).Line
		if line > last+1 && len(cur) > 0 {
			runs = append(runs, cur)
			cur = nil
		}
		for j, l := range strings.Split(lit, "\n") {
			cur = append(cur, srcLine{line + j, l})
		}
		last = line + strings.Count(lit, "\n")
	}
	if len(cur) > 0 {
		runs = append(runs, cur)
	}
	return runs
}

// split cuts one run into chunks of at most maxChunk bytes on line
// boundaries; a longer line is cut at rune boundaries. A run shorter than
// minChunk is dropped.
func split(f, kind string, run []srcLine) []chunk {
	var total strings.Builder
	for _, l := range run {
		total.WriteString(l.text)
	}
	if len(strings.TrimSpace(total.String())) < minChunk {
		return nil
	}
	var out []chunk
	var buf []string
	size, start := 0, 0
	flush := func() {
		if text := strings.TrimSpace(strings.Join(buf, "\n")); text != "" {
			out = append(out, chunk{File: f, Line: start, Kind: kind, Text: text})
		}
		buf, size = nil, 0
	}
	for _, l := range run {
		for _, piece := range cutRunes(l.text, maxChunk) {
			if size > 0 && size+1+len(piece) > maxChunk {
				flush()
			}
			if size == 0 {
				start = l.n
			}
			buf = append(buf, piece)
			size += len(piece) + 1
		}
	}
	flush()
	return out
}

// cutRunes cuts s into pieces of at most n bytes, never inside a rune.
func cutRunes(s string, n int) []string {
	var out []string
	for len(s) > n {
		i := n
		for i > 0 && !utf8.RuneStart(s[i]) {
			i--
		}
		out = append(out, s[:i])
		s = s[i:]
	}
	return append(out, s)
}

var reAllowlist = regexp.MustCompile(`(?s)\nALLOWLIST=\((.*?)\n\)`)

// allowlist reads the ALLOWLIST array of scripts/publish.sh.
func allowlist(publish string) ([]string, error) {
	m := reAllowlist.FindStringSubmatch(publish)
	if m == nil {
		return nil, errors.New("no ALLOWLIST=( ... ) in scripts/publish.sh")
	}
	var out []string
	for l := range strings.SplitSeq(m[1], "\n") {
		if i := strings.Index(l, "#"); i >= 0 {
			l = l[:i]
		}
		out = append(out, strings.Fields(l)...)
	}
	if len(out) == 0 {
		return nil, errors.New("ALLOWLIST in scripts/publish.sh is empty")
	}
	return out, nil
}

func git(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

func nulList(s string) []string {
	var out []string
	for f := range strings.SplitSeq(s, "\x00") {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// files lists the exported files at HEAD, only those changed since since
// when it is set.
func files(since string) ([]string, error) {
	publish, err := git("show", "HEAD:scripts/publish.sh")
	if err != nil {
		return nil, err
	}
	allow, err := allowlist(publish)
	if err != nil {
		return nil, err
	}
	tracked, err := git(append([]string{"ls-tree", "-r", "-z", "--name-only", "HEAD", "--"}, allow...)...)
	if err != nil {
		return nil, err
	}
	out := nulList(tracked)
	if since != "" {
		changed, err := git(append([]string{"diff", "--name-only", "-z", since + "...HEAD", "--"}, allow...)...)
		if err != nil {
			return nil, err
		}
		keep := map[string]bool{}
		for _, f := range nulList(changed) {
			keep[f] = true
		}
		var kept []string
		for _, f := range out {
			if keep[f] {
				kept = append(kept, f)
			}
		}
		out = kept
	}
	var prose []string
	for _, f := range out {
		if !skipped(f) {
			prose = append(prose, f)
		}
	}
	return prose, nil
}

// asker is the part of the TypeSafe client the sweep uses.
type asker interface {
	Ask(ctx context.Context, state any, qs map[string]typesafe.Question) (map[string]typesafe.Answer, error)
}

// judge asks every question about every chunk, workers at a time.
func judge(ctx context.Context, c asker, cs []chunk, workers int, progress io.Writer) []result {
	qs := make(map[string]typesafe.Question, len(questions))
	for _, q := range questions {
		qs[q.id] = q.q
	}
	out := make([]result, len(cs))
	next := make(chan int)
	var wg sync.WaitGroup
	var mu sync.Mutex
	done := 0
	for range workers {
		wg.Go(func() {
			for i := range next {
				r := result{chunk: cs[i]}
				ans, err := c.Ask(ctx, map[string]string{"text": cs[i].Text, "kind": cs[i].Kind}, qs)
				if err != nil {
					r.Error = err.Error()
				} else {
					r.Scores = make(map[string]float64, len(ans))
					for id, a := range ans {
						r.Scores[id] = a.Noul
					}
				}
				out[i] = r
				mu.Lock()
				done++
				if done%100 == 0 {
					sayf(progress, "sweep: %d of %d judged\n", done, len(cs))
				}
				mu.Unlock()
			}
		})
	}
	for i := range cs {
		next <- i
	}
	close(next)
	wg.Wait()
	return out
}

// report lists the hits per question and returns whether the sweep passed.
func report(w io.Writer, rs []result) bool {
	pass := true
	errs := 0
	for _, r := range rs {
		if r.Error != "" {
			errs++
		}
	}
	if errs > 0 {
		sayf(w, "sweep: %d of %d chunks could not be judged (see the output file)\n", errs, len(rs))
		pass = false
	}
	for _, q := range questions {
		var hits []result
		for _, r := range rs {
			if r.Error == "" && r.Scores[q.id] >= q.review {
				hits = append(hits, r)
			}
		}
		sort.SliceStable(hits, func(i, j int) bool { return hits[i].Scores[q.id] > hits[j].Scores[q.id] })
		failing := 0
		for _, h := range hits {
			if q.fail > 0 && h.Scores[q.id] >= q.fail {
				failing++
			}
		}
		if failing > 0 {
			pass = false
		}
		gate := "lists only"
		if q.fail > 0 {
			gate = fmt.Sprintf("fails at %.2f", q.fail)
		}
		sayf(w, "\n== %s: %d to read (review at %.2f, %s), %d failing\n", q.id, len(hits), q.review, gate, failing)
		for i, h := range hits {
			if i == 40 {
				sayf(w, "   ... and %d more in the output file\n", len(hits)-40)
				break
			}
			text := strings.Join(strings.Fields(h.Text), " ")
			if r := []rune(text); len(r) > 110 {
				text = string(r[:110]) + "…"
			}
			sayf(w, "   %.3f %s:%d | %s\n", h.Scores[q.id], h.File, h.Line, text)
		}
	}
	return pass
}

func writeResults(file string, rs []result) error {
	if err := os.MkdirAll(path.Dir(file), 0o755); err != nil {
		return err
	}
	f, err := os.Create(file)
	if err != nil {
		return err
	}
	bw := bufio.NewWriter(f)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	for _, r := range rs {
		if err := enc.Encode(r); err != nil {
			_ = f.Close()
			return err
		}
	}
	if err := bw.Flush(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func main() {
	since := flag.String("since", "", "read only the files changed between this ref and HEAD")
	out := flag.String("out", "bin/sweep.jsonl", "where each answer is written, one JSON line per chunk")
	workers := flag.Int("workers", 8, "requests in flight")
	flag.Parse()
	os.Exit(run(*since, *out, *workers, os.Getenv("TYPESAFE_API_KEY"), os.Stdout, os.Stderr))
}

func run(since, out string, workers int, key string, stdout, stderr io.Writer) int {
	c := typesafe.New(key)
	if c == nil {
		sayln(stdout, "sweep: TYPESAFE_API_KEY is not set; the sweep is optional and was skipped")
		return 0
	}
	return sweep(context.Background(), c, since, out, workers, stdout, stderr)
}

func sweep(ctx context.Context, c asker, since, out string, workers int, stdout, stderr io.Writer) int {
	if workers < 1 {
		workers = 1
	}
	fs, err := files(since)
	if err != nil {
		sayln(stderr, "sweep:", err)
		return 2
	}
	var cs []chunk
	for _, f := range fs {
		content, err := git("show", "HEAD:"+f)
		if err != nil {
			sayln(stderr, "sweep:", err)
			return 2
		}
		cs = append(cs, chunks(f, content)...)
	}
	scope := "every exported file"
	if since != "" {
		scope = "the exported files changed since " + since
	}
	sayf(stderr, "sweep: %d chunks in %d files (%s)\n", len(cs), len(fs), scope)
	rs := judge(ctx, c, cs, workers, stderr)
	if err := writeResults(out, rs); err != nil {
		sayln(stderr, "sweep: writing results:", err)
		return 2
	}
	sayf(stderr, "sweep: answers in %s\n", out)
	if !report(stdout, rs) {
		sayln(stdout, "\nsweep: FAILED")
		return 1
	}
	sayln(stdout, "\nsweep: passed (read the listed hits all the same)")
	return 0
}

// sayf and sayln print to w; a failed write to the terminal is not an error
// the sweep can act on.
func sayf(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }

func sayln(w io.Writer, a ...any) { _, _ = fmt.Fprintln(w, a...) }
