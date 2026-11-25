package plain

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"testing"
)

func TestLine(t *testing.T) {
	for in, want := range map[string]string{
		"w1":                                "w1",
		"héllo wörld — ok…":                 "héllo wörld — ok…",
		"a\x1b]0;title\x07b":                "a]0;titleb",
		"a\x1b[2Jb":                         "a[2Jb",
		"a\nfake row\r\tb":                  "a fake row  b",
		"a\x00\x08\x7fb":                    "ab",
		"a\u009b31m\u0085b":                 "a31m b",
		"abc\u202edcba\u2066\u200f":         "abcdcba",
		"a b c":                             "a b c",
		"raw\x9bbyte":                       "raw�byte",
		"family \U0001F468\u200d\U0001F469": "family \U0001F468\u200d\U0001F469",
	} {
		if got := Line(in); got != want {
			t.Errorf("Line(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWriterKeepsLinesAndTabs(t *testing.T) {
	var b bytes.Buffer
	in := "NAME\tSTATE\nw\x1b[31m\r1\u009b\u202e é\x07\n"
	n, err := io.WriteString(Writer(&b), in)
	if err != nil || n != len(in) {
		t.Fatalf("Write = %d, %v, want %d", n, err, len(in))
	}
	if want := "NAME\tSTATE\nw[31m1 é\n"; b.String() != want {
		t.Errorf("wrote %q, want %q", b.String(), want)
	}
}

// A character split across two writes arrives whole, and a control one split
// so is still dropped.
func TestWriterSplitRune(t *testing.T) {
	var b bytes.Buffer
	w := Writer(&b)
	for _, c := range []byte("é\u009b€x") {
		if n, err := w.Write([]byte{c}); n != 1 || err != nil {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	if b.String() != "é€x" {
		t.Errorf("wrote %q, want é€x", b.String())
	}
}

type full struct{}

func (full) Write([]byte) (int, error) { return 0, io.ErrShortWrite }

func TestWriterError(t *testing.T) {
	if _, err := Writer(full{}).Write([]byte("a")); !errors.Is(err, io.ErrShortWrite) {
		t.Errorf("err = %v, want the writer's", err)
	}
}

// Only the first line of a text stands at the start of a line.
func TestText(t *testing.T) {
	for in, want := range map[string]string{
		"":                       "",
		"one":                    "one",
		"one\ntwo\n":             "one\n  two",
		"one\r\ntwo\n\nthree":    "one\n  two\n  \n  three",
		"a\x1b[2Jb\u202e\nqm: c": "a[2Jb\n  qm: c",
		"a\vb c\u0085d\fe\rqm":   "a b c d e qm",
	} {
		if got := Text(in); got != want {
			t.Errorf("Text(%q) = %q, want %q", in, got, want)
		}
	}
}

// JSON stays the same value with no raw character that Writer drops.
func TestJSON(t *testing.T) {
	name := "a\u200fb\u202ec\u0085d\x7fe\u009b é"
	raw, err := json.MarshalIndent(map[string]string{"name": name}, "", "\t")
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if _, err := Writer(&b).Write(JSON(raw)); err != nil {
		t.Fatal(err)
	}
	for _, r := range b.String() {
		if r != '\n' && r != '\t' && dropped(r) {
			t.Errorf("%q is raw in %q", r, b.String())
		}
	}
	var got map[string]string
	if err := json.Unmarshal(b.Bytes(), &got); err != nil || got["name"] != name {
		t.Errorf("read back %q, %v, want %q", got["name"], err, name)
	}
}
