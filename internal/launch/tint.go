package launch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/paths"
)

// Tint is the colour of an agterm tab's sidebar row and where it shows: On is
// "icon", "name" or "icon,name". The zero Tint is no tint. agterm folds a
// colour to lower case and reports a placement in icon, name order; a Tint
// holds both in that form, so two Tints compare with ==.
type Tint struct {
	Color string `json:"color"`
	On    string `json:"on"`
}

// IsZero says the tab has no tint.
func (t Tint) IsZero() bool { return t.Color == "" }

// tintOf is the Tint for a colour shown on the given places; "" is no tint.
func tintOf(color, on string) Tint {
	c := strings.ToLower(strings.TrimSpace(color))
	if c == "" {
		return Tint{}
	}
	return Tint{Color: c, On: on}
}

// CoordinatorTint is the tint of a coordinator's tab: color on the icon and
// the name. An empty color is no tint.
func CoordinatorTint(color string) Tint { return tintOf(color, "icon,name") }

// WorkerTint is the tint of a worker's tab: color on the icon.
func WorkerTint(color string) Tint { return tintOf(color, "icon") }

// AttentionTint is the tint of a worker that needs attention (stopped,
// silent, gone): color on the icon and the name.
func AttentionTint(color string) Tint { return tintOf(color, "icon,name") }

// readTint is the Tint agterm's tree reports for a session: tint and tintOn,
// both left out of an untinted one. A tint without a placement shows on the
// icon.
func readTint(color string, on []string) Tint {
	if color == "" {
		return Tint{}
	}
	var places []string
	for _, p := range []string{"icon", "name"} {
		if slices.Contains(on, p) {
			places = append(places, p)
		}
	}
	if len(places) == 0 {
		places = []string{"icon"}
	}
	return Tint{Color: strings.ToLower(color), On: strings.Join(places, ",")}
}

// tintOwned is the one rule for touching a tab's tint: qm sets or clears it
// only when the tab has none, or when it shows exactly what qm last set on it
// (mem, remembered when has is true). Any other tint is the owner's, put there
// by hand, and is left alone. A tab the owner cleared by hand has none, so it
// is qm's again.
func tintOwned(cur, mem Tint, has bool) bool {
	return cur.IsZero() || (has && cur == mem)
}

// TintResult says what ApplyTint did.
type TintResult int

const (
	// TintUnchanged: the tab already showed the wanted tint.
	TintUnchanged TintResult = iota
	// TintApplied: the tint was set, or cleared.
	TintApplied
	// TintLeft: the tab holds the owner's own tint, or allow refused it;
	// nothing was changed.
	TintLeft
)

// ApplyTint makes the tab id show want (the zero Tint clears it), when the
// tab is qm's to tint (tintOwned): it reads the tab's tint from agterm, and
// what qm last set on it from the tint memory, and a tab with any other tint
// is left alone (TintLeft). What was set is remembered per session id, in
// tints.json in qm's state folder; the whole check and change runs under the
// memory's lock, so two qm processes never tint one tab at once. The memory
// is emptied of a tab before its tint changes and filled again after, so a
// failure in between leaves the tab unremembered, which reads as the owner's
// and is left alone, never a stale record that could claim a later hand-picked
// colour; the old record comes back only when the change certainly did not
// happen (unapplied). agtermctl has no conditional set, so a tint the owner
// picks in the instant between the read and the set is overwritten.
// A tab that is not in agterm's tree is an error matching ErrGone when every
// window was read, and not matching it with a window unread. Either way its
// entry stays, as agterm can bring a closed tab back: it is dropped once the
// tab has been missing for tintGone (Prune). No change begins once ctx ended;
// one that began is bounded by tintSetTime instead. ApplyTint, ApplyTintIf and
// ClearTint are the only ways qm changes the tint of a tab that exists.
func (a Agterm) ApplyTint(ctx context.Context, id string, want Tint) (TintResult, error) {
	return a.applyTint(ctx, id, want, nil, false)
}

// ApplyTintIf is ApplyTint for a tab that also passes allow, which gets the
// tab as listed under the memory's lock, right before the change; a tab it
// refuses is left alone (TintLeft).
func (a Agterm) ApplyTintIf(ctx context.Context, id string, want Tint, allow func(SessionRef) bool) (TintResult, error) {
	return a.applyTint(ctx, id, want, allow, false)
}

// ClearTint is ApplyTint with no tint, for a tab qm lets go of. A tab qm
// remembers no tint on holds none of qm's: it is TintUnchanged without a word
// to agterm.
func (a Agterm) ClearTint(ctx context.Context, id string) (TintResult, error) {
	return a.applyTint(ctx, id, Tint{}, nil, true)
}

// applyTint is ApplyTint, under allow (nil: every tab), and with known only
// for a tab the memory holds.
func (a Agterm) applyTint(ctx context.Context, id string, want Tint, allow func(SessionRef) bool, known bool) (TintResult, error) {
	var res TintResult
	err := withTints(ctx, func(b *tintBook) error {
		if _, has := b.Get(id); known && !has {
			return nil
		}
		refs, complete, err := a.allSessions(ctx)
		if err != nil {
			return err
		}
		var tab SessionRef
		found := false
		live := map[string]bool{}
		for _, s := range refs {
			live[s.ID] = true
			if s.ID == id {
				tab, found = s, true
			}
		}
		b.Prune(live, complete)
		if !found {
			if !complete {
				return fmt.Errorf("agterm: tab %s not found, but not every window could be read", id)
			}
			return fmt.Errorf("agterm: %w: no session %s", ErrGone, id)
		}
		mem, has := b.Get(id)
		cur := tab.Tint
		switch {
		case !tintOwned(cur, mem, has), allow != nil && !allow(tab):
			res = TintLeft
			return nil
		case cur == want:
			res = TintUnchanged
			if want.IsZero() {
				b.Delete(id)
			} else if !has || mem != want {
				b.Set(id, want)
			}
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err // too late to begin: the tab is untouched
		}
		// From here until the memory is written again nothing is cut off, by
		// ctx or by a signal: a change that ended halfway leaves a tab qm no
		// longer takes for its own.
		defer holdSignals()()
		b.Delete(id)
		if err := b.Flush(); err != nil {
			return err // the tab is untouched
		}
		sctx, cancel := context.WithTimeout(ownGroup(context.WithoutCancel(ctx)), tintSetTime)
		defer cancel()
		if err := a.setTint(sctx, id, want); err != nil {
			if has && unapplied(err) {
				b.Set(id, mem) // the tab is as it was
			}
			return errors.Join(err, b.Flush())
		}
		res = TintApplied
		if !want.IsZero() {
			b.Set(id, want)
		}
		return b.Flush()
	})
	return res, err
}

// holdSignals holds back the signals that end qm (a Ctrl-C, a closed terminal,
// a kill) until release, which raises again the first that came. One that is
// ignored stays so. A handler qm has for a signal sees it twice.
func holdSignals() (release func()) {
	var sigs []os.Signal
	for _, s := range []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		if !signal.Ignored(s) {
			sigs = append(sigs, s)
		}
	}
	if len(sigs) == 0 { // Notify of none is Notify of all
		return func() {}
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, sigs...)
	return func() {
		signal.Stop(ch)
		select {
		case s := <-ch:
			_ = syscall.Kill(os.Getpid(), s.(syscall.Signal))
		default:
		}
	}
}

// tintSetTime bounds the one call that changes a tab's tint, which ctx's end
// does not cut off.
const tintSetTime = 2 * time.Second

// setTint runs agtermctl session tint on id: the colour with its placement
// (always passed: agterm keeps the old placement when it is left out), or
// clear for the zero Tint.
func (a Agterm) setTint(ctx context.Context, id string, t Tint) error {
	args := []string{"session", "tint", "clear", "--target", id, "--json"}
	if !t.IsZero() {
		args = []string{"session", "tint", t.Color, "--on", t.On, "--target", id, "--json"}
	}
	if _, err := a.call(ctx, "session tint", args...); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such session") {
			return fmt.Errorf("agterm: %w: %v", ErrGone, err)
		}
		return fmt.Errorf("agterm: %w", err)
	}
	return nil
}

// unapplied says a failed session tint certainly changed nothing: agtermctl
// did not run, or says so itself (the refusals newErr knows). After a lost
// reply, a kill or an unreadable answer the tab may show the new tint.
func unapplied(err error) bool {
	return !errors.Is(newErr(err), ErrMaybeStarted)
}

// allSessions lists the sessions of every open window (agtermctl tree shows
// only one window, by default the frontmost). complete is false when the
// window list or one window's tree could not be read; the sessions read are
// returned all the same, from the frontmost window alone when the list fails.
func (a Agterm) allSessions(ctx context.Context) (refs []SessionRef, complete bool, err error) {
	windows, werr := a.openWindows(ctx)
	if werr != nil || len(windows) == 0 {
		wss, err := a.workspaces(ctx, "")
		if err != nil {
			return nil, false, err
		}
		return flatten(wss), false, nil
	}
	complete = true
	var firstErr error
	for _, w := range windows {
		wss, err := a.workspaces(ctx, w)
		if err != nil {
			complete, firstErr = false, err
			continue
		}
		refs = append(refs, flatten(wss)...)
	}
	if len(refs) == 0 && firstErr != nil {
		return nil, false, firstErr
	}
	return refs, complete, nil
}

func flatten(wss []Workspace) []SessionRef {
	var out []SessionRef
	for _, w := range wss {
		out = append(out, w.Sessions...)
	}
	return out
}

// openWindows lists the ids of agterm's open windows.
func (a Agterm) openWindows(ctx context.Context) ([]string, error) {
	raw, err := a.call(ctx, "window list", "window", "list", "--json")
	if err != nil {
		return nil, err
	}
	var res struct {
		Windows []struct {
			ID   string `json:"id"`
			Open bool   `json:"open"`
		} `json:"windows"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	var ids []string
	for _, w := range res.Windows {
		if w.Open {
			ids = append(ids, w.ID)
		}
	}
	return ids, nil
}

// unknownTintOption says err is agtermctl's own argument parser refusing
// --tint or --tint-on, which an agterm older than the flag does: agtermctl
// ran and exited with its usage status (usageExit) and the parser's message.
// The same words in an answer that could not be read, or after any other exit
// status, are not that. The parser refuses before it sends anything to
// agterm, so no tab was created.
func unknownTintOption(err error) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == usageExit && strings.Contains(err.Error(), "Error: Unknown option '--tint")
}

// tintsFile is the tint memory's file, in qm's state folder.
func tintsFile() (string, error) {
	dir, err := paths.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "tints.json"), nil
}

// tintsWait is how long withTints waits for another qm process's lock. A
// variable for the tests.
var tintsWait = 10 * time.Second

const (
	// tintGone is how long the entry of a tab is kept after it is first
	// missed in every open window: agterm keeps a closed window's tabs to
	// reopen them, and a tab that comes back must still be qm's.
	tintGone = 30 * 24 * time.Hour
	// tintMaxAge is how long an entry is kept after qm last set its tint or
	// saw its tab (Prune), whether or not the tab still exists. A tinted
	// spawn drops older ones before it adds its own, so the file stays
	// bounded without ApplyTint; so does ApplyTint's prune. A tab whose
	// entry expired reads as the owner's.
	tintMaxAge = 365 * 24 * time.Hour
	// tintSeenStep is how old At is before seeing the tab renews it, so that
	// not every look at the tabs rewrites the file.
	tintSeenStep = 24 * time.Hour
)

// tintEntry is what qm last set on a tab. At is when it was set or the tab
// was last seen (to tintSeenStep); Missing is when the tab was first missed,
// 0 while it is not missing.
type tintEntry struct {
	Tint
	At      int64 `json:"at"`
	Missing int64 `json:"missing,omitempty"`
}

// tintBook is the tint memory as withTints hands it out: agterm session id
// to the Tint qm last set on that tab.
type tintBook struct {
	path     string
	sessions map[string]tintEntry
	dirty    bool
}

// Get is what qm last set on tab id, if it remembers.
func (b *tintBook) Get(id string) (Tint, bool) {
	e, ok := b.sessions[id]
	return e.Tint, ok
}

// Set remembers that qm set t on tab id just now.
func (b *tintBook) Set(id string, t Tint) {
	b.sessions[id] = tintEntry{Tint: t, At: time.Now().Unix()}
	b.dirty = true
}

// Delete forgets tab id.
func (b *tintBook) Delete(id string) {
	if _, ok := b.sessions[id]; ok {
		delete(b.sessions, id)
		b.dirty = true
	}
}

// Expire drops every entry older than tintMaxAge. It needs no look at which
// tabs exist.
func (b *tintBook) Expire() {
	for id, e := range b.sessions {
		if time.Since(time.Unix(e.At, 0)) > tintMaxAge {
			b.Delete(id)
		}
	}
}

// Prune drops every expired entry (Expire), then notes which tabs were seen:
// an entry of a tab in live is not missing, and its At is renewed. With
// complete, live holds every tab of every open window: an entry whose tab is
// not in it is marked missing, and dropped once it has been so for tintGone.
func (b *tintBook) Prune(live map[string]bool, complete bool) {
	b.Expire()
	now := time.Now()
	for id, e := range b.sessions {
		switch {
		case live[id]:
			if e.Missing == 0 && now.Sub(time.Unix(e.At, 0)) <= tintSeenStep {
				continue
			}
			e.At, e.Missing = now.Unix(), 0
		case !complete:
			continue
		case e.Missing == 0:
			e.Missing = now.Unix()
		default:
			if now.Sub(time.Unix(e.Missing, 0)) > tintGone {
				b.Delete(id)
			}
			continue
		}
		b.sessions[id], b.dirty = e, true
	}
}

// Flush writes the memory now when it changed.
func (b *tintBook) Flush() error {
	if !b.dirty {
		return nil
	}
	if err := writeTints(b.path, b.sessions); err != nil {
		return err
	}
	b.dirty = false
	return nil
}

// withTints runs fn on the tint memory under an exclusive lock, and writes it
// back atomically (a 0600 temporary file renamed over tints.json) when it
// changed. The memory is a file of its own, not part of a project's state
// file, which qm watch does not rewrite. A file that does not parse reads as
// empty: no tab then looks qm's, so every tint is left alone. An error before
// fn runs (no state folder, lock not got in time or before ctx ended) means fn
// did not run.
func withTints(ctx context.Context, fn func(b *tintBook) error) error {
	path, err := tintsFile()
	if err != nil {
		return fmt.Errorf("tint memory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("tint memory: %w", err)
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("tint memory: %w", err)
	}
	defer func() { _ = lock.Close() }()
	deadline := time.Now().Add(tintsWait)
	for {
		err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
			return fmt.Errorf("tint memory: lock: %w", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("tint memory: lock: %w", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	m, err := readTints(path)
	if err != nil {
		return err
	}
	b := &tintBook{path: path, sessions: m}
	ferr := fn(b)
	if err := b.Flush(); err != nil {
		return errors.Join(ferr, err)
	}
	return ferr
}

// rememberedTint is what qm last set on tab id, if it remembers.
func rememberedTint(id string) (Tint, bool, error) {
	path, err := tintsFile()
	if err != nil {
		return Tint{}, false, fmt.Errorf("tint memory: %w", err)
	}
	m, err := readTints(path)
	if err != nil {
		return Tint{}, false, err
	}
	e, ok := m[id]
	return e.Tint, ok, nil
}

func readTints(path string) (map[string]tintEntry, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]tintEntry{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("tint memory: %w", err)
	}
	var f struct {
		Sessions map[string]tintEntry `json:"sessions"`
	}
	if json.Unmarshal(b, &f) != nil || f.Sessions == nil {
		return map[string]tintEntry{}, nil
	}
	return f.Sessions, nil
}

func writeTints(path string, m map[string]tintEntry) (err error) {
	b, err := json.MarshalIndent(struct {
		Sessions map[string]tintEntry `json:"sessions"`
	}{m}, "", "  ")
	if err != nil {
		return fmt.Errorf("tint memory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tints-*.json")
	if err != nil {
		return fmt.Errorf("tint memory: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(append(b, '\n')); err == nil {
		if err = tmp.Chmod(0o600); err == nil {
			if err = tmp.Sync(); err == nil {
				if err = tmp.Close(); err == nil {
					err = os.Rename(tmp.Name(), path)
				}
			}
		}
	}
	if err != nil {
		return fmt.Errorf("tint memory: %w", err)
	}
	return nil
}
