package up

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/launch"
)

// closedIDs are the sessions the saved state says qm down closed.
func closedIDs(t *testing.T, p *Project) string {
	t.Helper()
	got, err := readProject(mustPath(t, p.ID))
	if err != nil {
		t.Fatal(err)
	}
	if got.Closed == nil {
		t.Error("saved Closed is nil after qm down")
	}
	return strings.Join(slices.Sorted(maps.Keys(got.Closed)), ",")
}

func TestDownRecordsClosedSessions(t *testing.T) {
	for _, c := range []struct {
		name     string
		closer   fakeCloser
		keepOpen bool
		drop     string // a session with no terminal session recorded
		want     string
	}{
		{name: "all closed", want: "c1,w1,w2"},
		{name: "keep-open closes nothing", keepOpen: true, want: ""},
		{name: "a failed close", closer: fakeCloser{fail: map[string]string{"t-w1": "boom"}}, want: "c1,w2"},
		{name: "a failed close of the coordinator", closer: fakeCloser{fail: map[string]string{"t-c": "boom"}}, want: "w1,w2"},
		{name: "a close of a tab that is gone", closer: fakeCloser{gone: map[string]bool{"t-w1": true}}, want: "c1,w1,w2"},
		{name: "a running session left open", closer: fakeCloser{other: map[string]bool{"t-w1": true}}, want: "c1,w2"},
		{name: "a running session no longer listed", closer: fakeCloser{unlisted: map[string]bool{"t-w1": true}}, want: "c1,w2"},
		{name: "a running session with no tab", drop: "w1", want: "c1,w2"},
		// w2 does not run: there is nothing to close.
		{name: "no listing", closer: fakeCloser{listErr: errors.New("agterm: boom")}, want: "w2"},
		{name: "no second listing", closer: fakeCloser{laterErr: errors.New("agterm: boom")}, want: "w2"},
		{name: "an ended session's tab left open", closer: fakeCloser{busy: map[string]bool{"t-w2": true}}, want: "c1,w1,w2"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, steps := downFixture(t)
			delete(p.Sessions, c.drop)
			c.closer.p = p
			Down(context.Background(), p, steps, &c.closer, c.keepOpen)
			if got := closedIDs(t, p); got != c.want {
				t.Errorf("closed %q, want %q", got, c.want)
			}
		})
	}
}

// spyCloser reads the saved state when each tab is closed: qm down may end
// there (a coordinator, or a worker, that runs it itself).
type spyCloser struct {
	fakeCloser
	t     *testing.T
	saved map[string]string // terminal session: what Closed held
}

func (c *spyCloser) Close(ctx context.Context, ref launch.SessionRef) error {
	c.saved[ref.ID] = closedIDs(c.t, c.p)
	return c.fakeCloser.Close(ctx, ref)
}

func TestDownClosedIsSavedRightBeforeEachClose(t *testing.T) {
	p, steps := downFixture(t)
	steps = append(steps, Step{Candidate: Candidate{Role: RoleWorker, SessionID: "w3", Name: "w3"}, Action: Leave})
	p.Sessions["w3"] = launch.SessionRef{Launcher: "agterm", ID: "t-w3"}
	c := &spyCloser{fakeCloser: fakeCloser{p: p, fail: map[string]string{"t-w1": "boom"}}, t: t, saved: map[string]string{}}
	Down(context.Background(), p, steps, c, false)
	// w2 does not run; a running session is in it from its own close on.
	for id, want := range map[string]string{"t-w1": "w1,w2", "t-w3": "w2,w3", "t-c": "c1,w2,w3"} {
		if c.saved[id] != want {
			t.Errorf("saved when %s closes: %q, want %q", id, c.saved[id], want)
		}
	}
}

func TestDownKeepOpenKeepsEarlierClosed(t *testing.T) {
	p, steps := downFixture(t)
	Down(context.Background(), p, steps, &fakeCloser{p: p}, false)
	for i := range steps { // nothing but c1, resumed by hand, runs any more
		if steps[i].SessionID != "c1" {
			steps[i].Action = Resume
		}
	}
	Down(context.Background(), p, steps, &fakeCloser{p: p}, true)
	if got := closedIDs(t, p); got != "w1,w2" {
		t.Errorf("closed %q after qm down, then --keep-open", got)
	}
}

// An older qm's qm down saved only when it ran.
func TestDownKeepOpenKeepsAnOlderQMsDown(t *testing.T) {
	p, steps := downFixture(t)
	p.DownAt = time.Now().Add(-time.Hour)
	for i := range steps {
		steps[i].Action, steps[i].Started = Resume, p.DownAt.Add(-time.Hour)
	}
	steps[0].Started = p.DownAt.Add(time.Minute) // w1, started since
	steps[1].Action = Leave                      // c1, running again
	Down(context.Background(), p, steps, &fakeCloser{p: p}, true)
	if got := closedIDs(t, p); got != "w2" {
		t.Errorf("closed %q, want w2", got)
	}
}

// Not all running sessions were known: one not seen running is closed only
// when its idle tab is.
func TestDownMaybeLive(t *testing.T) {
	for _, c := range []struct {
		name   string
		closer fakeCloser
		want   string
	}{
		{"its tab is busy", fakeCloser{busy: map[string]bool{"t-w2": true}}, "c1,w1"},
		{"its tab is idle", fakeCloser{}, "c1,w1,w2"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, steps := downFixture(t)
			steps[2].MaybeLive = true
			c.closer.p = p
			Down(context.Background(), p, steps, &c.closer, false)
			if got := closedIDs(t, p); got != c.want {
				t.Errorf("closed %q, want %q", got, c.want)
			}
		})
	}
}
