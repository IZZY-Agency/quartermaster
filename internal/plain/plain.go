// Package plain keeps control characters out of what qm shows. Names reach qm
// from places it does not control (Claude Code's session files, a process's
// arguments, the terminal's tab list, pull request titles, folder names), and
// a control character in one can move the cursor, rewrite lines already
// printed or set the terminal's title.
package plain

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// dropped says whether r is not shown: a control character, or one of the
// marks that reorder the text around them.
func dropped(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r)
}

// Line is s as part of one line of text: white space (a tab and every kind of
// line break among it) becomes a space, other control characters and the
// bidirectional controls are removed, and a byte that is not UTF-8 reads as
// U+FFFD. It is for what is shown: a name that is compared, looked up or
// stored stays as it is.
func Line(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsSpace(r) && r != ' ':
			b.WriteByte(' ')
		case !dropped(r):
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Text is s for an error or another text of more than one line: each line is
// Line's, and each after the first is indented, so that only the first stands
// where a line of qm's own does. A line break at its end is dropped.
func Text(s string) string {
	s = strings.TrimRight(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	parts := strings.Split(s, "\n")
	for i, l := range parts {
		parts[i] = Line(l)
	}
	return strings.Join(parts, "\n  ")
}

// Exact says whether s can be printed on a line of its own through Writer and
// be read back as it is: it is UTF-8, with no character that Writer leaves out
// and no line break.
func Exact(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsFunc(s, func(r rune) bool {
		return r != '\t' && dropped(r) || r == '\u2028' || r == '\u2029'
	})
}

// Writer is w for a terminal: of what is written to it, every control
// character but newline and tab, and the bidirectional controls, are left
// out. It cannot know which newline belongs to a name, so what puts a name
// in a line of its own uses Line too.
func Writer(w io.Writer) io.Writer { return &writer{w: w} }

type writer struct {
	mu sync.Mutex
	w  io.Writer
	// part is the start of a character the last write ended in.
	part []byte
}

func (p *writer) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	in := slices.Concat(p.part, b)
	p.part = nil
	out := make([]byte, 0, len(in))
	for len(in) > 0 {
		if !utf8.FullRune(in) {
			p.part = append([]byte(nil), in...)
			break
		}
		r, n := utf8.DecodeRune(in)
		if r == '\n' || r == '\t' || !dropped(r) {
			// A byte that is not UTF-8 decodes as U+FFFD, and is written so.
			out = utf8.AppendRune(out, r)
		}
		in = in[n:]
	}
	if len(out) > 0 {
		if _, err := p.w.Write(out); err != nil {
			return 0, err
		}
	}
	return len(b), nil
}

// JSON is b, JSON text, with every character that Writer would leave out of
// one of its strings written as an escape: a program reads the same value, and
// the terminal gets none of them. White space between values stays.
func JSON(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for len(b) > 0 {
		r, n := utf8.DecodeRune(b)
		// White space is the only control character below DEL that JSON text
		// holds unescaped.
		if r >= 0x7f && dropped(r) {
			out = fmt.Appendf(out, "\\u%04x", r)
		} else {
			out = append(out, b[:n]...)
		}
		b = b[n:]
	}
	return out
}
