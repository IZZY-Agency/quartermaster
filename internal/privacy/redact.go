package privacy

import (
	"fmt"
	"maps"
	"math"
	"net/netip"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/IZZY-Agency/quartermaster/internal/config"
	"github.com/IZZY-Agency/quartermaster/internal/policy"
)

var (
	// emailRe takes letters and digits of any script, zero-width characters,
	// fullwidth and ideographic dots, a quoted local part, the at sign in its
	// fullwidth and small forms. The last label starts with a letter, so a
	// version after an @ (golang.org/x/text@v0.3.0-rc.1) is not an address.
	emailRe = regexp.MustCompile(`(?:"[^"\r\n]{1,64}"|[\p{L}\p{M}\p{N}\p{Cf}._%+\x{FF0E}\x{3002}\x{FF61}-]+)` +
		`[@\x{FF20}\x{FE6B}](?:[\p{L}\p{M}\p{N}\p{Cf}-]+[.\x{FF0E}\x{3002}\x{FF61}])+\p{L}[\p{L}\p{M}\p{N}\p{Cf}-]*`)
	ipv4Re = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	// ipv6Re finds runs that may hold an IPv6 address; replaceIPv6 decides.
	ipv6Re = regexp.MustCompile(`[0-9A-Za-z_:.]*:[0-9A-Za-z_:.]*`)
	// phoneRe is an optional country code and an area code in parentheses
	// before at least seven more characters, or ten or more digits, spaces,
	// dots and dashes from a digit to a digit.
	phoneRe      = regexp.MustCompile(phoneExpr)
	phoneWholeRe = regexp.MustCompile(`^(?:` + phoneExpr + `)$`)
	// dateRe is a year before month and day, or day and month in either
	// order before the year; dateSpans checks the numbers.
	dateRe = regexp.MustCompile(`(\d{4})[-./](\d{1,2})[-./](\d{1,2})|(\d{1,2})[./-](\d{1,2})[./-](\d{4})`)
	// timeRe is a time with fractional seconds, whose digits after the dot
	// would read as a number.
	timeRe = regexp.MustCompile(`\d{1,2}:\d{2}:\d{2}[.,]\d+`)
	// credentialRe finds the value of a field named like a credential (a key,
	// token, secret, password or auth field), written name=value or
	// name: value, quoted or not; isCredential decides.
	credentialRe = regexp.MustCompile(`(?i)\b(?:[a-z0-9]+[_.-])*(?:key|token|secret|passw(?:or)?d|pwd|credentials?|auth)\s*[:=]\s*["']?([A-Za-z0-9+/=_.~-]{16,})`)
	tokenRe      = regexp.MustCompile(`\b[A-Za-z0-9_-]{28,}\b`)
	hexRe        = regexp.MustCompile(`^[0-9a-fA-F]+$`)
	// vendorKeyRe finds keys with a well-known vendor prefix, which may be
	// shorter than tokenRe's floor or shaped like a slug. A match must not be
	// followed by a word character (see replaceVendorKeys).
	vendorKeyRe = regexp.MustCompile(`\b(?:` +
		`(?:AKIA|ASIA|ABIA|ACCA|AGPA|AIDA|AIPA|ANPA|ANVA|APKA|AROA|ASCA)[A-Z0-9]{16}` + // AWS key ids
		`|xox[abposr]-[A-Za-z0-9-]{10,}|xapp-[A-Za-z0-9-]{10,}` + // Slack
		`|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}` + // GitHub
		`|gl(?:pat|dt|rt|ptt)-[A-Za-z0-9_-]{20,}` + // GitLab
		`|(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{10,}` + // Stripe
		`|sk-(?:ant|proj|svcacct|admin)-[A-Za-z0-9_-]{20,}` + // Anthropic, OpenAI
		`|AIza[A-Za-z0-9_-]{35}|npm_[A-Za-z0-9]{36}|hf_[A-Za-z0-9]{30,}` + // Google, npm, Hugging Face
		`)`)
	// A slug is words of one case, or numbers, joined by - or _.
	slugRe = regexp.MustCompile(`^(?:[a-z]+|[A-Z]+|[0-9]+)(?:[-_](?:[a-z]+|[A-Z]+|[0-9]+))+$`)
	// base64Re finds base64-style runs, which the token rule cuts at + / =.
	base64Re = regexp.MustCompile(`[A-Za-z0-9+/=]{40,}`)
	// placeholderRe finds text shaped like the placeholders this package
	// writes, which the input may hold too.
	placeholderRe = regexp.MustCompile(`<([A-Z]+)_\d+>`)
	// markRe finds the stand-ins redact writes while it runs.
	markRe = regexp.MustCompile(mark + `(\d+)` + mark)
	// plainSegmentRe is a path segment that reads as an ordinary word.
	plainSegmentRe = regexp.MustCompile(`^(?:[A-Za-z][a-z]*|[a-z0-9]+)$`)
)

const phoneExpr = `(?:\+?\d{1,3}[ .-]?)?\(\d{1,4}\)[ .-]?\d[\d .-]{5,}\d|\+?\d[\d .-]{8,}\d`

// maxPhone is the longest part of a longer number-like run tried as a phone
// number: fifteen digits with their separators and an area code.
const maxPhone = 24

// allowedEmails are harmless addresses left in place.
var allowedEmails = map[string]bool{"noreply@anthropic.com": true}

// minTokenEntropy is the Shannon entropy, in bits per character, at or above
// which a long word reads as a token.
const minTokenEntropy = 4.0

// maxSlashedEntropy is the most entropy slashedEntropy asks of a run: long
// paths measure about 4.0 to 4.5.
const maxSlashedEntropy = 4.8

// slashedEntropy is the entropy at or above which a base64-style run of n
// characters that holds a / reads as a secret, not a path. Random base64 of n
// characters measures about 6 - 63/(2n ln 2) bits per character (4.8 at 40,
// 5.2 at 64); the floor sits 0.35 below that, 4.5 at 40, and never above
// maxSlashedEntropy.
func slashedEntropy(n int) float64 {
	return min(maxSlashedEntropy, 6-63/(2*float64(n)*math.Ln2)-0.35)
}

// minCredentialEntropy is the entropy at or above which the value of a
// credential field reads as an encoded secret: random values of 16 or more
// characters measure about 3.5 and above, words and names less.
const minCredentialEntropy = 3.0

// minHexEntropy is the entropy at or above which a long hex run reads as a
// key: random hex of 32 or more characters measures about 3 and above, a
// repeated word such as deadbeef about 2.
const minHexEntropy = 2.5

// mark frames a stand-in redact writes while it runs. It is a Unicode
// noncharacter, replaced in the input first, so text that only looks like a
// placeholder can never pass for one redact wrote.
const mark = "\uFDD0"

// span is one stand-in: a placeholder for value, or, with no kind, the
// client pseudonym value.
type span struct{ kind, value string }

// redactor records the stand-ins written; the same value of a kind always
// gets the same one. Numbers are given at the end (finish), after every
// placeholder-shaped string already in the text is known.
type redactor struct {
	spans     []span
	byVal     map[string]int    // kind + "\x00" + value -> index in spans
	spellings map[string]string // pseudonym -> each spelling it replaced, one per line
}

func newRedactor() *redactor {
	return &redactor{byVal: map[string]int{}, spellings: map[string]string{}}
}

// write returns the stand-in for value of kind ("" for a pseudonym).
func (r *redactor) write(kind, value string) string {
	k := kind + "\x00" + value
	i, ok := r.byVal[k]
	if !ok {
		i = len(r.spans)
		r.spans = append(r.spans, span{kind, value})
		r.byVal[k] = i
	}
	return mark + strconv.Itoa(i) + mark
}

// replace swaps every match of re that keep accepts for a placeholder.
func (r *redactor) replace(text string, re *regexp.Regexp, kind string, keep func(string) bool) string {
	return re.ReplaceAllStringFunc(text, func(m string) string {
		if !keep(m) {
			return m
		}
		return r.write(kind, m)
	})
}

// redaction is what redact did to a text.
type redaction struct {
	text string
	// view is text with every placeholder redact wrote as
	// policy.LeftoverPlaceholder, for the leftover check. Pseudonyms, and
	// placeholder-shaped text that was in the input, stay as written, so the
	// check judges them.
	view string
	// found maps each placeholder or pseudonym to the original it replaced;
	// a pseudonym's value lists every spelling it replaced, one per line.
	found map[string]string
	// kinds maps each placeholder redact wrote to its kind.
	kinds map[string]string
	// missing is true when a secret could no longer be found whole.
	missing bool
}

// finish numbers the placeholders per kind, in the order they were written,
// skipping every placeholder-shaped string the text already holds (a
// pseudonym's included), and writes the stand-ins out.
func (r *redactor) finish(text string) redaction {
	at := func(m string) span {
		i, _ := strconv.Atoi(m[len(mark) : len(m)-len(mark)])
		return r.spans[i]
	}
	pseudonyms := markRe.ReplaceAllStringFunc(text, func(m string) string {
		if s := at(m); s.kind == "" {
			return s.value
		}
		return m
	})
	taken := map[string]bool{}
	for _, p := range placeholderRe.FindAllString(pseudonyms, -1) {
		taken[p] = true
	}
	red := redaction{found: map[string]string{}, kinds: map[string]string{}}
	names := map[span]string{}
	n := map[string]int{}
	for _, s := range r.spans {
		if s.kind == "" {
			names[s] = s.value
			continue
		}
		var p string
		for p == "" || taken[p] {
			n[s.kind]++
			p = fmt.Sprintf("<%s_%d>", s.kind, n[s.kind])
		}
		names[s] = p
		red.found[p] = s.value
		red.kinds[p] = s.kind
	}
	maps.Copy(red.found, r.spellings)
	red.text = markRe.ReplaceAllStringFunc(text, func(m string) string { return names[at(m)] })
	red.view = markRe.ReplaceAllStringFunc(text, func(m string) string {
		if s := at(m); s.kind == "" {
			return s.value
		}
		return policy.LeftoverPlaceholder
	})
	return red
}

// Redact replaces private material in text with placeholders, in a fixed
// order (emails before client names, so a client rule cannot mangle an
// address). persons are confirmed person names. It returns the redacted text
// and each placeholder or pseudonym with the original it replaced; a
// pseudonym's value lists every spelling it replaced, one per line.
func Redact(text string, clients []config.Pseudonym, persons []string) (string, map[string]string) {
	red := redact(text, nil, clients, persons)
	return red.text, red.found
}

// redact is Redact with the secrets gitleaks found in the original text. They
// are replaced first, before any pattern can cut one apart; missing is true
// when a secret can no longer be found whole, so part of it may remain.
func redact(text string, secrets []string, clients []config.Pseudonym, persons []string) redaction {
	r := newRedactor()
	// The input cannot hold the mark, so every stand-in is one redact wrote.
	unmark := strings.NewReplacer(mark, "\uFFFD")
	text = unmark.Replace(text)
	// Secrets go first, on the text gitleaks read. A secret not found whole is
	// missing unless it lies inside a longer secret already replaced.
	var replaced []string
	missing := false
	for _, s := range longestFirst(secrets) {
		s = unmark.Replace(s)
		hit := false
		// Outside the stand-ins already written: a short secret must not
		// rewrite the number inside one.
		text = outside(text, markSpans, func(p string) string {
			if !strings.Contains(p, s) {
				return p
			}
			hit = true
			return strings.ReplaceAll(p, s, r.write("SECRET", s))
		})
		if hit {
			replaced = append(replaced, s)
		} else if !slices.ContainsFunc(replaced, func(done string) bool { return strings.Contains(done, s) }) {
			missing = true
		}
	}
	text = redactHome(text)
	text = r.replace(text, emailRe, "EMAIL", func(m string) bool { return !allowedEmails[strings.ToLower(m)] })
	text = r.replaceIPv6(text)
	text = r.replace(text, ipv4Re, "IP", isPublicIPv4)
	// Credential values, vendor keys, base64 runs, then tokens, before
	// phones: a phone match must not cut a digit run out of a key, leaving
	// the rest of it.
	text = r.replaceCredentials(text)
	text = r.replaceVendorKeys(text)
	text = base64Re.ReplaceAllStringFunc(text, func(m string) string {
		if o := base64Start(m); o >= 0 {
			return m[:o] + r.write("TOKEN", m[o:])
		}
		return m
	})
	text = r.replace(text, tokenRe, "TOKEN", isToken)
	text = outside(text, dateTimeSpans, func(s string) string { return r.replacePhones(s) })
	for _, c := range clients {
		if c.Pattern == "" {
			continue
		}
		re := c.Regexp()
		text = outside(text, markSpans, func(s string) string { return r.replaceClient(s, re, c.Replace) })
	}
	for _, p := range names(persons) {
		p = unmark.Replace(p)
		text = outside(text, markSpans, func(s string) string {
			return replaceName(s, p, func() string { return r.write("PERSON", p) })
		})
	}
	red := r.finish(text)
	red.missing = missing
	return red
}

// markSpans finds the stand-ins already written, which later client
// patterns and names must not rewrite.
func markSpans(text string) [][]int { return markRe.FindAllStringIndex(text, -1) }

// outside applies f to the text between the spans find returns.
func outside(text string, find func(string) [][]int, f func(string) string) string {
	var b strings.Builder
	last := 0
	for _, loc := range find(text) {
		b.WriteString(f(text[last:loc[0]]))
		b.WriteString(text[loc[0]:loc[1]])
		last = loc[1]
	}
	b.WriteString(f(text[last:]))
	return b.String()
}

// dateTimeSpans finds the dates and the times with fractional seconds in
// text, in order. A date is a date shape not inside a longer run of digits,
// whose month is 1 to 12 and day 1 to 31.
func dateTimeSpans(text string) [][]int {
	var out [][]int
	for _, m := range dateRe.FindAllStringSubmatchIndex(text, -1) {
		if m[0] > 0 && isDigit(text[m[0]-1]) || m[1] < len(text) && isDigit(text[m[1]]) {
			continue
		}
		num := func(g int) int {
			n, _ := strconv.Atoi(text[m[2*g]:m[2*g+1]])
			return n
		}
		var ok bool
		if m[2] >= 0 {
			ok = between(num(2), 1, 12) && between(num(3), 1, 31)
		} else {
			a, b := num(4), num(5)
			ok = between(a, 1, 31) && between(b, 1, 31) && (a <= 12 || b <= 12)
		}
		if ok {
			out = append(out, m[:2])
		}
	}
	for _, m := range timeRe.FindAllStringIndex(text, -1) {
		if (m[0] == 0 || !isDigit(text[m[0]-1])) && (m[1] == len(text) || !isDigit(text[m[1]])) {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

func between(n, lo, hi int) bool { return lo <= n && n <= hi }

func isDigit(b byte) bool { return '0' <= b && b <= '9' }

// replacePhones swaps phone numbers for placeholders. A match that continues
// a word (a letter, digit or _ next to it, or a - or . joining it to one) is
// part of an identifier or a file name, not a phone number; the part of it
// that does not is tried instead (see phoneIn).
func (r *redactor) replacePhones(text string) string {
	var b strings.Builder
	last := 0
	for pos := 0; pos < len(text); {
		loc := phoneRe.FindStringIndex(text[pos:])
		if loc == nil {
			break
		}
		start, end := pos+loc[0], pos+loc[1]
		a, z, ok := phoneIn(text, start, end)
		if !ok {
			pos = end
			continue
		}
		b.WriteString(text[last:a])
		b.WriteString(r.write("PHONE", text[a:z]))
		last, pos = z, z
	}
	b.WriteString(text[last:])
	return b.String()
}

// phoneIn returns the phone number in text[start:end], a phoneRe match: the
// whole match when it stands apart from the words around it, else the
// earliest and longest part of up to maxPhone bytes, cut at separators, that
// does. A number that starts with ( or + needs no space before it.
func phoneIn(text string, start, end int) (a, z int, ok bool) {
	apart := func(a, z int) bool {
		return (strings.ContainsRune("(+", rune(text[a])) || !joinsWord(text[:a], true)) && !joinsWord(text[z:], false)
	}
	if apart(start, end) {
		return start, end, true
	}
	sep := func(c byte) bool { return c == ' ' || c == '.' || c == '-' }
	for a := start; a < end; a++ {
		if a > start && !sep(text[a-1]) || sep(text[a]) {
			continue
		}
		for z := min(end, a+maxPhone); z > a; z-- {
			if z < end && !sep(text[z]) {
				continue
			}
			if phoneWholeRe.MatchString(text[a:z]) && apart(a, z) {
				return a, z, true
			}
		}
	}
	return 0, 0, false
}

// joinsWord reports whether side, the text before (before is true) or after
// a match, continues a word into it.
func joinsWord(side string, before bool) bool {
	next := func(s string) (rune, string) {
		if before {
			r, n := utf8.DecodeLastRuneInString(s)
			return r, s[:len(s)-n]
		}
		r, n := utf8.DecodeRuneInString(s)
		return r, s[n:]
	}
	if side == "" {
		return false
	}
	c, rest := next(side)
	if isWordRune(c) {
		return true
	}
	if (c != '-' && c != '.') || rest == "" {
		return false
	}
	c, _ = next(rest)
	return isWordRune(c)
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

// replaceIPv6 swaps IPv6 addresses for placeholders: a run of ipv6Re, or
// its part after a label and a colon (peer:2001:db8::1), that isIPv6
// accepts. A trailing dot ends a sentence, not the address.
func (r *redactor) replaceIPv6(text string) string {
	var b strings.Builder
	last := 0
	for _, loc := range ipv6Re.FindAllStringIndex(text, -1) {
		run := strings.TrimRight(text[loc[0]:loc[1]], ".")
		end := loc[0] + len(run)
		bracketed := loc[0] > 0 && text[loc[0]-1] == '[' && end < len(text) && text[end] == ']'
		for i := 0; i < len(run); i++ {
			if i > 0 && (run[i-1] != ':' || i > 1 && run[i-2] == ':' || run[i] == ':') {
				continue
			}
			if len(run)-i > 45 { // the longest IPv6 address text
				continue
			}
			if isIPv6(run[i:], bracketed && i == 0) {
				b.WriteString(text[last : loc[0]+i])
				b.WriteString(r.write("IP", run[i:]))
				last = end
				break
			}
		}
	}
	b.WriteString(text[last:])
	return b.String()
}

// isIPv6 reports whether s is an IPv6 address other than loopback or
// unspecified (:: and ::1, kept like 127.0.0.1). Unless it is in brackets
// or written in full (eight groups), it needs a digit and a group of three
// or more characters, so code such as dec::add or a::b is not an address.
func isIPv6(s string, bracketed bool) bool {
	a, err := netip.ParseAddr(s)
	if err != nil || !a.Is6() || a.IsLoopback() || a.IsUnspecified() {
		return false
	}
	groups := strings.Split(s, ":")
	if bracketed || len(groups) == 8 && !strings.Contains(s, "::") {
		return true
	}
	return strings.ContainsAny(s, "0123456789") && slices.ContainsFunc(groups, func(g string) bool { return len(g) >= 3 })
}

// replaceCredentials swaps the value of a credential field for a
// placeholder when isCredential accepts it.
func (r *redactor) replaceCredentials(text string) string {
	var b strings.Builder
	last := 0
	for _, m := range credentialRe.FindAllStringSubmatchIndex(text, -1) {
		v := text[m[2]:m[3]]
		if !isCredential(v) {
			continue
		}
		b.WriteString(text[last:m[2]])
		b.WriteString(r.write("TOKEN", v))
		last = m[3]
	}
	b.WriteString(text[last:])
	return b.String()
}

// isCredential: a credential field's value that is not a path (it starts
// with / . or ~) or a slug, holds a digit or one of + / =, and has the
// entropy of an encoded value. A word or a name (CreatedAtTimestamp) is not.
func isCredential(v string) bool {
	return !strings.ContainsAny(v[:1], "/.~") && !slugRe.MatchString(v) &&
		strings.ContainsAny(v, "0123456789+/=") && entropy(v) >= minCredentialEntropy
}

// replaceVendorKeys swaps vendor keys (vendorKeyRe) for placeholders. A
// match followed by a word character is the start of a longer word, which
// the token rule judges.
func (r *redactor) replaceVendorKeys(text string) string {
	var b strings.Builder
	last := 0
	for _, loc := range vendorKeyRe.FindAllStringIndex(text, -1) {
		if c, _ := utf8.DecodeRuneInString(text[loc[1]:]); loc[1] < len(text) && isWordRune(c) {
			continue
		}
		b.WriteString(text[last:loc[0]])
		b.WriteString(r.write("TOKEN", text[loc[0]:loc[1]]))
		last = loc[1]
	}
	b.WriteString(text[last:])
	return b.String()
}

// isToken: a word that is not a slug and has the entropy of random data. A
// hex word is a key only from 32 characters, so short hashes stay readable,
// and never at exactly 40, a git commit id.
func isToken(m string) bool {
	if hexRe.MatchString(m) {
		return len(m) >= 32 && len(m) != 40 && entropy(m) >= minHexEntropy
	}
	return !slugRe.MatchString(m) && entropy(m) >= minTokenEntropy
}

// base64Start returns where a secret starts inside a base64-style run, or -1.
// The run may begin with a path or URL segments, so each start after a / is
// tried too, longest first. A candidate holding a / is a secret when it does
// not begin with an ordinary word and its entropy is that of random data:
// at least maxSlashedEntropy, or at least slashedEntropy for its length when
// none of its segments is an ordinary word (a path almost always has one).
// One without a / follows isBase64Secret. The work is linear in the run: the
// character counts are kept as the start moves.
func base64Start(m string) int {
	var counts [256]int
	for i := 0; i < len(m); i++ {
		counts[m[i]]++
	}
	// lastPlain is where the last ordinary-word segment starts.
	lastPlain := -1
	for i, seg := 0, ""; i < len(m); i += len(seg) + 1 {
		seg, _, _ = strings.Cut(m[i:], "/")
		if plainSegmentRe.MatchString(seg) {
			lastPlain = i
		}
	}
	dropped := 0
	for o := 0; o < len(m); o++ {
		if o > 0 && m[o-1] != '/' {
			continue
		}
		c := m[o:]
		if len(c) < 40 {
			break
		}
		if c[0] == '/' || c[0] == '=' {
			continue
		}
		first, _, slashed := strings.Cut(c, "/")
		if !slashed {
			if isBase64Secret(c) {
				return o
			}
			continue
		}
		if plainSegmentRe.MatchString(first) {
			continue
		}
		for ; dropped < o; dropped++ {
			counts[m[dropped]]--
		}
		h := countsEntropy(&counts, len(c))
		if h >= maxSlashedEntropy || h >= slashedEntropy(len(c)) && lastPlain < o {
			return o
		}
	}
	return -1
}

// countsEntropy is the Shannon entropy, in bits per byte, of n bytes with
// these counts.
func countsEntropy(counts *[256]int, n int) float64 {
	var h float64
	for _, c := range counts {
		if c > 0 {
			p := float64(c) / float64(n)
			h -= p * math.Log2(p)
		}
	}
	return h
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
	var counts [256]int
	for i := 0; i < len(s); i++ {
		counts[s[i]]++
	}
	return countsEntropy(&counts, len(s))
}

// replaceClient swaps every match of a client pattern (re, from
// config.Pseudonym.Regexp) for its pseudonym, and records every spelling it
// replaced. A match of empty text is left alone, so a pattern that can match
// nothing never writes the pseudonym between characters.
func (r *redactor) replaceClient(text string, re *regexp.Regexp, pseudonym string) string {
	return re.ReplaceAllStringFunc(text, func(m string) string {
		if m == "" {
			return m
		}
		prev, ok := r.spellings[pseudonym]
		switch {
		case !ok:
			r.spellings[pseudonym] = m
		case !slices.Contains(strings.Split(prev, "\n"), m):
			r.spellings[pseudonym] = prev + "\n" + m
		}
		return r.write("", pseudonym)
	})
}

// replaceName swaps every whole-word occurrence of name for the placeholder
// ph returns. Both are compared folded (see fold): without case, across
// zero-width characters, with fullwidth letters as ASCII and any run of
// spaces as one. A zero-width character next to the name ends a word, and
// one can stand for a space in it (Pat, a zero-width space, Example).
func replaceName(text, name string, ph func() string) string {
	want, _ := fold(name)
	if len(want) == 0 {
		return text
	}
	got, at := fold(text)
	edge := func(i int) bool { // whether a word ends between got[i-1] and got[i]
		return i == 0 || i == len(got) || !isWordRune(got[i-1].r) || !isWordRune(got[i].r) || got[i].gap
	}
	var b strings.Builder
	last := 0
	for i := 0; i < len(got); i++ {
		j := matchName(got, i, want)
		if j < 0 || !edge(i) || !edge(j) {
			continue
		}
		b.WriteString(text[last:got[i].start])
		b.WriteString(ph())
		last = at[j-1]
		i = j - 1
	}
	b.WriteString(text[last:])
	return b.String()
}

// matchName returns where name, folded, ends when it starts at got[i], or
// -1. A space in the name matches a space, or a dropped zero-width
// character, in the text.
func matchName(got []folded, i int, want []folded) int {
	k := i
	for n, w := range want {
		switch {
		case k < len(got) && got[k].r == w.r:
			k++
		case w.r == ' ' && n > 0 && k < len(got) && got[k].gap:
		default:
			return -1
		}
	}
	return k
}

// folded is one rune of folded text, where it starts in the original, and
// whether a dropped zero-width character came just before it.
type folded struct {
	r     rune
	start int
	gap   bool
}

// fold returns s folded for name matching, and where each folded rune ends
// in s: format characters (zero-width spaces and joiners) are dropped,
// fullwidth ASCII becomes ASCII, a run of spaces of any kind becomes one
// space, and letters lose their case.
func fold(s string) ([]folded, []int) {
	var out []folded
	var end []int
	gap := false
	for i, c := range s {
		_, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case unicode.Is(unicode.Cf, c):
			gap = true
			continue
		case 0xFF01 <= c && c <= 0xFF5E:
			c -= 0xFEE0
		}
		if unicode.IsSpace(c) {
			if len(out) > 0 && out[len(out)-1].r == ' ' {
				end[len(end)-1] = i + size
				continue
			}
			c = ' '
		}
		out = append(out, folded{r: unicode.ToLower(unicode.ToUpper(c)), start: i, gap: gap})
		end = append(end, i+size)
		gap = false
	}
	return out, end
}

// isWordRune: a letter, digit, combining mark or _.
func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.Is(unicode.M, r) || r == '_'
}

// names returns the distinct person names, compared folded, longest first,
// so a name inside a longer one is not replaced first.
func names(persons []string) []string {
	type name struct {
		s string
		n int
	}
	seen := map[string]bool{}
	var list []name
	for _, p := range persons {
		p = strings.TrimSpace(p)
		f, _ := fold(p)
		k := string(runes(f))
		if len(f) == 0 || seen[k] {
			continue
		}
		seen[k] = true
		list = append(list, name{p, len(f)})
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].n > list[j].n })
	out := make([]string, len(list))
	for i, n := range list {
		out[i] = n.s
	}
	return out
}

func runes(f []folded) []rune {
	out := make([]rune, len(f))
	for i, x := range f {
		out[i] = x.r
	}
	return out
}

// longestFirst returns the distinct non-empty values, longest first, so a
// value inside a longer one is not replaced first.
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
