package outcomes

import (
	"bytes"
	"strings"
	"testing"

	"github.com/IZZY-Agency/quartermaster/internal/route"
)

// A name, a type or a record's detail cannot add a line to the report.
func TestReportKeepsNamesOnTheirLine(t *testing.T) {
	write := func(s string) string {
		var rep Report
		var w Worker
		w.Name, w.Type = s, s
		rep.Workers = []Worker{w}
		var c Coordinator
		c.Session = s
		rep.Coordinators = []Coordinator{c}
		rep.Softened = []route.Record{{Rule: s, Detail: s}}
		var b bytes.Buffer
		rep.Write(&b)
		return b.String()
	}
	out := write("a\x1b[2J\nfake")
	if got, want := strings.Count(out, "\n"), strings.Count(write("a"), "\n"); got != want || strings.ContainsRune(out, '\x1b') {
		t.Errorf("report has %d lines, want %d without control characters:\n%q", got, want, out)
	}
}
