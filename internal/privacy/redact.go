package privacy

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/IZZY-Agency/quartermaster/internal/config"
)

var (
	emailRe = regexp.MustCompile(`[A-Za-z0-9._%+-]+@(?:[A-Za-z0-9-]+\.)+[A-Za-z0-9-]+`)
	ipv4Re  = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	phoneRe = regexp.MustCompile(`\+?\d[\d .-]{8,}\d`)
	dateRe  = regexp.MustCompile(`\d{4}-\d{1,2}-\d{1,2}|\d{1,2}[./-]\d{1,2}[./-]\d{4}`)
	tokenRe = regexp.MustCompile(`\b[A-Za-z0-9_-]{28,}\b`)
	hexRe   = regexp.MustCompile(`^[0-9a-fA-F]+$`)
	// A slug is words of one case, or numbers, joined by - or _.
	slugRe = regexp.MustCompile(`^(?:[a-z]+|[A-Z]+|[0-9]+)(?:[-_](?:[a-z]+|[A-Z]+|[0-9]+))+$`)
	// base64Re finds base64-style runs, which the token rule cuts at + / =.
	base64Re = regexp.MustCompile(`[A-Za-z0-9+/=]{40,}`)
	// placeholderRe finds the placeholders this package writes.
	placeholderRe = regexp.MustCompile(`<([A-Z]+)_\d+>`)
	// plainSegmentRe is a path segment that reads as an ordinary word.
	plainSegmentRe = regexp.MustCompile(`^(?:[A-Za-z][a-z]*|[a-z0-9]+)$`)
)

// allowedEmails are harmless addresses left in place.
var allowedEmails = map[string]bool{"noreply@anthropic.com": true}

// minTokenEntropy is the Shannon entropy, in bits per character, at or above
// which a long word reads as a token.
const minTokenEntropy = 4.0

// minSlashedEntropy is the entropy at or above which a base64-style run that
// holds a / reads as a secret, not a path: long paths measure about 4.1 to
// 4.4, random base64 of 40 or more characters about 5 and above.
const minSlashedEntropy = 4.8

// redactor numbers placeholders per kind; the same value always gets the
// same placeholder.
type redactor struct {
	found map[string]string // placeholder -> original
	byVal map[string]string // kind + "\x00" + value -> placeholder
	n     map[string]int
}

func newRedactor() *redactor {
	return &redactor{found: map[string]string{}, byVal: map[string]string{}, n: map[string]int{}}
}

func (r *redactor) placeholder(kind, value string) string {
	k := kind + "\x00" + value
	if p, ok := r.byVal[k]; ok {
		return p
	}
	r.n[kind]++
	p := fmt.Sprintf("<%s_%d>", kind, r.n[kind])
	r.byVal[k] = p
	r.found[p] = value
	return p
}

// replace swaps every match of re that keep accepts for a placeholder.
func (r *redactor) replace(text string, re *regexp.Regexp, kind string, keep func(string) bool) string {
	return re.ReplaceAllStringFunc(text, func(m string) string {
		if !keep(m) {
			return m
		}
		return r.placeholder(kind, m)
	})
}

// Redact replaces private material in text with placeholders, in a fixed
// order (emails before client names, so a client rule cannot mangle an
// address). persons are confirmed person names. It returns the redacted text
// and each placeholder or pseudonym with the original it replaced; a
// pseudonym's value lists every spelling it replaced, one per line.
func Redact(text string, clients []config.Pseudonym, persons []string) (string, map[string]string) {
	out, found, _ := redact(text, nil, clients, persons)
	return out, found
}

// redact is Redact with the secrets gitleaks found in the original text. They
// are replaced first, before any pattern can cut one apart; missing is true
// when a secret can no longer be found whole, so part of it may remain.
func redact(text string, secrets []string, clients []config.Pseudonym, persons []string) (out string, found map[string]string, missing bool) {
	r := newRedactor()
	// Secrets go first, on the text gitleaks read. A secret not found whole is
	// missing unless it lies inside a longer secret already replaced.
	var replaced []string
	for _, s := range longestFirst(secrets) {
		if !strings.Contains(text, s) {
			if !slices.ContainsFunc(replaced, func(done string) bool { return strings.Contains(done, s) }) {
				missing = true
			}
			continue
		}
		text = strings.ReplaceAll(text, s, r.placeholder("SECRET", s))
		replaced = append(replaced, s)
	}
	text = redactHome(text)
	text = r.replace(text, emailRe, "EMAIL", func(m string) bool {
		return !allowedEmails[strings.ToLower(m)] && !strings.HasSuffix(m, "@127.0.0.1")
	})
	text = r.replace(text, ipv4Re, "IP", isPublicIPv4)
	// Base64 runs, then tokens, before phones: the phone rule has no word
	// boundaries and would cut a digit run out of a token, leaving its tail.
	text = base64Re.ReplaceAllStringFunc(text, func(m string) string {
		if o := base64Start(m); o >= 0 {
			return m[:o] + r.placeholder("TOKEN", m[o:])
		}
		return m
	})
	text = r.replace(text, tokenRe, "TOKEN", isToken)
	text = r.replace(text, phoneRe, "PHONE", func(m string) bool { return !dateRe.MatchString(m) })
	var pseudonyms []string
	for _, c := range clients {
		protect := protected(pseudonyms)
		text = outside(text, protect, func(s string) string { return replaceClient(s, c, r.found) })
		if c.Pattern != "" && c.Replace != "" {
			pseudonyms = append(pseudonyms, c.Replace)
		}
	}
	protect := protected(pseudonyms)
	for _, p := range longestFirst(persons) {
		text = outside(text, protect, func(s string) string {
			return replaceWord(s, p, func() string { return r.placeholder("PERSON", p) })
		})
	}
	return text, r.found, missing
}

// protected matches the placeholders and the pseudonyms already written,
// which later client patterns and names must not rewrite.
func protected(pseudonyms []string) *regexp.Regexp {
	alts := []string{placeholderRe.String()}
	for _, p := range longestFirst(pseudonyms) {
		alts = append(alts, regexp.QuoteMeta(p))
	}
	return regexp.MustCompile(strings.Join(alts, "|"))
}

// outside applies f to the text between matches of protect.
func outside(text string, protect *regexp.Regexp, f func(string) string) string {
	var b strings.Builder
	last := 0
	for _, loc := range protect.FindAllStringIndex(text, -1) {
		b.WriteString(f(text[last:loc[0]]))
		b.WriteString(text[loc[0]:loc[1]])
		last = loc[1]
	}
	b.WriteString(f(text[last:]))
	return b.String()
}

// redactHome turns the home folder into ~ and removes its Claude Code
// project-folder spelling (each / written as -). Both match whole path
// segments only: a longer folder name that starts with the same letters is
// left alone.
func redactHome(text string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return text
	}
	home = strings.TrimRight(home, "/")
	if home == "" {
		return text
	}
	plain := regexp.MustCompile(regexp.QuoteMeta(home) + `([^A-Za-z0-9._-]|$)`)
	text = plain.ReplaceAllString(text, "~$1")
	dashed := regexp.MustCompile(regexp.QuoteMeta(strings.ReplaceAll(home, "/", "-")) + `([^A-Za-z0-9._]|$)`)
	return dashed.ReplaceAllString(text, "$1")
}

func isPublicIPv4(m string) bool {
	parts := strings.Split(m, ".")
	for _, p := range parts {
		var n int
		if _, err := fmt.Sscanf(p, "%d", &n); err != nil || n > 255 {
			return false
		}
	}
	return parts[0] != "127" && m != "0.0.0.0"
}

func isToken(m string) bool {
	return !hexRe.MatchString(m) && !slugRe.MatchString(m) && entropy(m) >= minTokenEntropy
}

// base64Start returns where a secret starts inside a base64-style run, or -1.
// The run may begin with a path or URL segments, so each start after a / is
// tried too, longest first. A candidate holding a / is a secret only when
// its entropy is that of random data, not a path, and it does not begin with
// an ordinary word; one without a / follows isBase64Secret.
func base64Start(m string) int {
	starts := []int{0}
	for i := 0; i < len(m); i++ {
		if m[i] == '/' {
			starts = append(starts, i+1)
		}
	}
	for _, o := range starts {
		c := m[o:]
		if len(c) < 40 {
			break
		}
		if c[0] == '/' || c[0] == '=' {
			continue
		}
		first, _, slashed := strings.Cut(c, "/")
		switch {
		case !slashed:
			if isBase64Secret(c) {
				return o
			}
		case !plainSegmentRe.MatchString(first) && entropy(c) >= minSlashedEntropy:
			return o
		}
	}
	return -1
}

// isBase64Secret: a run with no / that holds + or =, or upper case, lower
// case and digits together.
func isBase64Secret(m string) bool {
	if strings.ContainsAny(m, "+=") {
		return true
	}
	return strings.ContainsAny(m, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") &&
		strings.ContainsAny(m, "abcdefghijklmnopqrstuvwxyz") &&
		strings.ContainsAny(m, "0123456789")
}

// entropy is the Shannon entropy of s in bits per byte.
func entropy(s string) float64 {
	counts := map[byte]int{}
	for i := 0; i < len(s); i++ {
		counts[s[i]]++
	}
	var h float64
	for _, c := range counts {
		p := float64(c) / float64(len(s))
		h -= p * math.Log2(p)
	}
	return h
}

// replaceClient swaps a client pattern (a regular expression, matched
// without case) for its pseudonym, and records every spelling it replaced. A
// pattern that does not compile is matched as plain text, so a typo never
// leaves the name in place.
func replaceClient(text string, c config.Pseudonym, found map[string]string) string {
	if c.Pattern == "" {
		return text
	}
	re, err := regexp.Compile("(?i)" + c.Pattern)
	if err != nil {
		re = regexp.MustCompile("(?i)" + regexp.QuoteMeta(c.Pattern))
	}
	return re.ReplaceAllStringFunc(text, func(m string) string {
		prev, ok := found[c.Replace]
		switch {
		case !ok:
			found[c.Replace] = m
		case !slices.Contains(strings.Split(prev, "\n"), m):
			found[c.Replace] = prev + "\n" + m
		}
		return c.Replace
	})
}

// replaceWord swaps every whole-word occurrence of w (not inside a longer
// word) for the placeholder ph returns.
func replaceWord(text, w string, ph func() string) string {
	if w == "" {
		return text
	}
	var b strings.Builder
	for {
		i := strings.Index(text, w)
		if i < 0 {
			b.WriteString(text)
			return b.String()
		}
		end := i + len(w)
		before, _ := utf8.DecodeLastRuneInString(text[:i])
		after, _ := utf8.DecodeRuneInString(text[end:])
		b.WriteString(text[:i])
		if i > 0 && isWordRune(before) || end < len(text) && isWordRune(after) {
			b.WriteString(w)
		} else {
			b.WriteString(ph())
		}
		text = text[end:]
	}
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

// longestFirst returns the distinct non-empty values, longest first, so a
// name inside a longer one is not replaced first.
func longestFirst(vals []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range vals {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}
