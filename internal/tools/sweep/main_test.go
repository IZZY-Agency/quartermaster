package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/IZZY-Agency/quartermaster/internal/typesafe"
)

func TestChunksCommentsAndParagraphs(t *testing.T) {
	src := "#!/usr/bin/env bash\n# a comment run that is long enough to keep\n# and its second line\necho hi\n# short\n"
	got := chunks("scripts/x.sh", src)
	if len(got) != 1 || got[0].Line != 2 || got[0].Kind != "code comment" || !strings.Contains(got[0].Text, "second line") {
		t.Fatalf("shell chunks = %+v", got)
	}
	gosrc := "package x\n\n// Doc comment for the function below it.\nfunc F() {}\n"
	if g := chunks("internal/x/x.go", gosrc); len(g) != 1 || g[0].Line != 3 {
		t.Fatalf("go chunks = %+v", g)
	}
	md := "# Title that is long enough\n\nFirst paragraph, long enough to count.\nStill the first.\n\nshort\n"
	g := chunks("README.md", md)
	if len(g) != 2 || g[1].Line != 3 || g[1].Kind != "documentation" {
		t.Fatalf("markdown chunks = %+v", g)
	}
}

func TestChunksSplitLongRuns(t *testing.T) {
	var b strings.Builder
	for i := range 60 {
		fmt.Fprintf(&b, "# line %02d of a long header comment, fifty bytes\n", i)
	}
	g := chunks("scripts/x.sh", b.String())
	if len(g) < 2 {
		t.Fatalf("a %d-byte run gave %d chunk(s)", b.Len(), len(g))
	}
	var joined []string
	for _, c := range g {
		if len(c.Text) > maxChunk {
			t.Errorf("chunk at line %d has %d bytes", c.Line, len(c.Text))
		}
		joined = append(joined, c.Text)
	}
	if got, want := strings.Join(joined, "\n"), strings.TrimSpace(b.String()); got != want {
		t.Errorf("the chunks do not cover the run:\ngot  %q\nwant %q", got, want)
	}
	if g[1].Line <= g[0].Line {
		t.Errorf("second chunk starts at line %d", g[1].Line)
	}
	// One line longer than maxChunk is cut at rune boundaries.
	long := "# " + strings.Repeat("é", 2000)
	g = chunks("Makefile", long)
	if len(g) != 2 || !utf8.ValidString(g[0].Text) || !utf8.ValidString(g[1].Text) || g[0].Text+g[1].Text != long {
		t.Fatalf("long line: %d chunks", len(g))
	}
}

func TestChunksTrailingComments(t *testing.T) {
	gosrc := "package x\n\nfunc f(p *int) {\n\t*p = 1 // set from the value the caller passed in\n\tx := \"// not a comment, a string\"\n\t_ = x\n}\n"
	g := chunks("internal/x/x.go", gosrc)
	if len(g) != 1 || g[0].Line != 4 || g[0].Text != "// set from the value the caller passed in" {
		t.Fatalf("go trailing = %+v", g)
	}
	sh := "run_it --flag  # see the folder where the stand-in lives\necho \"a # not a comment at all, in quotes\"\n"
	g = chunks("scripts/x.sh", sh)
	if len(g) != 1 || g[0].Line != 1 || !strings.HasPrefix(g[0].Text, "# see the folder") {
		t.Fatalf("shell trailing = %+v", g)
	}
	block := "package x\n\n/*\nA block comment whose lines\ndo not start with a star.\n*/\nvar v = 1\n"
	if g := chunks("internal/x/y.go", block); len(g) != 1 || !strings.Contains(g[0].Text, "do not start with a star") {
		t.Fatalf("go block = %+v", g)
	}
}

func TestAllowlist(t *testing.T) {
	src := "x\nALLOWLIST=(\n  a b # comment\n  c\n)\ny\n"
	got, err := allowlist(src)
	if err != nil || strings.Join(got, " ") != "a b c" {
		t.Fatalf("allowlist = %v, %v", got, err)
	}
	if _, err := allowlist("nothing"); err == nil {
		t.Fatal("want an error without ALLOWLIST")
	}
}

func TestSkipped(t *testing.T) {
	for f, want := range map[string]bool{
		"LICENSE": true, "THIRD_PARTY_NOTICES.md": true, "go.mod": true, "internal/x/f.json": true, "a.tar.gz.sha256": true,
		"README.md": false, "scripts/publish.sh": false, "Makefile": false,
	} {
		if skipped(f) != want {
			t.Errorf("skipped(%q) = %v, want %v", f, !want, want)
		}
	}
}

type fakeAsker struct{ score map[string]float64 }

func (f fakeAsker) Ask(_ context.Context, state any, qs map[string]typesafe.Question) (map[string]typesafe.Answer, error) {
	text := state.(map[string]string)["text"]
	if strings.Contains(text, "unjudged") {
		return nil, errors.New("boom")
	}
	out := map[string]typesafe.Answer{}
	for id := range qs {
		v := 0.0
		if strings.Contains(text, id) {
			v = f.score[id]
		}
		out[id] = typesafe.Answer{Type: "noul", Noul: v}
	}
	return out, nil
}

func judged(texts ...string) []result {
	cs := make([]chunk, len(texts))
	for i, s := range texts {
		cs[i] = chunk{File: "f.go", Line: i + 1, Text: s}
	}
	a := fakeAsker{score: map[string]float64{
		"security_sensitive": 0.7, "personal_identifier": 0.6, "private_reference": 0.95, "process_narrative": 0.9,
	}}
	var progress bytes.Buffer
	return judge(context.Background(), a, cs, 3, &progress)
}

func TestReportThresholds(t *testing.T) {
	var w bytes.Buffer
	if !report(&w, judged("plain text", "personal_identifier below its fail line", "private_reference", "process_narrative")) {
		t.Fatalf("listed-only hits must pass:\n%s", w.String())
	}
	if !strings.Contains(w.String(), "private_reference: 1 to read") {
		t.Fatalf("hit not listed:\n%s", w.String())
	}
	w.Reset()
	if report(&w, judged("security_sensitive")) {
		t.Fatalf("security_sensitive at 0.7 must fail:\n%s", w.String())
	}
	w.Reset()
	if report(&w, judged("ok", "unjudged")) {
		t.Fatalf("an unjudged chunk must fail:\n%s", w.String())
	}
}

func TestRunWithoutKeySkips(t *testing.T) {
	var out, errOut bytes.Buffer
	if rc := run("", "", 1, "", &out, &errOut); rc != 0 || !strings.Contains(out.String(), "TYPESAFE_API_KEY is not set") {
		t.Fatalf("rc=%d out=%q", rc, out.String())
	}
}
