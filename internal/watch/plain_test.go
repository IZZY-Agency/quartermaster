package watch

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// A name cannot add a row to the table, a line to an event, or send the
// terminal or the notifier a control character.
func TestNamesStayOnTheirLine(t *testing.T) {
	r := Row{Name: "w\x1b[2J\nfake", State: Stopped, Model: "m\x07", Folder: "f\r\ng", Handoff: "/h/note\n.md"}
	table := Table([]Row{r}, nil, "/h")
	if n := strings.Count(table, "\n"); n != 3 || strings.ContainsAny(table, "\x1b\x07\r") {
		t.Errorf("table has %d lines, want 3 without control characters:\n%q", n, table)
	}
	w := &watcher{}
	for what, s := range map[string]string{"event": w.line(r), "notice": notice(r)} {
		if strings.ContainsAny(s, "\x1b\x07\r\n") || !strings.Contains(s, "w[2J fake stopped") {
			t.Errorf("%s = %q, want one clean line", what, s)
		}
	}
	for _, st := range []State{Silent, Gone} {
		r.State = st
		for _, s := range []string{w.line(r), notice(r)} {
			if strings.ContainsAny(s, "\x1b\x07\r\n") {
				t.Errorf("%s = %q, want one clean line", st, s)
			}
		}
	}
}

// The launcher's own words are in the error: a line break in them starts no
// line that reads as one of qm watch's.
func TestTintFailureStaysOnItsLine(t *testing.T) {
	f := &fake{snaps: []Snapshot{snap(tab("a", Working, t0))}}
	tinted(t, f, timeout(40*time.Second), func(Row) error { return errors.New("no agterm\nqm watch: forged") })
	if got := f.err.String(); !strings.Contains(got, "no agterm\n  qm watch: forged") {
		t.Errorf("err %q, want the second line indented", got)
	}
}
