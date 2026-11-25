package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/config"
	"github.com/IZZY-Agency/quartermaster/internal/declog"
	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/up"
	"github.com/IZZY-Agency/quartermaster/internal/watch"
)

// tintTimeout bounds one pass of tints, every tab of it together. A variable
// for the tests.
var tintTimeout = 3 * time.Second

// tabTint is one agterm tab and what it is to show.
type tabTint struct {
	id   string
	want launch.Tint
	// clear: qm lets go of the tab; want is not read.
	clear bool
}

// tintTabs tints tabs, in order, beginning none after tintTimeout, and returns one note for
// all that failed ("" for none). They are agterm's, whatever launcher is
// picked now: a tab is recorded with its own. A tab that is gone, or whose
// tint is the owner's own, is no failure.
func tintTabs(tabs []tabTint) string {
	if len(tabs) == 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), tintTimeout)
	defer cancel()
	var first error
	failed := 0
	for _, t := range tabs {
		var err error
		if t.clear {
			_, err = upAgtermCtl.ClearTint(ctx, t.id)
		} else {
			_, err = upAgtermCtl.ApplyTint(ctx, t.id, t.want)
		}
		if err == nil || errors.Is(err, launch.ErrGone) {
			continue
		}
		failed++
		if first == nil {
			first = err
		}
	}
	if failed == 0 {
		return ""
	}
	return fmt.Sprintf("the tint of %d of %d agterm tabs not changed: %s", failed, len(tabs), strings.TrimSpace(first.Error()))
}

// roleTint is the tint of a tab of role.
func roleTint(role up.Role, t config.Tints) launch.Tint {
	if role == up.RoleCoordinator {
		return launch.CoordinatorTint(t.Coordinator)
	}
	return launch.WorkerTint(t.Worker)
}

// roleTab is the tint of ref's tab for role; ok is false for a session with
// no agterm tab.
func roleTab(ref launch.SessionRef, role up.Role, t config.Tints) (tab tabTint, ok bool) {
	if ref.Launcher != "agterm" || ref.ID == "" {
		return tab, false
	}
	return tabTint{id: ref.ID, want: roleTint(role, t)}, true
}

// tintStarted gives the tabs qm up started sessions in their role's tint, and
// takes qm's tint off the tabs recorded before that are not recorded after.
// It returns the notes to print; the tabs let go of are no word without agterm
// as the launcher (l), which may not run. It runs once everything is started,
// so a tint delays no start but that of a coordinator in this tab, by
// tintTimeout and the change in progress at most.
func tintStarted(cfg config.Config, l launch.Launcher, outs []up.Outcome, before, after map[string]launch.SessionRef) []string {
	tints, cerr := cfg.Tints()
	held := map[string]bool{}
	for _, r := range after {
		if r.Launcher == "agterm" {
			held[r.ID] = true
		}
	}
	var tabs, released []tabTint
	for _, o := range outs {
		if o.Err != nil || o.Step.Action != up.Resume && o.Step.Action != up.Fresh {
			continue
		}
		if t, ok := roleTab(o.Ref, o.Step.Role, tints); ok {
			tabs = append(tabs, t)
		}
	}
	for _, r := range before {
		if r.Launcher == "agterm" && r.ID != "" && !held[r.ID] {
			released = append(released, tabTint{id: r.ID, clear: true})
		}
	}
	if len(tabs) == 0 {
		cerr = nil // no colour was used
	}
	if _, ok := l.(launch.Agterm); !ok {
		note := tintTabs(tabs)
		tintTabs(released)
		return tintNotes(note, cerr)
	}
	return tintNotes(tintTabs(append(tabs, released...)), cerr)
}

// tintAdopted gives the adopted tabs their role's tint.
func tintAdopted(cfg config.Config, ap *up.AdoptPlan) []string {
	tints, cerr := cfg.Tints()
	var tabs []tabTint
	add := func(id string, role up.Role) {
		if t, ok := roleTab(ap.Project.Sessions[id], role, tints); ok {
			tabs = append(tabs, t)
		}
	}
	if !ap.Later {
		add(ap.Coordinator.SessionID, up.RoleCoordinator)
	}
	for _, w := range ap.Workers {
		add(w.SessionID, up.RoleWorker)
	}
	if len(tabs) == 0 {
		return nil
	}
	return tintNotes(tintTabs(tabs), cerr)
}

// tintNotes is the one note of a pass: what failed, and what is wrong with the
// config's colours.
func tintNotes(note string, cerr error) []string {
	var parts []string
	if cerr != nil {
		parts = append(parts, "config: "+strings.ReplaceAll(cerr.Error(), "\n", "; "))
	}
	if note != "" {
		parts = append(parts, note)
	}
	if len(parts) == 0 {
		return nil
	}
	return []string{strings.Join(parts, "; ")}
}

// clearTabs takes qm's tint off the tabs of refs; it returns the notes to
// print.
func clearTabs(refs ...launch.SessionRef) []string {
	var tabs []tabTint
	for _, r := range refs {
		if r.Launcher == "agterm" && r.ID != "" {
			tabs = append(tabs, tabTint{id: r.ID, clear: true})
		}
	}
	return tintNotes(tintTabs(tabs), nil)
}

// downTerms is qm down's terminals, and takes qm's tint off agterm tabs: the
// first failure is its note.
type downTerms struct {
	refTerms
	note string
}

func (d *downTerms) Untint(_ context.Context, ids []string) {
	var tabs []tabTint
	for _, id := range ids {
		tabs = append(tabs, tabTint{id: id, clear: true})
	}
	if n := tintTabs(tabs); d.note == "" {
		d.note = n
	}
}

// watchTints is what qm watch gives a worker's tab by its state: the role's
// tint, or the one for a worker that needs attention. start is when the watch
// began.
func watchTints(id string, tints config.Tints, start time.Time) func(context.Context, watch.Row, time.Time) error {
	return func(ctx context.Context, r watch.Row, observed time.Time) error {
		want := launch.WorkerTint(tints.Worker)
		switch r.State {
		case watch.Stopped:
			want = launch.AttentionTint(tints.Stopped)
		case watch.Silent:
			want = launch.AttentionTint(tints.Silent)
		case watch.Gone:
			want = launch.AttentionTint(tints.Gone)
		}
		refused := false
		_, err := upAgtermCtl.ApplyTintIf(ctx, r.Tab.ID, want, func(l launch.SessionRef) bool {
			refused = up.RunsOther(l, []string{r.SessionID, r.Current}, r.Cwd, r.State != watch.Gone) || !stillHeld(id, r, observed, start)
			return !refused
		})
		switch {
		case errors.Is(err, launch.ErrGone):
			return nil // the tab was closed: there is nothing to tint
		case err == nil && refused:
			return watch.ErrNotNow
		}
		return err
	}
}

// stillHeld says, under the tint memory's lock, whether project id still
// records r's tab for r, unfinished, and nothing that set the tab's tint ran
// since: the project did not go down since the watch began (start), so a watch
// from before a qm down, --keep-open too, tints none of its tabs until a qm up
// restores the project (up.Project.ClearDown); and no
// Record of r's session (a start, a resume, an adoption, a qm down; not one qm
// up left as it is) is newer than the reading (observed). A qm that took the tint off, or gave the role's,
// meanwhile has the last word.
func stillHeld(id string, r watch.Row, observed, start time.Time) bool {
	p, ok, err := up.LoadID(id)
	if err != nil || !ok || p.DownAt.After(start) {
		return false
	}
	recs, err := declog.Read(time.Time{})
	if err != nil {
		return false
	}
	for _, rec := range recs {
		if rec.ClaudeSession == r.SessionID && rec.Action != string(up.Leave) && rec.Time.After(observed) {
			return false
		}
	}
	cands, err := watch.Workers(p, recs, "")
	if err != nil {
		return false
	}
	tab, ok := watch.Tabs(p, recs, cands)[r.SessionID]
	return ok && tab.ID == r.Tab.ID
}
