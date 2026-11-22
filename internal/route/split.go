package route

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/IZZY-Agency/quartermaster/internal/policy"
	"github.com/IZZY-Agency/quartermaster/internal/typesafe"
)

// splitMargin is the room, in characters, kept free in every request for the
// envelope and rounding.
const splitMargin = 256

// minSection is the fewest characters of text a request must have room for;
// below it the question set is too large to split the text sensibly.
const minSection = 4096

// SplitState makes the Jev states for text under field: one state when the
// text fits, else one per section, so that every request stays under
// policy.JevRequestTokens and every state plus one question under
// policy.JevQuestionTokens (counted at policy.CharsPerToken characters per
// token of JSON). Sections end at line ends where they can; the sections
// joined give back text. It is an error when the questions leave less than
// minSection characters for the text, or cannot be encoded.
func SplitState(field, text string, qs map[string]typesafe.Question) ([]map[string]any, error) {
	allQ, err := jsonLen(qs)
	if err != nil {
		return nil, err
	}
	maxQ := 0
	for _, q := range qs {
		n, err := jsonLen(q)
		if err != nil {
			return nil, err
		}
		maxQ = max(maxQ, n)
	}
	envelope, err := jsonLen(map[string]any{field: ""})
	if err != nil {
		return nil, err
	}
	budget := min(
		policy.JevQuestionTokens*policy.CharsPerToken-maxQ,
		policy.JevRequestTokens*policy.CharsPerToken-allQ,
	) - splitMargin - envelope
	if budget < minSection {
		return nil, fmt.Errorf("route: the questions leave %d characters per request for %s, under %d", budget, field, minSection)
	}
	var states []map[string]any
	for _, s := range sections(text, budget) {
		states = append(states, map[string]any{field: s})
	}
	return states, nil
}

// sections cuts text into pieces whose escaped JSON length is at most budget,
// whole lines first, a line longer than budget by characters.
func sections(text string, budget int) []string {
	if escapedLen(text) <= budget {
		return []string{text}
	}
	var out []string
	var cur strings.Builder
	size := 0
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
			size = 0
		}
	}
	add := func(piece string, n int) {
		if size+n > budget {
			flush()
		}
		cur.WriteString(piece)
		size += n
	}
	for _, line := range strings.SplitAfter(text, "\n") {
		if n := escapedLen(line); n <= budget {
			add(line, n)
			continue
		}
		for i := 0; i < len(line); {
			_, w := utf8.DecodeRuneInString(line[i:])
			piece := line[i : i+w]
			add(piece, escapedLen(piece))
			i += w
		}
	}
	flush()
	return out
}

// escapedLen is the length of s inside a JSON string, as the TypeSafe client
// encodes it. A string always encodes.
func escapedLen(s string) int {
	n, _ := jsonLen(s)
	return n - 2
}

// jsonLen is the length of v encoded as the TypeSafe client encodes it (no
// HTML escaping), without the trailing newline.
func jsonLen(v any) (int, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return 0, fmt.Errorf("route: measuring a Jev request: %w", err)
	}
	return buf.Len() - 1, nil
}
