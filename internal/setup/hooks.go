package setup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/launch"
)

// hookEvent is agterm's event at its launch, and hookMark ends the line init
// adds to hooks.conf (a comment for the hook's shell).
const (
	hookEvent = "app.launched"
	hookMark  = "# added by qm init"
)

// agtermWait bounds each agtermctl call of init.
const agtermWait = 5 * time.Second

// agtermCtl drives agterm; tests replace it.
var agtermCtl = launch.Agterm{Bin: "agtermctl", Run: launch.Exec}

// hookLine is the hooks.conf line that runs this qm (exe) at agterm's
// launch; "" when its path cannot be on one line.
func hookLine(exe string) string {
	if strings.ContainsAny(exe, "\r\n") {
		return ""
	}
	return "on " + hookEvent + " " + launch.Quote(exe) + " up note --notify " + hookMark
}

// offerHook offers to add the agterm hook that says, at agterm's next
// launch, which projects an agterm quit without qm down left behind. It is
// offered when agterm is the launcher (choice is the config's), runs, and
// knows the event. The line is appended to hooks.conf, whose other lines are
// not touched; one that is a symbolic link or cannot be written is left
// alone. It returns an error only when the record of the change fails.
func offerHook(ctx context.Context, p prompter, choice string, rec *Record) error {
	call := func() (context.Context, context.CancelFunc) { return context.WithTimeout(ctx, agtermWait) }
	c, cancel := call()
	defer cancel()
	hooks, hooksErr := agtermCtl.Hooks(c)
	l, err := launch.PickUp(choice, os.Getenv, func() bool { return hooksErr == nil }, func(bin string) bool {
		_, err := exec.LookPath(bin)
		return err == nil
	})
	if _, ok := l.(launch.Agterm); err != nil || !ok {
		return nil
	}
	if hooksErr != nil {
		say(p.out, "agterm hook: not offered (%v): it needs a running agterm that has event hooks; run qm init again then\n", hooksErr)
		return nil
	}
	c, cancel = call()
	defer cancel()
	if !agtermCtl.KnowsEvent(c, hookEvent) {
		say(p.out, "agterm hook: not offered: this agterm does not know %s\n", hookEvent)
		return nil
	}
	exe, err := executable()
	if err != nil {
		return nil
	}
	line := hookLine(exe)
	if line == "" {
		say(p.out, "agterm hook: not offered: qm's path has a line break\n")
		return nil
	}
	path := hooks.Path
	fi, err := os.Lstat(path)
	missing := errors.Is(err, fs.ErrNotExist)
	switch {
	case missing:
	case err != nil:
		hookAlone(p, path, err.Error(), line)
		return nil
	case fi.Mode()&fs.ModeSymlink != 0:
		hookAlone(p, path, "it is a symbolic link", line)
		return nil
	case !fi.Mode().IsRegular():
		hookAlone(p, path, "it is not a plain file", line)
		return nil
	}
	var data []byte
	if !missing {
		if data, err = os.ReadFile(path); err != nil {
			hookAlone(p, path, err.Error(), line)
			return nil
		}
	}
	switch old := hooked(data); {
	case old == "":
	case old != line && old == rec.HookLine && path == rec.HooksPath:
		return repointHook(p, call, line, rec)
	default:
		say(p.out, "agterm hook: %s already runs qm up note:\n  %s\n", path, old)
		return nil
	}
	// What cannot be written is not asked about.
	if missing {
		err = writableDir(filepath.Dir(path))
	} else {
		err = writableHooks(path)
	}
	if err != nil {
		hookAlone(p, path, err.Error(), line)
		return nil
	}
	say(p.out, "agterm hook: after an agterm quit without qm down, its next launch notifies which projects qm up restores (qm up note lists them). Add to %s:\n+ %s\n", path, line)
	if !p.ask("Add this line?") {
		say(p.out, "agterm hook: not added\n")
		return nil
	}
	f, created, err := openHooks(path)
	if err != nil {
		hookAlone(p, path, err.Error(), line)
		return nil
	}
	defer func() { _ = f.Close() }()
	// Read again: the answer may have taken long.
	if data, err = io.ReadAll(f); err != nil {
		hookAlone(p, path, err.Error(), line)
		return nil
	}
	if old := hooked(data); old != "" {
		say(p.out, "agterm hook: %s already runs qm up note:\n  %s\n", path, old)
		return nil
	}
	// takeBack leaves the file as it was found.
	takeBack := func() {
		if created {
			_ = os.Remove(path)
		} else {
			_ = f.Truncate(int64(len(data)))
		}
	}
	add, before := line+"\n", ""
	if len(data) > 0 && data[len(data)-1] != '\n' {
		add, before = "\n"+add, sha(data)
	}
	// Recorded before the write, as the other changes are.
	rec.HookCreated = created || rec.HookCreated && rec.HooksPath == path
	rec.HooksPath, rec.HookLine, rec.HookBefore = path, line, before
	if err := writeRecord(*rec); err != nil {
		takeBack()
		return err
	}
	if _, err := io.WriteString(f, add); err != nil {
		takeBack()
		say(p.out, "agterm hook: %s not written: %v\n", path, err)
		rec.clearHook()
		return writeRecord(*rec)
	}
	say(p.out, "agterm hook: added\n")
	return reloadHooks(call, p, rec)
}

// repointHook offers to run this qm in the line init added, which runs
// another one (qm was installed another way since); no other line changes.
func repointHook(p prompter, call func() (context.Context, context.CancelFunc), line string, rec *Record) error {
	say(p.out, "agterm hook: it runs another qm than this one. Change in %s:\n- %s\n+ %s\n", rec.HooksPath, rec.HookLine, line)
	if !p.ask("Write this change?") {
		say(p.out, "agterm hook: left as it is\n")
		return nil
	}
	old := rec.HookLine
	rec.HookLine = line
	if err := writeRecord(*rec); err != nil {
		return err
	}
	if _, err := editHook(rec.HooksPath, old, line); err != nil {
		say(p.out, "agterm hook: %s left alone (%v)\n", rec.HooksPath, err)
		rec.HookLine = old
		return writeRecord(*rec)
	}
	say(p.out, "agterm hook: written\n")
	return reloadHooks(call, p, rec)
}

// reloadHooks makes agterm read the line just added. A line agterm does not
// take is taken back.
func reloadHooks(call func() (context.Context, context.CancelFunc), p prompter, rec *Record) error {
	c, cancel := call()
	defer cancel()
	if err := agtermCtl.HooksReload(c); err != nil {
		say(p.out, "agterm hook: not reloaded (%v): agterm reads it at its next launch\n", err)
		return nil
	}
	c, cancel = call()
	defer cancel()
	hooks, err := agtermCtl.Hooks(c)
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(rec.HooksPath)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(data), "\n")
	for i, l := range slices.Backward(lines) {
		if l != rec.HookLine {
			continue
		}
		why, refused := hooks.Refused[i+1]
		if !refused {
			return nil
		}
		say(p.out, "agterm hook: agterm does not take the line (%s): taken back\n", why)
		if !removeHook(p, *rec) {
			return nil
		}
		rec.clearHook()
		return writeRecord(*rec)
	}
	return nil
}

// hookAlone says hooks.conf is left alone, why, and the line to add by hand.
func hookAlone(p prompter, path, why, line string) {
	say(p.out, "agterm hook: %s left alone (%s). To be told, at agterm's launch, which projects qm up restores, add to it:\n  %s\n", path, why, line)
}

// hooked is the line of hooks.conf that runs qm up note at agterm's launch
// already ("" when none does), the last one if several do.
func hooked(conf []byte) string {
	found := ""
	for l := range bytes.Lines(conf) {
		f := strings.Fields(string(l))
		if len(f) > 2 && f[0] == "on" && f[1] == hookEvent && strings.Contains(strings.Join(f, " "), " up note --notify") {
			found = strings.TrimRight(string(l), "\r\n")
		}
	}
	return found
}

// clearHook forgets the hook line in r.
func (r *Record) clearHook() {
	r.HooksPath, r.HookLine, r.HookBefore, r.HookCreated = "", "", "", false
}

// writableDir is an error when a file cannot be made in dir.
func writableDir(dir string) error {
	const write, search = 2, 1 // access(2)'s W_OK and X_OK
	if err := syscall.Access(dir, write|search); err != nil {
		return &fs.PathError{Op: "write to", Path: dir, Err: err}
	}
	return nil
}

// writableHooks is an error when the hooks file path cannot be written to.
func writableHooks(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	return f.Close()
}

// openHooks opens the hooks file path for reading and appending, and makes a
// missing one (created); a symbolic link is an error.
func openHooks(path string) (f *os.File, created bool, err error) {
	const flag = os.O_RDWR | os.O_APPEND | syscall.O_NOFOLLOW
	f, err = os.OpenFile(path, flag, 0)
	if errors.Is(err, fs.ErrNotExist) {
		f, err = os.OpenFile(path, flag|os.O_CREATE|os.O_EXCL, 0o644)
		created = err == nil
	}
	return f, created, err
}

// errNoLine: the line is not in the hooks file as init wrote it.
var errNoLine = errors.New("the line is not in it as init wrote it")

// editHook replaces the last line of the hooks file path that is exactly old
// by line, or removes it when line is "", and changes nothing else. It
// returns what the file now holds, not written when that is nothing. A
// symbolic link, another kind of file and one that cannot be written to are
// errors.
func editHook(path, old, line string) (left []byte, err error) {
	// Read first, through a link too: a file that does not hold the line any
	// more needs no change, whatever it is.
	if fi, err := os.Stat(path); err != nil {
		return nil, err
	} else if !fi.Mode().IsRegular() {
		return nil, errors.New("it is not a plain file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := slices.Collect(bytes.Lines(data))
	at := -1
	for i, l := range lines {
		if strings.TrimSuffix(string(l), "\n") == old {
			at = i
		}
	}
	if at < 0 {
		return nil, errNoLine
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New("it is a symbolic link")
	}
	// Replaced by a rename, which a read-only file would not refuse.
	if err := writableHooks(path); err != nil {
		return nil, err
	}
	if line != "" {
		lines[at] = append([]byte(line), lines[at][len(old):]...)
	} else {
		lines = slices.Delete(lines, at, at+1)
	}
	left = bytes.Join(lines, nil)
	if len(left) == 0 {
		return nil, nil
	}
	return left, writeFileAtomic(path, left, fi.Mode().Perm())
}

// removeHook takes the line init added out of hooks.conf, and nothing else:
// the line break init put before it only while the rest is what init found,
// and the file only when init created it and nothing else is in it. agterm
// then reads the file again. It reports whether the line is gone.
func removeHook(p prompter, rec Record) bool {
	path := rec.HooksPath
	left, err := editHook(path, rec.HookLine, "")
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return true
	case err == nil && len(left) == 0 && rec.HookCreated:
		err = os.Remove(path)
	case err == nil && len(left) == 0:
		err = os.Truncate(path, 0)
	case err == nil && rec.HookBefore != "":
		if was := bytes.TrimSuffix(left, []byte("\n")); sha(was) == rec.HookBefore {
			err = os.Truncate(path, int64(len(was)))
		}
	}
	if err != nil {
		say(p.out, "agterm hook: %s left alone (%v); the line init added was:\n  %s\n", path, err, rec.HookLine)
		return errors.Is(err, errNoLine)
	}
	say(p.out, "agterm hook: removed from %s\n", path)
	ctx, cancel := context.WithTimeout(context.Background(), agtermWait)
	defer cancel()
	_ = agtermCtl.HooksReload(ctx) // an agterm that is not running reads it at launch
	return true
}
