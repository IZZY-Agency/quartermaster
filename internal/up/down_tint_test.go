package up

import (
	"context"
	"slices"
	"testing"

	"github.com/IZZY-Agency/quartermaster/internal/launch"
)

// tintCloser is a fakeCloser that records the tabs it is asked to untint, and
// the sessions the saved project records at that moment.
type tintCloser struct {
	*fakeCloser
	ids   [][]string
	saved []Project
	// closedBefore is how many tabs were closed at each Untint.
	closedBefore []int
}

func (c *tintCloser) Untint(_ context.Context, ids []string) {
	c.ids = append(c.ids, ids)
	p, _, _ := LoadID(c.p.ID)
	c.saved = append(c.saved, p)
	c.closedBefore = append(c.closedBefore, len(c.closed))
}

func agtermIDs(p *Project) []string {
	var ids []string
	for _, r := range p.Sessions {
		if r.Launcher == "agterm" {
			ids = append(ids, r.ID)
		}
	}
	slices.Sort(ids)
	return ids
}

// Every recorded agterm tab loses its tint before anything is closed, once
// the project is saved as down: a close of the coordinator's tab may end qm.
func TestDownUntintsTheRecordedAgtermTabsFirst(t *testing.T) {
	for _, keepOpen := range []bool{false, true} {
		p, steps := downFixture(t)
		p.Sessions["tmux-session"] = launch.SessionRef{Launcher: "tmux", ID: "@7", Name: "x"}
		p.Sessions["no-tab"] = launch.SessionRef{Name: "y"}
		want := agtermIDs(p)
		if len(want) == 0 {
			t.Fatal("the fixture records no agterm tab")
		}
		c := &tintCloser{fakeCloser: &fakeCloser{p: p}}
		Down(context.Background(), p, steps, c, keepOpen)
		if len(c.ids) != 1 || !slices.Equal(c.ids[0], want) || c.closedBefore[0] != 0 {
			t.Errorf("keepOpen %v: untinted %v after %v closes, want %v first", keepOpen, c.ids, c.closedBefore, want)
		}
		if c.saved[0].DownAt.IsZero() {
			t.Errorf("keepOpen %v: untinted before the project was saved as down", keepOpen)
		}
	}
}

// A tab left open is untinted once more, when the saved project no longer
// records it, so a watch that still saw it recorded leaves no tint on it.
func TestDownUntintsALeftOpenTabAfterItIsDropped(t *testing.T) {
	p, steps := downFixture(t)
	var id string
	for i := range steps {
		if ref := p.Sessions[steps[i].SessionID]; steps[i].Role == RoleWorker && ref.Launcher == "agterm" {
			id = ref.ID
			steps[i].Action = Leave
			break
		}
	}
	c := &tintCloser{fakeCloser: &fakeCloser{p: p, other: map[string]bool{id: true}}}
	Down(context.Background(), p, steps, c, false)
	if len(c.ids) != 2 || !slices.Equal(c.ids[1], []string{id}) {
		t.Fatalf("untinted %v, want %s again", c.ids, id)
	}
	for _, r := range c.saved[1].Sessions {
		if r.ID == id {
			t.Error("the tab was still recorded when its tint was taken off again")
		}
	}
}

func TestRunsOther(t *testing.T) {
	ids := []string{"s-1", "s-2"}
	const other = "00000000-0000-4000-8000-0000000000ff"
	const mine = "00000000-0000-4000-8000-0000000000a1"
	ids = append(ids, mine)
	for _, c := range []struct {
		name, fg, tabCwd string
		live, want       bool
	}{
		{"idle shell", "", "/w", false, false},
		{"another program", "npm run dev", "/w", false, false},
		{"its own session", "claude --resume " + mine, "/w", true, false},
		{"its own session, resumed since it was gone", "claude --resume " + mine, "/w", false, true},
		{"another session of an installed version", "/opt/share/claude/versions/2.1.3 --resume " + other, "/w", true, true},
		{"its own session of an installed version", "/opt/share/claude/versions/2.1.3 --resume " + mine, "/w", true, false},
		{"another session", "claude --session-id " + other, "/w", true, true},
		{"another session, worker gone", "claude --resume " + other, "/w", false, true},
		{"unnamed, live, its folder", "claude", "/w", true, false},
		{"unnamed, live, elsewhere", "claude", "/x", true, true},
		{"unnamed, gone, its folder", "claude", "/w", false, true},
	} {
		l := launch.SessionRef{Launcher: "agterm", ID: "t", Foreground: c.fg, Cwd: c.tabCwd}
		if got := RunsOther(l, ids, "/w", c.live); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
