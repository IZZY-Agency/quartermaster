package up

import (
	"strings"
	"testing"
	"time"
)

// bad is a name with an escape sequence and a line break.
const bad = "a\x1b[2J\x07\nfake"

func checkPlain(t *testing.T, what, out string, lines int) {
	t.Helper()
	if n := strings.Count(out, "\n"); n != lines || strings.ContainsAny(out, "\x1b\x07\r") {
		t.Errorf("%s has %d lines, want %d without control characters:\n%q", what, n, lines, out)
	}
}

// What the plan tables show of a session cannot add a line or send the
// terminal a control character.
func TestRenderKeepsNamesOnTheirLine(t *testing.T) {
	step := func(s string) []Step {
		var st Step
		st.Name, st.SessionID, st.Branch, st.Why, st.Action = s, s, s, s, Leave
		return []Step{st}
	}
	now := time.Now()
	checkPlain(t, "plan", Render(step(bad), now), strings.Count(Render(step("a"), now), "\n"))

	cand := func(s string) []AdoptCandidate {
		var c AdoptCandidate
		c.Name, c.SessionID, c.Cwd, c.Branch, c.Tab = s, s, s, s, s
		return []AdoptCandidate{c}
	}
	checkPlain(t, "candidates", RenderCandidates(cand(bad), now), strings.Count(RenderCandidates(cand("a"), now), "\n"))

	adopt := func(s string) AdoptPlan {
		return AdoptPlan{
			Moves: []AdoptMove{{Name: s, SessionID: s, From: s, To: s}},
			Left:  []AdoptCandidate{{Name: s, SessionID: s, Cwd: s, Subfolder: true}, {Name: s, SessionID: s}},
			Notes: []string{s},
		}
	}
	checkPlain(t, "adopt plan", RenderAdopt(adopt(bad)), strings.Count(RenderAdopt(adopt("a")), "\n"))
	if got := oneLine("x\x1b]0;t\x07  y\u202e"); got != "x]0;t y" {
		t.Errorf("oneLine = %q", got)
	}
}
